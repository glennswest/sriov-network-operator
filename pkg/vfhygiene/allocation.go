package vfhygiene

import (
	"fmt"
	"os"
	"path/filepath"
)

// CNIAllocationChecker reports VF allocation using the state sriov-cni already
// maintains on the node, rather than inventing a second allocation model.
//
// sriov-cni writes one file per allocated VF under its data dir, named by PCI
// address, whose contents are the consumer's network namespace path. Resolving
// that netns is what makes this crash-aware: if the consumer died without a CNI
// DEL ever running, the netns no longer exists and the VF is genuinely free —
// which is precisely the case the sweep exists to clean up.
type CNIAllocationChecker struct {
	// DataDir is sriov-cni's data directory, DefaultCNIDataDir by default.
	DataDir string
	// netnsExists reports whether a netns path is still live. Overridable for
	// tests.
	netnsExists func(path string) bool
}

// NewCNIAllocationChecker returns a checker reading sriov-cni's default data dir.
func NewCNIAllocationChecker() *CNIAllocationChecker {
	return &CNIAllocationChecker{DataDir: DefaultCNIDataDir}
}

// IsAllocated returns true when the VF is in use by a live consumer.
//
// The check is conservative in both directions that matter:
//   - unreadable state means "allocated", so the sweep leaves the VF alone;
//   - a recorded netns that no longer exists means "not allocated", which is the
//     departed-consumer case.
func (c *CNIAllocationChecker) IsAllocated(pciAddress string) (bool, error) {
	dir := c.DataDir
	if dir == "" {
		dir = DefaultCNIDataDir
	}

	path := filepath.Join(dir, pciAddress)
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			// No allocation record: the VF is not in use by the CNI.
			return false, nil
		}
		// Unknown state: report allocated so the caller skips this VF.
		return true, fmt.Errorf("failed to read allocation state for %s: %w", pciAddress, err)
	}

	netnsPath := string(data)
	if netnsPath == "" {
		// Allocation file with no netns recorded: cannot prove the consumer
		// is gone, so treat it as in use.
		return true, nil
	}

	exists := c.netnsExists
	if exists == nil {
		exists = defaultNetnsExists
	}
	if exists(netnsPath) {
		return true, nil
	}

	// The recorded consumer namespace is gone. This is the netns-gone case
	// that CNI DEL cannot cover.
	return false, nil
}

func defaultNetnsExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
