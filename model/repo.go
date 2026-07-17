package model

import (
	"context"

	"github.com/colespringer/waxbin/query"
)

// PutScannedTrackInput carries everything needed to persist one scanned audio
// track atomically. The store owns PID assignment: PIDs on File/Item are
// ignored on input and assigned (new) or preserved (existing match) by the
// store, keyed by File.EssenceHash/Path and Item.IdentityKey.
type PutScannedTrackInput struct {
	LibraryID int64
	File      File
	Item      PlayableItem
	Track     Track
	// Lyrics is the track's structured lyrics (sidecar .lrc or embedded), or nil.
	// The store writes them alongside the item; a nil value clears any prior row.
	Lyrics *Lyrics
	// CoverArt is the track's front-cover image (embedded or directory), or nil.
	// The store dedups it into the content-addressed art store and maps it onto the
	// track. Album art is derived from current track covers at read time.
	CoverArt *ArtImage
	// AuxObservations records the on-disk state of this file's sidecars (a sibling
	// .lrc, the directory cover) so a later rescan can stat-compare them and re-parse
	// only a changed one instead of re-reading every sidecar every scan.
	AuxObservations []AuxObservation
	// PreferredItemPID is a HINT read from the file's WAXBIN_ITEM_PID tag, used only
	// during a rebuild to restore the backing item's original PID. It is adopted only
	// when creating a new item and only when the PID is valid and not already taken
	// (essence-first, PID-as-hint); on any conflict the store mints a fresh PID. It is
	// ignored on a normal scan, where the store owns PID assignment.
	PreferredItemPID PID
	// Acquisition is origin provenance from the file's own tags. The store records it
	// only when the item has no acquisition row yet, so an event-recorded origin (a
	// podcast download) is never clobbered by a re-derived tag.
	Acquisition TagAcquisition
	// Diagnostics are this file's scan-origin observations. The store replaces the
	// scan's whole set, so a file that comes back clean clears its own stale rows.
	Diagnostics []FileDiagnostic
}

// TagWaxbinItemPID is the custom tag key that carries a backing item's stable WaxBin
// PID (stamped by organize when configured). It is a hint for rebuild only; identity
// is essence-first and the tag is copyable, so it is never authoritative.
const TagWaxbinItemPID = "WAXBIN_ITEM_PID"

// PutScannedBookInput carries one scanned audiobook file for atomic persistence.
// A book groups one or many files by Item.IdentityKey (the book key); each file is
// attached as a part in reading order, the first becoming the representative
// primary. The store owns PID assignment (PIDs on input are ignored). Book.Authors
// and Book.Narrators drive the role-tagged contributor links and their artist
// entities.
type PutScannedBookInput struct {
	LibraryID int64
	File      File
	Item      PlayableItem // Kind=KindBook, IdentityKey=identity.BookKey(...)
	Book      Book         // subtype fields
	// Position orders this file among the book's parts (from disc/track tags, else
	// 0; the read path falls back to rel-path order for unnumbered parts).
	Position int
	// Chapters are this file's navigation chapters, file-relative (Title/StartMS/
	// EndMS). The scanner may synthesize a single whole-file chapter for a part with
	// none so multi-file books still navigate by part.
	Chapters []Chapter
	// ChapterSource records where Chapters came from: "embedded" (audio tags), "cue"
	// (a sibling .cue sidecar), or "synthetic" (a synthesized single chapter). It sets
	// the chapter rows' source so embedded chapters stay authoritative over cue ones.
	// Empty defaults to "embedded".
	ChapterSource string
	// CoverArt is the book's cover image (embedded or directory), or nil.
	CoverArt *ArtImage
	// AuxObservations records the on-disk state of this file's sidecars for the scan
	// fast-path (see PutScannedTrackInput.AuxObservations).
	AuxObservations []AuxObservation
	// PreferredItemPID is a HINT read from the file's WAXBIN_ITEM_PID tag (see
	// PutScannedTrackInput.PreferredItemPID). organize stamps books too, so rebuild
	// restores a book item's original PID when the hint is valid and unclaimed; any
	// conflict falls back to a fresh PID. Parts of one book share the same stamp, so
	// whichever part first creates the item adopts it and the rest join by book key.
	PreferredItemPID PID
	// Acquisition is origin provenance from this file's own tags (see
	// PutScannedTrackInput.Acquisition). acquisition is item-level while tags are
	// file-level, so for a multi-file book the first part scanned supplies the row and
	// later parts leave it alone.
	Acquisition TagAcquisition
	// Diagnostics are this part's scan-origin observations (see
	// PutScannedTrackInput.Diagnostics). They are per-FILE, so each part carries its
	// own rather than the book carrying a merged set.
	Diagnostics []FileDiagnostic
}

