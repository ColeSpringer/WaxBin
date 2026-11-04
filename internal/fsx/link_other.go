//go:build !windows

package fsx

import "io/fs"

// isLink reports a symbolic link.
func isLink(info fs.FileInfo) bool { return info.Mode()&fs.ModeSymlink != 0 }
