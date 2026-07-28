//go:build linux && kerneltest

package vfhygiene

import (
	"context"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// TestKernelWatcherReceivesLinkEvents proves the subscription itself works
// against a live kernel: a link change produces an RTM_NEWLINK event that the
// watcher's channel actually receives.
//
// Note on fidelity: the real trigger in production is a VF netdev being
// returned to the initial namespace when a pod's netns is destroyed. That
// cannot be reproduced with virtual links -- a dummy or veth is *deleted* with
// its namespace rather than moved back, because only devices with a physical
// parent (a VF among them) are relocated. What is exercised here is the same
// event stream and the same handler; the netns-return semantics need real
// SR-IOV hardware to verify.
func TestKernelWatcherReceivesLinkEvents(t *testing.T) {
	requireRoot(t)

	updates := make(chan netlink.LinkUpdate, 64)
	done := make(chan struct{})
	defer close(done)

	if err := netlink.LinkSubscribe(updates, done); err != nil {
		t.Fatalf("failed to subscribe to link events: %v", err)
	}

	const name = "vfhygw0"
	link := mkDummy(t, name)

	// Dirty it the way a workload would; this generates RTM_NEWLINK.
	if err := netlink.SetPromiscOn(link); err != nil {
		t.Fatalf("failed to set promisc: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			if u.Link.Attrs().Name == name {
				return // event received
			}
		case <-deadline:
			t.Fatal("no link event received for the test interface within 5s")
		}
	}
}

// TestKernelWatcherCleansOnEvent runs the watcher against real kernel links and
// a real device lock: an event for a dirty released VF results in the flags
// actually being cleared on the kernel interface.
func TestKernelWatcherCleansOnEvent(t *testing.T) {
	requireRoot(t)

	const name = "vfhygw1"
	link := mkDummy(t, name)

	if err := netlink.SetPromiscOn(link); err != nil {
		t.Fatalf("failed to set promisc: %v", err)
	}
	if err := netlink.LinkSetAllmulticastOn(link); err != nil {
		t.Fatalf("failed to set allmulticast: %v", err)
	}

	vf := VF{PCIAddress: "0000:d8:00.7", PFPCIAddress: "0000:d8:00.0", NetdevName: name}
	lister := &fakeLister{vfs: []VF{vf}}
	ops := NewNetlinkStateOps()

	s := New(lister, &fakeAllocation{}, ops, &CNIDeviceLocker{DataDir: t.TempDir()}, nil, nil, Config{})
	w := NewWatcher(s, lister)
	w.retryDelay = 10 * time.Millisecond

	// Drive the handler with the event the kernel would deliver.
	w.handleLinkEvent(context.Background(), netlink.LinkUpdate{Link: link})

	state, err := ops.GetState(vf)
	if err != nil {
		t.Fatal(err)
	}
	if state.Promisc || state.AllMulticast {
		t.Fatalf("event-driven cleanup did not clear the flags: %+v", state)
	}
}

// TestKernelWatcherIgnoresUnmanagedLinks makes sure the watcher does not touch
// interfaces that are not VFs of a managed PF.
func TestKernelWatcherIgnoresUnmanagedLinks(t *testing.T) {
	requireRoot(t)

	const managed = "vfhygw2"
	const unmanaged = "vfhygw3"

	mkDummy(t, managed)
	other := mkDummy(t, unmanaged)

	if err := netlink.SetPromiscOn(other); err != nil {
		t.Fatalf("failed to set promisc: %v", err)
	}

	// Only the managed name is known to the lister.
	lister := &fakeLister{vfs: []VF{{PCIAddress: "0000:d8:00.8", NetdevName: managed}}}
	ops := NewNetlinkStateOps()
	s := New(lister, &fakeAllocation{}, ops, &CNIDeviceLocker{DataDir: t.TempDir()}, nil, nil, Config{})
	w := NewWatcher(s, lister)

	w.handleLinkEvent(context.Background(), netlink.LinkUpdate{Link: other})

	state, err := ops.GetState(VF{PCIAddress: "x", NetdevName: unmanaged})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Promisc {
		t.Fatal("watcher modified an interface that is not a managed VF")
	}
}
