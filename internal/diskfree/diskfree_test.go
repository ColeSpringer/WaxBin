package diskfree

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/pathx"
)

func TestNeed(t *testing.T) {
	t.Parallel()
	const most = math.MaxInt64
	tests := []struct{ size, reserve, want int64 }{
		{0, 0, 0},
		{400, 100, 500},
		{400, 0, 400},
		{0, 100, 100},
		{-1, 100, 100},
		{400, -100, 400},
		{math.MinInt64, math.MinInt64, 0},
		{most, 0, most},
		{most, 1, most},
		{1, most, most},
		{most, most, most},
		{most - 5, 5, most},
		{most - 5, 4, most - 1},
	}
	for _, tc := range tests {
		if got := Need(tc.size, tc.reserve); got != tc.want {
			t.Errorf("Need(%d, %d) = %d, want %d", tc.size, tc.reserve, got, tc.want)
		}
	}
}

// probed runs Available on dir, skipping the test on a platform with no probe.
func probed(t *testing.T, dir string) int64 {
	t.Helper()
	n, err := Available(dir)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("no free-space probe on this platform")
	}
	if err != nil {
		t.Fatalf("Available(%q): %v", dir, err)
	}
	if n < 0 {
		t.Fatalf("Available(%q) = %d, want a count", dir, n)
	}
	return n
}

func TestAvailable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if n := probed(t, dir); n == 0 {
		t.Errorf("Available(%q) = 0 on a folder a test can write to", dir)
	}
	probed(t, dir+string(filepath.Separator))
}

// TestAvailableOnALongPath: a folder past the 260 characters Windows holds a plain path to
// answers like any other.
func TestAvailableOnALongPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("d", 40))
	}
	if err := os.MkdirAll(pathx.Long(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	probed(t, dir)
}

// TestAvailableNamesAMissingFolder: the error for a folder that is not there says so and
// names it.
func TestAvailableNamesAMissingFolder(t *testing.T) {
	t.Parallel()
	gone := filepath.Join(t.TempDir(), "gone")
	_, err := Available(gone)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("no free-space probe on this platform")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want one that is fs.ErrNotExist", err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != gone {
		t.Errorf("err = %#v, want a path error naming %q", err, gone)
	}
}
