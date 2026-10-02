package model

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

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
	// it no usable INDEX 01, the next TRACK starts on the same frame and leaves it
	// holding nothing, or its datatype names a data mode rather than audio. Such a
	// track is dropped rather than anchored at 0, since a virtual track's content
	// window is carved from its start offset and a book chapter's end is read off the
	// next chapter's start; a fabricated 0 would claim the head of the file and
	// truncate the track before it. The sheet's other tracks are used as usual.
	//
	// The same row lists the lines of a sheet that could not be read, which the parser
	// skips, and says why a sheet could not be applied at all: it indexes several audio
	// FILEs or none, or its tracks cannot divide the file (they do not ascend, a cooked
	// MODE1/2048 data track sits ahead of audio, or no INDEX places a data track between
	// audio tracks). A rip is not carved from such a sheet: an existing rip keeps its
	// tracks and a new file stays one whole-file track. A book applies the lines that did
	// read, so a misspelled TRACK line costs it a chapter and gives the chapter before
	// the lost track's title. Whenever a sheet was refused or had unread lines, the
	// detail ends by saying what became of it.
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
	// could not store. The enrichment write-back records it under its own origin for a
	// write that failed, which stays owed until a later pass lands it and clears the
	// row, and for a shared file it refuses, which it settles since the refusal would
	// only repeat.
	DiagTagWriteUnsynced DiagnosticCode = "tag_write_unsynced"
	// DiagTagWriteOwed marks a value an edit changed in the catalog that the file does not
	// carry yet. It is the edit writer's, one row per value with its key in TagKey: an
	// item field or credit ("genre", "credit.composer"), a custom tag ("tag.MOOD"), or one
	// of the Owed keys. The edit records it in its own transaction, and a write-back that
	// lands the value clears it, so a failed or refused write-back leaves it standing, apart
	// from a custom tag the file's format writes as a field of its own, which no write-back
	// can ever carry there. A scan that re-reads the file clears it once the catalog holds
	// what the file says; a writer replacing its set never drops one.
	DiagTagWriteOwed DiagnosticCode = "tag_write_owed"
	// DiagCorruptAudio marks audio that is truncated, has no frames, or would not
	// decode cleanly. Two writers record it. The scan's half comes from the tag parse
	// and is format-partial: its signals exist for MP3, AAC, AIFF, MP4, WAV, and a FLAC
	// whose STREAMINFO states its length, and not for Opus, Vorbis, or Matroska. The
	// analyze half comes from the analyze pass's decodes, so it covers every format
	// WaxFlow decodes (a truncated Opus surfaces once analyze has run): a warning when
	// the read worked around damage, an error when the decode failed on it. It
	// describes the audio analyze read, and reads as absent once the file changes. Until
	// analyze has read a file the code proves nothing when it does not fire, so its
	// absence is not evidence of health.
	DiagCorruptAudio DiagnosticCode = "corrupt_audio"
	// DiagSortNameFallback marks a file whose empty display fields were filled from its
	// sort tags (an iTunes file carrying only sonm, soar and soal) or whose number came
	// from its file name. It also names a sort value left unused because it was the
	// inverted "Last, First" form, which explains a display field that stayed empty.
	DiagSortNameFallback DiagnosticCode = "sort_name_fallback"
	// DiagFingerprintFallback marks a file fpcalc could not fingerprint, so the analyze
	// pass stored the pure-Go fingerprint instead, with fpcalc's reason in Detail. Such a
	// file does not group with copies fingerprinted by Chromaprint, and every analyze run
	// tries fpcalc on it again; the row goes once one succeeds, or once a run finds no
	// fpcalc at all. It is the analyze origin's, at Info.
	DiagFingerprintFallback DiagnosticCode = "fingerprint_fallback"
	// DiagDuplicateCopy marks a file with the same audio as its item's primary file,
	// attached to the item as an alternate rather than given an item of its own. Its
	// tags do not describe the item. Detail names the primary's path. It is the scan
	// origin's, at Info.
	DiagDuplicateCopy DiagnosticCode = "duplicate_copy"
	// DiagAlternateEncoding marks a file holding another encoding of its item's
	// recording (the same MusicBrainz recording id, different audio) that ranked below
	// the primary, a writable library first and then quality, so it is attached as an
	// alternate. Detail names the primary's path. It is the scan origin's, at Info.
	DiagAlternateEncoding DiagnosticCode = "alternate_encoding"
)

// The DiagTagWriteOwed keys other than an item field or a credit: the item's front cover,
// its acquisition, an album's front cover, and the two MusicBrainz release ids a detach or
// an mbid clear takes off the files. An entity value written to member files is keyed
// "<entity>.<field>" ("album.label", "artist.sort").
const (
	OwedArt              = "art"
	OwedAcquisition      = "acquisition"
	OwedAlbumArt         = "album.art"
	OwedAlbumMBID        = "album.mbid"
	OwedReleaseGroupMBID = "release_group.mbid"
)

