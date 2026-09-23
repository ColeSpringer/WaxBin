package meta

import (
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

// TestParseCueSheetRefusesMalformedTime covers the three bounds and the shapes that
// are not a timestamp at all. Upstream's parse is syntactic, so each costs the whole
// sheet rather than the one track, and the refusal names the line to go look at.
func TestParseCueSheetRefusesMalformedTime(t *testing.T) {
	for _, ts := range []string{
		"00:60:00",   // SS past 59
		"00:00:75",   // FF past 74 (a second holds 75 frames, 0-74)
		"6001:00:00", // MM past the 100-hour cap, which is what bounds the arithmetic
		"1:2",        // not MM:SS:FF at all
		"-1:00:00",   // signed: Atoi takes it, a position cannot
	} {
		// The INDEX sits on line 3, so the refusal names it.
		sheet, err := ParseCueSheet("FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 " + ts + "\n")
		if err == nil {
			t.Errorf("INDEX %q parsed to %+v; want the sheet refused", ts, sheet)
			continue
		}
		if !strings.Contains(err.Error(), "line 3") {
			t.Errorf("INDEX %q: error %q does not name line 3", ts, err)
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
	// A malformed INDEX 01 is a different condition: the sheet is refused outright.
	if sheet, err := ParseCueSheet("FILE \"a.flac\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:99:00\n"); err == nil {
		t.Errorf("ParseCueSheet = %+v, want a malformed INDEX 01 to refuse the sheet", sheet)
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
	chs, err := ParseCue("FILE \"book.m4b\" WAVE\n" +
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n" +
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:05:00\n" +
		"  TRACK 03 AUDIO\n    TITLE \"Broken\"\n    INDEX 00 00:07:00\n" +
		"  TRACK 04 AUDIO\n    TITLE \"Four\"\n    INDEX 01 00:10:00\n")
	if err != nil {
		t.Fatalf("ParseCue: %v", err)
	}
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

// TestParseCueSheetUnquotedTitleTakesOneToken pins a limit of the upstream parser,
// which splits an unquoted operand on whitespace and keeps the first token. The
// retired parser took the rest of the line. It is filed in docs/upstream-requests.md;
// the day upstream takes the whole operand this test fails and the entry retires.
func TestParseCueSheetUnquotedTitleTakesOneToken(t *testing.T) {
	s := parseSheet(t, "FILE \"a.flac\" WAVE\nTITLE Jazz Album\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n")
	if s == nil {
		t.Fatal("ParseCueSheet returned nil")
	}
	if s.Title != "Jazz" {
		t.Errorf("title = %q, want Jazz (upstream keeps the first token of an unquoted operand)", s.Title)
	}
}

// TestParseCueSheetToleratesAMissingFILE: upstream refuses a TRACK before any FILE,
// but a sidecar beside one file rarely writes one and a hand-written chapter sheet
// never does. The adapter supplies the implied FILE and takes it back out of a
// refusal's line number, so the line named is the sheet's own. Also filed upstream.
func TestParseCueSheetToleratesAMissingFILE(t *testing.T) {
	s := parseSheet(t, "TITLE \"No File Line\"\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n")
	if s == nil || len(s.Tracks) != 1 || !s.Tracks[0].StartValid {
		t.Fatalf("ParseCueSheet = %+v, want one usable track through the synthetic FILE", s)
	}
	if s.Title != "No File Line" {
		t.Errorf("title = %q, want No File Line", s.Title)
	}
	_, err := ParseCueSheet("TITLE \"x\"\n  TRACK 01 AUDIO\n    INDEX 01 00:99:00\n")
	if err == nil {
		t.Fatal("a malformed INDEX parsed; want the sheet refused")
	}
	if !strings.Contains(err.Error(), `line 3: time "00:99:00"`) {
		t.Errorf("error %q does not name the sheet's own line 3 and the time", err)
	}
	if strings.Contains(err.Error(), "cue:") {
		t.Errorf("error %q still carries upstream's prefix", err)
	}
}

// TestParseCueSheetStripsABOM: a Windows editor saves UTF-8 with a byte-order mark,
// and upstream strips one only at the very start of the text. Supplying the FILE
// line ahead of it would leave the mark glued to the first command, which then reads
// as an unknown word: a sheet opening with TRACK was refused, and one opening with
// TITLE lost the album title.
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

// TestParseCueSheetFILETokenMatchesUpstream: the FILE check reads a line the way
// upstream does, with space and tab as the only separators. A no-break space is part
// of the word there, so such a line is not a FILE and the adapter must supply one.
func TestParseCueSheetFILETokenMatchesUpstream(t *testing.T) {
	s := parseSheet(t, "FILE\u00a0\"a.wav\" WAVE\nTRACK 01 AUDIO\n  INDEX 01 00:00:00\n")
	if s == nil || len(s.Tracks) != 1 {
		t.Errorf("ParseCueSheet = %+v, want the track read against a supplied FILE", s)
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
// spaces, so a tab-separated FILE line is a FILE line. Missing it would prepend a
// second one and refuse the sheet as multi-file.
func TestParseCueSheetTabSeparatedFILECountsAsOne(t *testing.T) {
	s := parseSheet(t, "FILE\t\"album.flac\"\tWAVE\n\tTRACK 01 AUDIO\n\t\tINDEX 01 00:00:00\n")
	if s == nil || len(s.Tracks) != 1 || !s.Tracks[0].StartValid {
		t.Fatalf("ParseCueSheet = %+v, want one usable track", s)
	}
}
