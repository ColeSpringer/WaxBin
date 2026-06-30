//go:build unix

package diskfree

import "syscall"

// Available returns the bytes available to an unprivileged user on the filesystem
// containing path.
func Available(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bavail counts blocks available to non-root; Bsize is the block size. The
	// casts cover platforms where Bsize is signed (Linux) or 32-bit (Darwin).
	return st.Bavail * uint64(st.Bsize), nil
}
