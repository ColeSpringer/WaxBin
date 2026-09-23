package meta

import (
	"cmp"
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
	// File is the FILE name as the sheet spells it, empty for the implied file.
	File   string
	Tracks []CueTrack
	// Warnings are the lines the parse could not read and skipped, in line order.
	Warnings []CueWarning
	// WarningsTruncated reports that Warnings stops at upstream's cap, so more lines
	// may have gone unread.
	WarningsTruncated bool

	precededByData bool
}

// CueWarning is one line of a sheet the parse could not read.
type CueWarning struct {
	Line int
	Msg  string
}

func (w CueWarning) String() string { return fmt.Sprintf("line %d: %s", w.Line, w.Msg) }

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
	// Type is the TRACK datatype token: AUDIO, or a data mode such as MODE1/2352.
	// Empty when the sheet omits it.
	Type        string
	Title       string
	Performer   string
	StartFrames int64
	StartValid  bool

	indexes []cue.Index
}

// UsableTracks returns the audio tracks that declared a parseable INDEX 01, in sheet
// order. A track without one has no start offset, and the start offset is what every
// consumer here anchors on, so admitting it at a fabricated 0 does damage in both
// directions: it claims the head of the file for itself, and it truncates the track
// or chapter before it, whose end is read off the next one's start.
//
// A data track is dropped too, since carving it as audio would name a piece of
// filesystem after a song.
//
// It reports nothing; Carve names what it drops, for the scanner's diagnostic.
func (s *CueSheet) UsableTracks() []CueTrack {
	out := make([]CueTrack, 0, len(s.Tracks))
	for _, t := range s.Tracks {
		if t.StartValid && t.IsAudio() {
			out = append(out, t)
		}
	}
	return out
}

// IsAudio reports whether the track is an audio track, by upstream's rule: only the
// MODE and CDI data modes are not.
func (t CueTrack) IsAudio() bool { return cue.Track{Type: t.Type}.IsAudio() }

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
// TRACK, for chapters set --file. It returns nil chapters when the sheet has no usable
// tracks, and an error naming the first line it could not read or the first audio
// track with no INDEX 01 (a misspelled INDEX line is skipped without a warning): an
// explicit command refuses rather than setting a partial list the user would redo.
func ParseCue(text string) ([]model.Chapter, error) {
	sheet, err := ParseCueSheet(text)
	if err != nil || sheet == nil {
		return nil, err
	}
	if len(sheet.Warnings) > 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, "meta.ParseCue", sheet.Warnings[0].String())
	}
	for _, t := range sheet.Tracks {
		if t.IsAudio() && !t.StartValid {
			return nil, waxerr.New(waxerr.CodeInvalid, "meta.ParseCue", cueTrackDesc(t, "has no usable INDEX 01"))
		}
	}
	return sheet.Chapters(), nil
}

// ParseCueSheet parses a .cue sheet into its album-level fields and per-track
// entries. A line it cannot read is skipped and reported in Warnings. It returns a nil
// sheet and no error when the sheet declares no TRACK and has nothing to warn about,
// so a caller can treat an empty sheet the same as an absent one, and an error when
// the sheet indexes several audio files or none.
//
// The parse itself is waxflow/cue's tolerant one. This adapter trims each value,
// since a quoted operand arrives with its padding intact.
func ParseCueSheet(text string) (*CueSheet, error) {
	sheet := cue.ParseTolerant([]byte(text))
	out := &CueSheet{Title: strings.TrimSpace(sheet.Title), Performer: strings.TrimSpace(sheet.Performer)}
	for _, w := range sheet.Warnings {
		out.Warnings = append(out.Warnings, CueWarning{Line: w.Line, Msg: w.Msg})
	}
	out.WarningsTruncated = len(out.Warnings) >= maxCueWarnings
	if g, ok := sheet.Rem("GENRE"); ok {
		out.Genre = strings.TrimSpace(g)
	}
	if d, ok := sheet.Rem("DATE"); ok {
		out.Year = cueYear(d)
	}
	var tracks []cue.Track
	if len(sheet.Files) > 0 {
		file, err := sheet.SingleFile()
		if err != nil {
			return nil, waxerr.New(waxerr.CodeInvalid, "meta.ParseCueSheet", cueRefusal(err))
		}
		out.File, out.precededByData, tracks = file.Name, file.PrecededByData, file.Tracks
	}
	if len(tracks) == 0 && len(out.Warnings) == 0 {
		return nil, nil
	}
	for _, t := range tracks {
		start, ok := t.Start()
		out.Tracks = append(out.Tracks, CueTrack{
			Number:      t.Number,
			Type:        t.Type,
			Title:       strings.TrimSpace(t.Title),
			Performer:   strings.TrimSpace(t.Performer),
			StartFrames: int64(start),
			StartValid:  ok,
			indexes:     t.Indexes,
		})
	}
	return out, nil
}

