//go:build windows

package fsx

import (
	"io/fs"
	"syscall"
)

// isLink reports a symbolic link, or a junction or mount point: Go reports those as
// irregular without the folder bit, the one case a folder reparse point is irregular,
// while a folder carrying another kind of reparse point (a cloud placeholder) keeps it.
func isLink(info fs.FileInfo) bool {
	m := info.Mode()
	if m&fs.ModeSymlink != 0 {
		return true
	}
	if m&fs.ModeIrregular == 0 || m&fs.ModeDir != 0 {
		return false
	}
	sys, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && sys.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0
}
