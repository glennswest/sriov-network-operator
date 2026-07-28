package vfhygiene

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SysfsLister discovers VFs by walking sysfs. It is driver-agnostic on purpose:
// the sweep is a service every SR-IOV NIC needs, so nothing here is specific to
// mlx5, ice, i40e or any other PF driver.
type SysfsLister struct {
	// SysBusPCI is the PCI sysfs root, "/sys/bus/pci/devices" by default.
	SysBusPCI string
}

// NewSysfsLister returns a lister rooted at the default sysfs paths.
func NewSysfsLister() *SysfsLister {
	return &SysfsLister{SysBusPCI: "/sys/bus/pci/devices"}
}

// ListVFs returns every VF of every SR-IOV-capable PF on the host.
func (l *SysfsLister) ListVFs() ([]VF, error) {
	root := l.SysBusPCI
	if root == "" {
		root = "/sys/bus/pci/devices"
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("failed to read PCI devices from %s: %w", root, err)
	}

	var vfs []VF
	for _, entry := range entries {
		pfAddr := entry.Name()
		devPath := filepath.Join(root, pfAddr)

		// A PF with SR-IOV enabled has sriov_numvfs > 0.
		numVFs, err := readIntFile(filepath.Join(devPath, "sriov_numvfs"))
		if err != nil || numVFs == 0 {
			continue
		}

		// The PF netdev is needed to read and write PF-side VF state, which
		// is the only plane available for VFs bound to a userspace driver.
		pfNetdev := l.netdevName(pfAddr)

		for i := 0; i < numVFs; i++ {
			// virtfnN is a symlink to the VF's PCI device directory.
			link, err := os.Readlink(filepath.Join(devPath, fmt.Sprintf("virtfn%d", i)))
			if err != nil {
				continue
			}
			vfAddr := filepath.Base(link)
			vfs = append(vfs, VF{
				PCIAddress:   vfAddr,
				PFPCIAddress: pfAddr,
				PFNetdevName: pfNetdev,
				VFIndex:      i,
				NetdevName:   l.netdevName(vfAddr),
			})
		}
	}

	sort.Slice(vfs, func(i, j int) bool { return vfs[i].PCIAddress < vfs[j].PCIAddress })
	return vfs, nil
}

// netdevName returns the VF's netdev name, or "" when the VF is bound to a
// userspace driver (vfio-pci) and therefore has no netdev.
func (l *SysfsLister) netdevName(vfAddr string) string {
	netDir := filepath.Join(l.SysBusPCI, vfAddr, "net")
	entries, err := os.ReadDir(netDir)
	if err != nil || len(entries) == 0 {
		return ""
	}
	return entries[0].Name()
}

func readIntFile(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
