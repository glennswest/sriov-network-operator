package vfhygiene

import (
	"os"
	"path/filepath"
	"testing"
)

// --- fakes -----------------------------------------------------------------

type fakeLister struct {
	vfs []VF
	err error
}

func (f *fakeLister) ListVFs() ([]VF, error) { return f.vfs, f.err }

type fakeAllocation struct {
	allocated map[string]bool
	errs      map[string]error
}

func (f *fakeAllocation) IsAllocated(pci string) (bool, error) {
	if err, ok := f.errs[pci]; ok {
		return true, err
	}
	return f.allocated[pci], nil
}

type restoreCall struct {
	target State
	attrs  []Attribute
}

type fakeState struct {
	state       map[string]State
	restores    map[string][]restoreCall
	restoreEr   map[string]error
	getErrs     map[string]error
	unsupported map[Attribute]bool
}

func newFakeState() *fakeState {
	return &fakeState{
		state:       map[string]State{},
		restores:    map[string][]restoreCall{},
		restoreEr:   map[string]error{},
		getErrs:     map[string]error{},
		unsupported: map[Attribute]bool{},
	}
}

func (f *fakeState) GetState(vf VF) (State, error) {
	if err, ok := f.getErrs[vf.PCIAddress]; ok {
		return State{}, err
	}
	return f.state[vf.PCIAddress], nil
}

func (f *fakeState) Restore(vf VF, target State, attrs []Attribute) ([]Attribute, error) {
	if err, ok := f.restoreEr[vf.PCIAddress]; ok {
		return nil, err
	}
	// Attributes this "driver" does not implement are skipped, mirroring the
	// real implementation.
	var applied []Attribute
	for _, a := range attrs {
		if !f.unsupported[a] {
			applied = append(applied, a)
		}
	}
	attrs = applied
	f.restores[vf.PCIAddress] = append(f.restores[vf.PCIAddress], restoreCall{target: target, attrs: attrs})

	// Apply, so tests can assert on resulting state.
	s := f.state[vf.PCIAddress]
	for _, a := range attrs {
		switch a {
		case AttrPromisc:
			s.Promisc = false
		case AttrAllMulticast:
			s.AllMulticast = false
		case AttrXDP:
			s.XDPAttached = false
		case AttrMTU:
			s.MTU = target.MTU
		case AttrEffectiveMAC:
			s.EffectiveMAC = target.EffectiveMAC
		case AttrAdminUp:
			s.AdminUp = target.AdminUp
		case AttrAdminMAC:
			s.AdminMAC = target.AdminMAC
		case AttrVLAN:
			s.VLAN, s.VLANQoS, s.VLANProto = target.VLAN, target.VLANQoS, target.VLANProto
		case AttrSpoofChk:
			s.SpoofChk = target.SpoofChk
		case AttrTrust:
			s.Trust = target.Trust
		case AttrTxRate:
			s.MinTxRate, s.MaxTxRate = target.MinTxRate, target.MaxTxRate
		case AttrLinkState:
			s.LinkState = target.LinkState
		}
	}
	f.state[vf.PCIAddress] = s
	return applied, nil
}

// fixedKnown returns one known state for every VF, which is how the real
// provider behaves: the target does not depend on the VF's history.
type fixedKnown struct {
	state State
	err   error
}

func (f *fixedKnown) KnownValues(VF) (State, error) { return f.state, f.err }

// defaultKnown is the known state used by most tests: a released VF is down,
// not promiscuous, no VLAN, no rate limits, spoofchk on.
func defaultKnown() *fixedKnown {
	return &fixedKnown{state: State{
		HasNetdev: true, HasVFInfo: true,
		MTU: 1500, AdminMAC: "00:00:00:00:00:00", SpoofChk: true,
	}}
}

func hasAttr(attrs []Attribute, want Attribute) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}

const testVFPCI = "0000:d8:00.2"

func testVF() VF {
	return VF{
		PCIAddress:   testVFPCI,
		PFPCIAddress: "0000:d8:00.0",
		PFNetdevName: "enp216s0f0np0",
		VFIndex:      0,
		NetdevName:   "enp216s0f0v0",
	}
}

// --- zero-valued attributes need no baseline -------------------------------

func TestSweepClearsPromiscWithoutBaseline(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{})
	res, err := s.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	if res.Dirty != 1 || res.Restored != 1 {
		t.Fatalf("expected 1 dirty and 1 restored, got %+v", res)
	}
	if st.state[testVFPCI].Promisc {
		t.Fatal("promiscuous mode should have been cleared without a baseline")
	}
}

