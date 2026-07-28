//go:build linux || darwin

package vfhygiene

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// flockTry takes a non-blocking exclusive flock on path, matching the locking
// protocol sriov-cni uses (unix.Flock with LOCK_EX on a per-VF lock file).
func flockTry(path string) (func(), bool, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDONLY|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("failed to open lock file %s: %w", path, err)
	}

	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		if err == unix.EWOULDBLOCK {
			// Someone else is configuring this device right now.
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to lock %s: %w", path, err)
	}

	unlock := func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	}
	return unlock, true, nil
}
