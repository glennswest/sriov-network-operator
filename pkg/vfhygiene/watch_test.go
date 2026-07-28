package vfhygiene

import (
	"context"
	"testing"
	"time"
)

// TestWatcherCleansOnEvent covers the event path end to end with fakes: a
// release event for a dirty, released VF results in a restore.
func TestWatcherCleansOnEvent(t *testing.T) {
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}
	lister := &fakeLister{vfs: []VF{testVF()}}

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{})
	w := NewWatcher(s, lister)

	w.processWithRetry(context.Background(), testVF())

	if st.state[testVFPCI].Promisc {
		t.Fatal("released VF was not cleaned on the event path")
	}
}

// TestWatcherDoesNotCleanAllocatedVF proves the event path shares the sweep's
// safety: an event for a VF that is in use must not touch it.
func TestWatcherDoesNotCleanAllocatedVF(t *testing.T) {
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}
	lister := &fakeLister{vfs: []VF{testVF()}}
	alloc := &fakeAllocation{allocated: map[string]bool{testVFPCI: true}}

	s := New(lister, alloc, st, nil, defaultKnown(), nil, Config{})
	w := NewWatcher(s, lister)

	w.processWithRetry(context.Background(), testVF())

	if !st.state[testVFPCI].Promisc {
		t.Fatal("event path modified a VF that is allocated to a live consumer")
	}
}

// TestWatcherRetriesWhileDeviceBusy simulates sriov-cni holding the device lock
// when the release event arrives: the first attempts are refused, and the
// watcher retries rather than dropping the cleanup on the floor.
func TestWatcherRetriesWhileDeviceBusy(t *testing.T) {
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}
	lister := &fakeLister{vfs: []VF{testVF()}}

	busy := &countingBusyLocker{refuseTimes: 2}
	s := New(lister, &fakeAllocation{}, st, busy, defaultKnown(), nil, Config{})
	w := NewWatcher(s, lister)
	w.retryDelay = time.Millisecond

	w.processWithRetry(context.Background(), testVF())

	if busy.attempts < 3 {
		t.Fatalf("expected retries while the device was busy, got %d attempts", busy.attempts)
	}
	if st.state[testVFPCI].Promisc {
		t.Fatal("VF was never cleaned despite the lock becoming free")
	}
}

// TestWatcherGivesUpAndLeavesItToTheSweep bounds the retry loop.
func TestWatcherGivesUpAndLeavesItToTheSweep(t *testing.T) {
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}
	lister := &fakeLister{vfs: []VF{testVF()}}

	busy := &countingBusyLocker{refuseTimes: 1000}
	s := New(lister, &fakeAllocation{}, st, busy, defaultKnown(), nil, Config{})
	w := NewWatcher(s, lister)
	w.retryDelay = time.Millisecond
	w.maxRetries = 3

	w.processWithRetry(context.Background(), testVF())

	if busy.attempts > 4 {
		t.Fatalf("retries not bounded: %d attempts", busy.attempts)
	}
	if !st.state[testVFPCI].Promisc {
		t.Fatal("VF should remain untouched while the device stayed busy")
	}
}

func TestWatcherFindsVFByNetdev(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}
	w := NewWatcher(New(lister, &fakeAllocation{}, newFakeState(), nil, defaultKnown(), nil, Config{}), lister)

	vf, found, err := w.findVFByNetdev("enp216s0f0v0")
	if err != nil || !found || vf.PCIAddress != testVFPCI {
		t.Fatalf("failed to map netdev to VF (found=%v err=%v vf=%+v)", found, err, vf)
	}

	if _, found, _ := w.findVFByNetdev("eth0"); found {
		t.Fatal("an unrelated netdev must not map to a managed VF")
	}
}

// countingBusyLocker refuses the lock a fixed number of times, then grants it.
type countingBusyLocker struct {
	refuseTimes int
	attempts    int
}

func (l *countingBusyLocker) TryLock(string) (func(), bool, error) {
	l.attempts++
	if l.attempts <= l.refuseTimes {
		return nil, false, nil
	}
	return func() {}, true, nil
}
