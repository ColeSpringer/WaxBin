package model

import "strings"

// SyncedLine is one timed lyric line: its display text and the playback offset it
// appears at. Empty Text is meaningful (an LRC clear marker that blanks the line).
type SyncedLine struct {
	TimeMS int64
	Text   string
}

// Lyrics is an item's structured lyrics: timed (synced) lines and/or a plain
// unsynchronized block. WaxBin parses a sibling .lrc sidecar directly and reads
// embedded USLT/SYLT through WaxLabel; the catalog row is authoritative for reads.
// Source records which producer the stored copy came from.
type Lyrics struct {
	ItemPID  PID
	Source   string       // "lrc" (sidecar) | "embedded"
	Synced   []SyncedLine // timed lines, ordered by TimeMS; nil when none
	Unsynced string       // plain unsynchronized text; "" when none
}

// HasContent reports whether the lyrics carry any synced lines or unsynced text.
// A producer that yields neither is dropped rather than written as an empty row.
func (l *Lyrics) HasContent() bool {
	return l != nil && (len(l.Synced) > 0 || strings.TrimSpace(l.Unsynced) != "")
}
