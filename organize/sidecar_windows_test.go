//go:build windows

package organize

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/internal/fsx"
)

// TestMoveSidecarsFollowACaseOnlyRename: on a case-insensitive filesystem the renamed
// file answers to its old name, which must not read as a second encoding keeping the
// lyrics; they are respelled with it.
func TestMoveSidecarsFollowACaseOnlyRename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "Song.mp3"), filepath.Join(dir, "song.mp3")
	mustWrite(t, src)
	mustWrite(t, filepath.Join(dir, "Song.lrc"))
	if err := fsx.Move(src, dst); err != nil {
		t.Fatal(err)
	}
	if n := New(nil, nil, nil).moveSidecars(fsx.NewSpeller(dir, nil), src, dst, nil); n != 1 {
		t.Fatalf("carried %d sidecars, want the lyrics", n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"song.lrc", "song.mp3"}) {
		t.Fatalf("folder holds %v, want the lyrics respelled with the file", names)
	}
}
