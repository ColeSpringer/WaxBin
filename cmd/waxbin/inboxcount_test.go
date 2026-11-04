package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testpath"
)

// TestInboxCountFollowsALinkedFolder: the inbox listing's pending count walks a
// staging folder given as a link to a folder as that folder.
func TestInboxCountFollowsALinkedFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "inbox")
	for _, name := range []string{"a.mp3", filepath.Join("sub", "b.flac"), "notes.txt"} {
		p := filepath.Join(real, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	testpath.Symlink(t, real, link)
	if n := countAudio(link); n != 2 {
		t.Errorf("countAudio(link) = %d, want 2", n)
	}
}
