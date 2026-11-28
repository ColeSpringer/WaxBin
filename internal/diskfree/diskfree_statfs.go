//go:build darwin || freebsd || linux || netbsd || openbsd

package diskfree

import (
	"math"
	"math/bits"
	"os"

	"golang.org/x/sys/unix"
)

// available retries a call a signal interrupted, which a FUSE or NFS mount can report.
func available(dir string) (int64, error) {
	for {
		n, err := statfs(dir)
		if err == nil {
			return n, nil
		}
		if err != unix.EINTR {
			return 0, &os.PathError{Op: probe, Path: dir, Err: err}
		}
	}
}

// span returns blocks times size held within an int64. FreeBSD and OpenBSD count blocks
// signed and go negative once a filesystem is into the space kept for root, which is no
// room.
func span[B, S int32 | int64 | uint32 | uint64](blocks B, size S) int64 {
	if blocks <= 0 || size <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(blocks), uint64(size))
	if hi != 0 || lo > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(lo)
}
