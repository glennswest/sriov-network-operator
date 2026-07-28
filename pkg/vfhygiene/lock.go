package vfhygiene

import (
	"fmt"
	"os"
	"path/filepath"
)

// DeviceLocker serialises access to a VF against other components that
// configure the same device.
//
// This is what makes an asynchronous sweep safe. Without it the sweep is a
// check-then-act race: it could observe a VF as free, have the VF handed to a
// new pod, and then clear receive flags the *new* consumer had legitimately
// set. sriov-cni already takes an exclusive flock per VF for the whole duration
// of CNI ADD and DEL, so the sweep takes the same lock, in the same place, by
// the same protocol — rather than inventing a second scheme that would not
// actually exclude anything.
type DeviceLocker interface {
	// TryLock attempts to take the device lock without blocking. It returns
	// ok=false when the device is currently locked by someone else, in which
	// case the caller must leave the device alone this round.
	TryLock(pciAddress string) (unlock func(), ok bool, err error)
}

// CNIDeviceLocker takes the per-VF lock that sriov-cni uses, at
// <DataDir>/vf_lock/<pci>.lock.
type CNIDeviceLocker struct {
	// DataDir is sriov-cni's PCI data directory, DefaultCNIDataDir by default.
	DataDir string
}

// NewCNIDeviceLocker returns a locker using sriov-cni's default lock directory.
func NewCNIDeviceLocker() *CNIDeviceLocker {
	return &CNIDeviceLocker{DataDir: DefaultCNIDataDir}
}

// TryLock takes a non-blocking exclusive lock on the VF.
//
// Non-blocking is deliberate: if sriov-cni is mid-ADD or mid-DEL on this
// device, the right behaviour is to skip the VF and re-examine it next sweep,
// not to stall the sweep behind another component's work.
func (l *CNIDeviceLocker) TryLock(pciAddress string) (func(), bool, error) {
	dir := l.DataDir
	if dir == "" {
		dir = DefaultCNIDataDir
	}

	lockDir := filepath.Join(dir, "vf_lock")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, false, fmt.Errorf("failed to create lock directory %s: %w", lockDir, err)
	}

	return flockTry(filepath.Join(lockDir, fmt.Sprintf("%s.lock", pciAddress)))
}

// noopLocker is used when locking is disabled. It is not safe against
// concurrent CNI operations and exists only for dry-run and tests.
type noopLocker struct{}

func (noopLocker) TryLock(string) (func(), bool, error) { return func() {}, true, nil }
