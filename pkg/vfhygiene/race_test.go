package vfhygiene

import (
	"os"
	"path/filepath"
	"testing"
)

// trackingLocker records lock activity and can simulate the device being
// allocated by another component while the lock is held.
type trackingLocker struct {
	locked      []string
	refuse      map[string]bool
	onLock      func(pci string)
	unlockCount int
}

func (l *trackingLocker) TryLock(pci string) (func(), bool, error) {
	if l.refuse[pci] {
		return nil, false, nil
	}
	l.locked = append(l.locked, pci)
	if l.onLock != nil {
		l.onLock(pci)
	}
	return func() { l.unlockCount++ }, true, nil
}

// TestSweepReChecksAllocationUnderLock is the important one: a VF that looks
// free during the unlocked pre-check gets allocated to a new pod before the
// sweep takes the lock. The sweep must notice and leave the VF alone, because
// the promiscuous flag now belongs to the new consumer.
func TestSweepReChecksAllocationUnderLock(t *testing.T) {
	const pci = testVFPCI

	lister := &fakeLister{vfs: []VF{testVF()}}
	alloc := &fakeAllocation{allocated: map[string]bool{pci: false}}
	st := newFakeState()
	st.state[pci] = State{HasNetdev: true, Promisc: true}

	// Simulate CNI ADD completing between the pre-check and the lock.
	locker := &trackingLocker{onLock: func(string) { alloc.allocated[pci] = true }}

	s := New(lister, alloc, st, locker, defaultKnown(), nil, Config{})
	res, err := s.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	if res.Restored != 0 || len(st.restores) != 0 {
		t.Fatalf("VF re-allocated before the lock must not be restored, got %+v", res)
	}
	if !st.state[pci].Promisc {
		t.Fatal("the new consumer's promiscuous flag was cleared — this is the race the lock exists to prevent")
	}
	if res.Skipped != 1 {
		t.Fatalf("expected the VF to be skipped, got %+v", res)
	}
}

// TestSweepSkipsDeviceHeldByAnotherComponent covers sriov-cni holding the lock
// mid ADD/DEL: the sweep must not block and must not touch the device.
func TestSweepSkipsDeviceHeldByAnotherComponent(t *testing.T) {
	const pci = testVFPCI

	lister := &fakeLister{vfs: []VF{testVF()}}
	st := newFakeState()
	st.state[pci] = State{HasNetdev: true, Promisc: true}
	locker := &trackingLocker{refuse: map[string]bool{pci: true}}

	s := New(lister, &fakeAllocation{}, st, locker, defaultKnown(), nil, Config{})
	res, _ := s.SweepOnce()

	if res.Restored != 0 || res.Skipped != 1 {
		t.Fatalf("busy device must be skipped, got %+v", res)
	}
	if !st.state[pci].Promisc {
		t.Fatal("busy device was modified")
	}
}

// TestSweepTakesAndReleasesLock checks the lock is actually taken for a VF that
// gets reset, and released afterwards.
func TestSweepTakesAndReleasesLock(t *testing.T) {
	const pci = testVFPCI

	lister := &fakeLister{vfs: []VF{testVF()}}
	st := newFakeState()
	st.state[pci] = State{HasNetdev: true, Promisc: true}
	locker := &trackingLocker{}

	s := New(lister, &fakeAllocation{}, st, locker, defaultKnown(), nil, Config{})
	if _, err := s.SweepOnce(); err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	if len(locker.locked) != 1 || locker.locked[0] != pci {
		t.Fatalf("expected the VF to be locked before restore, got %v", locker.locked)
	}
	if locker.unlockCount != 1 {
		t.Fatalf("expected the lock to be released exactly once, got %d", locker.unlockCount)
	}
}

// TestCNIDeviceLockerExcludesItself proves the real flock implementation
// actually excludes a second holder, which is what makes it safe against
// sriov-cni.
func TestCNIDeviceLockerExcludesItself(t *testing.T) {
	dir := t.TempDir()
	l := &CNIDeviceLocker{DataDir: dir}

	unlock, ok, err := l.TryLock("0000:d8:00.2")
	if err != nil || !ok {
		t.Fatalf("first lock should succeed (ok=%v err=%v)", ok, err)
	}

	// The lock file must exist where sriov-cni expects it.
	if _, err := os.Stat(filepath.Join(dir, "vf_lock", "0000:d8:00.2.lock")); err != nil {
		t.Fatalf("lock file not created at the sriov-cni path: %v", err)
	}

	// A different VF must remain lockable.
	unlockOther, ok, err := l.TryLock("0000:d8:00.3")
	if err != nil || !ok {
		t.Fatalf("locking a different VF should succeed (ok=%v err=%v)", ok, err)
	}
	unlockOther()

	unlock()
}
