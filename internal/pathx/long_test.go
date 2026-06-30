package pathx

import "testing"

// TestLongLeavesShortPathsAlone holds on every platform: a path short enough to
// fit MAX_PATH is returned unchanged (no-op off Windows; below the threshold on
// Windows), so logs and the catalog keep the plain form.
func TestLongLeavesShortPathsAlone(t *testing.T) {
	for _, p := range []string{"/music/artist/album/01 - song.flac", `C:\music\a.mp3`} {
		if got := Long(p); got != p {
			t.Errorf("Long(%q) = %q, want unchanged", p, got)
		}
	}
}
