//go:build darwin || freebsd || linux || netbsd || openbsd

package diskfree

import (
	"math"
	"testing"
)

// TestSpan covers each width a platform reports its counts in.
func TestSpan(t *testing.T) {
	t.Parallel()
	const most = math.MaxInt64
	check := func(name string, got, want int64) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	check("blocks of 4096", span(uint64(1000), int64(4096)), 4096000)
	check("a 32-bit size", span(uint64(1000), uint32(512)), 512000)
	check("signed blocks", span(int64(1000), uint64(512)), 512000)
	check("no blocks", span(uint64(0), int64(4096)), 0)
	check("blocks into the root reserve", span(int64(-25), uint64(4096)), 0)
	check("no size", span(uint64(1000), int64(0)), 0)
	check("a negative size", span(uint64(1000), int32(-1)), 0)
	check("the most an int64 holds", span(uint64(most), int64(1)), most)
	check("one past it", span(uint64(most)+1, int64(1)), most)
	check("a product past an int64", span(uint64(1)<<51, int64(4096)), most)
	check("a product past a uint64", span(uint64(math.MaxUint64), uint64(math.MaxUint64)), most)
	check("just within", span(uint64(1)<<50, int64(4096)), 1<<62)
}
