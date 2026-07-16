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

// CueTrack is one TRACK of a cue sheet: its declared number, title, performer (the
// track's own PERFORMER, empty when it inherits the album performer), and INDEX 01
// start offset in milliseconds.
type CueTrack struct {
	Number    int
	Title     string
	Performer string
	StartMS   int64
}

// Chapters projects a cue sheet's tracks into file-relative navigation chapters,
// one per TRACK, using each track's INDEX 01 as the start and its TITLE as the
// label. End offsets are left open (0) for the book read path to fill from the next
// chapter's start.
func (s *CueSheet) Chapters() []model.Chapter {
	chapters := make([]model.Chapter, len(s.Tracks))
	for i, t := range s.Tracks {
		chapters[i] = model.Chapter{Position: i, Title: t.Title, FileStartMS: t.StartMS}
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
			cur.StartMS = parseCueTime(strings.TrimSpace(line[len("INDEX 01 "):]))
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

// parseCueTime converts a cue MM:SS:FF timestamp (FF = frames, 75 per second) to
// milliseconds. A malformed value yields 0.
func parseCueTime(s string) int64 {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	mm, e1 := strconv.Atoi(parts[0])
	ss, e2 := strconv.Atoi(parts[1])
	ff, e3 := strconv.Atoi(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return 0
	}
	return int64(mm)*60000 + int64(ss)*1000 + int64(ff)*1000/75
}
