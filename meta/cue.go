package meta

import (
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
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

// CueTrack is one TRACK of a cue sheet: its declared number, title, and performer
// (the track's own PERFORMER, empty when it inherits the album performer), plus its
// INDEX 01 start offset in CD frames.
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
	Number      int
	Title       string
	Performer   string
	StartFrames int64
	StartValid  bool
}

// UsableTracks returns the tracks that declared a parseable INDEX 01, in sheet
// order. A track without one has no start offset, and the start offset is what every
// consumer here anchors on, so admitting it at a fabricated 0 does damage in both
// directions: it claims the head of the file for itself, and it truncates the track
// or chapter before it, whose end is read off the next one's start.
//
// Callers that must report the drop read Tracks and filter on StartValid themselves;
// the scanner does, so the sheet's own diagnostics name what was skipped.
func (s *CueSheet) UsableTracks() []CueTrack {
	out := make([]CueTrack, 0, len(s.Tracks))
	for _, t := range s.Tracks {
		if t.StartValid {
			out = append(out, t)
		}
	}
	return out
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
// TRACK. It returns nil when the sheet has no usable tracks. A book with no
// embedded chapters uses these, marked source='cue' so embedded chapters stay
// authoritative.
func ParseCue(text string) []model.Chapter {
	sheet := ParseCueSheet(text)
	if sheet == nil {
		return nil
	}
	return sheet.Chapters()
}

// ParseCueSheet parses a .cue sheet into its album-level fields and per-track
// entries. It returns nil when the sheet declares no TRACK, so a caller can treat
// an empty or unparseable sheet the same as an absent one.
func ParseCueSheet(text string) *CueSheet {
	sheet := &CueSheet{}
	inTrack := false
	var cur CueTrack
	flush := func() {
		if inTrack {
			sheet.Tracks = append(sheet.Tracks, cur)
		}
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "TRACK "):
			flush()
			inTrack = true
			cur = CueTrack{Number: cueTrackNumber(line)}
		case strings.HasPrefix(upper, "TITLE "):
			// A TITLE line before the first TRACK names the album; one inside a TRACK
			// names that track.
			if inTrack {
				cur.Title = cueQuoted(line[len("TITLE "):])
			} else {
				sheet.Title = cueQuoted(line[len("TITLE "):])
			}
		case strings.HasPrefix(upper, "PERFORMER "):
			if inTrack {
				cur.Performer = cueQuoted(line[len("PERFORMER "):])
			} else {
				sheet.Performer = cueQuoted(line[len("PERFORMER "):])
			}
		case inTrack && strings.HasPrefix(upper, "INDEX 01 "):
			cur.StartFrames, cur.StartValid = parseCueTime(strings.TrimSpace(line[len("INDEX 01 "):]))
		case strings.HasPrefix(upper, "REM GENRE "):
			sheet.Genre = cueQuoted(strings.TrimSpace(line[len("REM GENRE "):]))
		case strings.HasPrefix(upper, "REM DATE "):
			sheet.Year = cueYear(strings.TrimSpace(line[len("REM DATE "):]))
		}
	}
	flush()
	if len(sheet.Tracks) == 0 {
		return nil
	}
	return sheet
}

// cueTrackNumber reads the decimal track number from a "TRACK NN AUDIO" line,
// returning 0 when it is missing or unparseable.
func cueTrackNumber(line string) int {
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		if n, err := strconv.Atoi(fields[1]); err == nil {
			return n
		}
	}
	return 0
}

// cueYear extracts a leading four-digit year from a REM DATE value ("1998" or
// "1998-05-01"), returning 0 when there is none.
func cueYear(s string) int {
	s = cueQuoted(s)
	if len(s) < 4 {
		return 0
	}
	n, err := strconv.Atoi(s[:4])
	if err != nil {
		return 0
	}
	return n
}

// cueQuoted strips surrounding double quotes from a cue value and trims whitespace,
// including any that padded the value inside the quotes (TITLE " Jazz " -> "Jazz"),
// so a display name or date never carries stray leading/trailing spaces.
func cueQuoted(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// maxCueMinutes bounds MM, which is what keeps parseCueTime's arithmetic from
// wrapping. A sheet is not always a CD: a disc tops out near 80 minutes, but sheets
// pair with single-file sources that were never discs, and an audiobook or a DJ set
// can run 40 hours and index itself honestly. 100 hours clears the longest of those
// by better than twice while staying far from int64's limits.
const maxCueMinutes = 100 * 60

// parseCueTime converts a cue MM:SS:FF timestamp to CD frames (75/sec), the unit
// the sheet is written in and the one we store. It rejects what it cannot
// represent rather than yielding 0: a silently zeroed INDEX used to be a bad seek,
// but a start offset is content identity now, and zero would serve the album from
// its beginning in place of track five. SS > 59 and FF > 74 are malformed, and MM
// is capped because an unbounded one overflows int64 into a negative frame count,
// which becomes a negative sample offset downstream. waxflow/internal/cue.ParseTime
// bounds the same three, for the same reason.
func parseCueTime(s string) (int64, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	var n [3]int
	for i, p := range parts {
		// Reject a signed or space-padded field rather than letting Atoi take it: "-0"
		// and "+1" parse fine and mean nothing here.
		if p == "" || strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, false
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		n[i] = v
	}
	mm, ss, ff := n[0], n[1], n[2]
	if mm > maxCueMinutes || ss > 59 || ff > model.FramesPerSecond-1 {
		return 0, false
	}
	return (int64(mm)*60+int64(ss))*model.FramesPerSecond + int64(ff), true
}
