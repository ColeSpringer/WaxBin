//go:build windows

package testpath

import (
	"testing"

	"golang.org/x/sys/windows"
)

// ShortPath is path spelled with the 8.3 short name of each name the volume made one
// for, and path itself where it made none.
func ShortPath(tb testing.TB, path string) string {
	tb.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		tb.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
		if err != nil {
			tb.Fatalf("short name of %s: %v", path, err)
		}
		if n < uint32(len(buf)) {
			return windows.UTF16ToString(buf[:n])
		}
		buf = make([]uint16, n) // too small: n is the size it needs
	}
}
