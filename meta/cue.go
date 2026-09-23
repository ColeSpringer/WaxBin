package meta

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxflow/cue"
	flowerr "github.com/colespringer/waxflow/waxerr"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// CueSheet is a parsed .cue sheet describing a single backing audio file: the
// album-level fields declared before the first TRACK, plus one entry per TRACK.
// Two readers consume it: ParseCue derives book-navigation chapters from the
// tracks, and the scanner carves a single-file album rip into virtual tracks (each
// TRACK its own item and offset window). WaxBin reads .cue sidecars directly
// (WaxLabel has no cue parser).
type CueSheet struct {
	Title     string // album title (a TITLE line before the first TRACK)
	Performer string // album artist (a PERFORMER line before the first TRACK)
	Genre     string // REM GENRE
	Year      int    // REM DATE (leading four-digit year)
	Tracks    []CueTrack
}

// CueTrack is one TRACK of a cue sheet: its declared number, datatype, title, and
// performer (the track's own PERFORMER, empty when it inherits the album performer),
// plus its INDEX 01 start offset in CD frames.
//
// StartValid reports that the track declared an INDEX 01 and that it parsed. A track
// without a usable one has no start at all, yet StartFrames still reads 0, and 0 is a
// real offset naming the file's first sample rather than an absent one.
//
// Consumers should read UsableTracks, which drops those tracks already, or check
// StartValid themselves. Letting one default to 0 does damage in two directions,
// because no consumer here reads a start in isolation: the scanner carves a content
// window from it, and a book chapter's end is read off the next chapter's start, so a
// fabricated 0 misplaces its own track and truncates the one before it.
type CueTrack struct {
	Number int
	// Type is the TRACK datatype token: AUDIO, or one of the data modes a
	// mixed-mode disc gives its first track. Empty when the sheet omits it.
	Type        string
	Title       string
	Performer   string
	StartFrames int64
	StartValid  bool
}

// UsableTracks returns the audio tracks that declared a parseable INDEX 01, in sheet
// order. A track without one has no start offset, and the start offset is what every
// consumer here anchors on, so admitting it at a fabricated 0 does damage in both
// directions: it claims the head of the file for itself, and it truncates the track
// or chapter before it, whose end is read off the next one's start.
//
// A data track (a mixed-mode disc's first) is dropped too, since carving it as audio
// would name a piece of filesystem after a song.
//
// Callers that must report the drop read Tracks and filter themselves; the scanner
// does, so the sheet's own diagnostics name what was skipped.
func (s *CueSheet) UsableTracks() []CueTrack {
	out := make([]CueTrack, 0, len(s.Tracks))
	for _, t := range s.Tracks {
		if t.StartValid && t.IsAudio() {
			out = append(out, t)
		}
	}
	return out
}

// IsAudio reports whether the track is an audio track.
func (t CueTrack) IsAudio() bool { return isAudioType(t.Type) }

// isAudioType reports whether a TRACK datatype is audio. Only the MODE and CDI data
// modes are not: AUDIO is, CDG karaoke carries its audio like any other track, and
// an absent or unfamiliar token is taken as audio rather than dropping a song.
func isAudioType(typ string) bool {
	return !strings.HasPrefix(typ, "MODE") && !strings.HasPrefix(typ, "CDI")
}

// Chapters projects a cue sheet's tracks into file-relative navigation chapters,
// one per usable TRACK, using each track's INDEX 01 as the start and its TITLE as
// the label. End offsets are left open (0) for the book read path to fill from the
// next chapter's start.
//
// A track with no usable INDEX 01 is dropped rather than anchored at 0. A chapter's
// start is not only its own coordinate: the read path fills each open end from the
// next chapter's start, so one fabricated 0 would report the chapter before it as
// ending at the start of the book.
func (s *CueSheet) Chapters() []model.Chapter {
	usable := s.UsableTracks()
	chapters := make([]model.Chapter, len(usable))
	for i, t := range usable {
		// The one place the lossy frames->ms direction is correct: a chapter is a seek
		// coordinate, not content identity, so the third of a millisecond a frame can
		// round away is beneath what a listener resuming a book can perceive.
		chapters[i] = model.Chapter{Position: i, Title: t.Title, FileStartMS: model.FramesToMS(t.StartFrames)}
	}
	return chapters
}

// ParseCue parses a .cue sheet into file-relative navigation chapters, one per
// TRACK. It returns nil chapters when the sheet has no usable tracks, and an error
// when the sheet itself could not be read. A book with no embedded chapters uses
// these, marked source='cue' so embedded chapters stay authoritative.
func ParseCue(text string) ([]model.Chapter, error) {
	sheet, err := ParseCueSheet(text)
	if err != nil || sheet == nil {
		return nil, err
	}
	return sheet.Chapters(), nil
}

