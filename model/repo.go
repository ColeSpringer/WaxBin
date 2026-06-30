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
}

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
	// CoverArt is the book's cover image (embedded or directory), or nil.
	CoverArt *ArtImage
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
	FileByPath(ctx context.Context, path []byte) (*File, error)
	FileByEssence(ctx context.Context, essence string) (*File, error)

	QueryItems(ctx context.Context, q query.Query) ([]*ItemView, error)
	CountItems(ctx context.Context, q query.Query) (int, error)
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
