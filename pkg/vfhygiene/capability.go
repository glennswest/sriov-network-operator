package vfhygiene

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Capability handling.
//
// There is deliberately no table of "which NIC supports what". VF operations
// are implemented independently by each PF driver, and the subsets differ:
// trust, min tx rate, link state and VLAN protocol are all optional in
// practice, and which of them a given driver implements varies by driver and by
// kernel version. A vendor table would encode a snapshot of that and would be
// wrong for the next driver, the next kernel, or an out-of-tree driver.
//
// Instead the kernel is asked. An operation the driver does not implement fails
// with EOPNOTSUPP (or a close relative), which is a definitive answer about this
// device on this kernel, and is treated as "not applicable here" rather than as
// a failure.

// isUnsupported reports whether an error means the driver does not implement
// this operation for this device, as opposed to the operation being attempted
// and failing.
func isUnsupported(err error) bool {
	if err == nil {
		return false
	}
	for _, code := range []error{unix.EOPNOTSUPP, unix.ENOTSUP, unix.ENOTTY, unix.ENODEV} {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}
