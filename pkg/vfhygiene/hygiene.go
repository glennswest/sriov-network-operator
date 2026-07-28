package vfhygiene

import (
	"context"
	"errors"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	// errDeviceBusy means another component holds the VF's device lock.
	errDeviceBusy = errors.New("device is locked by another component")
	// errReallocated means the VF was allocated between the unlocked
	// pre-check and taking the lock.
	errReallocated = errors.New("VF was re-allocated before the lock was taken")
)

// Config configures a Sweeper.
type Config struct {
	// Interval is the sweep period. Zero means DefaultInterval.
	Interval time.Duration
	// Scope lists the attributes the sweep restores. Nil means AllAttributes.
	Scope []Attribute
	// DryRun reports what would be restored without changing anything.
	DryRun bool
}

// Sweeper periodically restores released VFs that were left dirty by a departed
// consumer.
type Sweeper struct {
	lister    VFLister
	allocated AllocationChecker
	state     StateOps
	locker    DeviceLocker
	known     KnownValuesProvider
	recorder  Recorder
	cfg       Config
}

// Result summarises one sweep.
type Result struct {
	// Scanned is the number of VFs discovered.
	Scanned int
	// Skipped is the number of VFs skipped because they are allocated, busy,
	// or could not be evaluated.
	Skipped int
	// Dirty is the number of released VFs found not in the known state.
	Dirty int
	// Restored is the number of VFs successfully restored (0 in dry-run).
	Restored int
	// Errors is the number of VFs whose restore failed.
	Errors int
}

// New returns a Sweeper. locker, known and recorder may be nil; a nil locker
// disables device locking, which is only safe in dry-run and in tests, and a
// nil known-values provider uses the standard one.
func New(lister VFLister, allocated AllocationChecker, state StateOps, locker DeviceLocker,
	known KnownValuesProvider, recorder Recorder, cfg Config) *Sweeper {
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Scope == nil {
		cfg.Scope = AllAttributes
	}
	if recorder == nil {
		recorder = nopRecorder{}
	}
	if locker == nil {
		locker = noopLocker{}
	}
	if known == nil {
		known = NewStandardKnownValues()
	}
	return &Sweeper{
		lister:    lister,
		allocated: allocated,
		state:     state,
		locker:    locker,
		known:     known,
		recorder:  recorder,
		cfg:       cfg,
	}
}

// Run sweeps on a timer until ctx is cancelled.
//
// This is the feature's own trigger, and it is intentionally independent of the
// SriovNetworkNodeState reconcile loop: a VF is dirtied at pod teardown, which
// produces no nodeState event, so an event-driven check would never fire.
func (s *Sweeper) Run(ctx context.Context) error {
	funcLog := log.Log.WithName("vfhygiene")
	funcLog.V(0).Info("starting VF hygiene sweep",
		"interval", s.cfg.Interval, "scope", s.cfg.Scope, "dry-run", s.cfg.DryRun)

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			funcLog.V(0).Info("stopping VF hygiene sweep")
			return ctx.Err()
		case <-ticker.C:
			res, err := s.SweepOnce()
			if err != nil {
				funcLog.Error(err, "sweep failed")
				continue
			}
			if res.Dirty > 0 || res.Errors > 0 {
				funcLog.V(0).Info("sweep completed",
					"scanned", res.Scanned, "skipped", res.Skipped,
					"dirty", res.Dirty, "restored", res.Restored, "errors", res.Errors)
			} else {
				funcLog.V(2).Info("sweep completed, nothing to do", "scanned", res.Scanned)
			}
		}
	}
}

