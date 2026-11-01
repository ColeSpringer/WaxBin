//go:build unix

package fsx

import (
	"errors"
	"io/fs"
	"syscall"
)

func device(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

func inUse(err error) bool { return errors.Is(err, syscall.EBUSY) }

// crossDevice reports a rename refused because it would cross filesystems.
func crossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }
