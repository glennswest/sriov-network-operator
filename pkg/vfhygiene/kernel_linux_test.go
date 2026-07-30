//go:build linux && kerneltest

// Package-level integration test that exercises the real netlink code path
// against a live kernel. It creates dummy netdevs, dirties them exactly as a
// workload would (`ip link set X promisc on`), and asserts the sweep resets
// them.
//
// Requires root (CAP_NET_ADMIN). Run with:
//
//	go test -tags kerneltest ./pkg/vfhygiene/...
//
// No SR-IOV hardware is involved: the VF discovery layer is faked, while the
// netdev read/reset path under test is the real one.
package vfhygiene

import (
	"net"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("kernel test requires root for netlink link operations")
	}
}

// mkDummy creates a dummy netdev and registers its cleanup.
func mkDummy(t *testing.T, name string) netlink.Link {
	t.Helper()

	// Remove a leftover from a previous failed run.
	if old, err := netlink.LinkByName(name); err == nil {
		_ = netlink.LinkDel(old)
	}

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("failed to create dummy link %s: %v", name, err)
	}
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(l)
		}
	})

	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to look up dummy link %s: %v", name, err)
	}
	return l
}

func TestKernelGetStateReadsPromiscAndAllmulti(t *testing.T) {
	requireRoot(t)

	const name = "vfhyg0"
	link := mkDummy(t, name)
	ops := NewNetlinkStateOps()

	vf := VF{PCIAddress: "0000:d8:00.2", NetdevName: name}

	state, err := ops.GetState(vf)
	if err != nil {
		t.Fatalf("GetState failed: %v", err)
	}
	if state.Promisc || state.AllMulticast {
		t.Fatalf("fresh dummy should be clean, got %+v", state)
	}
	if !state.HasNetdev {
		t.Fatal("HasNetdev should be set for a VF with a netdev")
	}

	// Dirty it the way a workload does.
	if err := netlink.SetPromiscOn(link); err != nil {
		t.Fatalf("failed to set promisc on: %v", err)
	}
	if err := netlink.LinkSetAllmulticastOn(link); err != nil {
		t.Fatalf("failed to set allmulticast on: %v", err)
	}

	state, err = ops.GetState(vf)
	if err != nil {
		t.Fatalf("GetState failed: %v", err)
	}
	if !state.Promisc {
		t.Fatal("promiscuous mode set on the kernel link was not observed")
	}
	if !state.AllMulticast {
		t.Fatal("all-multicast set on the kernel link was not observed")
	}
}

func TestKernelResetClearsFlags(t *testing.T) {
	requireRoot(t)

	const name = "vfhyg1"
	link := mkDummy(t, name)
	ops := NewNetlinkStateOps()
	vf := VF{PCIAddress: "0000:d8:00.2", NetdevName: name}

	if err := netlink.SetPromiscOn(link); err != nil {
		t.Fatalf("failed to set promisc on: %v", err)
	}
	if err := netlink.LinkSetAllmulticastOn(link); err != nil {
		t.Fatalf("failed to set allmulticast on: %v", err)
	}

	applied, err := ops.Restore(vf, State{}, []Attribute{AttrPromisc, AttrAllMulticast})
	if err != nil {
		t.Fatalf("Restore failed: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("expected both attributes applied, got %v", applied)
	}

	state, err := ops.GetState(vf)
	if err != nil {
		t.Fatalf("GetState failed: %v", err)
	}
	if state.Promisc {
		t.Fatal("promiscuous mode was not cleared on the real link")
	}
	if state.AllMulticast {
		t.Fatal("all-multicast was not cleared on the real link")
	}

	// Confirm against the kernel directly, not just through our own reader.
	fresh, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to re-read link: %v", err)
	}
	if fresh.Attrs().Promisc != 0 {
		t.Fatalf("kernel still reports promisc=%d", fresh.Attrs().Promisc)
	}
}