// ParseCueSheet parses a .cue sheet into its album-level fields and per-track
// entries. It returns a nil sheet and no error when the sheet declares no TRACK, so
// a caller can treat an empty sheet the same as an absent one, and an error when the
// sheet is malformed.
//
// The parse itself is waxflow/cue's, which is syntactic: one unreadable line refuses
// the whole sheet rather than dropping that line. This adapter trims each value,
// since a quoted operand arrives with its padding intact.
func ParseCueSheet(text string) (*CueSheet, error) {
	const op = "meta.ParseCueSheet"
	// Upstream strips a BOM only at the very start, so it has to go before anything
	// is prepended.
	text = strings.TrimPrefix(text, "\ufeff")
	// A sidecar beside one file rarely bothers with FILE and a hand-written chapter
	// sheet never does, while upstream refuses a TRACK before any FILE. The file is
	// implied, so supply it.
	supplied := !hasCueFileLine(text)
	if supplied {
		text = "FILE \"\" WAVE\n" + text
	}
	sheet, err := cue.Parse([]byte(text))
	if err != nil {
		return nil, waxerr.New(waxerr.CodeInvalid, op, cueRefusal(err, supplied))
	}
	if len(sheet.Files) == 0 {
		return nil, nil
	}
	file, err := cueAudioFile(sheet)
	if err != nil {
		return nil, waxerr.New(waxerr.CodeInvalid, op, cueRefusal(err, supplied))
	}
	if len(file.Tracks) == 0 {
		return nil, nil
	}
	out := &CueSheet{Title: strings.TrimSpace(sheet.Title), Performer: strings.TrimSpace(sheet.Performer)}
	if g, ok := sheet.Rem("GENRE"); ok {
		out.Genre = strings.TrimSpace(g)
	}
	if d, ok := sheet.Rem("DATE"); ok {
		out.Year = cueYear(d)
	}
	for _, t := range file.Tracks {
		start, ok := t.Start()
		out.Tracks = append(out.Tracks, CueTrack{
			Number:      t.Number,
			Type:        t.Type,
			Title:       strings.TrimSpace(t.Title),
			Performer:   strings.TrimSpace(t.Performer),
			StartFrames: int64(start),
			StartValid:  ok,
		})
	}
	return out, nil
}

// cueAudioFile is the one FILE the sheet's audio is indexed against. A sheet with
// several is a rip already split per track, unless only one of them holds audio: an
// Enhanced CD's sheet can give its data track a FILE of its own.
func cueAudioFile(sheet *cue.Sheet) (*cue.File, error) {
	file, err := sheet.SingleFile()
	if err == nil || len(sheet.Files) < 2 {
		return file, err
	}
	var audio *cue.File
	for i := range sheet.Files {
		if !slices.ContainsFunc(sheet.Files[i].Tracks, func(t cue.Track) bool { return isAudioType(t.Type) }) {
			continue
		}
		if audio != nil {
			return nil, err
		}
		audio = &sheet.Files[i]
	}
	if audio == nil {
		return nil, err
	}
	return audio, nil
}

// cueRefusal renders upstream's refusal as a person reads it: the line (counted in
// the sheet as written, so one less when this adapter supplied the FILE) and why.
// Upstream stamps "cue: " on every layer, which says nothing here.
func cueRefusal(err error, supplied bool) string {
	var parts []string
	for e := error(err); e != nil; e = errors.Unwrap(e) {
		fe, ok := e.(*flowerr.Error)
		if !ok || fe.Msg == "" {
			continue
		}
		msg := strings.TrimPrefix(fe.Msg, "cue: ")
		var n int
		if _, serr := fmt.Sscanf(msg, "line %d", &n); serr == nil && msg == fmt.Sprintf("line %d", n) && supplied {
			msg = fmt.Sprintf("line %d", n-1)
		}
		parts = append(parts, msg)
	}
	if len(parts) == 0 {
		return err.Error()
	}
	return strings.Join(parts, ": ")
}

// hasCueFileLine reports whether the sheet declares a FILE, reading each line's first
// token exactly as upstream does: a trailing CR dropped, then space and tab as the
// only separators.
func hasCueFileLine(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		tok := strings.TrimLeft(strings.TrimRight(line, "\r"), " \t")
		if i := strings.IndexAny(tok, " \t"); i >= 0 {
			tok = tok[:i]
		}
		if strings.EqualFold(tok, "FILE") {
			return true
		}
	}
	return false
}

// cueYear extracts a leading four-digit year from a REM DATE value ("1998" or
// "1998-05-01"), returning 0 when there is none. It trims its own input: a quoted
// REM DATE keeps whatever padding sat inside the quotes.
func cueYear(s string) int {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return 0
	}
	n, err := strconv.Atoi(s[:4])
	if err != nil {
		return 0
	}
	return n
}
