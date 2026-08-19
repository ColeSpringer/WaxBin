package scan

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A sub-path a user types can spell the library root in a different case. UnderRoot
// folds, so the guard passes, but scopePrefix and every walked path are then built
// from the wrong bytes: the index preload misses the path byte-range and each file
// misses fileByPathTx, so the whole subtree re-inserts as new file rows. Scan
// re-anchors the walk root on the stored spelling to stop that. This got more
// reachable once EnsureLibrary started folding, because a re-cased root no longer
// errors out before a scan is ever run.
func TestScanReAnchorsRecasedSubPathOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("path case folding is a Windows filesystem property")
	}
	_, lib, sc, _, root := fastPathFixture(t)
	sub := filepath.Join(root, "SomeArtist")
	writeMP3(t, filepath.Join(sub, "a.mp3"), "A", 1)

	if r := scanAll(t, sc, lib, false); r.ItemsCreated != 1 {
		t.Fatalf("first scan created %d items, want 1", r.ItemsCreated)
	}

	// The root spelled differently, with the sub-directory's real on-disk casing
	// under it: `waxbin scan --sub-path c:\Music\SomeArtist` against a C:\Music
	// library. Re-casing a deep component is a different thing and stays out of scope
	// (see the note beside fileByPathDB).
	recased := filepath.Join(strings.ToUpper(root), "SomeArtist")
	res, err := sc.Scan(context.Background(), Request{Library: lib, SubPath: recased}, nil)
	if err != nil {
		t.Fatalf("re-cased sub-path scan: %v", err)
	}
	if res.AudioFiles != 1 {
		t.Fatalf("re-cased sub-path scan saw %d audio files, want 1", res.AudioFiles)
	}
	if res.ItemsCreated != 0 || res.Unchanged != 1 {
		t.Errorf("re-cased sub-path scan = %d created / %d unchanged, want 0 / 1 (it re-inserted the subtree)",
			res.ItemsCreated, res.Unchanged)
	}
}
