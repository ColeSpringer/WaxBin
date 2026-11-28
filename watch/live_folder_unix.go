//go:build unix

package watch

import (
	"io/fs"
	"syscall"
)

// fileID is the inode of the file info describes, which a file renamed over another
// does not share with it.
func fileID(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
