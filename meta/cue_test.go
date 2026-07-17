package meta

import (
	"testing"

	"github.com/colespringer/waxbin/model"
)

const ripCue = `PERFORMER "Album Performer"
TITLE "The Album"
REM GENRE "Jazz"
REM DATE 1997
FILE "album.flac" WAVE
  TRACK 01 AUDIO
    TITLE "First"
    PERFORMER "Alice"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Second"
    INDEX 01 00:05:00
`

// TestParseCueSheet checks album-level fields, per-track fields, and that a track
// with no PERFORMER of its own leaves it empty (the scanner inherits the album's).
func TestParseCueSheet(t *testing.T) {
	s := ParseCueSheet(ripCue)
	if s == nil {
		t.Fatal("ParseCueSheet returned nil for a two-track sheet")
	}
	if s.Title != "The Album" || s.Performer != "Album Performer" {
		t.Errorf("album = %q by %q, want The Album by Album Performer", s.Title, s.Performer)
	}
	if s.Genre != "Jazz" || s.Year != 1997 {
		t.Errorf("genre/year = %q/%d, want Jazz/1997", s.Genre, s.Year)
	}
	if len(s.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(s.Tracks))
	}
	if s.Tracks[0].Number != 1 || s.Tracks[0].Title != "First" ||
		s.Tracks[0].Performer != "Alice" || s.Tracks[0].StartFrames != 0 || !s.Tracks[0].StartValid {
		t.Errorf("track 1 = %+v, want {1 First Alice 0 true}", s.Tracks[0])
	}
	// 00:05:00 is MM:SS:FF = 5 seconds = 375 frames; no track performer (inherits the
	// album's).
	if s.Tracks[1].Number != 2 || s.Tracks[1].Performer != "" ||
		s.Tracks[1].StartFrames != 375 || !s.Tracks[1].StartValid {
		t.Errorf("track 2 = %+v, want number 2, no performer, start 375 frames", s.Tracks[1])
	}
}

// TestParseCueTimeKeepsTheFrame is the regression guard the frame unit exists for: an
// INDEX on a frame not divisible by 3 does not land on a whole millisecond, so the
// old ms parser truncated it away. 03:15:22 is (3*60+15)*75 + 22 = 14647 frames,
// which at 44.1 kHz is sample 14647*588 = 8612436. The ms path reached 195293 ms ->
// sample 8612421, missing by 15 samples: a third of a millisecond of the neighboring
// track served under this one's name.
func TestParseCueTimeKeepsTheFrame(t *testing.T) {
	frames, ok := parseCueTime("03:15:22")
	if !ok {
		t.Fatal("parseCueTime(03:15:22) rejected a well-formed timestamp")
	}
	if frames != 14647 {
		t.Fatalf("frames = %d, want 14647", frames)
	}
	if got := frames * 44100 / 75; got != 8612436 {
		t.Errorf("sample at 44.1 kHz = %d, want 8612436", got)
	}
	// The derived millisecond is allowed to be lossy; that is FramesToMS's whole
	// contract. Pinned here so the direction of the loss stays documented.
	if got := model.FramesToMS(frames); got != 195293 {
		t.Errorf("FramesToMS(14647) = %d, want 195293", got)
	}
}

// TestParseCueTimeRejects covers the three bounds ported alongside the unit. Each
// used to yield 0, which a seek could absorb and a content window cannot: it names
// the first sample of the album.
func TestParseCueTimeRejects(t *testing.T) {
	for _, s := range []string{
		"00:60:00",   // SS past 59
		"00:00:75",   // FF past 74 (a second holds 75 frames, 0-74)
		"6001:00:00", // MM past the 100-hour cap, which is what bounds the arithmetic
		"12:34",      // not MM:SS:FF at all
		"aa:bb:cc",   // non-numeric
		"-1:00:00",   // signed: Atoi takes it, a position cannot
		"",
	} {
		if frames, ok := parseCueTime(s); ok {
			t.Errorf("parseCueTime(%q) = %d, true; want rejected", s, frames)
		}
	}
	// The bounds are inclusive at the top: 59 seconds and frame 74 are both legal.
	if frames, ok := parseCueTime("00:59:74"); !ok || frames != 59*75+74 {
		t.Errorf("parseCueTime(00:59:74) = %d, %v; want %d, true", frames, ok, 59*75+74)
	}
}

