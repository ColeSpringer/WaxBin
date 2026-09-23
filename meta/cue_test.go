package meta

import (
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/cue"

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

// parseSheet parses text and fails the test if it was refused.
func parseSheet(t *testing.T, text string) *CueSheet {
	t.Helper()
	s, err := ParseCueSheet(text)
	if err != nil {
		t.Fatalf("ParseCueSheet: %v", err)
	}
	return s
}

// TestParseCueSheet checks album-level fields, per-track fields, and that a track
// with no PERFORMER of its own leaves it empty (the scanner inherits the album's).
func TestParseCueSheet(t *testing.T) {
	s := parseSheet(t, ripCue)
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
	if s.Tracks[0].Number != 1 || s.Tracks[0].Title != "First" || s.Tracks[0].Type != "AUDIO" ||
		s.Tracks[0].Performer != "Alice" || s.Tracks[0].StartFrames != 0 || !s.Tracks[0].StartValid {
		t.Errorf("track 1 = %+v, want {1 AUDIO First Alice 0 true}", s.Tracks[0])
	}
	// 00:05:00 is MM:SS:FF = 5 seconds = 375 frames; no track performer (inherits the
	// album's).
	if s.Tracks[1].Number != 2 || s.Tracks[1].Performer != "" ||
		s.Tracks[1].StartFrames != 375 || !s.Tracks[1].StartValid {
		t.Errorf("track 2 = %+v, want number 2, no performer, start 375 frames", s.Tracks[1])
	}
}

// TestCueFrameRateAgreesWithUpstream: model.FramesPerSecond and the sheet parser's
// own frame rate are the same number, which is what lets a stored frame count be
// handed to cue.Samples. model may not import waxflow, so the comparison lives here.
func TestCueFrameRateAgreesWithUpstream(t *testing.T) {
	if model.FramesPerSecond != cue.FramesPerSecond {
		t.Errorf("model.FramesPerSecond = %d, cue.FramesPerSecond = %d; the two address the same frames",
			model.FramesPerSecond, cue.FramesPerSecond)
	}
}

// TestParseCueTimeKeepsTheFrame is the regression guard the frame unit exists for: an
// INDEX on a frame not divisible by 3 does not land on a whole millisecond, so the
// old ms parser truncated it away. 03:15:22 is (3*60+15)*75 + 22 = 14647 frames,
// which at 44.1 kHz is sample 14647*588 = 8612436. The ms path reached 195293 ms ->
// sample 8612421, missing by 15 samples: a third of a millisecond of the neighboring
// track served under this one's name.
func TestParseCueTimeKeepsTheFrame(t *testing.T) {
	s := parseSheet(t, "FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 03:15:22\n")
	if s == nil || len(s.Tracks) != 1 || !s.Tracks[0].StartValid {
		t.Fatalf("ParseCueSheet = %+v, want one track with a start", s)
	}
	if s.Tracks[0].StartFrames != 14647 {
		t.Fatalf("frames = %d, want 14647", s.Tracks[0].StartFrames)
	}
	if got := cue.Samples(14647, 44100); got != 8612436 {
		t.Errorf("sample at 44.1 kHz = %d, want 8612436", got)
	}
	// The derived millisecond is allowed to be lossy; that is FramesToMS's whole
	// contract. Pinned here so the direction of the loss stays documented.
	if got := model.FramesToMS(14647); got != 195293 {
		t.Errorf("FramesToMS(14647) = %d, want 195293", got)
	}
}

// TestParseCueSheetReportsMalformedTime covers the three bounds and the shapes that
// are not a timestamp at all. Each is a warning naming the line to go look at, and
// the INDEX it spelled is dropped, so the track has no start.
func TestParseCueSheetReportsMalformedTime(t *testing.T) {
	for _, ts := range []string{
		"00:60:00",   // SS past 59
		"00:00:75",   // FF past 74 (a second holds 75 frames, 0-74)
		"6001:00:00", // MM past the 100-hour cap, which is what bounds the arithmetic
		"1:2",        // not MM:SS:FF at all
		"-1:00:00",   // signed: Atoi takes it, a position cannot
	} {
		s, err := ParseCueSheet("FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 " + ts + "\n")
		if err != nil {
			t.Errorf("INDEX %q: %v; want a warning, not a refusal", ts, err)
			continue
		}
		if s == nil || len(s.Warnings) != 1 || s.Warnings[0].Line != 3 {
			t.Errorf("INDEX %q parsed to %+v; want one warning on line 3", ts, s)
			continue
		}
		if len(s.Tracks) != 1 || s.Tracks[0].StartValid {
			t.Errorf("INDEX %q: tracks = %+v, want one track with no start", ts, s.Tracks)
		}
	}
	// The bounds are inclusive at the top: 59 seconds and frame 74 are both legal.
	s := parseSheet(t, "FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:59:74\n")
	if s == nil || len(s.Tracks) != 1 || s.Tracks[0].StartFrames != 59*75+74 {
		t.Errorf("INDEX 00:59:74 = %+v, want %d frames", s, 59*75+74)
	}
}

// TestParseCueSheetTrackWithoutIndex01IsNotZero: a track that declares no INDEX 01
// must come back StartValid=false rather than StartFrames=0, because 0 is a real
// offset naming the head of the rip and the scanner would carve the album's opening
// under this track's name.
func TestParseCueSheetTrackWithoutIndex01IsNotZero(t *testing.T) {
	// INDEX 00 is the pregap start, which addresses the previous track's tail; it is
	// not a start and must not be read as one.
	s := parseSheet(t, "FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"Pregap only\"\n    INDEX 00 00:00:05\n")
	if s == nil || len(s.Tracks) != 1 {
		t.Fatalf("ParseCueSheet = %+v, want one track", s)
	}
	if s.Tracks[0].StartValid {
		t.Errorf("track = %+v, want StartValid false for a track with only an INDEX 00", s.Tracks[0])
	}
	// A TRACK that declares no INDEX at all is the same condition.
	s = parseSheet(t, "FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"Indexless\"\n")
	if s == nil || len(s.Tracks) != 1 {
		t.Fatalf("ParseCueSheet = %+v, want one track", s)
	}
	if s.Tracks[0].StartValid {
		t.Errorf("track = %+v, want StartValid false for a track with no INDEX 01", s.Tracks[0])
	}
	// A malformed INDEX 01 leaves the track startless too, and says so in a warning.
	s = parseSheet(t, "FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:99:00\n")
	if s == nil || len(s.Tracks) != 1 || s.Tracks[0].StartValid || len(s.Warnings) != 1 {
		t.Errorf("ParseCueSheet = %+v, want one startless track and one warning", s)
	}
}

// TestParseCueSheetReadsPastAnUnreadableLine: one mistyped INDEX costs its own track's
// start and nothing else. The rest of the sheet reads as written.
func TestParseCueSheetReadsPastAnUnreadableLine(t *testing.T) {
	s := parseSheet(t, "TITLE \"Kept\"\nFILE \"a.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    INDEX 01 00:0x:00\n"+
		"  TRACK 02 AUDIO\n    INDEX 01 00:00:10\n")
	if s == nil {
		t.Fatal("ParseCueSheet returned nil")
	}
	if s.Title != "Kept" {
		t.Errorf("title = %q, want Kept", s.Title)
	}
	if len(s.Tracks) != 2 || s.Tracks[0].StartValid || !s.Tracks[1].StartValid || s.Tracks[1].StartFrames != 10 {
		t.Errorf("tracks = %+v, want track 1 startless and track 2 at frame 10", s.Tracks)
	}
	if len(s.Warnings) != 1 || s.Warnings[0].String() != `line 4: track 1: time "00:0x:00" is not MM:SS:FF` {
		t.Errorf("warnings = %+v, want the one on line 4", s.Warnings)
	}
}

// TestParseCueSheetKeepsWarningsWithoutTracks: a sheet whose every TRACK line is
// misspelled opens no track, and the warnings are all there is to say about it. They
// have to reach the caller, so the sheet is not reported as empty.
func TestParseCueSheetKeepsWarningsWithoutTracks(t *testing.T) {
	s := parseSheet(t, "TRCK 01 AUDIO\n  INDEX 01 00:00:00\nTRCK 02 AUDIO\n  INDEX 01 00:05:00\n")
	if s == nil {
		t.Fatal("ParseCueSheet returned nil; want the warnings kept")
	}
	if len(s.Tracks) != 0 {
		t.Errorf("tracks = %+v, want none", s.Tracks)
	}
	if len(s.Warnings) != 2 || s.Warnings[0].Line != 2 || s.Warnings[1].Line != 4 {
		t.Errorf("warnings = %+v, want lines 2 and 4", s.Warnings)
	}
}

// TestParseCueSheetMarksTruncatedWarnings: upstream stops recording warnings at 64,
// so a sheet that reaches that many may have more, and the report must not claim an
// exact count.
func TestParseCueSheetMarksTruncatedWarnings(t *testing.T) {
	bad := func(n int) string { return strings.Repeat("INDEX 01 00:00:00\n", n) }
	s := parseSheet(t, bad(70))
	if s == nil || len(s.Warnings) != 64 || !s.WarningsTruncated {
		t.Errorf("70 unread lines = %d warnings, truncated %v; want 64, true", len(s.Warnings), s.WarningsTruncated)
	}
	if s = parseSheet(t, bad(10)); s == nil || len(s.Warnings) != 10 || s.WarningsTruncated {
		t.Errorf("10 unread lines = %+v, want 10 warnings, not truncated", s)
	}
}

// TestParseCueRefusesAWarnedSheet: chapters set --file is an explicit command, so it
// refuses a sheet with an unread line and names it rather than setting a partial list
// the user would have to redo.
func TestParseCueRefusesAWarnedSheet(t *testing.T) {
	chs, err := ParseCue("FILE \"book.m4b\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:99:00\n" +
		"  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n")
	if err == nil {
		t.Fatalf("ParseCue = %+v, want the sheet refused", chs)
	}
	if !strings.Contains(err.Error(), `line 3: track 1: time "00:99:00"`) {
		t.Errorf("error %q does not name line 3", err)
	}
	if strings.Contains(err.Error(), "cue:") {
		t.Errorf("error %q still carries upstream's prefix", err)
	}
}

// TestParseCueChaptersUnchanged confirms ParseCue still projects the sheet's tracks
// into file-relative navigation chapters (the book path's contract).
func TestParseCueChaptersUnchanged(t *testing.T) {
	chs, err := ParseCue(ripCue)
	if err != nil {
		t.Fatalf("ParseCue: %v", err)
	}
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
	chs := parseSheet(t, "FILE \"book.m4b\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:05:00\n"+
		"  TRACK 03 AUDIO\n    TITLE \"Broken\"\n    INDEX 00 00:07:00\n"+
		"  TRACK 04 AUDIO\n    TITLE \"Four\"\n    INDEX 01 00:10:00\n").Chapters()
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

// TestChapterDropsMatchTheStoredChapters: a book's diagnostic names the tracks that do
// not end up as chapters. Two on one start collapse into the later once sorted, as the
// catalog stores them, even when the sheet lists them out of order.
func TestChapterDropsMatchTheStoredChapters(t *testing.T) {
	s := parseSheet(t, "FILE \"book.m4b\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"Late\"\n    INDEX 01 00:00:10\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Opening\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 03 AUDIO\n    TITLE \"Same Start\"\n    INDEX 01 00:00:10\n"+
		"  TRACK 04 MODE1/2352\n    INDEX 01 00:00:20\n")
	want := []string{
		`TRACK 04 is a data track`,
		`TRACK 01 ("Late") is empty (the next TRACK's INDEX 01 names the same frame)`,
	}
	if got := s.ChapterDrops(); !slices.Equal(got, want) {
		t.Errorf("ChapterDrops = %q, want %q", got, want)
	}
}

// TestCueTrackDescNamesAnUnnumberedTrack: a TRACK line whose number cannot be read is
// numbered -1 upstream, which is no number a sheet can hold, so the report says so in
// words.
func TestCueTrackDescNamesAnUnnumberedTrack(t *testing.T) {
	s := parseSheet(t, "FILE \"book.m4b\" WAVE\n  TRACK AUDIO\n    TITLE \"Lost\"\n    INDEX 00 00:00:05\n")
	want := []string{`an unnumbered TRACK ("Lost") has no usable INDEX 01`}
	if got := s.ChapterDrops(); !slices.Equal(got, want) {
		t.Errorf("ChapterDrops = %q, want %q", got, want)
	}
}

// TestParseCueRefusesATrackWithoutIndex01: a misspelled INDEX line is an unknown
// command upstream, skipped with no warning, and it leaves its track with no start.
// chapters set --file names that track rather than setting the chapters around it.
func TestParseCueRefusesATrackWithoutIndex01(t *testing.T) {
	chs, err := ParseCue("TRACK 01 AUDIO\n  TITLE \"Opening\"\n  INDEX 01 00:00:00\n" +
		"TRACK 02 AUDIO\n  TITLE \"Middle\"\n  INDX 01 00:05:00\n" +
		"TRACK 03 AUDIO\n  TITLE \"End\"\n  INDEX 01 00:10:00\n")
	if err == nil {
		t.Fatalf("ParseCue = %+v, want the sheet refused", chs)
	}
	if !strings.Contains(err.Error(), `TRACK 02 ("Middle") has no usable INDEX 01`) {
		t.Errorf("error %q does not name TRACK 02", err)
	}
}

// TestParseCueChaptersSkipDataTracks: a mixed-mode disc's first track is data, and
// data is not a chapter. It carries an INDEX 01 like any other track, so only its
// datatype tells it apart. A CD+G karaoke track is audio with graphics beside it, so
// it stays.
func TestParseCueChaptersSkipDataTracks(t *testing.T) {
	chs, err := ParseCue("FILE \"disc.flac\" WAVE\n" +
		"  TRACK 01 MODE1/2352\n    TITLE \"Data\"\n    INDEX 01 00:00:00\n" +
		"  TRACK 02 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:05:00\n" +
		"  TRACK 03 CDG\n    TITLE \"Two\"\n    INDEX 01 00:10:00\n")
	if err != nil {
		t.Fatalf("ParseCue: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("chapters = %d, want 2 (the data track is dropped): %+v", len(chs), chs)
	}
	if chs[0].Title != "One" || chs[1].Title != "Two" {
		t.Errorf("chapters = %q/%q, want One/Two", chs[0].Title, chs[1].Title)
	}
}

// TestParseCueSheetTrimsQuotedPadding: whitespace padding inside quoted values is
// stripped from album/track fields and the REM DATE year, not just the outer quotes.
// Upstream keeps a quoted run verbatim, so the trimming is this adapter's.
func TestParseCueSheetTrimsQuotedPadding(t *testing.T) {
	padded := "PERFORMER \"  Padded Band  \"\n" +
		"TITLE \" Spaced Album \"\n" +
		"REM GENRE \" Jazz \"\n" +
		"REM DATE \" 1999 \"\n" +
		"  TRACK 01 AUDIO\n" +
		"    TITLE \"  Padded Track  \"\n" +
		"    PERFORMER \" Solo \"\n" +
		"    INDEX 01 00:00:00\n"
	s := parseSheet(t, padded)
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

// TestParseCueSheetEmpty returns a nil sheet and no error when nothing is there to
// read: a sheet with no TRACK is as good as an absent one.
func TestParseCueSheetEmpty(t *testing.T) {
	s := parseSheet(t, "REM just a comment\nTITLE \"Nope\"\n")
	if s != nil {
		t.Errorf("ParseCueSheet(trackless) = %+v, want nil", s)
	}
	s = parseSheet(t, "FILE \"album.flac\" WAVE\n")
	if s != nil {
		t.Errorf("ParseCueSheet(FILE with no TRACK) = %+v, want nil", s)
	}
}

// TestParseCueSheetRefusesMultipleFiles: a sheet indexing several files describes a
// rip whose tracks are already separate, so there is nothing to carve. Refusing by
// name beats picking the first file, which would be a plausible wrong answer.
func TestParseCueSheetRefusesMultipleFiles(t *testing.T) {
	_, err := ParseCueSheet("FILE \"one.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
		"FILE \"two.flac\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n")
	if err == nil {
		t.Fatal("a multi-FILE sheet parsed; want it refused")
	}
	if !strings.Contains(err.Error(), "files") {
		t.Errorf("error %q does not say the sheet indexes several files", err)
	}
}

// TestParseCueSheetUnquotedTitleReadsTheLine: an unquoted one-string operand is the
// rest of its line, as a hand-written sheet means it.
func TestParseCueSheetUnquotedTitleReadsTheLine(t *testing.T) {
	s := parseSheet(t, "FILE \"a.flac\" WAVE\nTITLE Jazz Album\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n")
	if s == nil {
		t.Fatal("ParseCueSheet returned nil")
	}
	if s.Title != "Jazz Album" {
		t.Errorf("title = %q, want Jazz Album", s.Title)
	}
}

// TestParseCueSheetImpliedFILE: a sidecar beside one file rarely writes a FILE line
// and a hand-written chapter sheet never does, so a TRACK before any FILE is indexed
// against the implied file. A warning names the line as the sheet numbers it.
func TestParseCueSheetImpliedFILE(t *testing.T) {
	s := parseSheet(t, "TITLE \"No File Line\"\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n")
	if s == nil || len(s.Tracks) != 1 || !s.Tracks[0].StartValid {
		t.Fatalf("ParseCueSheet = %+v, want one usable track in the implied file", s)
	}
	if s.Title != "No File Line" {
		t.Errorf("title = %q, want No File Line", s.Title)
	}
	s = parseSheet(t, "TITLE \"x\"\n  TRACK 01 AUDIO\n    INDEX 01 00:99:00\n")
	if s == nil || len(s.Warnings) != 1 {
		t.Fatalf("ParseCueSheet = %+v, want one warning", s)
	}
	if w := s.Warnings[0].String(); !strings.HasPrefix(w, `line 3: track 1: time "00:99:00"`) {
		t.Errorf("warning %q does not name the sheet's own line 3, the track and the time", w)
	}
}

// TestParseCueSheetReadsCROnlyLines: a sheet saved on classic Mac OS ends each line
// with a bare CR. The album header ahead of FILE is the usual layout, and it must
// read as separate lines rather than one run-on TITLE.
func TestParseCueSheetReadsCROnlyLines(t *testing.T) {
	s := parseSheet(t, "PERFORMER \"Band\"\rTITLE \"Album\"\rFILE \"a.wav\" WAVE\r"+
		"  TRACK 01 AUDIO\r    TITLE \"One\"\r    INDEX 01 00:00:00\r"+
		"  TRACK 02 AUDIO\r    TITLE \"Two\"\r    INDEX 01 00:05:00\r")
	if s == nil {
		t.Fatal("ParseCueSheet returned nil for a CR-only sheet")
	}
	if s.Performer != "Band" || s.Title != "Album" {
		t.Errorf("album = %q by %q, want Album by Band", s.Title, s.Performer)
	}
	if len(s.Tracks) != 2 || s.Tracks[1].Title != "Two" || s.Tracks[1].StartFrames != 375 {
		t.Errorf("tracks = %+v, want two, the second Two at 375 frames", s.Tracks)
	}
}

// TestParseCueSheetKeepsInnerQuotes: the format has no escape, so only the pair of
// quotes around an operand is stripped. The padding inside them is still trimmed.
func TestParseCueSheetKeepsInnerQuotes(t *testing.T) {
	s := parseSheet(t, "TITLE \" The \"Best\" Of \"\nFILE \"a.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"12\" Remix\"\n    INDEX 01 00:00:00\n")
	if s == nil || len(s.Tracks) != 1 {
		t.Fatalf("ParseCueSheet = %+v, want one track", s)
	}
	if s.Title != `The "Best" Of` {
		t.Errorf("album title = %q, want %q", s.Title, `The "Best" Of`)
	}
	if s.Tracks[0].Title != `12" Remix` {
		t.Errorf("track title = %q, want %q", s.Tracks[0].Title, `12" Remix`)
	}
}

// TestParseCueSheetStripsABOM: a Windows editor saves UTF-8 with a byte-order mark,
// which must not glue itself to the first command: a sheet opening with TRACK or
// TITLE behind one still reads.
func TestParseCueSheetStripsABOM(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	s := parseSheet(t, bom+"TRACK 01 AUDIO\n  INDEX 01 00:00:00\nTRACK 02 AUDIO\n  INDEX 01 00:05:00\n")
	if s == nil || len(s.Tracks) != 2 {
		t.Errorf("a sheet opening with a BOM and TRACK = %+v, want two tracks", s)
	}
	s = parseSheet(t, bom+"TITLE \"Album\"\nTRACK 01 AUDIO\n  INDEX 01 00:00:00\n")
	if s == nil || s.Title != "Album" {
		t.Errorf("a sheet opening with a BOM and TITLE = %+v, want the title Album", s)
	}
}

// TestParseCueSheetFILETokenMatchesUpstream: space and tab are the only separators, so
// a FILE line with a no-break space is an unknown command, skipped, and the track
// opens the implied file instead of being lost.
func TestParseCueSheetFILETokenMatchesUpstream(t *testing.T) {
	s := parseSheet(t, "FILE\u00a0\"a.wav\" WAVE\nTRACK 01 AUDIO\n  INDEX 01 00:00:00\n")
	if s == nil || len(s.Tracks) != 1 || s.File != "" {
		t.Errorf("ParseCueSheet = %+v, want the track read against the implied file", s)
	}
}

// TestParseCueSheetEnhancedCDDataFILE: an Enhanced CD's sheet can index its data track
// against a FILE of its own. That is still one audio file, so it is carved rather
// than refused as a rip already split per track; a sheet whose FILEs each hold audio
// still is refused.
func TestParseCueSheetEnhancedCDDataFILE(t *testing.T) {
	s := parseSheet(t, "FILE \"album.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n  TRACK 02 AUDIO\n    INDEX 01 00:05:00\n"+
		"FILE \"data.bin\" BINARY\n  TRACK 03 MODE2/2352\n    INDEX 01 00:00:00\n")
	if s == nil || len(s.Tracks) != 2 || s.Tracks[0].Number != 1 || s.Tracks[1].Number != 2 {
		t.Fatalf("ParseCueSheet = %+v, want the two audio tracks of the audio FILE", s)
	}
	if _, err := ParseCueSheet("FILE \"a.wav\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n" +
		"FILE \"b.wav\" WAVE\n  TRACK 02 AUDIO\n    INDEX 01 00:00:00\n"); err == nil {
		t.Error("a sheet whose two FILEs both hold audio parsed; want it refused")
	}
}

// TestParseCueSheetTabSeparatedFILECountsAsOne: upstream tokenizes on tabs as well as
// spaces, so a tab-separated sheet reads like a space-separated one.
func TestParseCueSheetTabSeparatedFILECountsAsOne(t *testing.T) {
	s := parseSheet(t, "FILE\t\"album.flac\"\tWAVE\n\tTRACK 01 AUDIO\n\t\tINDEX 01 00:00:00\n")
	if s == nil || len(s.Tracks) != 1 || !s.Tracks[0].StartValid || s.File != "album.flac" {
		t.Fatalf("ParseCueSheet = %+v, want one usable track in album.flac", s)
	}
}

// rip joins sheet lines under one FILE line, for sheets written a line per entry.
func rip(lines ...string) string {
	return "FILE \"album.wav\" WAVE\n" + strings.Join(lines, "\n") + "\n"
}

// TestCarve pins the windows a rip is carved into, in CD frames, for the shapes real
// sheets have: mixed-mode images with a data track first, EAC images whose data track
// occupies nothing, Enhanced CDs with the data track last, PC Engine discs with it
// between audio tracks, and discs with audio ahead of track 1. An end of 0 is open.
func TestCarve(t *testing.T) {
	type win struct {
		num        int
		title      string // compared when set
		start, end int64
	}
	for _, tc := range []struct {
		name   string
		sheet  string
		fileMS int64 // the file's probed length, 0 when unknown
		want   []win
		drops  int
		err    string // a substring of the refusal
	}{
		{
			name: "a data track first starts the audio at the first audio INDEX 01",
			sheet: rip(`TRACK 01 MODE1/2352`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 00 00:10:00`, `INDEX 01 00:12:00`,
				`TRACK 03 AUDIO`, `INDEX 01 00:20:00`),
			want:  []win{{num: 2, start: 900, end: 1500}, {num: 3, start: 1500}},
			drops: 1,
		},
		{
			name: "a data track beside audio at frame 0 occupies nothing",
			sheet: rip(`TRACK 01 MODE1/2352`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 03 AUDIO`, `INDEX 01 00:05:00`),
			want:  []win{{num: 2, start: 0, end: 375}, {num: 3, start: 375}},
			drops: 1,
		},
		{
			name: "a data track between audio tracks ends the track before it",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 MODE1/2352`, `INDEX 01 00:05:00`,
				`TRACK 03 AUDIO`, `INDEX 00 00:09:00`, `INDEX 01 00:11:00`),
			want:  []win{{num: 1, start: 0, end: 375}, {num: 3, start: 825}},
			drops: 1,
		},
		{
			name: "the audio before a data track ends at its INDEX 00",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 MODE1/2352`, `INDEX 00 00:04:00`, `INDEX 01 00:05:00`,
				`TRACK 03 AUDIO`, `INDEX 01 00:11:00`),
			want:  []win{{num: 1, start: 0, end: 300}, {num: 3, start: 825}},
			drops: 1,
		},
		{
			name: "a trailing data track inside the file ends the last track at its INDEX 00",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:05:00`,
				`TRACK 03 MODE1/2352`, `INDEX 00 00:09:00`, `INDEX 01 00:11:00`),
			fileMS: 20000,
			want:   []win{{num: 1, start: 0, end: 375}, {num: 2, start: 375, end: 675}},
			drops:  1,
		},
		{
			name: "a trailing data track with only an INDEX 01 ends the last track there",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:05:00`,
				`TRACK 03 MODE1/2352`, `INDEX 01 00:09:00`),
			fileMS: 20000,
			want:   []win{{num: 1, start: 0, end: 375}, {num: 2, start: 375, end: 675}},
			drops:  1,
		},
		{
			name: "a trailing data track at the file's end leaves the last track open",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:05:00`,
				`TRACK 03 MODE1/2352`, `INDEX 00 00:19:25`, `INDEX 01 00:21:25`),
			fileMS: 20000,
			want:   []win{{num: 1, start: 0, end: 375}, {num: 2, start: 375}},
			drops:  1,
		},
		{
			name: "a trailing data track past the file leaves the last track open",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:05:00`,
				`TRACK 03 MODE1/2352`, `INDEX 01 01:00:00`),
			fileMS: 20000,
			want:   []win{{num: 1, start: 0, end: 375}, {num: 2, start: 375}},
			drops:  1,
		},
		{
			name: "a trailing data track in a file of unknown length leaves the last track open",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:05:00`,
				`TRACK 03 MODE1/2352`, `INDEX 00 00:09:00`, `INDEX 01 00:11:00`),
			want:  []win{{num: 1, start: 0, end: 375}, {num: 2, start: 375}},
			drops: 1,
		},
		{
			name: "a trailing data track placed before the last audio track leaves it open",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:05:00`,
				`TRACK 03 MODE1/2352`, `INDEX 01 00:01:00`),
			fileMS: 20000,
			want:   []win{{num: 1, start: 0, end: 375}, {num: 2, start: 375}},
			drops:  1,
		},
		{
			name:  "one audio track is not a rip",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`),
		},
		{
			name: "one audio track behind a data track is not a rip",
			sheet: rip(`TRACK 01 MODE1/2352`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:04:00`),
			drops: 1,
		},
		{
			name: "tracks out of order are refused",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:10`,
				`TRACK 02 AUDIO`, `INDEX 01 00:00:05`,
				`TRACK 03 AUDIO`, `INDEX 01 00:00:20`),
			err: `file "album.wav" track 2 starts at frame 5`,
		},
		{
			name: "a cooked data track ahead of audio is refused",
			sheet: rip(`TRACK 01 MODE1/2048`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:04:00`,
				`TRACK 03 AUDIO`, `INDEX 01 00:08:00`),
			drops: 1,
			err:   "a data track occupying the file ahead of audio",
		},
		{
			name: "a data track after the last audio track needs no INDEX",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:05:00`,
				`TRACK 03 MODE1/2352`),
			want:  []win{{num: 1, start: 0, end: 375}, {num: 2, start: 375}},
			drops: 1,
		},
		{
			name: "an Enhanced CD data track given only its session gap is dropped",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:05:00`,
				`TRACK 03 MODE1/2352`, `PREGAP 02:32:00`),
			fileMS: 20000,
			want:   []win{{num: 1, start: 0, end: 375}, {num: 2, start: 375}},
			drops:  1,
		},
		{
			name: "a data track between audio tracks with no INDEX is refused",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 MODE1/2352`,
				`TRACK 03 AUDIO`, `INDEX 01 00:10:00`),
			drops: 1,
			err:   "nothing says where the audio before it ends",
		},
		{
			name: "the earlier of two audio tracks on one frame is dropped",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 03 AUDIO`, `INDEX 01 00:00:10`),
			want:  []win{{num: 2, start: 0, end: 10}, {num: 3, start: 10}},
			drops: 1,
		},
		{
			name: "a lead-in of ten seconds is a gap",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 00 00:00:00`, `INDEX 01 00:10:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:20:00`),
			want: []win{{num: 1, start: 750, end: 1500}, {num: 2, start: 1500}},
		},
		{
			name: "a longer lead-in is hidden track one audio",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 00 00:00:00`, `INDEX 01 00:10:01`,
				`TRACK 02 AUDIO`, `INDEX 01 00:20:00`),
			want: []win{{num: 0, title: "Hidden Track", start: 0, end: 751}, {num: 1, start: 751, end: 1500}, {num: 2, start: 1500}},
		},
		{
			name: "a sheet's own TRACK 00 is kept as written",
			sheet: rip(`TRACK 00 AUDIO`, `TITLE "Secret"`, `INDEX 01 00:00:00`,
				`TRACK 01 AUDIO`, `INDEX 01 00:20:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:40:00`),
			want: []win{{num: 0, title: "Secret", start: 0, end: 1500}, {num: 1, start: 1500, end: 3000}, {num: 2, start: 3000}},
		},
		{
			name: "a TRACK 00 with a long pregap of its own adds nothing",
			sheet: rip(`TRACK 00 AUDIO`, `INDEX 01 00:20:00`,
				`TRACK 01 AUDIO`, `INDEX 01 00:40:00`),
			want: []win{{num: 0, start: 1500, end: 3000}, {num: 1, start: 3000}},
		},
		{
			name: "a partial rip's lead-in is not hidden track one audio",
			sheet: rip(`TRACK 05 AUDIO`, `INDEX 01 00:30:00`,
				`TRACK 06 AUDIO`, `INDEX 01 00:50:00`),
			want: []win{{num: 5, start: 2250, end: 3750}, {num: 6, start: 3750}},
		},
		{
			name: "a track dropped ahead of track 1 leaves its lead-in unnamed",
			sheet: rip(`TRACK 00 AUDIO`, `INDEX 00 00:00:00`,
				`TRACK 01 AUDIO`, `INDEX 01 00:20:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:40:00`),
			want:  []win{{num: 1, start: 1500, end: 3000}, {num: 2, start: 3000}},
			drops: 1,
		},
		{
			name: "a dropped first track leaves the audio ahead of the next one unnamed",
			sheet: rip(`TRACK 01 AUDIO`, `TITLE "Broken"`, `INDEX 00 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:20:00`,
				`TRACK 03 AUDIO`, `INDEX 01 00:40:00`),
			want:  []win{{num: 2, start: 1500, end: 3000}, {num: 3, start: 3000}},
			drops: 1,
		},
		{
			name:  "one audio track with a long lead-in is still not a rip",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:20:00`),
		},
		{
			name: "the audio ahead of track 1 after a data FILE is the data track's pregap",
			sheet: "FILE \"data.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
				rip(`TRACK 02 AUDIO`, `INDEX 01 00:20:00`, `TRACK 03 AUDIO`, `INDEX 01 00:40:00`),
			want: []win{{num: 2, start: 1500, end: 3000}, {num: 3, start: 3000}},
		},
		{
			name: "a data FILE ahead of track 1 makes its lead-in the data track's pregap",
			sheet: "FILE \"data.bin\" BINARY\n  TRACK 00 MODE1/2352\n    INDEX 01 00:00:00\n" +
				rip(`TRACK 01 AUDIO`, `INDEX 01 00:20:00`, `TRACK 02 AUDIO`, `INDEX 01 00:40:00`),
			want: []win{{num: 1, start: 1500, end: 3000}, {num: 2, start: 3000}},
		},
		{
			name: "a sheet with an unread line is not divided",
			sheet: rip(`TRACK 01 AUDIO`, `INDEX 01 00:00:00`,
				`TRACK 02 AUDIO`, `INDEX 01 00:0x:00`,
				`TRACK 03 AUDIO`, `INDEX 01 00:10:00`),
			drops: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped, err := parseSheet(t, tc.sheet).Carve(tc.fileMS)
			if len(dropped) != tc.drops {
				t.Errorf("dropped = %q, want %d", dropped, tc.drops)
			}
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || strings.Contains(err.Error(), "cue:") {
					t.Fatalf("err = %v, want %q without upstream's prefix", err, tc.err)
				}
				if got != nil {
					t.Errorf("windows = %+v beside a refusal, want none", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Carve: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("windows = %+v, want %+v", got, tc.want)
			}
			for i, w := range tc.want {
				g := got[i]
				if g.Track.Number != w.num || g.StartFrames != w.start || g.EndFrames != w.end ||
					(w.title != "" && g.Track.Title != w.title) {
					t.Errorf("window %d = track %d %q [%d,%d), want track %d %q [%d,%d)",
						i, g.Track.Number, g.Track.Title, g.StartFrames, g.EndFrames, w.num, w.title, w.start, w.end)
				}
			}
		})
	}
}
