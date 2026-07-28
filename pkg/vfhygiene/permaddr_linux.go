//go:build linux

package vfhygiene

import "github.com/safchain/ethtool"

// permanentAddress returns the device's permanent hardware address -- the one
// burned into the NIC, which is what makes it a "known" value rather than a
// remembered one.
func permanentAddress(netdev string) (string, error) {
	e, err := ethtool.NewEthtool()
	if err != nil {
		return "", err
	}
	defer e.Close()
	return e.PermAddr(netdev)
}
