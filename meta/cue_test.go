package meta

import "testing"

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
		s.Tracks[0].Performer != "Alice" || s.Tracks[0].StartMS != 0 {
		t.Errorf("track 1 = %+v, want {1 First Alice 0}", s.Tracks[0])
	}
	// 00:05:00 is MM:SS:FF = 5 seconds = 5000 ms; no track performer (inherits the
	// album's).
	if s.Tracks[1].Number != 2 || s.Tracks[1].Performer != "" || s.Tracks[1].StartMS != 5000 {
		t.Errorf("track 2 = %+v, want number 2, no performer, start 5000", s.Tracks[1])
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
