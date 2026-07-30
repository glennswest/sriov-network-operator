//go:build linux

package vfhygiene

import (
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// NetlinkStateOps reads and restores VF state over netlink.
//
// It covers two distinct planes:
//
//   - netdev-level state, read from the VF's own kernel netdev. Only available
//     when the VF is bound to a kernel driver.
//   - PF-side administrative state, read from the parent PF's VF table. This is
//     available regardless of what the VF is bound to, which is what lets the
//     sweep clean up VFs bound to vfio-pci — the DPDK case sriov-cni's netns
//     path never reaches.
//
// The VF is never unbound and never re-created, so no node drain is involved.
type NetlinkStateOps struct{}

// NewNetlinkStateOps returns state ops backed by netlink.
func NewNetlinkStateOps() *NetlinkStateOps { return &NetlinkStateOps{} }

// GetState returns the current state of a VF.
func (o *NetlinkStateOps) GetState(vf VF) (State, error) {
	state := State{}

	// --- netdev-level ---
	if vf.NetdevName != "" {
		link, err := netlink.LinkByName(vf.NetdevName)
		if err != nil {
			return state, fmt.Errorf("failed to look up netdev %s: %w", vf.NetdevName, err)
		}
		attrs := link.Attrs()
		state.HasNetdev = true
		state.Promisc = attrs.Promisc != 0
		state.AllMulticast = attrs.Allmulti != 0
		state.MTU = attrs.MTU
		state.AdminUp = attrs.Flags&net.FlagUp != 0
		if attrs.HardwareAddr != nil {
			state.EffectiveMAC = attrs.HardwareAddr.String()
		}
		state.XDPAttached = attrs.Xdp != nil && attrs.Xdp.Attached
	}

	// --- PF-side administrative ---
	if vf.PFNetdevName != "" {
		pfLink, err := netlink.LinkByName(vf.PFNetdevName)
		if err != nil {
			return state, fmt.Errorf("failed to look up PF netdev %s: %w", vf.PFNetdevName, err)
		}
		if info := findVFInfo(pfLink, vf.VFIndex); info != nil {
			state.HasVFInfo = true
			if info.Mac != nil {
				state.AdminMAC = info.Mac.String()
			}
			state.VLAN = info.Vlan
			state.VLANQoS = info.Qos
			state.VLANProto = int(info.VlanProto)
			state.SpoofChk = info.Spoofchk
			state.Trust = info.Trust != 0
			state.MinTxRate = int(info.MinTxRate)
			state.MaxTxRate = int(info.MaxTxRate)
			state.LinkState = info.LinkState
			state.RSSQuery = info.RssQuery != 0
		}
	}

	return state, nil
}

// Restore sets the given attributes of a VF back to the values in target.
//
// Only the attributes listed are written. Zero-valued attributes (promiscuous
// mode, all-multicast, XDP) are cleared outright; the rest are written from the
// recorded baseline carried in target.
func (o *NetlinkStateOps) Restore(vf VF, target State, attrs []Attribute) ([]Attribute, error) {
	var vfLink netlink.Link
	var pfLink netlink.Link
	var applied []Attribute
	var failures []error
	var err error

	for _, attr := range attrs {
		if NetdevAttributes[attr] {
			if vfLink == nil {
				if vf.NetdevName == "" {
					// Not applicable: no netdev to act on.
					continue
				}
				if vfLink, err = netlink.LinkByName(vf.NetdevName); err != nil {
					return applied, fmt.Errorf("failed to look up netdev %s: %w", vf.NetdevName, err)
				}
			}
		} else {
			if pfLink == nil {
				if vf.PFNetdevName == "" {
					continue
				}
				if pfLink, err = netlink.LinkByName(vf.PFNetdevName); err != nil {
					return applied, fmt.Errorf("failed to look up PF netdev %s: %w", vf.PFNetdevName, err)
				}
			}
		}

		if err := o.restoreOne(vf, vfLink, pfLink, target, attr); err != nil {
			// The driver does not implement this operation for this
			// device. That is an answer, not a failure: skip it and
			// carry on with the attributes it does implement.
			if isUnsupported(err) {
				log.Log.V(2).Info("attribute not supported by this driver, skipping",
					"vf", vf.PCIAddress, "attribute", attr)
				continue
			}
			failures = append(failures, err)
			continue
		}
		applied = append(applied, attr)
	}

	return applied, errors.Join(failures...)
}

func (o *NetlinkStateOps) restoreOne(vf VF, vfLink, pfLink netlink.Link, target State, attr Attribute) error {
	switch attr {
	// --- netdev-level ---
	case AttrPromisc:
		// Promiscuity is a reference count, not a flag. IFLA_PROMISCUITY
		// reports how many outstanding requests for promiscuous mode
		// exist, and clearing IFF_PROMISC over rtnetlink decrements it by
		// one. A workload that requested it more than once, or a device
		// with another requester still attached, therefore stays
		// promiscuous after a single clear.
		//
		// Equivalent to: ip link set $netdev promisc off, repeated until
		// `ip -d link` shows promiscuity 0.
		if err := clearPromiscuity(vfLink, vf.NetdevName); err != nil {
			return err
		}
	case AttrAllMulticast:
		// Equivalent to: ip link set $netdev allmulticast off
		if err := netlink.LinkSetAllmulticastOff(vfLink); err != nil {
			return fmt.Errorf("failed to disable all-multicast on %s: %w", vf.NetdevName, err)
		}
	case AttrXDP:
		// Equivalent to: ip link set $netdev xdp off
		if err := netlink.LinkSetXdpFd(vfLink, -1); err != nil {
			return fmt.Errorf("failed to detach XDP program from %s: %w", vf.NetdevName, err)
		}
	case AttrMTU:
		if target.MTU <= 0 {
			return nil
		}
		if err := netlink.LinkSetMTU(vfLink, target.MTU); err != nil {
			return fmt.Errorf("failed to restore MTU %d on %s: %w", target.MTU, vf.NetdevName, err)
		}
	case AttrEffectiveMAC:
		if target.EffectiveMAC == "" {
			return nil
		}
		mac, err := net.ParseMAC(target.EffectiveMAC)
		if err != nil {
			return fmt.Errorf("invalid baseline MAC %q for %s: %w", target.EffectiveMAC, vf.NetdevName, err)
		}
		if err := netlink.LinkSetHardwareAddr(vfLink, mac); err != nil {
			return fmt.Errorf("failed to restore MAC on %s: %w", vf.NetdevName, err)
		}
	case AttrAdminUp:
		if target.AdminUp {
			if err := netlink.LinkSetUp(vfLink); err != nil {
				return fmt.Errorf("failed to set %s up: %w", vf.NetdevName, err)
			}
			return nil
		}
		if err := netlink.LinkSetDown(vfLink); err != nil {
			return fmt.Errorf("failed to set %s down: %w", vf.NetdevName, err)
		}

	// --- PF-side administrative ---
	case AttrAdminMAC:
		if target.AdminMAC == "" {
			return nil
		}
		mac, err := net.ParseMAC(target.AdminMAC)
		if err != nil {
			return fmt.Errorf("invalid baseline admin MAC %q for VF %d: %w", target.AdminMAC, vf.VFIndex, err)
		}
		if err := netlink.LinkSetVfHardwareAddr(pfLink, vf.VFIndex, mac); err != nil {
			return fmt.Errorf("failed to restore admin MAC on VF %d of %s: %w", vf.VFIndex, vf.PFNetdevName, err)
		}
	case AttrVLAN:
		proto := target.VLANProto
		if proto == 0 {
			proto = vlanProto8021q
		}
		if err := netlink.LinkSetVfVlanQosProto(pfLink, vf.VFIndex, target.VLAN, target.VLANQoS, proto); err != nil {
			return fmt.Errorf("failed to restore VLAN on VF %d of %s: %w", vf.VFIndex, vf.PFNetdevName, err)
		}
	case AttrSpoofChk:
		if err := netlink.LinkSetVfSpoofchk(pfLink, vf.VFIndex, target.SpoofChk); err != nil {
			return fmt.Errorf("failed to restore spoofchk on VF %d of %s: %w", vf.VFIndex, vf.PFNetdevName, err)
		}
	case AttrTrust:
		if err := netlink.LinkSetVfTrust(pfLink, vf.VFIndex, target.Trust); err != nil {
			return fmt.Errorf("failed to restore trust on VF %d of %s: %w", vf.VFIndex, vf.PFNetdevName, err)
		}
	case AttrTxRate:
		if err := netlink.LinkSetVfRate(pfLink, vf.VFIndex, target.MinTxRate, target.MaxTxRate); err != nil {
			return fmt.Errorf("failed to restore tx rate on VF %d of %s: %w", vf.VFIndex, vf.PFNetdevName, err)
		}
	case AttrLinkState:
		if err := netlink.LinkSetVfState(pfLink, vf.VFIndex, target.LinkState); err != nil {
			return fmt.Errorf("failed to restore link state on VF %d of %s: %w", vf.VFIndex, vf.PFNetdevName, err)
		}
	case AttrRSSQuery:
		// IFLA_VF_RSS_QUERY_EN is readable through the VF table but the
		// netlink library exposes no setter for it. Reported as
		// unsupported so it is skipped rather than counted as a failure;
		// closing this needs a LinkSetVfRssQueryEn upstream.
		return fmt.Errorf("restoring RSS query permission is not implemented: %w", unix.EOPNOTSUPP)

	default:
		return fmt.Errorf("unknown attribute %q", attr)
	}
	return nil
}

// vlanProto8021q is the default VLAN protocol, matching sriov-cni's fallback
// when no protocol was cached.
const vlanProto8021q = 0x8100

// maxPromiscClears bounds the decrement loop. A released VF should have a
// reference count of one or a small number; a count that will not come down is
// a device still in use by something, and continuing to decrement it would be
// worse than reporting the failure.
const maxPromiscClears = 8

// clearPromiscuity drives the promiscuity reference count to zero.
//
// Each clear decrements the count by one, so the device is re-read after every
// attempt rather than assuming one call was enough. Reaching zero is the
// success condition; exhausting the attempts means something else holds a
// reference and the VF must not be reported as clean.
func clearPromiscuity(link netlink.Link, name string) error {
	for i := 0; i < maxPromiscClears; i++ {
		fresh, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("failed to re-read netdev %s: %w", name, err)
		}
		if fresh.Attrs().Promisc == 0 {
			return nil
		}
		if err := netlink.SetPromiscOff(fresh); err != nil {
			return fmt.Errorf("failed to disable promiscuous mode on %s: %w", name, err)
		}
	}

	fresh, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("failed to re-read netdev %s: %w", name, err)
	}
	if count := fresh.Attrs().Promisc; count != 0 {
		return fmt.Errorf("promiscuity on %s did not reach zero after %d clears (still %d): "+
			"another requester holds a reference", name, maxPromiscClears, count)
	}
	return nil
}

func findVFInfo(pfLink netlink.Link, index int) *netlink.VfInfo {
	vfs := pfLink.Attrs().Vfs
	for i := range vfs {
		if vfs[i].ID == index {
			return &vfs[i]
		}
	}
	return nil
}
