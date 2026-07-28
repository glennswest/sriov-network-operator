// Package vfhygiene keeps released SR-IOV virtual functions from being handed
// out in an unknown state.
//
// A VF returned to the pool still carrying the previous consumer's settings is
// not a cosmetic problem. The next workload receives a device configured in a
// way it did not ask for and cannot see, which surfaces as interrupt load,
// routing-protocol instability, packet loss and other failures whose cause is
// several layers away from the symptom. Attributing that back to a stale VF
// attribute is difficult, which is what makes it dangerous: the damage is
// diffuse and the diagnosis is expensive.
//
// This is therefore a correctness property of handing out a device, not an
// optional capability, and it is enabled by default.
//
// Scope is deliberately *all* VF state a workload can change, not just
// promiscuous mode. Promiscuity is the attribute this was first reported
// against, but an application that fails to clean up can leave behind any of
// the netdev-level settings it touched (MTU, effective MAC, all-multicast, an
// attached XDP program, admin up/down) and any of the PF-side administrative
// settings (admin MAC, VLAN/QoS, spoofchk, trust, tx rates, link state).
//
// The PF-side half matters for a second reason: it is readable and settable
// through the parent PF regardless of what the VF is bound to. That covers VFs
// bound to a userspace driver (vfio-pci), which have no netdev at all and which
// sriov-cni's ReleaseVF path never touches.
//
// It exists because neither in-band cleanup path closes the gap:
//
//   - The application is expected to revert what it set, but does not do so when
//     it exits without cleanup, or crashes/segfaults.
//   - sriov-cni restores VF state on CNI DEL, but DEL is skipped when the pod
//     netns is already gone (node crash), returns early when its cached netconf
//     is missing, and does not enter the netns path at all in DPDK mode.
//
// The sweep is deliberately NOT part of the config-daemon's nodeState reconcile
// path: that path is event-driven (it never fires on pod teardown) and any work
// routed through it requests a node drain, which is precisely what this feature
// must avoid.
package vfhygiene

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Attribute identifies a single piece of VF state the sweep can restore.
type Attribute string

const (
	// --- netdev-level state (requires the VF to have a kernel netdev) ---

	// AttrPromisc is promiscuous mode (IFF_PROMISC). Always restored to off.
	AttrPromisc Attribute = "promisc"
	// AttrAllMulticast is all-multicast mode (IFF_ALLMULTI). Always restored
	// to off.
	AttrAllMulticast Attribute = "allmulticast"
	// AttrXDP is an attached XDP/eBPF program. Always restored to detached.
	AttrXDP Attribute = "xdp"
	// AttrMTU is the netdev MTU. Restored to the recorded baseline.
	AttrMTU Attribute = "mtu"
	// AttrEffectiveMAC is the MAC address set on the VF netdev itself, as
	// distinct from the administrative MAC set through the PF. Restored to
	// the recorded baseline.
	AttrEffectiveMAC Attribute = "effective-mac"
	// AttrAdminUp is the netdev administrative up/down state. Restored to the
	// recorded baseline.
	AttrAdminUp Attribute = "admin-up"

	// --- PF-side administrative state (works without a VF netdev) ---

	// AttrAdminMAC is the administrative MAC assigned through the PF.
	AttrAdminMAC Attribute = "admin-mac"
	// AttrVLAN is the VF VLAN id, QoS priority and protocol.
	AttrVLAN Attribute = "vlan"
	// AttrSpoofChk is the VF spoof-check setting.
	AttrSpoofChk Attribute = "spoofchk"
	// AttrTrust is the VF trust setting.
	AttrTrust Attribute = "trust"
	// AttrTxRate is the VF min/max tx rate limits.
	AttrTxRate Attribute = "tx-rate"
	// AttrLinkState is the VF link state (auto/enable/disable).
	AttrLinkState Attribute = "link-state"
	// AttrRSSQuery is the VF's permission to query the PF's RSS redirection
	// table and hash key (IFLA_VF_RSS_QUERY_EN).
	AttrRSSQuery Attribute = "rss-query"
)