// maxCueWarnings is where waxflow/cue stops recording warnings.
const maxCueWarnings = 64

// hiddenTrackMinFrames is how long the audio ahead of track 1 has to run (ten seconds)
// to be carved as a track of its own. A rip read from LBA 0 holds whatever of track 1's
// pregap runs past the two seconds every disc keeps in its lead-in: a few frames to a
// few seconds of silence on most discs, and a song on a disc with hidden track one audio.
const hiddenTrackMinFrames = 10 * cue.FramesPerSecond

// CueWindow is one track of a rip and the frames it spans. An EndFrames of 0 runs to
// the end of the file.
type CueWindow struct {
	Track       CueTrack
	StartFrames int64
	EndFrames   int64
}

// Carve divides a single-file rip into one window per audio track and reports the
// tracks it dropped: an audio track with no usable INDEX 01, the earlier of two audio
// tracks on one frame, and every data track, which is never a window but still bounds
// the audio around it. A sheet whose tracks cannot divide the file is refused.
//
// The division is waxflow/cue's File.Pieces at rate 75, so it answers in the sheet's
// own frames, and with no length, since a header duration can run short or long. A
// data track after the last audio track is not handed to Pieces and needs no INDEX.
// It ends the last window only where the file holds it: at its INDEX 00, else INDEX
// 01, when that lies at least dataEndMargin inside fileMS, the file's exact length (0
// when unknown or only estimated). A file made from a whole disc image carries such a
// track's sectors as noise; an Enhanced CD's data sits in a second session an audio
// rip does not hold, and there the last window stays open. The lead-in ahead of track
// 1 becomes track 0, "Hidden Track", when it runs past hiddenTrackMinFrames and no
// track was dropped ahead of track 1; any other lead-in, the pregap after a data FILE
// among them, is in no window. Fewer than two audio tracks, or a sheet with warnings
// (a skipped line can merge two tracks), yields no windows and no error, whatever
// Pieces would say: the file stays a whole-file track either way.
//
// It reads the indexes ParseCueSheet stored, so it works on a parsed sheet only.
func (s *CueSheet) Carve(fileMS int64) (windows []CueWindow, dropped []string, err error) {
	var cands []CueTrack
	firstKept := false
	for i, ct := range s.Tracks {
		if why := unplaced(ct); why != "" {
			dropped = append(dropped, cueTrackDesc(ct, why))
			if ct.IsAudio() {
				continue
			}
		}
		firstKept = firstKept || i == 0
		cands = append(cands, ct)
	}
	var kept []CueTrack
	for i, ct := range cands {
		if ct.IsAudio() && i+1 < len(cands) && cands[i+1].IsAudio() && cands[i+1].StartFrames == ct.StartFrames {
			dropped = append(dropped, cueTrackDesc(ct, sameFrame))
			firstKept = firstKept && i != 0
			continue
		}
		kept = append(kept, ct)
	}
	if len(s.Warnings) > 0 {
		return nil, dropped, nil
	}
	last := len(kept) - 1
	for last >= 0 && !kept[last].IsAudio() {
		last--
	}
	kept, trail := kept[:last+1], kept[last+1:]
	audio := 0
	for _, ct := range kept {
		if ct.IsAudio() {
			audio++
		}
	}
	if audio < 2 {
		return nil, dropped, nil
	}
	file := cue.File{Name: s.File, PrecededByData: s.precededByData}
	for _, ct := range kept {
		file.Tracks = append(file.Tracks, ct.upstream())
	}
	pieces, err := file.Pieces(cue.FramesPerSecond, -1)
	if err != nil {
		return nil, dropped, waxerr.New(waxerr.CodeInvalid, "meta.CueSheet.Carve", cueRefusal(err))
	}
	for _, p := range pieces {
		switch {
		case p.Audio && p.Track >= 0:
			windows = append(windows, CueWindow{Track: kept[p.Track], StartFrames: p.From, EndFrames: max(p.To, 0)})
		case p.Audio && firstKept && kept[0].Number == 1 && p.To-p.From > hiddenTrackMinFrames:
			hidden := CueTrack{Type: "AUDIO", Title: "Hidden Track", StartValid: true}
			windows = append(windows, CueWindow{Track: hidden, StartFrames: p.From, EndFrames: p.To})
		}
	}
	// The length only places the data track and becomes no stored value, so its
	// milliseconds are fine here.
	if n := len(windows); n > 0 && len(trail) > 0 {
		bound := cue.File{Tracks: []cue.Track{kept[len(kept)-1].upstream(), trail[0].upstream()}}
		if starts, serr := bound.Starts(cue.FramesPerSecond); serr == nil && starts[1]+dataEndMargin <= fileMS*cue.FramesPerSecond/1000 {
			windows[n-1].EndFrames = starts[1]
		}
	}
	return windows, dropped, nil
}

