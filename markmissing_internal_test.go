package waxbin

import (
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestLibraryForRawPathMatchesRawBytes pins that the mount gate resolves a file to
// its library on raw OS bytes, both sides. DisplayRoot is a lossy UTF-8 rendering, so
// a root carrying invalid bytes renders with replacement runes: matching or statting
// that string would miss the real directory and refuse every non-forced mark-missing
// under the library. The end-to-end shape cannot be staged everywhere, since a
// filesystem that enforces UTF-8 names (APFS, HFS+) rejects such a root outright.
func TestLibraryForRawPathMatchesRawBytes(t *testing.T) {
	rawRootBytes := []byte("/mnt/m\xffusic")
	libs := []*model.Library{
		{PID: "other", Root: []byte("/mnt/spoken"), DisplayRoot: "/mnt/spoken"},
		// DisplayRoot is what a lossy rendering of the raw bytes looks like: it is not
		// the path on disk, and it is not even the same length.
		{PID: "music", Root: rawRootBytes, DisplayRoot: "/mnt/m�usic"},
	}

	got := libraryForRawPath(libs, []byte("/mnt/m\xffusic/album/one.mp3"))
	if got == nil || got.PID != "music" {
		t.Fatalf("libraryForRawPath = %v, want the music library (raw bytes must match raw bytes)", got)
	}
	if r := rawRoot(got); r != string(rawRootBytes) {
		t.Errorf("rawRoot = %q, want the raw bytes %q (the caller stats this)", r, string(rawRootBytes))
	}

	// The lossy rendering names no library, which is exactly why it must not be the
	// string the gate matches or stats.
	if got := libraryForRawPath(libs, []byte("/mnt/m�usic/album/one.mp3")); got != nil {
		t.Errorf("a display-rendered path matched %s; the gate would stat the wrong directory", got.PID)
	}

	if got := libraryForRawPath(libs, []byte("/elsewhere/one.mp3")); got != nil {
		t.Errorf("path under no root matched %s, want nil (nothing to gate on)", got.PID)
	}
}

// TestRawRootFallsBackToDisplay covers the degenerate row: a library with no raw root
// bytes still resolves to something statable rather than an empty path, which would
// stat the process working directory.
func TestRawRootFallsBackToDisplay(t *testing.T) {
	if r := rawRoot(&model.Library{DisplayRoot: "/mnt/music"}); r != "/mnt/music" {
		t.Errorf("rawRoot with no raw bytes = %q, want the display root", r)
	}
}