// TestParseCueSheetRejectedIndexIsNotZero: a track whose INDEX will not parse must
// come back StartValid=false rather than StartFrames=0, because 0 is a real offset
// naming the head of the rip and the scanner would carve the album's opening under
// this track's name.
func TestParseCueSheetRejectedIndexIsNotZero(t *testing.T) {
	s := ParseCueSheet("  TRACK 01 AUDIO\n    TITLE \"Broken\"\n    INDEX 01 00:99:00\n")
	if s == nil || len(s.Tracks) != 1 {
		t.Fatalf("ParseCueSheet = %+v, want one track", s)
	}
	if s.Tracks[0].StartValid {
		t.Errorf("track = %+v, want StartValid false for a malformed INDEX", s.Tracks[0])
	}
	// A TRACK that declares no INDEX 01 at all is the same condition.
	s = ParseCueSheet("  TRACK 01 AUDIO\n    TITLE \"Indexless\"\n")
	if s == nil || len(s.Tracks) != 1 {
		t.Fatalf("ParseCueSheet = %+v, want one track", s)
	}
	if s.Tracks[0].StartValid {
		t.Errorf("track = %+v, want StartValid false for a track with no INDEX 01", s.Tracks[0])
	}
}

// TestParseCueChaptersUnchanged confirms ParseCue still projects the sheet's tracks
// into file-relative navigation chapters (the book path's contract).
func TestParseCueChaptersUnchanged(t *testing.T) {
	chs := ParseCue(ripCue)
	if len(chs) != 2 {
		t.Fatalf("chapters = %d, want 2", len(chs))
	}
	if chs[0].Position != 0 || chs[0].Title != "First" || chs[0].FileStartMS != 0 {
		t.Errorf("chapter 0 = %+v, want position 0 First start 0", chs[0])
	}
	if chs[1].Position != 1 || chs[1].Title != "Second" || chs[1].FileStartMS != 5000 {
		t.Errorf("chapter 1 = %+v, want position 1 Second start 5000", chs[1])
	}
}

// TestParseCueChaptersDropUnindexedTracks: a chapter's start is not only its own
// coordinate, because the book read path fills each open end from the next chapter's
// start. So a track with no usable INDEX 01 has to be dropped rather than anchored at
// 0: anchoring it would misplace that chapter and also report the chapter before it
// as ending at the start of the book.
func TestParseCueChaptersDropUnindexedTracks(t *testing.T) {
	chs := ParseCue("FILE \"book.m4b\" WAVE\n" +
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n" +
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:05:00\n" +
		"  TRACK 03 AUDIO\n    TITLE \"Broken\"\n    INDEX 01 00:99:00\n" +
		"  TRACK 04 AUDIO\n    TITLE \"Four\"\n    INDEX 01 00:10:00\n")
	if len(chs) != 3 {
		t.Fatalf("chapters = %d, want 3 (the unindexed TRACK 03 is dropped): %+v", len(chs), chs)
	}
	for _, c := range chs {
		if c.Title == "Broken" {
			t.Fatalf("the unindexed track became a chapter at %d ms", c.FileStartMS)
		}
	}
	// Positions stay dense and ordered, and no chapter after the first sits at 0. A
	// spurious 0 here is what would truncate its predecessor on read.
	for i, c := range chs {
		if c.Position != i {
			t.Errorf("chapter %d has position %d; the drop must not leave a gap", i, c.Position)
		}
		if i > 0 && c.FileStartMS == 0 {
			t.Errorf("chapter %d (%q) starts at 0; the preceding chapter's end is read off "+
				"this value and would collapse", i, c.Title)
		}
	}
}

// TestParseCueSheetTrimsQuotedPadding: whitespace padding inside quoted values is
// stripped from album/track fields and the REM DATE year, not just the outer quotes.
func TestParseCueSheetTrimsQuotedPadding(t *testing.T) {
	padded := "PERFORMER \"  Padded Band  \"\n" +
		"TITLE \" Spaced Album \"\n" +
		"REM GENRE \" Jazz \"\n" +
		"REM DATE \" 1999 \"\n" +
		"  TRACK 01 AUDIO\n" +
		"    TITLE \"  Padded Track  \"\n" +
		"    PERFORMER \" Solo \"\n" +
		"    INDEX 01 00:00:00\n"
	s := ParseCueSheet(padded)
	if s == nil {
		t.Fatal("ParseCueSheet returned nil")
	}
	if s.Performer != "Padded Band" || s.Title != "Spaced Album" || s.Genre != "Jazz" || s.Year != 1999 {
		t.Errorf("album = perf %q title %q genre %q year %d, want Padded Band/Spaced Album/Jazz/1999",
			s.Performer, s.Title, s.Genre, s.Year)
	}
	if len(s.Tracks) != 1 || s.Tracks[0].Title != "Padded Track" || s.Tracks[0].Performer != "Solo" {
		t.Errorf("track = %+v, want title Padded Track performer Solo", s.Tracks)
	}
}

// TestParseCueSheetEmpty returns nil when the sheet declares no TRACK.
func TestParseCueSheetEmpty(t *testing.T) {
	if s := ParseCueSheet("REM just a comment\nTITLE \"Nope\"\n"); s != nil {
		t.Errorf("ParseCueSheet(trackless) = %+v, want nil", s)
	}
}