func TestSweepClearsAllMulticastAndXDPWithoutBaseline(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, AllMulticast: true, XDPAttached: true}

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{})
	if _, err := s.SweepOnce(); err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	got := st.state[testVFPCI]
	if got.AllMulticast || got.XDPAttached {
		t.Fatalf("all-multicast and XDP should have been cleared, got %+v", got)
	}
}

// --- baseline-driven attributes --------------------------------------------

func TestSweepRestoresAllStateToKnownValues(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}

	known := &fixedKnown{state: State{
		HasNetdev: true, MTU: 1500, EffectiveMAC: "aa:bb:cc:dd:ee:01", AdminUp: false,
		HasVFInfo: true, AdminMAC: "00:00:00:00:00:00", VLAN: 0, VLANQoS: 0, VLANProto: 0x8100,
		SpoofChk: true, Trust: false, MinTxRate: 0, MaxTxRate: 0, LinkState: 0,
	}}

	// A workload changed essentially everything it could.
	st := newFakeState()
	st.state[testVFPCI] = State{
		HasNetdev: true, Promisc: true, AllMulticast: true, XDPAttached: true,
		MTU: 9000, EffectiveMAC: "de:ad:be:ef:00:01", AdminUp: true,
		HasVFInfo: true, AdminMAC: "de:ad:be:ef:00:02", VLAN: 4000, VLANQoS: 7, VLANProto: 0x88a8,
		SpoofChk: false, Trust: true, MinTxRate: 10, MaxTxRate: 1000, LinkState: 2,
		RSSQuery: true,
	}

	s := New(lister, &fakeAllocation{}, st, nil, known, nil, Config{})
	res, err := s.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}
	if res.Restored != 1 {
		t.Fatalf("expected the VF to be restored, got %+v", res)
	}

	calls := st.restores[testVFPCI]
	if len(calls) != 1 {
		t.Fatalf("expected one restore call, got %d", len(calls))
	}
	// Every attribute should have been detected as dirty.
	for _, want := range AllAttributes {
		if !hasAttr(calls[0].attrs, want) {
			t.Errorf("attribute %q was changed by the workload but not restored", want)
		}
	}

	got := st.state[testVFPCI]
	if got.MTU != 1500 || got.EffectiveMAC != "aa:bb:cc:dd:ee:01" || got.AdminUp {
		t.Errorf("netdev-level state not restored to known values: %+v", got)
	}
	if got.AdminMAC != "00:00:00:00:00:00" || got.VLAN != 0 || got.VLANProto != 0x8100 {
		t.Errorf("PF-side identity not restored to known values: %+v", got)
	}
	if !got.SpoofChk || got.Trust || got.MaxTxRate != 0 || got.LinkState != 0 {
		t.Errorf("PF-side policy not restored to known values: %+v", got)
	}
}

func TestSweepRestoresNeverBeforeSeenVF(t *testing.T) {
	// The key property of known values over a learned baseline: a VF that was
	// already dirty the first time it was ever observed is still restored,
	// because the target does not depend on having seen it clean.
	lister := &fakeLister{vfs: []VF{testVF()}}
	st := newFakeState()
	st.state[testVFPCI] = State{
		HasNetdev: true, MTU: 9000, Promisc: true,
		HasVFInfo: true, Trust: true, VLAN: 4000,
	}

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{})
	if _, err := s.SweepOnce(); err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	got := st.state[testVFPCI]
	if got.Promisc {
		t.Error("promiscuous mode not cleared")
	}
	if got.MTU != 1500 {
		t.Errorf("MTU not returned to the known value, got %d", got.MTU)
	}
	if got.VLAN != 0 || got.Trust {
		t.Errorf("PF-side state not returned to known values: %+v", got)
	}
}

// --- PF-side plane works for VFs with no netdev (the DPDK case) ------------

func TestSweepRestoresPFSideStateForVFWithoutNetdev(t *testing.T) {
	vf := testVF()
	vf.NetdevName = "" // bound to vfio-pci

	known := &fixedKnown{state: State{HasVFInfo: true, VLAN: 0, Trust: false, SpoofChk: true}}

	st := newFakeState()
	st.state[testVFPCI] = State{HasVFInfo: true, VLAN: 4000, Trust: true, SpoofChk: false}

	s := New(&fakeLister{vfs: []VF{vf}}, &fakeAllocation{}, st, nil, known, nil, Config{})
	res, err := s.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}
	if res.Restored != 1 {
		t.Fatalf("a VF with no netdev must still have PF-side state restored, got %+v", res)
	}

	got := st.state[testVFPCI]
	if got.VLAN != 0 || got.Trust || !got.SpoofChk {
		t.Fatalf("PF-side state not restored for vfio-bound VF: %+v", got)
	}
}