// TestKernelEndToEndSweep runs the whole Sweeper against real kernel links:
// two dirty dummies, one standing in for a released VF and one for a VF still
// allocated to a live pod. Only the released one may be touched.
func TestKernelEndToEndSweep(t *testing.T) {
	requireRoot(t)

	const released = "vfhyg2"
	const inUse = "vfhyg3"

	relLink := mkDummy(t, released)
	useLink := mkDummy(t, inUse)

	for _, l := range []netlink.Link{relLink, useLink} {
		if err := netlink.SetPromiscOn(l); err != nil {
			t.Fatalf("failed to set promisc on %s: %v", l.Attrs().Name, err)
		}
	}

	lister := &fakeLister{vfs: []VF{
		{PCIAddress: "0000:d8:00.2", PFPCIAddress: "0000:d8:00.0", NetdevName: released},
		{PCIAddress: "0000:d8:00.3", PFPCIAddress: "0000:d8:00.0", NetdevName: inUse},
	}}
	relVF := VF{PCIAddress: "0000:d8:00.2", NetdevName: released}
	useVF := VF{PCIAddress: "0000:d8:00.3", NetdevName: inUse}
	alloc := &fakeAllocation{allocated: map[string]bool{
		"0000:d8:00.2": false, // consumer gone
		"0000:d8:00.3": true,  // live pod
	}}

	locker := &CNIDeviceLocker{DataDir: t.TempDir()}
	s := New(lister, alloc, NewNetlinkStateOps(), locker, defaultKnown(), nil, Config{})

	res, err := s.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce failed: %v", err)
	}
	if res.Restored != 1 {
		t.Fatalf("expected exactly 1 restore, got %+v", res)
	}

	relState, _ := NewNetlinkStateOps().GetState(relVF)
	if relState.Promisc {
		t.Fatal("released VF was not cleaned")
	}

	useState, _ := NewNetlinkStateOps().GetState(useVF)
	if !useState.Promisc {
		t.Fatal("in-use VF was cleaned — the sweep touched a live consumer's device")
	}
}

// TestKernelRestoresMTUAndMACFromBaseline proves the baseline-driven half of
// the feature against a real kernel: a workload changes MTU and MAC, and the
// sweep puts back exactly what was recorded.
func TestKernelRestoresMTUAndMACFromBaseline(t *testing.T) {
	requireRoot(t)

	const name = "vfhyg4"
	link := mkDummy(t, name)
	ops := NewNetlinkStateOps()
	vf := VF{PCIAddress: "0000:d8:00.4", NetdevName: name}

	// The known state for this device, as the real provider would compute it.
	pristine, err := ops.GetState(vf)
	if err != nil {
		t.Fatalf("GetState failed: %v", err)
	}
	known := &fixedKnown{state: State{
		HasNetdev: true, MTU: pristine.MTU, EffectiveMAC: pristine.EffectiveMAC,
	}}

	// A workload changes MTU, MAC and sets promiscuous mode.
	if err := netlink.LinkSetMTU(link, 9000); err != nil {
		t.Fatalf("failed to change MTU: %v", err)
	}
	newMAC, _ := net.ParseMAC("de:ad:be:ef:00:99")
	if err := netlink.LinkSetHardwareAddr(link, newMAC); err != nil {
		t.Fatalf("failed to change MAC: %v", err)
	}
	if err := netlink.SetPromiscOn(link); err != nil {
		t.Fatalf("failed to set promisc: %v", err)
	}

	s := New(
		&fakeLister{vfs: []VF{vf}},
		&fakeAllocation{},
		ops,
		&CNIDeviceLocker{DataDir: t.TempDir()},
		known,
		nil,
		Config{},
	)

	res, err := s.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce failed: %v", err)
	}
	if res.Restored != 1 {
		t.Fatalf("expected the VF to be restored, got %+v", res)
	}

	got, err := ops.GetState(vf)
	if err != nil {
		t.Fatal(err)
	}
	if got.MTU != pristine.MTU {
		t.Errorf("MTU not restored: got %d want %d", got.MTU, pristine.MTU)
	}
	if got.EffectiveMAC != pristine.EffectiveMAC {
		t.Errorf("MAC not restored: got %s want %s", got.EffectiveMAC, pristine.EffectiveMAC)
	}
	if got.Promisc {
		t.Error("promiscuous mode not cleared")
	}
}

