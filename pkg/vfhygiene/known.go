package vfhygiene

// Known values.
//
// A consumer configures a device when it opens it: it sets the MAC, MTU, VLAN,
// offloads and filters it needs, and does not inherit anything meaningful from
// whoever held the device before. There is therefore no value in preserving a
// particular VF's previous settings, and no association between one consumer
// and the next.
//
// What matters is the opposite: that a released VF is returned to a *known*
// state, identical for every VF of that device, so the next consumer starts
// from a defined position rather than from the debris of the last one. This
// removes any need to have observed the VF while it was clean -- a VF that was
// already dirty the first time it was seen is still restored correctly, which a
// remembered baseline could never do.
//
// Two sources of "known":
//
//   - The device itself, where it defines the value. The permanent hardware
//     address is burned into the NIC and is read back from it, so the VF's MAC
//     is restored to the address the hardware says it has, not to a remembered
//     one.
//   - The kernel's documented default, where the device does not. An
//     administratively unset VF MAC is all zeros, an unset VLAN is 0, unset
//     rate limits are 0, link state is auto, and the receive-path flags are
//     off.

// KnownValuesProvider computes the state a released VF must be returned to.
type KnownValuesProvider interface {
	// KnownValues returns the target state for a VF.
	KnownValues(vf VF) (State, error)
}

// Kernel defaults for VF state, used where the device does not define a value.
const (
	// vlanNone is an unset VF VLAN.
	vlanNone = 0
	// rateUnlimited is an unset VF tx rate limit.
	rateUnlimited = 0
	// linkStateAuto is IFLA_VF_LINK_STATE_AUTO: follow the PF's link.
	linkStateAuto = 0
	// adminMACUnset is an administratively unset VF MAC.
	adminMACUnset = "00:00:00:00:00:00"
	// defaultEthernetMTU is the kernel's default MTU for an Ethernet netdev.
	defaultEthernetMTU = 1500
)

// StandardKnownValues is the default provider: device-sourced where the device
// defines the value, kernel defaults otherwise.
type StandardKnownValues struct {
	// MTU is the MTU a released VF is returned to. The kernel's default for
	// Ethernet is used when unset. It is configurable because a fabric that
	// runs jumbo frames everywhere may legitimately define a different
	// "known" MTU for its VFs.
	MTU int

	// SpoofChk is the spoof-check setting a released VF is returned to.
	// Defaults to enabled, the safe position for a device about to be handed
	// to an unknown consumer.
	SpoofChk bool

	// permAddr reads a device's permanent hardware address. Overridable for
	// tests.
	permAddr func(netdev string) (string, error)
}

// NewStandardKnownValues returns the default known-values provider.
func NewStandardKnownValues() *StandardKnownValues {
	return &StandardKnownValues{
		MTU:      defaultEthernetMTU,
		SpoofChk: true,
		permAddr: permanentAddress,
	}
}

// KnownValues returns the state a released VF must be returned to.
func (k *StandardKnownValues) KnownValues(vf VF) (State, error) {
	mtu := k.MTU
	if mtu <= 0 {
		mtu = defaultEthernetMTU
	}

	state := State{
		// Receive-path flags: a released VF is never promiscuous, never in
		// all-multicast, and never has a program attached.
		Promisc:      false,
		AllMulticast: false,
		XDPAttached:  false,

		MTU: mtu,
		// A released VF is down. The consumer brings it up when it takes
		// the device, as sriov-cni does on CNI ADD.
		AdminUp: false,

		// PF-side administrative state, returned to "nothing set".
		AdminMAC:  adminMACUnset,
		VLAN:      vlanNone,
		VLANQoS:   0,
		VLANProto: vlanProto8021q,
		SpoofChk:  k.SpoofChk,
		Trust:     false,
		MinTxRate: rateUnlimited,
		MaxTxRate: rateUnlimited,
		LinkState: linkStateAuto,
		RSSQuery:  false,

		// Both planes are always targets: unlike a remembered baseline,
		// known values exist for a VF that has never been seen clean.
		HasVFInfo: true,
	}

	if vf.NetdevName != "" {
		state.HasNetdev = true

		// The MAC comes from the hardware, not from memory: the permanent
		// address is what this device is, independent of what any consumer
		// set on it.
		if k.permAddr != nil {
			perm, err := k.permAddr(vf.NetdevName)
			if err == nil && perm != "" && perm != adminMACUnset {
				state.EffectiveMAC = perm
			}
			// A device that cannot report a permanent address leaves
			// EffectiveMAC empty, and the MAC is then left alone rather
			// than set to a guess.
		}
	}

	return state, nil
}
