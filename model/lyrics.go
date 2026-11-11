package model

import (
	"cmp"
	"slices"
	"strings"
)

// SyncedLine is one timed lyric line: its display text and the playback offset it
// appears at, from 0 to MaxSyncedLineMS in a stored line. Empty Text is meaningful (an
// LRC clear marker that blanks the line).
type SyncedLine struct {
	TimeMS int64
	Text   string
}

// MaxSyncedLineMS is the latest time a stored synced line carries: 2^21 minutes, a little
// under four years and the last minute an .lrc timestamp can name, so every stored line
// prints as .lrc and reads back.
const MaxSyncedLineMS = int64(1<<21) * 60_000

// NormalizeSyncedLines returns lines as the catalog stores them: in time order, lines
// given at one time in the order given, a time before the start at 0 and one past
// MaxSyncedLineMS at it. It sorts by the time given before it moves either, so the lines
// it moves keep their order in time. Lines already in that form come back as they are,
// and otherwise a copy does, so the caller's slice is never changed.
func NormalizeSyncedLines(lines []SyncedLine) []SyncedLine {
	byTime := func(a, b SyncedLine) int { return cmp.Compare(a.TimeMS, b.TimeMS) }
	n := len(lines)
	if slices.IsSortedFunc(lines, byTime) && (n == 0 || lines[0].TimeMS >= 0 && lines[n-1].TimeMS <= MaxSyncedLineMS) {
		return lines
	}
	out := slices.Clone(lines)
	slices.SortStableFunc(out, byTime)
	for i := range out {
		out[i].TimeMS = min(max(out[i].TimeMS, 0), MaxSyncedLineMS)
	}
	return out
}

// Lyrics is an item's structured lyrics: timed (synced) lines and/or a plain
// unsynchronized block. WaxBin parses a sibling .lrc sidecar directly and reads
// embedded USLT/SYLT through WaxLabel; the catalog row is authoritative for reads.
// Source records which producer the stored copy came from, in the same vocabulary
// art uses, so a consumer can draw one mark under both.
type Lyrics struct {
	ItemPID PID
	// tag (embedded USLT/SYLT) | sidecar (an .lrc beside the audio) | user | enrichment.
	Source   ProvenanceSource
	Provider string // lyrics provider id, when Source is enrichment
	// Synced is the timed lines, nil when none. A read answers them in time order, lines
	// sent at one time in the order their writer sent them (NormalizeSyncedLines).
	Synced    []SyncedLine
	Unsynced  string // plain unsynchronized text; "" when none
	UpdatedAt int64  // unix nanoseconds
}

// HasContent reports whether the lyrics carry any synced lines or unsynced text.
// A producer that yields neither is dropped rather than written as an empty row.
func (l *Lyrics) HasContent() bool {
	return l != nil && (len(l.Synced) > 0 || strings.TrimSpace(l.Unsynced) != "")
}
