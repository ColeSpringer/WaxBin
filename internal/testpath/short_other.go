//go:build !windows

package testpath

import "testing"

// ShortPath is path itself: only Windows gives a name an 8.3 short spelling.
func ShortPath(_ testing.TB, path string) string { return path }