// dataEndMargin is how far inside the file a trailing data track has to start to end
// the last window. A CD track runs at least four seconds, so one the file holds starts
// well clear of the end, and the margin keeps a length that differs from the decoder's
// by a little from closing a window past the end, which the decoder would refuse.
const dataEndMargin = 2 * cue.FramesPerSecond

// sameFrame is why the earlier of two tracks on one frame is dropped.
const sameFrame = "is empty (the next TRACK's INDEX 01 names the same frame)"

// ChapterDrops names the tracks that give a book no chapter: the ones Chapters leaves
// out, and the earlier of two on one start, which the catalog collapses into the later
// once the chapters are in start order.
func (s *CueSheet) ChapterDrops() []string {
	var dropped []string
	for _, ct := range s.Tracks {
		if why := unplaced(ct); why != "" {
			dropped = append(dropped, cueTrackDesc(ct, why))
		}
	}
	usable := s.UsableTracks()
	slices.SortStableFunc(usable, func(a, b CueTrack) int { return cmp.Compare(a.StartFrames, b.StartFrames) })
	for i := 0; i+1 < len(usable); i++ {
		if usable[i+1].StartFrames == usable[i].StartFrames {
			dropped = append(dropped, cueTrackDesc(usable[i], sameFrame))
		}
	}
	return dropped
}

// unplaced says why a track can never be a window or a chapter, or "" when it can: a
// data track is not audio, and an audio track with no INDEX 01 has no start.
func unplaced(ct CueTrack) string {
	switch {
	case !ct.IsAudio():
		return "is a data track"
	case !ct.StartValid:
		return "has no usable INDEX 01"
	}
	return ""
}

// upstream is the track as waxflow/cue models it.
func (t CueTrack) upstream() cue.Track {
	return cue.Track{Number: t.Number, Type: t.Type, Indexes: t.indexes}
}

// cueTrackDesc names one dropped TRACK by the sheet's own track number and title,
// which is what the user has to go look at in the .cue. A TRACK line whose number
// could not be read carries none.
func cueTrackDesc(ct CueTrack, reason string) string {
	name := fmt.Sprintf("TRACK %02d", ct.Number)
	if ct.Number < 0 {
		name = "an unnumbered TRACK"
	}
	if ct.Title != "" {
		return fmt.Sprintf("%s (%q) %s", name, ct.Title, reason)
	}
	return name + " " + reason
}

// cueRefusal renders upstream's refusal without the "cue: " it opens with, which
// says nothing here.
func cueRefusal(err error) string {
	var fe *flowerr.Error
	if errors.As(err, &fe) {
		return strings.TrimPrefix(fe.Error(), "cue: ")
	}
	return err.Error()
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
