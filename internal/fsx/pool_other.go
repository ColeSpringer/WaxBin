//go:build !(darwin || freebsd || linux || netbsd)

package fsx

// pool has no pool to read here, so a pool is a device.
func pool(path string) (string, bool) { return volume(path) }

func peers(string) []string { return nil }

func mountPoints() ([]string, bool) { return nil, false }