func TestNetdevAttributesSkippedWhenVFHasNoNetdev(t *testing.T) {
	vf := testVF()
	vf.NetdevName = ""

	st := newFakeState()
	// Already in the known state on the PF-side plane; with no netdev, the
	// netdev-level attributes are not applicable.
	st.state[testVFPCI] = State{HasVFInfo: true, AdminMAC: "00:00:00:00:00:00", SpoofChk: true}

	s := New(&fakeLister{vfs: []VF{vf}}, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{})
	res, err := s.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}
	if res.Dirty != 0 || res.Restored != 0 {
		t.Fatalf("nothing to do for a clean vfio-bound VF, got %+v", res)
	}
}

// --- allocation safety ------------------------------------------------------

func TestSweepNeverTouchesAllocatedVF(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}
	alloc := &fakeAllocation{allocated: map[string]bool{testVFPCI: true}}
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}

	s := New(lister, alloc, st, nil, defaultKnown(), nil, Config{})
	res, err := s.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	if res.Restored != 0 || len(st.restores) != 0 {
		t.Fatalf("in-use VF must not be touched, got %+v", res)
	}
	if !st.state[testVFPCI].Promisc {
		t.Fatal("in-use VF state was modified")
	}
}

func TestSweepSkipsVFWhenAllocationStateUnknown(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}
	alloc := &fakeAllocation{errs: map[string]error{testVFPCI: os.ErrPermission}}
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}

	s := New(lister, alloc, st, nil, defaultKnown(), nil, Config{})
	res, _ := s.SweepOnce()

	if res.Restored != 0 || !st.state[testVFPCI].Promisc {
		t.Fatal("VF with unknown allocation state must be left alone")
	}
}

// --- scope, dry-run, learning, errors --------------------------------------

func TestSweepRestoresOnlyAttributesInScope(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true, AllMulticast: true}

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{Scope: []Attribute{AttrPromisc}})
	if _, err := s.SweepOnce(); err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	got := st.state[testVFPCI]
	if got.Promisc {
		t.Error("promiscuous mode should have been cleared")
	}
	if !got.AllMulticast {
		t.Error("all-multicast is outside the configured scope and must be untouched")
	}
}

func TestSweepDryRunReportsButDoesNotChange(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true, AllMulticast: true}

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{DryRun: true})
	res, _ := s.SweepOnce()

	if res.Dirty != 1 {
		t.Fatalf("dry run should still report dirty VFs, got %+v", res)
	}
	if res.Restored != 0 || len(st.restores) != 0 {
		t.Fatal("dry run must not modify anything")
	}
}

func TestSweepCountsRestoreFailures(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}
	st := newFakeState()
	st.state[testVFPCI] = State{HasNetdev: true, Promisc: true}
	st.restoreEr[testVFPCI] = os.ErrPermission

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{})
	res, _ := s.SweepOnce()

	if res.Errors != 1 || res.Restored != 0 {
		t.Fatalf("expected the failure to be counted, got %+v", res)
	}
}

func TestSweepCleanVFIsNoOp(t *testing.T) {
	lister := &fakeLister{vfs: []VF{testVF()}}
	st := newFakeState()
	// "Clean" now means: already in the known state.
	st.state[testVFPCI] = defaultKnown().state

	s := New(lister, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{})
	res, _ := s.SweepOnce()

	if res.Dirty != 0 || res.Restored != 0 || len(st.restores) != 0 {
		t.Fatalf("clean VF should be a no-op, got %+v", res)
	}
}

// --- allocation checker -----------------------------------------------------

