package meta

import (
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
)

// ParseCue parses a .cue sheet into file-relative navigation chapters, one per
// TRACK, using each track's INDEX 01 as the start and its TITLE as the label. End
// offsets are left open (0) for the book read path to fill from the next chapter's
// start. It returns nil when the sheet has no usable tracks. WaxBin reads .cue
// sidecars directly (WaxLabel has no cue parser); a book with no embedded chapters
// uses these, marked source='cue' so embedded chapters stay authoritative.
func ParseCue(text string) []model.Chapter {
	var chapters []model.Chapter
	inTrack := false
	var cur model.Chapter
	flush := func() {
		if inTrack {
			cur.Position = len(chapters)
			chapters = append(chapters, cur)
		}
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "TRACK "):
			flush()
			inTrack = true
			cur = model.Chapter{}
		case inTrack && strings.HasPrefix(upper, "TITLE "):
			cur.Title = cueQuoted(line[len("TITLE "):])
		case inTrack && strings.HasPrefix(upper, "INDEX 01 "):
			cur.FileStartMS = parseCueTime(strings.TrimSpace(line[len("INDEX 01 "):]))
		}
	}
	flush()
	if len(chapters) == 0 {
		return nil
	}
	return chapters
}

// cueQuoted strips surrounding double quotes from a cue value, or returns it trimmed.
func cueQuoted(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
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
