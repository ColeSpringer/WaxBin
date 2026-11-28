// Package diskfree reports the room left on the filesystem holding a folder, for the
// import and download preflight checks. It answers on Linux, macOS, Windows, FreeBSD,
// OpenBSD and NetBSD; elsewhere Available returns ErrUnsupported and callers skip the
// check.
package diskfree

import (
	"errors"
	"math"
)

// ErrUnsupported is returned by Available where no free-space probe is available.
var ErrUnsupported = errors.New("diskfree: unsupported on this platform")

// Available returns the bytes an unprivileged caller can still write to the filesystem
// holding dir, an existing folder. A filesystem reporting more than an int64 holds
// answers math.MaxInt64.
func Available(dir string) (int64, error) { return available(dir) }

// Need returns the room a write of size bytes asks for when reserve bytes are to stay
// free after it. A negative amount counts as none, and the sum stops at math.MaxInt64
// rather than wrap into a figure any disk would meet.
func Need(size, reserve int64) int64 {
	size, reserve = max(size, 0), max(reserve, 0)
	if size > math.MaxInt64-reserve {
		return math.MaxInt64
	}
	return size + reserve
}