func TestCNIAllocationChecker(t *testing.T) {
	dir := t.TempDir()
	liveNetns := filepath.Join(dir, "live-netns")
	if err := os.WriteFile(liveNetns, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	// VF allocated to a live consumer.
	if err := os.WriteFile(filepath.Join(dir, "0000:d8:00.2"), []byte(liveNetns), 0o600); err != nil {
		t.Fatal(err)
	}
	// VF whose consumer netns is gone: the crash / netns-gone case.
	if err := os.WriteFile(filepath.Join(dir, "0000:d8:00.3"), []byte(filepath.Join(dir, "dead-netns")), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &CNIAllocationChecker{DataDir: dir}

	if allocated, err := c.IsAllocated("0000:d8:00.2"); err != nil || !allocated {
		t.Fatalf("VF with a live netns must read as allocated (allocated=%v err=%v)", allocated, err)
	}
	if allocated, err := c.IsAllocated("0000:d8:00.3"); err != nil || allocated {
		t.Fatalf("VF whose netns is gone must read as released (allocated=%v err=%v)", allocated, err)
	}
	if allocated, err := c.IsAllocated("0000:d8:00.9"); err != nil || allocated {
		t.Fatalf("VF with no allocation record must read as released (allocated=%v err=%v)", allocated, err)
	}
}

// --- baseline store ---------------------------------------------------------

// --- sysfs discovery --------------------------------------------------------

// TestSysfsListerWalksVFs builds a synthetic sysfs tree shaped like a real one:
// a PF with sriov_numvfs=2, virtfnN symlinks, one VF with a netdev (kernel
// driver) and one without (vfio-pci bound).
func TestSysfsListerWalksVFs(t *testing.T) {
	root := t.TempDir()

	pf := "0000:d8:00.0"
	vfNetdev := "0000:d8:00.2"
	vfDPDK := "0000:d8:00.3"

	for _, dev := range []string{pf, vfNetdev, vfDPDK} {
		if err := os.MkdirAll(filepath.Join(root, dev), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, pf, "sriov_numvfs"), []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, pf, "net", "enp216s0f0np0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, vfNetdev), filepath.Join(root, pf, "virtfn0")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, vfDPDK), filepath.Join(root, pf, "virtfn1")); err != nil {
		t.Fatal(err)
	}
	// Only the kernel-driver VF has a net/ directory.
	if err := os.MkdirAll(filepath.Join(root, vfNetdev, "net", "enp216s0f0v0"), 0o755); err != nil {
		t.Fatal(err)
	}

	l := &SysfsLister{SysBusPCI: root}
	vfs, err := l.ListVFs()
	if err != nil {
		t.Fatalf("ListVFs returned error: %v", err)
	}
	if len(vfs) != 2 {
		t.Fatalf("expected 2 VFs, got %d (%+v)", len(vfs), vfs)
	}

	byPCI := map[string]VF{}
	for _, vf := range vfs {
		byPCI[vf.PCIAddress] = vf
	}

	got := byPCI[vfNetdev]
	if got.NetdevName != "enp216s0f0v0" || got.PFPCIAddress != pf {
		t.Fatalf("kernel-driver VF discovered incorrectly: %+v", got)
	}
	if got.PFNetdevName != "enp216s0f0np0" || got.VFIndex != 0 {
		t.Fatalf("PF netdev / VF index not discovered: %+v", got)
	}

	dpdk := byPCI[vfDPDK]
	if dpdk.NetdevName != "" {
		t.Fatalf("vfio-bound VF should have no netdev, got %+v", dpdk)
	}
	// The PF-side plane must still be reachable for this VF.
	if dpdk.PFNetdevName != "enp216s0f0np0" || dpdk.VFIndex != 1 {
		t.Fatalf("vfio-bound VF must still carry PF netdev and index: %+v", dpdk)
	}
}

func TestSysfsListerIgnoresNonSriovDevices(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "0000:00:1f.6"), 0o755); err != nil {
		t.Fatal(err)
	}
	// sriov_numvfs = 0 -> not an active SR-IOV PF.
	if err := os.WriteFile(filepath.Join(root, "0000:00:1f.6", "sriov_numvfs"), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	vfs, err := (&SysfsLister{SysBusPCI: root}).ListVFs()
	if err != nil {
		t.Fatalf("ListVFs returned error: %v", err)
	}
	if len(vfs) != 0 {
		t.Fatalf("expected no VFs, got %+v", vfs)
	}
}

// TestSweepSkipsUnsupportedAttributesWithoutBlockingOthers is the driver-
// capability case: a NIC whose driver does not implement, say, trust or min tx
// rate must still have everything else restored. A single unsupported attribute
// aborting the restore would leave promiscuous mode set on a released VF.
func TestSweepSkipsUnsupportedAttributesWithoutBlockingOthers(t *testing.T) {
	st := newFakeState()
	st.unsupported[AttrTrust] = true
	st.unsupported[AttrTxRate] = true
	st.state[testVFPCI] = State{
		HasNetdev: true, Promisc: true,
		HasVFInfo: true, Trust: true, MaxTxRate: 1000,
	}

	s := New(&fakeLister{vfs: []VF{testVF()}}, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{})
	res, err := s.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}
	if res.Errors != 0 {
		t.Fatalf("unsupported attributes must not count as errors, got %+v", res)
	}
	if res.Restored != 1 {
		t.Fatalf("expected the supported attributes to be restored, got %+v", res)
	}
	if st.state[testVFPCI].Promisc {
		t.Fatal("promiscuous mode was left set because another attribute was unsupported")
	}
}

