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
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == ModeManaged || m == ModeInPlace }

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