// AllAttributes is every attribute the sweep understands, and the default
// scope.
//
// Known gaps in coverage, recorded rather than hidden:
//
//   - IFLA_VF_IB_NODE_GUID / IFLA_VF_IB_PORT_GUID: settable through netlink but
//     not readable back through the VF table, so a deviation cannot be detected.
//   - IFLA_VF_RSS_QUERY_EN: readable, but with no setter available, so it is
//     detected and reported, then skipped as unsupported.
//   - Device-level settings reachable through ethtool -- offload features, ring
//     sizes, interrupt coalescing, channel counts, RSS hash key and indirection
//     table -- and the netdev's unicast/multicast filter lists and VLAN filter
//     table. A consumer can change any of these and they survive its exit.
//
// The first two need upstream netlink support. The third is the larger piece of
// remaining work.
//
// This list is the operator's new contract. Today the operator restores only
// the attributes it configured itself; everything here is state the operator
// may never have set and now takes responsibility for restoring on a released
// VF. It is deliberately explicit so the contract can be reviewed as a list.
var AllAttributes = []Attribute{
	AttrPromisc,
	AttrAllMulticast,
	AttrXDP,
	AttrMTU,
	AttrEffectiveMAC,
	AttrAdminUp,
	AttrAdminMAC,
	AttrVLAN,
	AttrSpoofChk,
	AttrTrust,
	AttrTxRate,
	AttrLinkState,
	AttrRSSQuery,
}

// ZeroValuedAttributes are attributes with a known-clean value that needs no
// recorded baseline: a released VF should never be promiscuous, never be in
// all-multicast, and never have an XDP program attached. These are restored
// even for a VF the sweep has never seen before.
var ZeroValuedAttributes = map[Attribute]bool{
	AttrPromisc:      true,
	AttrAllMulticast: true,
	AttrXDP:          true,
}

// NetdevAttributes are attributes that can only be handled when the VF has a
// kernel netdev.
var NetdevAttributes = map[Attribute]bool{
	AttrPromisc:      true,
	AttrAllMulticast: true,
	AttrXDP:          true,
	AttrMTU:          true,
	AttrEffectiveMAC: true,
	AttrAdminUp:      true,
}

// DefaultInterval is the sweep period used when none is configured.
const DefaultInterval = 60 * time.Second

// DefaultCNIDataDir is where sriov-cni records VF allocations. Each file is
// named by VF PCI address and contains the consumer's network namespace path.
// Note the "pci" suffix: sriov-cni's NewPCIAllocator appends it to its own data
// dir, so the files are not directly under /var/lib/cni/sriov.
const DefaultCNIDataDir = "/var/lib/cni/sriov/pci"

// DefaultBaselineDir is where the sweep persists per-VF baseline snapshots.
const DefaultBaselineDir = "/var/lib/sriov/vfhygiene"

// VF describes a virtual function as discovered on the host.
type VF struct {
	// PCIAddress is the VF PCI address, e.g. "0000:d8:00.2".
	PCIAddress string
	// PFPCIAddress is the parent physical function's PCI address.
	PFPCIAddress string
	// PFNetdevName is the parent PF's netdev name, needed to read and write
	// PF-side administrative state for this VF.
	PFNetdevName string
	// VFIndex is the VF's index on its PF (the N in virtfnN).
	VFIndex int
	// NetdevName is the VF's kernel netdev name, empty when the VF is bound
	// to a userspace driver (vfio-pci) and has no netdev.
	NetdevName string
}

// State is the full observed state of a VF.
//
// Fields are pointer-free but paired with Has* flags where "absent" is
// meaningful: a VF with no netdev has no netdev-level state to compare.
type State struct {
	// HasNetdev reports whether netdev-level fields are populated.
	HasNetdev bool

	// netdev-level
	Promisc      bool
	AllMulticast bool
	XDPAttached  bool
	MTU          int
	EffectiveMAC string
	AdminUp      bool

	// PF-side administrative
	HasVFInfo bool
	AdminMAC  string
	VLAN      int
	VLANQoS   int
	VLANProto int
	SpoofChk  bool
	Trust     bool
	MinTxRate int
	MaxTxRate int
	LinkState uint32
	RSSQuery  bool
}