func TestParseAttributes(t *testing.T) {
	got, err := ParseAttributes([]string{"promisc", " allmulticast "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != AttrPromisc || got[1] != AttrAllMulticast {
		t.Fatalf("unexpected scope: %v", got)
	}

	if _, err := ParseAttributes([]string{"promisc", "nonsense"}); err == nil {
		t.Fatal("an unknown attribute name must be rejected")
	}
	if _, err := ParseAttributes(nil); err == nil {
		t.Fatal("an empty attribute list must be rejected")
	}
}

// --- stopping delivery to a vport whose consumer is gone --------------------
//
// Clearing promiscuous mode removes only the eSwitch rule that replicates
// everything to a vport. The vport's own unicast MAC filter, the multicast
// addresses the workload subscribed to, its VLAN filter entries and its queues
// all survive, so traffic addressed to that VF continues to be delivered into
// queues nobody is draining. Field evidence is consistent with this: with the
// VF released and promiscuous mode cleared, discard and pause counters kept
// climbing, and only a full unbind stopped them.
//
// The cleanup therefore cannot rely on clearing promiscuity alone. These tests
// pin the two levers that stop delivery without a rebind.

// TestReleasedVFIsBroughtDown checks that a released VF is not left
// administratively up. An up vport keeps accepting frames that match its
// filters even with promiscuous mode off.
func TestReleasedVFIsBroughtDown(t *testing.T) {
	st := newFakeState()
	st.state[testVFPCI] = State{
		HasNetdev: true, AdminUp: true, Promisc: true,
		HasVFInfo: true, AdminMAC: "00:00:00:00:00:00", SpoofChk: true, MTU: 1500,
	}

	s := New(&fakeLister{vfs: []VF{testVF()}}, &fakeAllocation{}, st, nil, defaultKnown(), nil, Config{})
	if _, err := s.SweepOnce(); err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	got := st.state[testVFPCI]
	if got.AdminUp {
		t.Error("a released VF must not be left administratively up; an up vport still " +
			"accepts frames matching its filters after promiscuous mode is cleared")
	}
	if got.Promisc {
		t.Error("promiscuous mode not cleared")
	}
}

// TestClearingPromiscAloneIsNotTreatedAsSufficient guards the scope itself: if
// someone narrows the default set to promiscuous mode only, the VF is left up
// and its identity intact, which is the configuration that reproduced the
// original symptom. The test documents that promisc-only is a deliberate
// choice, not the default.
func TestClearingPromiscAloneIsNotTreatedAsSufficient(t *testing.T) {
	for _, attr := range []Attribute{AttrAdminUp, AttrLinkState, AttrAdminMAC} {
		found := false
		for _, a := range AllAttributes {
			if a == attr {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q must be in the default scope: clearing promiscuous mode alone "+
				"leaves the vport receiving traffic addressed to it", attr)
		}
	}
}

// TestReleasedVFLinkStateReturnsToKnownValue pins the second lever. Link state
// is PF-side, so it applies to a VF with no netdev -- the userspace-driver case
// where there is nothing to bring down.
func TestReleasedVFLinkStateReturnsToKnownValue(t *testing.T) {
	vf := testVF()
	vf.NetdevName = "" // vfio-pci bound: no netdev to set down

	st := newFakeState()
	st.state[testVFPCI] = State{
		HasVFInfo: true, LinkState: 1, AdminMAC: "00:00:00:00:00:00", SpoofChk: true,
	}

	known := &fixedKnown{state: State{
		HasVFInfo: true, LinkState: 0, AdminMAC: "00:00:00:00:00:00", SpoofChk: true,
	}}

	s := New(&fakeLister{vfs: []VF{vf}}, &fakeAllocation{}, st, nil, known, nil, Config{})
	if _, err := s.SweepOnce(); err != nil {
		t.Fatalf("SweepOnce returned error: %v", err)
	}

	if st.state[testVFPCI].LinkState != 0 {
		t.Fatalf("link state not returned to the known value, got %d", st.state[testVFPCI].LinkState)
	}
}