// Valid reports whether c is a known diagnostic code. Every writer records
// codes from this vocabulary, so a filter naming anything else is a typo to
// reject rather than an empty result to return.
func (c DiagnosticCode) Valid() bool {
	switch c {
	case DiagUnsupportedFormat, DiagLegacyOnlyTags, DiagLyricsPartial, DiagSidecarSkipped,
		DiagCueTrackDropped, DiagTagWriteLost, DiagTagWriteUnsynced, DiagTagWriteOwed,
		DiagCorruptAudio, DiagSortNameFallback, DiagFingerprintFallback, DiagDuplicateCopy,
		DiagAlternateEncoding:
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
	OriginEnrichment DiagnosticOrigin = "enrichment"
	// OriginAnalyze is the analyze pass's decode, which reads every file end to end.
	OriginAnalyze DiagnosticOrigin = "analyze"
)

// Valid reports whether o is a known diagnostic writer.
func (o DiagnosticOrigin) Valid() bool {
	switch o {
	case OriginScan, OriginOrganize, OriginReplayGain, OriginEdit, OriginEnrichment, OriginAnalyze:
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
// and an unknown LibraryPID, FilePID, or ItemPID is CodeNotFound. A non-positive
// Limit is uncapped and a non-positive Offset skips nothing: both are treated as
// unset and never reach the SQL, so a negative value cannot produce a surprising
// window. Offset pages over the deterministic path/origin/code order, which is
// enough at this table's grain (a curated finding vocabulary, not a per-track
// table), so there is no keyset cursor here.
//
// Every dimension ANDs with the rest, so FilePID together with ItemPID means "that
// file, if it backs that item".
//
// It lives in model, not read, because the audit's Store port consumes it and
// audit depends only on model (the established seam: the port's other option
// and result types live here too).
type DiagnosticFilter struct {
	Origin     DiagnosticOrigin
	Code       DiagnosticCode
	Severity   AuditSeverity
	LibraryPID PID // scope to files under one library root
	// FilePID scopes to one file, the grain a diagnostic row is recorded at.
	FilePID PID
	// ItemPID scopes to every file backing one item, which is what "this item's
	// issues" actually asks: an item can be multi-file (a multi-part audiobook), so a
	// FilePID-only API would leave the consumer resolving the parts and fanning out
	// one call per part.
	ItemPID PID
	Limit   int
	Offset  int
}

// DiagnosticCount is one bucket of the grouped diagnostic summary: how many
// diagnostics one writer recorded under one code and severity.
type DiagnosticCount struct {
	Origin   DiagnosticOrigin
	Code     DiagnosticCode
	Severity AuditSeverity
	Count    int
}

// MaxDetailBytes bounds a persisted diagnostic detail. A tag_write_lost detail comes
// from a WaxLabel warning whose message can embed a file-derived snippet, which
// upstream sanitizes but does not bound, and an analyze detail carries a decoder's
// error text, so the store bounds every writer's detail.
const MaxDetailBytes = 512

// CapDetail makes s a stored detail: one line a terminal prints as text, at most
// MaxDetailBytes long. What a terminal would act on comes out escaped (see
// escapedRune), a byte that is not UTF-8 as \xNN, and the result is truncated on a
// rune boundary. The store applies it to every writer's detail.
func CapDetail(s string) string { return capBytes(escapeDetail(s), MaxDetailBytes) }

// CapDetailWithTail is CapDetail for a detail that has to end with tail: s gives way,
// so a summary that closes by saying what happened still says it.
func CapDetailWithTail(s, tail string) string {
	return capBytes(escapeDetail(s), max(MaxDetailBytes-len(tail), 0)) + tail
}

// escapeDetail writes an ASCII control or a byte that is not UTF-8 as \xNN and any
// other rune escapedRune names as \uXXXX. Escaped text has nothing left to escape.
func escapeDetail(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, escapedRune) < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1, r < utf8.RuneSelf && escapedRune(r):
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case escapedRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// escapedRune reports a rune a terminal acts on rather than prints: a control
// character (the tab and newline included, since a detail is one line), a
// bidirectional control, a zero width space, word joiner or byte order mark, or a line
// or paragraph separator. The joiners emoji and Indic text need stay.
func escapedRune(r rune) bool {
	switch r {
	case '\u200b', '\u2060', '\ufeff', '\u2028', '\u2029':
		return true
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
}

// capBytes truncates s to n bytes on a rune boundary.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	b := s[:n]
	for len(b) > 0 {
		// size > 1 distinguishes a genuine U+FFFD in the text from the RuneError the
		// decoder returns for a byte sequence cut in half.
		if r, size := utf8.DecodeLastRuneInString(b); r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}
