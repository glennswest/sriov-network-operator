//go:build !linux

package vfhygiene

import "errors"

// errNotLinux is returned by the state ops stub on non-Linux platforms. The
// operator only runs on Linux; this file exists so the package still builds for
// local tooling on other platforms.
var errNotLinux = errors.New("vfhygiene: VF state operations are only supported on linux")

// NetlinkStateOps is a stub on non-Linux platforms.
type NetlinkStateOps struct{}

// NewNetlinkStateOps returns the stub state ops.
func NewNetlinkStateOps() *NetlinkStateOps { return &NetlinkStateOps{} }

// GetState always fails on non-Linux platforms.
func (o *NetlinkStateOps) GetState(VF) (State, error) { return State{}, errNotLinux }

// Restore always fails on non-Linux platforms.
func (o *NetlinkStateOps) Restore(VF, State, []Attribute) ([]Attribute, error) {
	return nil, errNotLinux
}