// PutScannedVirtualTracksInput carries a single-file album rip and the virtual
// tracks a .cue sheet carves out of it. Every track shares the one backing File
// through its own primary item_file edge, which carries the track's
// [StartFrames, EndFrames) offset window; the tracks are reconciled as a SET, so a
// rescan adds, updates, and removes the whole set against the file at once, without
// the single-owner detach that PutScannedTrack applies to a file. The store owns PID
// assignment.
type PutScannedVirtualTracksInput struct {
	LibraryID int64
	File      File
	// Tracks are the virtual tracks to represent, each carved from File. An entry's
	// Item.IdentityKey is identity.VirtualTrackKey(...) and its Track carries the cue's
	// per-track and album-level display metadata.
	Tracks []VirtualTrack
	// CoverArt is the rip's cover image (embedded or directory), mapped onto each
	// virtual track, or nil.
	CoverArt *ArtImage
	// AuxObservations records this file's sidecar state (the .cue, a directory cover)
	// for the scan fast-path, exactly as the track/book inputs do.
	AuxObservations []AuxObservation
	// Acquisition is origin provenance from the file's own tags, recorded on each
	// virtual track when it has no acquisition row yet.
	Acquisition TagAcquisition
	// Diagnostics are this file's scan-origin observations, replacing the file's whole
	// scan-origin set.
	Diagnostics []FileDiagnostic
}

// VirtualTrack is one cue TRACK of a single-file rip: a full track item plus the
// [StartFrames, EndFrames) window it plays within the shared file, in CD frames
// (75/sec), the unit the .cue is written in. EndFrames is 0 for the final track,
// which runs to the end of the file. The sheet names no end for that one, and the
// file's own duration is stored in milliseconds, which cannot be converted back to a
// frame without losing the sample the boundary sits on.
type VirtualTrack struct {
	Item        PlayableItem // Kind=KindTrack, IdentityKey=identity.VirtualTrackKey(...)
	Track       Track
	StartFrames int64
	EndFrames   int64
}

// ScanItemResult reports what the store did for a PutScannedTrack call, enough
// for the scanner to log and for tests to assert identity behavior.
type ScanItemResult struct {
	FilePID        PID
	ItemPID        PID
	ItemCreated    bool // a new logical item was created
	FileCreated    bool // a new file row was created
	Relinked       bool // an existing file (matched by essence) moved to a new path
	ContentChanged bool // content_hash changed; if essence is stable this is a tag-only update
	// SidecarsChanged reports that the write changed the item's lyrics or cover art
	// without changing the audio bytes.
	//
	// It exists because ContentChanged cannot stand in for it: a .lrc or cover edit
	// does not touch the audio, so a sidecar-only change routed through the full path
	// would otherwise report every counter zero, and the scan would report
	// changed=false, silently skipping watch mode's downstream schedulers.
	SidecarsChanged bool
}

// ItemFileRef is one backing file of an item, in reading order. organize uses it
// to move every part of a multi-file book, not just the representative primary,
// so a book is never split across folders.
type ItemFileRef struct {
	FilePID     PID
	Path        []byte // raw bytes of the current path
	DisplayPath string
	Position    int
}

// RelocateInput records a completed filesystem move so the store can update the
// file's path columns and append an organize_journal entry plus a change_log
// row in one transaction.
type RelocateInput struct {
	FilePID        PID
	JobPID         PID
	SrcPath        []byte
	NewPath        []byte
	NewDisplayPath string
	NewRelPath     []byte
}

// Sidecar-observation kinds recorded in file_aux_state. They are the file kinds the
// scanner stat-compares beside an audio file to decide whether to re-parse a sidecar.
const (
	AuxLyrics   = "lrc"      // a sibling .lrc lyrics sidecar
	AuxCover    = "cover"    // the directory cover image
	AuxCue      = "cue"      // an external .cue / chapter sidecar
	AuxChapters = "chapters" // a JSON/other external chapter file
)

// AuxObservation records the on-disk state of one sidecar (a .lrc/.cue/chapter file
// or a directory cover) beside an audio file. The scanner os.Stat-compares size and
// mtime against the stored observation and re-parses only on a difference, so an
// untouched file and its sidecars cost one stat each and no hashing or parsing.
type AuxObservation struct {
	Kind    string // one of the Aux* constants
	Path    []byte
	Size    int64
	MTimeNS int64
	Hash    string
	Missing bool // observed before but now absent from disk
}

