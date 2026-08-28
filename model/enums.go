// Package model holds WaxBin's unified domain types and the repository
// interfaces the rest of the engine depends on. Concrete persistence lives in
// store/sqlite; defining the interfaces here inverts that dependency so scan,
// organize, jobs, and the facade depend on the domain, not on SQLite.
package model

import "strings"

// Mode is how a library root is handled.
type Mode string

const (
	// ModeManaged means WaxBin may organize (move/rename) files under the root.
	ModeManaged Mode = "managed"
	// ModeInPlace means WaxBin only indexes/watches; it never moves files.
	ModeInPlace Mode = "in-place"
	// ModePodcast marks the internal library for downloaded podcast episode files.
	// Podcast code creates it, scan/organize skip it, and Mode.Valid rejects it so
	// users cannot configure it as a normal root.
	ModePodcast Mode = "podcast"
)

// Valid reports whether m is a user-settable library mode. ModePodcast is internal
// and intentionally excluded.
func (m Mode) Valid() bool { return m == ModeManaged || m == ModeInPlace }

// MediaType is the content class a managed root holds. It lets organize and import
// route tracks and books to type-specific roots. A mixed root keeps the single-tree
// behavior where both kinds share one library.
type MediaType string

const (
	MediaMusic     MediaType = "music"
	MediaAudiobook MediaType = "audiobook"
	MediaMixed     MediaType = "mixed"
)

// Valid reports whether t is a settable media type.
func (t MediaType) Valid() bool {
	return t == MediaMusic || t == MediaAudiobook || t == MediaMixed
}

// Accepts reports whether a library of this media type is the routing target for a
// kind. A mixed library accepts every managed kind (track/book); a typed library
// accepts only its matching kind. Episodes normally live in the internal podcast
// library, but a mixed root still accepts them for callers that need a fallback.
func (t MediaType) Accepts(k Kind) bool {
	switch t {
	case MediaMusic:
		return k == KindTrack
	case MediaAudiobook:
		return k == KindBook
	case MediaMixed:
		return true
	default:
		return false
	}
}

// SourceType is the acquisition origin of a show or item. For shows, it also selects
// the provider used for sync and download. SourceLocal marks an item that was locally
// scanned and therefore carries no acquisition row.
type SourceType string

const (
	SourceLocal   SourceType = "local"   // locally scanned/imported without a remote origin
	SourceRSS     SourceType = "rss"     // an HTTP podcast feed (the built-in provider)
	SourceYouTube SourceType = "youtube" // an injected YouTube provider
	// SourceManual reads two ways by rung. A show carries it when it is user-curated
	// with no feed to sync. An item carries it when it was acquired by unspecified
	// means, which is what the store records for an event that named no mechanism and
	// for tag-derived origin. Passing it explicitly is a claim, and beats a standing
	// type on a re-record; leaving the type empty is not.
	SourceManual SourceType = "manual"
)

// ValidShowSource reports whether s is a valid podcast (show) source type. A show
// is rss, youtube, or manual; SourceLocal is item-only and excluded.
func (s SourceType) ValidShowSource() bool {
	return s == SourceRSS || s == SourceYouTube || s == SourceManual
}

// ValidItemSource reports whether s is a valid source type on an item's acquisition
// row. It delegates rather than repeating the list, which would drift. SourceLocal is
// excluded here too: on an item it is the absence of a row, so asking for it is asking
// for a clear.
func (s SourceType) ValidItemSource() bool { return s.ValidShowSource() }

// FileKind classifies a file on disk. Audio is the only decodable kind; the
// rest are sidecars. "foreign" marks interop sidecars WaxBin recognizes but
// does not own (they are never treated as orphans).
type FileKind string

const (
	FileAudio      FileKind = "audio"
	FileImage      FileKind = "image"
	FileLyrics     FileKind = "lyrics"
	FileTranscript FileKind = "transcript"
	FileCue        FileKind = "cue"
	FileChapters   FileKind = "chapters"
	FilePeaks      FileKind = "peaks"
	FileNFO        FileKind = "nfo"
	FileForeign    FileKind = "foreign"
)

// Kind is the supertype of a playable_item. The three map onto the media types a
// consumer thinks in: a track is music, a book is an audiobook, an episode is a
// podcast episode.
type Kind string

const (
	KindTrack   Kind = "track"
	KindBook    Kind = "book"
	KindEpisode Kind = "episode"
)

// Valid reports whether k is a known item kind. A caller filtering by kind should
// check it rather than passing the string through: an unknown kind matches no rows,
// so a typo would read as an empty library instead of a mistake.
func (k Kind) Valid() bool {
	switch k {
	case KindTrack, KindBook, KindEpisode:
		return true
	default:
		return false
	}
}

// Kinds lists the item kinds, for help text and validation messages.
func Kinds() []Kind { return []Kind{KindTrack, KindBook, KindEpisode} }

// ItemState decouples a logical item from the presence of its files.
type ItemState string

const (
	StatePresent  ItemState = "present"  // has at least one present file
	StateArchived ItemState = "archived" // files gone, history kept
	StateRemote   ItemState = "remote"   // known but not local (e.g. unfetched episode)
	StateMissing  ItemState = "missing"  // expected file absent at scan
)

// Valid reports whether s is a known item state. A caller narrowing by state should
// check it rather than passing the string through: an unknown state matches no rows,
// so a typo would read as an empty catalog instead of a mistake.
func (s ItemState) Valid() bool {
	switch s {
	case StatePresent, StateArchived, StateRemote, StateMissing:
		return true
	default:
		return false
	}
}

// ItemStates lists the item states, for help text and validation messages. The
// playable_item.state column has no CHECK constraint, so this is the vocabulary's
// only authority.
func ItemStates() []ItemState {
	return []ItemState{StatePresent, StateArchived, StateRemote, StateMissing}
}

// ItemStateList renders the state vocabulary for a help string or a rejection
// message. It lives here so the store and the CLI, which both validate a caller's
// states, quote the same list rather than keeping one each.
func ItemStateList() string {
	sts := ItemStates()
	names := make([]string, len(sts))
	for i, st := range sts {
		names[i] = string(st)
	}
	return strings.Join(names, "|")
}

// MarkMissingOutcome is what a mark-missing did, or why it did nothing. A bare
// bool would collapse four different answers into false, and for a caller repairing
// the catalog the distinction is the payload: a worker requeuing doomed work needs
// to tell "the bytes really are on disk, so your failure is something else" apart
// from "already recorded".
type MarkMissingOutcome string

const (
	OutcomeMarked         MarkMissingOutcome = "marked"
	OutcomeAlreadyMissing MarkMissingOutcome = "already-missing"
	OutcomeFilesPresent   MarkMissingOutcome = "files-present"
	OutcomeArchived       MarkMissingOutcome = "archived"
	OutcomeRemote         MarkMissingOutcome = "remote"
)

// ScanState tracks where a file is in the scan/analyze lifecycle.
type ScanState string

const (
	ScanIndexed       ScanState = "indexed"        // cataloged
	ScanNeedsAnalysis ScanState = "needs_analysis" // queued for the analyze pass
	ScanAnalyzed      ScanState = "analyzed"
)

// ChangeOp is the verb in a change_log row.
type ChangeOp string

const (
	OpCreate ChangeOp = "create"
	OpUpdate ChangeOp = "update"
	OpDelete ChangeOp = "delete"
)
