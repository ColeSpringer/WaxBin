package model

// TrashDirName is the per-library directory that holds trashed files. It lives
// under the library root so a trash move is same-volume (atomic); the scanner
// skips it so trashed files are never re-cataloged.
const TrashDirName = ".waxbin-trash"

// DeleteMode is the deletion policy for a file-backed item. User deletes default
// to the reversible trash; pruning and explicit permanent deletes bypass it to
// reclaim space. Every mode preserves the logical item, archiving it when it
// loses its last file.
type DeleteMode string

const (
	// DeleteTrash moves files to the same-volume trash with an undo journal.
	DeleteTrash DeleteMode = "trash"
	// DeletePrune bypasses the trash to reclaim space (retention/dedup policy).
	DeletePrune DeleteMode = "prune"
	// DeletePermanent bypasses the trash on an explicit user request.
	DeletePermanent DeleteMode = "permanent"
)

// Valid reports whether m is a known mode.
func (m DeleteMode) Valid() bool {
	return m == DeleteTrash || m == DeletePrune || m == DeletePermanent
}

// BypassesTrash reports whether the mode deletes from disk directly (no undo).
func (m DeleteMode) BypassesTrash() bool {
	return m == DeletePrune || m == DeletePermanent
}

// Reason is the recorded provenance of a deletion.
func (m DeleteMode) Reason() string {
	switch m {
	case DeletePrune:
		return "prune"
	case DeletePermanent:
		return "permanent"
	default:
		return "user"
	}
}

// TrashEntry is one record in the trash undo journal.
type TrashEntry struct {
	PID          PID
	ItemPID      PID
	OrigPath     []byte
	OrigDisplay  string
	TrashPath    []byte
	TrashDisplay string
	Reason       string
	Size         int64
	TrashedAt    int64 // unix nanoseconds
	RestoredAt   int64 // 0 = still in the trash
}

// TrashFileInput records a file that was moved into the trash on disk so the
// store can drop its catalog row, archive its now-fileless item, and write the
// undo journal row in one transaction.
type TrashFileInput struct {
	FilePID      PID
	Reason       string
	TrashPath    []byte
	TrashDisplay string
}