// ScopedFile is one present file in a library scope, preloaded so the scanner can
// fast-path an unchanged file (size+mtime match) entirely in memory and reconcile a
// vanished one at end-of-walk, without a per-file SELECT. It carries the item the
// file backs so a sidecar-only change can be applied without re-resolving identity,
// and its known sidecar observations so a sidecar re-parse is stat-gated.
type ScopedFile struct {
	FilePID PID
	ItemPID PID
	// ItemKind is the kind of the item the file backs (track/book/episode), or empty
	// for an edge-less file. The fast-path routes a changed .cue by kind: a book
	// applies chapters cheaply in place, while a track (or a virtual-track container)
	// falls through to the full path, which owns the virtual-track set reconcile.
	ItemKind Kind
	Size     int64
	MTimeNS  int64
	Aux      []AuxObservation
}

// SidecarUpdate carries an item's freshly re-parsed sidecar data plus the on-disk
// observations to record, applied outside the audio-change gate in one transaction.
// Nil Lyrics/CoverArt leave those untouched (a scan never clears art on a failed
// read); ReplaceChapters replaces the file's cue/chapter-file-sourced chapters.
type SidecarUpdate struct {
	ItemPID         PID
	FilePID         PID
	Lyrics          *Lyrics
	CoverArt        *ArtImage
	Chapters        []Chapter
	ReplaceChapters bool             // when true, Chapters replace FilePID's cue-sourced chapters
	ChapterSource   string           // the chapter source tag to write (e.g. "cue"); defaults to "cue"
	Observations    []AuxObservation // sidecar observations to persist for FilePID
}

// ReplayGainRow is one file's current ReplayGain measurement plus the on-disk file
// state, for the post-analysis tag write-back pass. HasAlbum reports whether an
// album aggregate exists (a standalone track has none).
type ReplayGainRow struct {
	FilePID     PID
	Path        []byte
	Container   string
	Codec       string
	Size        int64
	MTimeNS     int64
	TrackGainDB float64
	TrackPeak   float64
	HasAlbum    bool
	AlbumGainDB float64
	AlbumPeak   float64
}

// OrphanGCReport tallies an orphan-entity sweep: how many childless entities of each
// kind were deleted, and how many are newly recorded as candidates still within the
// grace window (not yet swept).
type OrphanGCReport struct {
	Albums        int
	ReleaseGroups int
	Artists       int
	Genres        int
	Series        int
	Pending       int // candidates recorded but still within the grace window
}

// Total returns the number of entities deleted across all kinds.
func (r OrphanGCReport) Total() int {
	return r.Albums + r.ReleaseGroups + r.Artists + r.Genres + r.Series
}

// FileStateUpdate records the result of an on-disk tag write so the catalog's file
// row matches the bytes now on disk. It is applied only when the stored size and
// mtime still match ExpectedSize/ExpectedMTimeNS (optimistic concurrency): a match
// means the writer's read is still current, a mismatch means a concurrent scan/move
// touched the file and the update is skipped, to be reconciled by the next scan.
type FileStateUpdate struct {
	FilePID         PID
	ExpectedSize    int64
	ExpectedMTimeNS int64
	NewSize         int64
	NewMTimeNS      int64
	NewContentHash  string
}

