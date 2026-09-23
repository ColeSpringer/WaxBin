package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
// sibling .cue (source='cue'), and a cue-only edit reaches the full path, which is
// the one place a cue diagnostic is re-derived and so can be cleared.
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

	// A typo in the added third chapter costs that chapter alone and is reported;
	// fixing it brings the chapter in and clears the report. Both edits leave the
	// audio alone.
	cuePath := filepath.Join(root, "book.cue")
	threeTrack := twoTrackCue + "  TRACK 03 AUDIO\n    TITLE \"Chapter Three\"\n    INDEX 01 10:00:00\n"
	for i, cue := range []string{strings.Replace(threeTrack, "10:00:00", "10:60:00", 1), threeTrack} {
		if err := os.WriteFile(cuePath, []byte(cue), 0o644); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Duration(i+1) * time.Hour)
		_ = os.Chtimes(cuePath, future, future)
		// The typo pass keeps the same two chapters, so only the fix is a sidecar change.
		r := scanAll(t, sc, lib, false)
		if r.Unchanged != 0 || r.SidecarsUpdated != i {
			t.Fatalf("pass %d: a cue-only edit should reach the full path, got %+v", i, r)
		}
		detail := cueDropDetail(t, st)
		if i == 0 && (!strings.Contains(detail, "line 10: track 3: time") ||
			!strings.HasSuffix(detail, "; the readable lines were applied")) {
			t.Errorf("pass 0: detail = %q, want line 10 named and the readable lines applied", detail)
		}
		if i == 1 && detail != "" {
			t.Errorf("pass 1: a stale cue_track_dropped survived the fix: %q", detail)
		}
		chs, err := st.Chapters(ctx, pid)
		if err != nil {
			t.Fatalf("pass %d: chapters: %v", i, err)
		}
		if want := 2 + i; len(chs) != want {
			t.Errorf("pass %d: chapters = %d, want %d", i, len(chs), want)
		}
	}
	if got := currentItemPID(t, st, "My Book"); got != pid {
		t.Errorf("the book's pid moved from %s to %s across cue edits", pid, got)
	}
}

// TestCueSheetWithNoReadableTrackKeepsTheBookChapter: a book sheet whose every TRACK
// line is misspelled yields warnings and no chapter. The part keeps its whole-file
// chapter rather than being left with none, and the diagnostic lists the lines.
func TestCueSheetWithNoReadableTrackKeepsTheBookChapter(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	spec := testaudio.MP3Spec{Title: "Part", Artist: "Auth", AlbumArtist: "Auth", Album: "Typo Book", Audio: testaudio.AudioWithSeed(6)}
	writeMP3Raw(t, filepath.Join(root, "book.m4b"), testaudio.BuildMP3FromSpec(spec))
	if err := os.WriteFile(filepath.Join(root, "book.cue"),
		[]byte("TRCK 01 AUDIO\n  INDEX 01 00:00:00\nTRCK 02 AUDIO\n  INDEX 01 05:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)

	chs, err := st.Chapters(ctx, currentItemPID(t, st, "Typo Book"))
	if err != nil {
		t.Fatalf("chapters: %v", err)
	}
	if len(chs) != 1 || chs[0].Title != "Part" {
		t.Errorf("chapters = %+v, want the one whole-file chapter", chs)
	}
	detail := cueDropDetail(t, st)
	if !strings.HasPrefix(detail, "2 line(s) of the cue sheet could not be read: line 2: INDEX outside a TRACK") ||
		!strings.HasSuffix(detail, "; the sheet was not applied") {
		t.Errorf("detail = %q, want both lines and the sheet not applied", detail)
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

// TestCueMultiFileSheetOnABookIsNotApplied: a book sheet that cannot be applied at all
// leaves the part its whole-file chapter, and the detail ends by saying so.
func TestCueMultiFileSheetOnABookIsNotApplied(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	spec := testaudio.MP3Spec{Title: "Part", Artist: "Auth", AlbumArtist: "Auth", Album: "Split Book", Audio: testaudio.AudioWithSeed(7)}
	writeMP3Raw(t, filepath.Join(root, "book.m4b"), testaudio.BuildMP3FromSpec(spec))
	if err := os.WriteFile(filepath.Join(root, "book.cue"), []byte(twoTrackCue+
		"FILE \"other.m4b\" MP3\n  TRACK 03 AUDIO\n    INDEX 01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)

	chs, err := st.Chapters(ctx, currentItemPID(t, st, "Split Book"))
	if err != nil {
		t.Fatalf("chapters: %v", err)
	}
	if len(chs) != 1 || chs[0].Title != "Part" {
		t.Errorf("chapters = %+v, want the one whole-file chapter", chs)
	}
	if detail := cueDropDetail(t, st); !strings.HasPrefix(detail, "the sheet indexes 2 files") ||
		!strings.HasSuffix(detail, "; the sheet was not applied") {
		t.Errorf("detail = %q, want the refusal and the sheet not applied", detail)
	}
}

// TestCueBookDiagnosticMatchesItsChapters: the tracks a book's diagnostic names are
// exactly the ones missing from its chapters, including the earlier of two on one
// start, which the catalog collapses into the later.
func TestCueBookDiagnosticMatchesItsChapters(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	spec := testaudio.MP3Spec{Title: "Part", Artist: "Auth", AlbumArtist: "Auth", Album: "Paired Book", Audio: testaudio.AudioWithSeed(9)}
	writeMP3Raw(t, filepath.Join(root, "book.m4b"), testaudio.BuildMP3FromSpec(spec))
	if err := os.WriteFile(filepath.Join(root, "book.cue"), []byte("FILE \"book.m4b\" MP3\n"+
		"  TRACK 01 AUDIO\n    TITLE \"Intro\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Chapter One\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 03 AUDIO\n    TITLE \"Chapter Two\"\n    INDEX 01 00:00:10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)

	chs, err := st.Chapters(ctx, currentItemPID(t, st, "Paired Book"))
	if err != nil {
		t.Fatalf("chapters: %v", err)
	}
	if len(chs) != 2 || chs[0].Title != "Chapter One" || chs[1].Title != "Chapter Two" {
		t.Errorf("chapters = %+v, want Chapter One and Chapter Two", chs)
	}
	want := `1 cue TRACK(s) dropped from the sheet: TRACK 01 ("Intro") is empty (the next TRACK's INDEX 01 names the same frame)`
	if detail := cueDropDetail(t, st); detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
}

func writeMP3Raw(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

var _ = model.KindBook
