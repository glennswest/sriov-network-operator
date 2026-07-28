//go:build !linux

package vfhygiene

// permanentAddress is unavailable off Linux.
func permanentAddress(string) (string, error) { return "", errNotLinux }
