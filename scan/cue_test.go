package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
)

const twoTrackCue = `FILE "book.m4b" MP3
  TRACK 01 AUDIO
    TITLE "Chapter One"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Chapter Two"
    INDEX 01 05:00:00
`

// TestCueChaptersForBook: a book with no embedded chapters picks up chapters from a
// sibling .cue (source='cue'), and a cue-only edit is applied on the fast-path.
func TestCueChaptersForBook(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()

	// An .m4b (classified as a book) with no embedded chapters, plus a sibling .cue.
	book := filepath.Join(root, "book.m4b")
	spec := testaudio.MP3Spec{Title: "T", Artist: "Auth", AlbumArtist: "Auth", Album: "My Book", Audio: testaudio.AudioWithSeed(4)}
	writeMP3Raw(t, book, testaudio.BuildMP3FromSpec(spec))
	if err := os.WriteFile(filepath.Join(root, "book.cue"), []byte(twoTrackCue), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)

	pid := currentItemPID(t, st, "My Book")
	chs, err := st.Chapters(ctx, pid)
	if err != nil {
		t.Fatalf("chapters: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("cue chapters = %d, want 2 (%+v)", len(chs), chs)
	}
	if chs[0].Title != "Chapter One" || chs[1].Title != "Chapter Two" {
		t.Errorf("chapter titles = %q/%q, want Chapter One/Two", chs[0].Title, chs[1].Title)
	}
	// Chapter two starts at 5:00 = 300000 ms (file-relative).
	if chs[1].StartMS != 300000 {
		t.Errorf("chapter two start = %d ms, want 300000", chs[1].StartMS)
	}

	// Cue-only edit over unchanged audio: add a third chapter, bump the .cue mtime.
	threeTrack := twoTrackCue + "  TRACK 03 AUDIO\n    TITLE \"Chapter Three\"\n    INDEX 01 10:00:00\n"
	if err := os.WriteFile(filepath.Join(root, "book.cue"), []byte(threeTrack), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(root, "book.cue"), future, future)

	r := scanAll(t, sc, lib, false)
	if r.Unchanged != 1 {
		t.Fatalf("expected the book to fast-path (Unchanged=1), got %+v", r)
	}
	chs, err = st.Chapters(ctx, pid)
	if err != nil {
		t.Fatalf("chapters after edit: %v", err)
	}
	if len(chs) != 3 {
		t.Fatalf("chapters after cue edit = %d, want 3", len(chs))
	}
}

// TestCueChaptersOnForcedRescan: a .cue added to an unchanged book is imported by a
// forced rescan (which bypasses the fast-path), not skipped by the content gate.
func TestCueChaptersOnForcedRescan(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()

	book := filepath.Join(root, "book.m4b")
	spec := testaudio.MP3Spec{Title: "T", Artist: "Auth", AlbumArtist: "Auth", Album: "Forced Book", Audio: testaudio.AudioWithSeed(8)}
	writeMP3Raw(t, book, testaudio.BuildMP3FromSpec(spec))
	scanAll(t, sc, lib, false) // no .cue yet
	pid := currentItemPID(t, st, "Forced Book")

	// Add the .cue AFTER the first scan, then force a rescan (bypasses the fast-path).
	if err := os.WriteFile(filepath.Join(root, "book.cue"), []byte(twoTrackCue), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, true) // --force

	chs, err := st.Chapters(ctx, pid)
	if err != nil {
		t.Fatalf("chapters: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("forced rescan imported %d cue chapters, want 2", len(chs))
	}
}

func writeMP3Raw(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

var _ = model.KindBook