// Diff returns the attributes of s that deviate from the known state b, limited
// to scope.
func (s State) Diff(b *State, scope []Attribute) []Attribute {
	var dirty []Attribute

	for _, attr := range scope {
		// Netdev-level attributes need a netdev to compare.
		if NetdevAttributes[attr] && !s.HasNetdev {
			continue
		}
		// PF-side attributes need VF info read from the PF.
		if !NetdevAttributes[attr] && !s.HasVFInfo {
			continue
		}

		switch attr {
		// --- known-clean values, no baseline required ---
		case AttrPromisc:
			if s.Promisc {
				dirty = append(dirty, attr)
			}
		case AttrAllMulticast:
			if s.AllMulticast {
				dirty = append(dirty, attr)
			}
		case AttrXDP:
			if s.XDPAttached {
				dirty = append(dirty, attr)
			}

		// --- everything else needs a recorded baseline ---
		case AttrMTU:
			if b != nil && b.HasNetdev && s.MTU != b.MTU {
				dirty = append(dirty, attr)
			}
		case AttrEffectiveMAC:
			if b != nil && b.HasNetdev && b.EffectiveMAC != "" && s.EffectiveMAC != b.EffectiveMAC {
				dirty = append(dirty, attr)
			}
		case AttrAdminUp:
			if b != nil && b.HasNetdev && s.AdminUp != b.AdminUp {
				dirty = append(dirty, attr)
			}
		case AttrAdminMAC:
			if b != nil && b.HasVFInfo && s.AdminMAC != b.AdminMAC {
				dirty = append(dirty, attr)
			}
		case AttrVLAN:
			if b != nil && b.HasVFInfo &&
				(s.VLAN != b.VLAN || s.VLANQoS != b.VLANQoS || s.VLANProto != b.VLANProto) {
				dirty = append(dirty, attr)
			}
		case AttrSpoofChk:
			if b != nil && b.HasVFInfo && s.SpoofChk != b.SpoofChk {
				dirty = append(dirty, attr)
			}
		case AttrTrust:
			if b != nil && b.HasVFInfo && s.Trust != b.Trust {
				dirty = append(dirty, attr)
			}
		case AttrTxRate:
			if b != nil && b.HasVFInfo && (s.MinTxRate != b.MinTxRate || s.MaxTxRate != b.MaxTxRate) {
				dirty = append(dirty, attr)
			}
		case AttrLinkState:
			if b != nil && b.HasVFInfo && s.LinkState != b.LinkState {
				dirty = append(dirty, attr)
			}
		case AttrRSSQuery:
			if b != nil && b.HasVFInfo && s.RSSQuery != b.RSSQuery {
				dirty = append(dirty, attr)
			}
		}
	}

	return dirty
}

// VFLister discovers the VFs present on this node.
type VFLister interface {
	// ListVFs returns every VF of every SR-IOV-capable PF on the host.
	ListVFs() ([]VF, error)
}

// AllocationChecker reports whether a VF is currently allocated to a live
// consumer. Implementations must be conservative: when the answer is not
// knowable, report the VF as allocated so the sweep leaves it alone.
type AllocationChecker interface {
	// IsAllocated returns true if the VF is in use by a live consumer.
	IsAllocated(pciAddress string) (bool, error)
}

// StateOps reads and restores VF state.
type StateOps interface {
	// GetState returns the current state of a VF.
	GetState(vf VF) (State, error)
	// Restore sets the given attributes of a VF back to the values in target.
	//
	// It returns the attributes that were actually applied. Attributes the
	// driver does not implement for this device are skipped rather than
	// treated as failures: VF operations are implemented per PF driver and
	// the supported subset differs between drivers and kernel versions, so
	// one unsupported attribute must not prevent the rest from being
	// restored. An error is returned only for attributes that were attempted
	// and genuinely failed.
	Restore(vf VF, target State, attrs []Attribute) (applied []Attribute, err error)
}

// Recorder receives sweep outcomes. Restoring state the operator never
// configured is a behaviour change, so every restore must be observable.
type Recorder interface {
	// VFReleased is called when the kernel signalled that a VF returned to
	// the host and the VF was confirmed to have no live consumer. This is
	// the release notification Kubernetes does not provide.
	VFReleased(vf VF)
	// VFRestored is called after a released VF has been restored.
	VFRestored(vf VF, attrs []Attribute)
	// VFRestoreFailed is called when a restore was attempted and failed.
	VFRestoreFailed(vf VF, attrs []Attribute, err error)
}

// ParseAttributes converts a comma-separated list of attribute names into a
// scope, rejecting anything unrecognised.
//
// This is what makes the restored set configurable at runtime rather than fixed
// at build time: an operator who wants the receive-path flags cleaned but does
// not want the platform touching MTU or VLAN can narrow the scope without a
// rebuild.
func ParseAttributes(list []string) ([]Attribute, error) {
	known := map[Attribute]bool{}
	for _, a := range AllAttributes {
		known[a] = true
	}

	out := make([]Attribute, 0, len(list))
	for _, raw := range list {
		name := Attribute(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, fmt.Errorf("unknown VF attribute %q; valid attributes are %s",
				name, AttributeNames())
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, errors.New("no VF attributes specified")
	}
	return out, nil
}

// AttributeNames returns every valid attribute name, for help text and errors.
func AttributeNames() string {
	parts := make([]string, 0, len(AllAttributes))
	for _, a := range AllAttributes {
		parts = append(parts, string(a))
	}
	return strings.Join(parts, ", ")
}