// SweepOnce performs a single pass and returns what it found.
//
// The pass never unbinds a VF and never requests a node drain: it only restores
// state on VFs whose consumer is already gone.
func (s *Sweeper) SweepOnce() (Result, error) {
	funcLog := log.Log.WithName("vfhygiene")
	res := Result{}

	vfs, err := s.lister.ListVFs()
	if err != nil {
		return res, err
	}
	res.Scanned = len(vfs)

	for _, vf := range vfs {
		// First pass, unlocked: cheap filter so the sweep does not take a
		// lock on every VF on the node. The authoritative check happens
		// again below, under the lock.
		allocated, err := s.allocated.IsAllocated(vf.PCIAddress)
		if err != nil {
			funcLog.V(2).Info("skipping VF, allocation state unknown",
				"vf", vf.PCIAddress, "error", err.Error())
			res.Skipped++
			continue
		}
		if allocated {
			res.Skipped++
			continue
		}

		if err := s.processReleasedVF(vf, &res); err != nil {
			funcLog.V(2).Info("skipping VF", "vf", vf.PCIAddress, "reason", err.Error())
		}
	}

	return res, nil
}

// processReleasedVF evaluates and, if required, restores a single VF that the
// unlocked pre-check believed to be released.
//
// Everything that matters happens under the per-device lock: a VF can be handed
// to a new pod at any moment, so observing it as free and then writing to it
// without holding the lock would be a check-then-act race that could revert
// settings a new consumer had legitimately applied. Holding the same lock
// sriov-cni takes for CNI ADD/DEL makes the sequence atomic with respect to
// allocation.
func (s *Sweeper) processReleasedVF(vf VF, res *Result) error {
	funcLog := log.Log.WithName("vfhygiene")

	unlock, locked, err := s.locker.TryLock(vf.PCIAddress)
	if err != nil {
		res.Skipped++
		return err
	}
	if !locked {
		// sriov-cni is mid ADD/DEL on this device. Leave it alone; the
		// next sweep will pick it up.
		res.Skipped++
		return errDeviceBusy
	}
	defer unlock()

	// Re-check allocation now that the lock is held. If a pod was scheduled
	// onto this VF between the unlocked pre-check and here, CNI ADD has
	// either already recorded the allocation (this returns true) or is
	// blocked on the lock we now hold (and will observe our completed work).
	allocated, err := s.allocated.IsAllocated(vf.PCIAddress)
	if err != nil {
		res.Skipped++
		return err
	}
	if allocated {
		res.Skipped++
		return errReallocated
	}

	// Re-read state under the lock as well: the pre-check told us nothing
	// about the current settings.
	current, err := s.state.GetState(vf)
	if err != nil {
		res.Skipped++
		return err
	}

	// The target is the same known state for every VF of this device: what a
	// consumer must be able to assume when it opens the device. It does not
	// depend on having seen this VF clean beforehand.
	target, err := s.known.KnownValues(vf)
	if err != nil {
		res.Skipped++
		return err
	}

	dirty := current.Diff(&target, s.cfg.Scope)
	if len(dirty) == 0 {
		return nil
	}
	res.Dirty++

	funcLog.V(0).Info("released VF is not in the known state",
		"vf", vf.PCIAddress, "pf", vf.PFPCIAddress, "netdev", vf.NetdevName,
		"attributes", dirty, "dry-run", s.cfg.DryRun)

	if s.cfg.DryRun {
		return nil
	}

	applied, err := s.state.Restore(vf, target, dirty)
	if err != nil {
		funcLog.Error(err, "failed to restore released VF",
			"vf", vf.PCIAddress, "netdev", vf.NetdevName,
			"attempted", dirty, "applied", applied)
		s.recorder.VFRestoreFailed(vf, dirty, err)
		res.Errors++
		return err
	}

	// Report what was actually written. Attributes this driver does not
	// implement are skipped inside Restore, so reporting the attempted set
	// would overstate what happened.
	if len(applied) == 0 {
		funcLog.V(2).Info("no attributes were applicable to this device",
			"vf", vf.PCIAddress, "attempted", dirty)
		return nil
	}

	s.recorder.VFRestored(vf, applied)
	res.Restored++
	return nil
}

type nopRecorder struct{}

func (nopRecorder) VFReleased(VF)                          {}
func (nopRecorder) VFRestored(VF, []Attribute)             {}
func (nopRecorder) VFRestoreFailed(VF, []Attribute, error) {}