// TestKernelClearsPromiscuityReferenceCount covers the fact that promiscuity is
// a reference count rather than a flag.
//
// IFLA_PROMISCUITY reports how many outstanding requests exist, and clearing
// IFF_PROMISC over rtnetlink decrements it by one. A workload that requested
// promiscuous mode more than once therefore leaves a count above one, and a
// single clear would leave the device promiscuous while reporting success.
func TestKernelClearsPromiscuityReferenceCount(t *testing.T) {
	requireRoot(t)

	const name = "vfhyg5"
	link := mkDummy(t, name)
	ops := NewNetlinkStateOps()
	vf := VF{PCIAddress: "0000:d8:00.5", NetdevName: name}

	// First reference: the administrative flag, as `ip link set promisc on`
	// leaves it.
	if err := netlink.SetPromiscOn(link); err != nil {
		t.Fatalf("failed to set promisc on: %v", err)
	}

	// Second reference: a packet socket joined to the promiscuous group,
	// which is how a workload that reads raw frames takes its own reference.
	// Repeating the rtnetlink call cannot produce this -- dev_change_flags
	// only increments on the 0 to 1 transition -- so the count can only be
	// raised above one by an in-kernel requester of this kind.
	closeSock := addPacketPromisc(t, name)
	closed := false
	_ = closed
	defer func() { closeSock() }()

	fresh, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.Attrs().Promisc; got < 2 {
		t.Skipf("kernel reported promiscuity %d, expected a reference count of at least 2", got)
	}

	// With a live in-kernel requester the count cannot be driven to zero, and
	// it would be wrong to try: rtnetlink can only release the
	// administrative reference, and once IFF_PROMISC is clear further
	// requests are no-ops. The requirement is therefore not that cleanup
	// succeeds, but that it does not claim to have cleaned a device that is
	// still promiscuous.
	if _, err := ops.Restore(vf, State{}, []Attribute{AttrPromisc}); err == nil {
		t.Fatal("cleanup reported success while another requester still holds promiscuity; " +
			"a released VF would be recorded as clean while still promiscuous")
	}

	state, err := ops.GetState(vf)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Promisc {
		t.Fatal("device should still be promiscuous while the packet socket holds a reference")
	}

	// Once the requester releases, the device drops out of promiscuous mode
	// on its own and cleanup has nothing left to do. This is why a workload
	// that takes promiscuity through a socket does not leave it behind on
	// exit, and why the state that does persist is the administrative flag.
	closeSock()

	fresh, err = netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if count := fresh.Attrs().Promisc; count != 0 {
		t.Fatalf("expected promiscuity to fall to 0 after the socket closed, got %d", count)
	}
}

// addPacketPromisc opens a packet socket and joins the interface's promiscuous
// group, taking a kernel reference on promiscuity exactly as a workload reading
// raw frames does. It returns a function that closes the socket.
func addPacketPromisc(t *testing.T, iface string) func() {
	t.Helper()

	link, err := net.InterfaceByName(iface)
	if err != nil {
		t.Fatalf("look up %s: %v", iface, err)
	}

	proto := int(htons16(unix.ETH_P_ALL))
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, proto)
	if err != nil {
		t.Skipf("packet socket unavailable: %v", err)
	}

	mreq := unix.PacketMreq{
		Ifindex: int32(link.Index),
		Type:    unix.PACKET_MR_PROMISC,
	}
	if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &mreq); err != nil {
		unix.Close(fd)
		t.Skipf("cannot join promiscuous group: %v", err)
	}

	return func() { unix.Close(fd) }
}

func htons16(v uint16) uint16 { return (v<<8)&0xff00 | v>>8 }
