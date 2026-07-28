//go:build linux

package vfhygiene

import (
	"context"
	"time"

	"github.com/vishvananda/netlink"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Watcher turns VF release into an event-driven operation instead of a polled
// one.
//
// Kubernetes provides no release notification: the kubelet device-plugin API
// has an Allocate RPC and no Free/Deallocate RPC at all, so neither the device
// plugin nor the operator is ever told that a VF came back. sriov-cni's CNI DEL
// is the only in-band signal, and it is precisely the path that does not run
// when a workload terminates abruptly.
//
// The kernel, however, does signal it. When a pod's network namespace is
// destroyed -- on graceful exit and on crash alike -- the kernel returns the
// VF's netdev to the initial namespace, which emits RTM_NEWLINK there. That is
// the real "device freed" event, and it is what this watcher subscribes to.
//
// Coverage boundary, and why the periodic sweep still exists:
//
//   - A VF bound to a userspace driver (vfio-pci) has no netdev, so its release
//     produces no link event. Nothing to subscribe to.
//   - Events that occur while the daemon is restarting are missed outright;
//     netlink delivers no history.
//   - The socket can overrun under load, in which case the kernel drops events.
//
// So: the watcher provides timeliness, the sweep provides completeness. Neither
// alone is sufficient, and both funnel into the same locked, allocation-checked
// path so they cannot fight each other.
type Watcher struct {
	sweeper *Sweeper
	lister  VFLister

	// retryDelay is how long to wait before re-examining a VF that was busy
	// when its event arrived (typically because sriov-cni still held the
	// device lock mid-DEL).
	retryDelay time.Duration
	// maxRetries bounds per-event retries; the periodic sweep is the
	// backstop beyond that.
	maxRetries int
}

// NewWatcher returns a Watcher feeding into the given Sweeper.
func NewWatcher(sweeper *Sweeper, lister VFLister) *Watcher {
	return &Watcher{
		sweeper:    sweeper,
		lister:     lister,
		retryDelay: 2 * time.Second,
		maxRetries: 5,
	}
}

// Run subscribes to kernel link events and cleans released VFs as they come
// back, until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	funcLog := log.Log.WithName("vfhygiene-watch")
	funcLog.V(0).Info("subscribing to kernel link events for VF release")

	updates := make(chan netlink.LinkUpdate, 256)
	done := make(chan struct{})
	defer close(done)

	if err := netlink.LinkSubscribeWithOptions(updates, done, netlink.LinkSubscribeOptions{
		// A dropped event must not be silent: it is a missed cleanup that
		// only the periodic sweep will now catch.
		ErrorCallback: func(err error) {
			funcLog.Error(err, "link event subscription error; the periodic sweep remains the backstop")
		},
	}); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			funcLog.V(0).Info("stopping link event watcher")
			return ctx.Err()

		case update, ok := <-updates:
			if !ok {
				funcLog.V(0).Info("link event channel closed")
				return nil
			}
			w.handleLinkEvent(ctx, update)
		}
	}
}

// handleLinkEvent evaluates a single kernel link event.
//
// The event only tells us "something about this link changed". Whether the link
// is a VF we manage, whether it is genuinely released, and whether it is dirty
// are all decided by the same locked path the periodic sweep uses -- so an
// event that arrives while the VF is being handed to a new pod is safely
// ignored rather than acted on.
func (w *Watcher) handleLinkEvent(ctx context.Context, update netlink.LinkUpdate) {
	funcLog := log.Log.WithName("vfhygiene-watch")

	name := update.Link.Attrs().Name
	if name == "" {
		return
	}

	vf, found, err := w.findVFByNetdev(name)
	if err != nil {
		funcLog.V(2).Info("failed to map link to a VF", "netdev", name, "error", err.Error())
		return
	}
	if !found {
		// Not a VF of a PF we manage.
		return
	}

	funcLog.V(2).Info("link event for a managed VF", "netdev", name, "vf", vf.PCIAddress)
	w.processWithRetry(ctx, vf)
}

// processWithRetry runs the cleanup for one VF, retrying briefly when the
// device is busy.
//
// The retry matters because the release event and sriov-cni's CNI DEL race by
// construction: the kernel emits the link event as the namespace goes away,
// while the CNI may still hold the device lock finishing its own teardown. The
// first attempt is then correctly refused, and a short retry catches it without
// waiting for the next sweep.
func (w *Watcher) processWithRetry(ctx context.Context, vf VF) {
	funcLog := log.Log.WithName("vfhygiene-watch")

	for attempt := 0; attempt <= w.maxRetries; attempt++ {
		res := Result{}
		err := w.sweeper.processReleasedVF(vf, &res)
		if err == nil {
			// The locked check confirmed there is no live consumer, so this
			// kernel event really is a release. Surface it to the cluster as
			// a Kubernetes Event: nothing else in Kubernetes reports that a
			// VF came back, because the device-plugin API has no Free RPC.
			w.sweeper.recorder.VFReleased(vf)

			if res.Restored > 0 {
				funcLog.V(0).Info("released VF cleaned on kernel link event",
					"vf", vf.PCIAddress, "netdev", vf.NetdevName)
			}
			return
		}

		// Allocated to a live consumer: nothing to do, and retrying would
		// be wrong.
		if err == errReallocated {
			return
		}
		// Anything other than a busy device is not worth retrying here;
		// the periodic sweep will re-examine it.
		if err != errDeviceBusy {
			funcLog.V(2).Info("VF not cleaned on event",
				"vf", vf.PCIAddress, "reason", err.Error())
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(w.retryDelay):
		}
	}

	funcLog.V(2).Info("VF still busy after retries, leaving it to the periodic sweep",
		"vf", vf.PCIAddress)
}

// findVFByNetdev maps a netdev name to a managed VF.
func (w *Watcher) findVFByNetdev(name string) (VF, bool, error) {
	vfs, err := w.lister.ListVFs()
	if err != nil {
		return VF{}, false, err
	}
	for _, vf := range vfs {
		if vf.NetdevName == name {
			return vf, true, nil
		}
	}
	return VF{}, false, nil
}
