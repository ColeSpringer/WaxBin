// Package model holds WaxBin's unified domain types and the repository
// interfaces the rest of the engine depends on. Concrete persistence lives in
// store/sqlite; defining the interfaces here inverts that dependency so scan,
// organize, jobs, and the facade depend on the domain, not on SQLite.
package model

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
	SourceManual  SourceType = "manual"  // a user-curated show with no feed to sync
)

// ValidShowSource reports whether s is a valid podcast (show) source type. A show
// is rss, youtube, or manual; SourceLocal is item-only and excluded.
func (s SourceType) ValidShowSource() bool {
	return s == SourceRSS || s == SourceYouTube || s == SourceManual
}

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

// Kind is the supertype of a playable_item.
type Kind string

const (
	KindTrack   Kind = "track"
	KindBook    Kind = "book"
	KindEpisode Kind = "episode"
)

// ItemState decouples a logical item from the presence of its files.
type ItemState string

const (
	StatePresent  ItemState = "present"  // has at least one present file
	StateArchived ItemState = "archived" // files gone, history kept
	StateRemote   ItemState = "remote"   // known but not local (e.g. unfetched episode)
	StateMissing  ItemState = "missing"  // expected file absent at scan
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