// Catalog is the persistence port for the catalog. store/sqlite implements it;
// scan, organize, and the facade depend on it rather than on SQLite directly.
// Each method is individually atomic (it manages its own write transaction,
// including the matching change_log row).
type Catalog interface {
	EnsureLibrary(ctx context.Context, lib *Library) (*Library, error)
	LibraryByRoot(ctx context.Context, root []byte) (*Library, error)
	Libraries(ctx context.Context) ([]*Library, error)

	PutScannedTrack(ctx context.Context, in PutScannedTrackInput) (*ScanItemResult, error)
	PutScannedBook(ctx context.Context, in PutScannedBookInput) (*ScanItemResult, error)
	// PutScannedVirtualTracks persists the virtual tracks a .cue sheet carves out of
	// one single-file album rip, reconciling them as a set against the shared file
	// (add/update/remove), each with its own offset window. The result summarizes the
	// file-level outcome; ItemCreated reports whether any virtual track was created.
	PutScannedVirtualTracks(ctx context.Context, in PutScannedVirtualTracksInput) (*ScanItemResult, error)
	FileByPath(ctx context.Context, path []byte) (*File, error)
	FileByEssence(ctx context.Context, essence string) (*File, error)

	// LoadScopedFileIndex bulk-loads the present files under a library scope (a raw
	// path prefix; nil/empty spans the whole library) into path->ScopedFile, so the
	// scanner fast-paths unchanged files and reconciles vanished ones without a
	// per-file query.
	LoadScopedFileIndex(ctx context.Context, libraryID int64, scopePrefix []byte) (map[string]ScopedFile, error)
	// MarkFilesMissing marks the items backing the given files as missing, but only
	// when every file of an item is in the set (so a multi-file book that lost one
	// part stays present). Rows are preserved, so a rescan restores present state.
	// Returns the number of items newly marked missing.
	MarkFilesMissing(ctx context.Context, filePIDs []PID) (int, error)
	// UpdateItemSidecars refreshes an item's sidecar-sourced lyrics/art/chapters
	// outside the audio-change gate and records the new sidecar observations, in one
	// transaction. It returns whether anything changed and emits an item change_log
	// delta when it does. It never touches the audio file row or entity resolution.
	UpdateItemSidecars(ctx context.Context, in SidecarUpdate) (bool, error)
	// UpdateFileStateIfUnchanged updates a file's size/mtime/content_hash only when
	// its stored size and mtime still match the expected values (optimistic
	// concurrency), so an on-disk tag write can record its own result without
	// clobbering a concurrent scan/move. Returns whether the row was updated.
	UpdateFileStateIfUnchanged(ctx context.Context, in FileStateUpdate) (bool, error)
	// PutFileDiagnostics replaces one writer's diagnostics for a file. Each writer
	// owns its own origin's rows, so writers never clear each other's findings and a
	// clean re-run clears the writer's own stale rows. It is keyed by FilePID because
	// a path-keyed call would be order-sensitive against organize's retag-then-move
	// window, while file_diagnostic is keyed by file_id and follows a move for free.
	PutFileDiagnostics(ctx context.Context, filePID PID, origin DiagnosticOrigin, ds []FileDiagnostic) error

	// IsFieldLocked reports whether a metadata field is locked, so a writer (organize
	// tag write-back, enrichment) skips it and curated data survives.
	IsFieldLocked(ctx context.Context, itemPID PID, field string) (bool, error)
	// LockedFields returns an item's locked fields in one query, so a writer checking
	// several fields avoids a per-field round trip.
	LockedFields(ctx context.Context, itemPID PID) (map[string]bool, error)
	// SetFieldProvenance records that a field was set by a non-tag source (e.g.
	// organize). It refuses a locked field unless force is set.
	SetFieldProvenance(ctx context.Context, itemPID PID, field string, source ProvenanceSource, value string, force bool) error

	// QueryItems/CountItems evaluate q against the item whitelist. If q references a
	// per-user field such as starred, rating, or play_count, it is scoped to userPID's
	// play_state (empty selects the default user). A query with no user-state field is
	// not scoped by user.
	QueryItems(ctx context.Context, q query.Query, userPID PID) ([]*ItemView, error)
	CountItems(ctx context.Context, q query.Query, userPID PID) (int, error)
	ItemByPID(ctx context.Context, pid PID) (*ItemView, error)
	// ItemFiles returns every file backing an item in reading order (one for a
	// track or single-file book, all parts for a multi-file book).
	ItemFiles(ctx context.Context, pid PID) ([]ItemFileRef, error)

	// Two-phase organize journaling: PlanMove records a 'planned' organize_journal
	// row before the on-disk move (returning its journal pid); CommitMove updates
	// the file's path and marks the row 'committed'; AbortMove marks it
	// 'rolled_back' when the move fails. This leaves a recoverable trail even if a
	// crash interleaves the rename and the catalog update.
	PlanMove(ctx context.Context, in RelocateInput) (PID, error)
	CommitMove(ctx context.Context, journalPID PID, in RelocateInput) error
	AbortMove(ctx context.Context, journalPID PID) error

	ChangesSince(ctx context.Context, seq int64) ([]Change, error)
	LatestChangeSeq(ctx context.Context) (int64, error)

	// RefreshRollups recomputes the maintained catalog-structural rollups
	// (per artist/release_group/genre) from the base tables. Normal scans maintain
	// touched rows transactionally; this is the repair path for db verify drift.
	RefreshRollups(ctx context.Context) error
}

// JobStore is the persistence port for jobs and leases.
type JobStore interface {
	// AcquireLease inserts a lease for lease.Scope, returning false (without
	// error) if the scope is already held in-process.
	AcquireLease(ctx context.Context, lease *Lease) (bool, error)
	RenewLease(ctx context.Context, scope, owner string, ts int64) error
	ReleaseLease(ctx context.Context, scope, owner string) error

	CreateJob(ctx context.Context, j *Job) error // assigns ID + PID
	UpdateJob(ctx context.Context, j *Job) error
	Heartbeat(ctx context.Context, jobID, ts int64, progress float64, msg string) error
	ListJobs(ctx context.Context, limit int) ([]*Job, error)
	JobByPID(ctx context.Context, pid PID) (*Job, error)

	// ReclaimOrphans is called on Open while the caller holds the exclusive
	// write flock: any still-running job/lease belongs to a dead prior owner, so
	// mark those jobs crashed and drop their leases. Returns the count reclaimed.
	ReclaimOrphans(ctx context.Context, ts int64) (int, error)
}
