package vfhygiene

import (
	"context"
	"errors"
	"testing"
)

// captureRecorder records callbacks for assertions.
type captureRecorder struct {
	released []VF
	restored []VF
	failed   []VF
}

func (c *captureRecorder) VFReleased(vf VF)                { c.released = append(c.released, vf) }
func (c *captureRecorder) VFRestored(vf VF, _ []Attribute) { c.restored = append(c.restored, vf) }
func (c *captureRecorder) VFRestoreFailed(vf VF, _ []Attribute, _ error) {
	c.failed = append(c.failed, vf)
}

// TestWatcherEmitsReleaseEvent covers the ask directly: a captured kernel
// release event must produce a release notification for the cluster.
func TestWatcherEmitsReleaseEvent(t *testing.T) {
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}
	lister := &fakeLister{vfs: []VF{testVF()}}
	rec := &captureRecorder{}

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), rec, Config{})
	w := NewWatcher(s, lister)

	w.processWithRetry(context.Background(), testVF())

	if len(rec.released) != 1 {
		t.Fatalf("expected one release notification, got %d", len(rec.released))
	}
	if rec.released[0].PCIAddress != testVFPCI {
		t.Fatalf("release reported for the wrong VF: %+v", rec.released[0])
	}
	if len(rec.restored) != 1 {
		t.Fatalf("expected one restore notification, got %d", len(rec.restored))
	}
}

// TestWatcherEmitsReleaseEventForCleanVF: a VF released without being dirtied
// still produces the release notification, because the event is about the
// lifecycle transition, not about remediation.
func TestWatcherEmitsReleaseEventForCleanVF(t *testing.T) {
	st := newFakeState()
	st.state[testVFPCI] = defaultKnown().state
	lister := &fakeLister{vfs: []VF{testVF()}}
	rec := &captureRecorder{}

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), rec, Config{})
	NewWatcher(s, lister).processWithRetry(context.Background(), testVF())

	if len(rec.released) != 1 {
		t.Fatalf("expected a release notification for a clean VF, got %d", len(rec.released))
	}
	if len(rec.restored) != 0 {
		t.Fatalf("clean VF must not report a restore, got %d", len(rec.restored))
	}
}

// TestWatcherEmitsNoReleaseEventForAllocatedVF: an event for a VF still in use
// is not a release and must not be reported as one.
func TestWatcherEmitsNoReleaseEventForAllocatedVF(t *testing.T) {
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}
	lister := &fakeLister{vfs: []VF{testVF()}}
	rec := &captureRecorder{}
	alloc := &fakeAllocation{allocated: map[string]bool{testVFPCI: true}}

	s := New(lister, alloc, st, nil, defaultKnown(), rec, Config{})
	NewWatcher(s, lister).processWithRetry(context.Background(), testVF())

	if len(rec.released) != 0 {
		t.Fatalf("an in-use VF must not be reported as released, got %d", len(rec.released))
	}
}

func TestSweepReportsRestoreFailure(t *testing.T) {
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}
	st.restoreEr[testVFPCI] = errors.New("boom")
	rec := &captureRecorder{}

	s := New(&fakeLister{vfs: []VF{testVF()}}, &fakeAllocation{}, st, nil, defaultKnown(), rec, Config{})
	if _, err := s.SweepOnce(); err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	if len(rec.failed) != 1 {
		t.Fatalf("expected a failure notification, got %d", len(rec.failed))
	}
}
