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

// FileDiagnostic is one persisted observation about a file. Severity reuses
// AuditSeverity because the audit is the only consumer and its sort already ranks
// that vocabulary. TagKey is the canonical tag key a key-specific diagnostic
// concerns, or "" when it names none.
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
