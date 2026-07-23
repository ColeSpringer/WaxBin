package model

// DiagnosticCode names a persisted per-file observation. Each word names the
// consequence rather than the tag library's underlying condition.
//
// The vocabulary is a curated subset of what the parser reports, not the whole of
// it. A trailing ID3v1 tag, a legacy APE block, and an inherited encoder stamp all
// fire on most pre-2005 rips, so folding them in would store millions of rows and
// leave the audit reporting problems on a healthy library. The parser's own code
// word travels in Detail, where it informs without becoming a finding.
type DiagnosticCode string

const (
	// DiagUnsupportedFormat marks a file whose container has no parser. It is still
	// cataloged, with a filename-derived title and no tags; the read path used to
	// swallow the condition silently.
	DiagUnsupportedFormat DiagnosticCode = "unsupported_format"
	// DiagLegacyOnlyTags marks a file whose display fields were filled from a legacy
	// container because the authoritative tag set had none. It is emitted only when a
	// fallback was applied, not whenever a legacy-only key exists, since the latter
	// fires on a legacy-only ENCODEDBY that no consumer reads.
	DiagLegacyOnlyTags DiagnosticCode = "legacy_only_tags"
	// DiagLyricsPartial marks a .lrc sidecar that parsed with some lines timed and
	// some dropped. A fully untimed .lrc is plain text rather than a broken sidecar,
	// and is not reported.
	DiagLyricsPartial DiagnosticCode = "lyrics_partial"
	// DiagSidecarSkipped marks a sidecar that was on disk but not applied, because it
	// is far larger than any real .lrc/.cue and reading it whole into memory during a
	// scan is what the size guard exists to prevent. Without the diagnostic the skip
	// is invisible: the user sees a sidecar beside the audio, no lyrics or chapters,
	// and no explanation.
	DiagSidecarSkipped DiagnosticCode = "sidecar_skipped"
	// DiagCueTrackDropped marks a cue TRACK the scanner could not use: the sheet gave
	// it no usable INDEX 01, or the next TRACK starts on the same frame and leaves it
	// holding nothing. Such a track is dropped rather than anchored at 0, since a
	// virtual track's content window is carved from its start offset and a book
	// chapter's end is read off the next chapter's start; a fabricated 0 would claim
	// the head of the file and truncate the track before it. The sheet's other tracks
	// are used as usual.
	//
	// Without the diagnostic the drop is invisible: the user sees fewer tracks or
	// chapters than the sheet declares, and no explanation.
	DiagCueTrackDropped DiagnosticCode = "cue_track_dropped"
	// DiagTagWriteLost marks an on-disk tag write that did not land a value as asked.
	DiagTagWriteLost DiagnosticCode = "tag_write_lost"
	// DiagTagWriteUnsynced marks a catalog field edit whose on-disk tag write-back did
	// not apply, leaving the file's tags out of sync with the catalog until they are
	// re-written. It fires when write-back is refused, as for a file shared by several
	// items, or when it fails, as on a read-only mount or a permission error. This is
	// not DiagTagWriteLost, which is for a write that ran but hit a value the format
	// could not store.
	DiagTagWriteUnsynced DiagnosticCode = "tag_write_unsynced"
	// DiagCorruptAudio marks a parse that found truncated audio or no audio frames.
	//
	// Its coverage is format-partial, and callers must present it that way. The
	// underlying signals exist for MP3, AAC, AIFF, MP4, and WAV, and not for FLAC,
	// Opus, Vorbis, or Matroska. The code is a true positive when it fires and proves
	// nothing when it does not, so its absence is not evidence of health.
	DiagCorruptAudio DiagnosticCode = "corrupt_audio"
)

// Valid reports whether c is a known diagnostic code. Every writer records
// codes from this vocabulary, so a filter naming anything else is a typo to
// reject rather than an empty result to return.
func (c DiagnosticCode) Valid() bool {
	switch c {
	case DiagUnsupportedFormat, DiagLegacyOnlyTags, DiagLyricsPartial, DiagSidecarSkipped,
		DiagCueTrackDropped, DiagTagWriteLost, DiagTagWriteUnsynced, DiagCorruptAudio:
		return true
	default:
		return false
	}
}

// DiagnosticOrigin identifies the writer that produced a diagnostic, rather than the
// phase it occurred in. Each writer replaces its own rows wholesale, so cross-writer
// isolation is a property of the schema (origin sits in the primary key) instead of
// a delete predicate, and a retry that comes back clean clears its own stale rows
// without extra work.
//
// The alternative, a scan/write pair plus a delete predicate over the keys an edit
// attempted, rests on an assumption the tag library does not honor. Its MP4 path
// reports dropped values across the whole edited tag set with no changed-key map
// (the ID3 path does pass one), so a PID-only write to an .m4a can report a drop for
// a key the edit never touched. That row falls outside any attempted-key set and can
// never be deleted. Per-writer origins drop the assumption, and the over-broad MP4
// row clears on that writer's next run.
type DiagnosticOrigin string

const (
	OriginScan       DiagnosticOrigin = "scan"
	OriginOrganize   DiagnosticOrigin = "organize"
	OriginReplayGain DiagnosticOrigin = "replaygain"
	OriginEdit       DiagnosticOrigin = "edit"
)

// Valid reports whether o is a known diagnostic writer.
func (o DiagnosticOrigin) Valid() bool {
	switch o {
	case OriginScan, OriginOrganize, OriginReplayGain, OriginEdit:
		return true
	default:
		return false
	}
}

// FileDiagnostic is one persisted observation about a file. Severity reuses
// AuditSeverity because the audit's sort already ranks that vocabulary. TagKey
// is the canonical tag key a key-specific diagnostic concerns, or "" when it
// names none.
type FileDiagnostic struct {
	FilePID     PID
	DisplayPath string
	Origin      DiagnosticOrigin
	Code        DiagnosticCode
	Severity    AuditSeverity
	TagKey      string
	Detail      string
	SeenAt      int64
}

// DiagnosticFilter selects a slice of the persisted per-file diagnostics. The
// zero filter selects everything, which is what the audit reads. A zero
// dimension means "any"; a non-empty Origin, Code, or Severity outside its
// vocabulary is CodeInvalid (a typo fails closed instead of matching nothing),
// and an unknown LibraryPID is CodeNotFound. A non-positive Limit is uncapped
// and a non-positive Offset skips nothing: both are treated as unset and never
// reach the SQL, so a negative value cannot produce a surprising window.
// Offset pages over the deterministic path/origin/code order, which is enough
// at this table's grain (a curated finding vocabulary, not a per-track table),
// so there is no keyset cursor here.
//
// It lives in model, not read, because the audit's Store port consumes it and
// audit depends only on model (the established seam: the port's other option
// and result types live here too).
type DiagnosticFilter struct {
	Origin     DiagnosticOrigin
	Code       DiagnosticCode
	Severity   AuditSeverity
	LibraryPID PID // scope to files under one library root
	Limit      int
	Offset     int
}

// DiagnosticCount is one bucket of the grouped diagnostic summary: how many
// diagnostics one writer recorded under one code and severity.
type DiagnosticCount struct {
	Origin   DiagnosticOrigin
	Code     DiagnosticCode
	Severity AuditSeverity
	Count    int
}
