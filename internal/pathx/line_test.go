package pathx

import (
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testpath"
)

// TestPlaylistLinesAnchorAlike: a line that names a path from a root is anchored on every
// platform, absolute, rooted below no drive, or written on Windows with backslashes, and a
// relative line is anchored on none, whichever separator it uses.
func TestPlaylistLinesAnchorAlike(t *testing.T) {
	t.Parallel()
	for _, line := range []string{testpath.Vol() + "/m/a.flac", "/m/a.flac", `\m\a.flac`} {
		if !LineAnchored(line) {
			t.Errorf("%q reads as unanchored", line)
		}
	}
	for _, line := range []string{"m/a.flac", `m\a.flac`, "a.flac", "./a.flac", "../a.flac"} {
		if LineAnchored(line) {
			t.Errorf("%q reads as anchored", line)
		}
	}
	// The line as written comes first; a backslash line is read again only where a
	// backslash is no separator.
	want := 1
	if filepath.Separator == '/' {
		want = 2
	}
	if got := LineReadings(`m\a.flac`); len(got) != want || got[0] != filepath.Clean(filepath.FromSlash(`m\a.flac`)) {
		t.Errorf(`readings of m\a.flac = %q, want %d starting with the line as written`, got, want)
	}
}
