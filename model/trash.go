package model

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/internal/pathx"
)

// TrashDirName is the per-library directory that holds trashed files. It lives
// under the library root so a trash move is same-volume (atomic); the scanner
// skips it so trashed files are never re-cataloged.
const TrashDirName = ".waxbin-trash"

var trashFold = pathx.FoldName(TrashDirName)

// IsTrashName reports whether a folder name is the trash folder's, as a filesystem that
// ignores case and Unicode form reads it. That is the rule on every platform, as it is
// for the layout: a library stays whole wherever it is moved.
func IsTrashName(name string) bool { return pathx.FoldName(name) == trashFold }

// InTrash reports whether a path relative to its library root is the trash folder or
// lies below it.
func InTrash(rel string) bool {
	return slices.ContainsFunc(strings.Split(rel, string(filepath.Separator)), IsTrashName)
}

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

// Reason is the trash journal's default text for a deletion, what `trash list` shows
// when the caller gave none ("user" for a trash delete). ArchiveReason is what the
// delete archives an item as.
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

// ArchiveReason is what a delete in this mode archives an item as when it takes the
// item's last file (ArchiveTrash for a trash delete). Reason is the journal's text.
func (m DeleteMode) ArchiveReason() ArchiveReason {
	switch m {
	case DeletePrune:
		return ArchivePrune
	case DeletePermanent:
		return ArchivePermanent
	default:
		return ArchiveTrash
	}
}

// TrashEntry is one record in the trash undo journal.
type TrashEntry struct {
	PID PID
	// ItemPID is the item the journal reports for the file, the first track of a rip. A
	// file backing several items restores all of them, and RestorableTrash lists the entry
	// under each, so match an entry to an item by that key or through Items, never by this
	// field.
	ItemPID      PID
	OrigPath     []byte
	OrigDisplay  string
	TrashPath    []byte
	TrashDisplay string
	Reason       string
	Size         int64
	TrashedAt    int64 // unix nanoseconds
	RestoredAt   int64 // 0 = still in the trash
	// LibraryPID is the library the file was trashed from, empty when the journal row
	// records none.
	LibraryPID PID
	// EssenceHash is the file's audio as its row read when the trash took it (what
	// File.EssenceHash answered then, the catalog's last reading rather than a hash taken
	// at the trash), empty when the row had none.
	EssenceHash string
	// Items is every item the file backed, each with the window this file gave it, in
	// window order: one for a track or a book part, each track of a rip. It is empty for a
	// file that backed no item, and a pid in it may no longer name an item.
	Items []TrashItem
}

// Item returns what the entry records for one of the items its file backed.
func (e TrashEntry) Item(pid PID) (TrashItem, bool) {
	for _, it := range e.Items {
		if it.ItemPID == pid {
			return it, true
		}
	}
	return TrashItem{}, false
}

// TrashItem is one item a trashed file backed and the part of the file it played. Virtual
// and the frames read as ItemView's do: a whole-file item has none, and EndFrames is 0 for
// a window running to the file's end. The window is the trashed file's own, so for a copy
// of a rip it is the copy's sheet's, which the item's view does not show while the copy
// is an alternate.
type TrashItem struct {
	ItemPID     PID
	Virtual     bool
	StartFrames int64
	EndFrames   int64
}

// TrashFileInput records a file that was moved into the trash on disk so the
// store can drop its catalog row, archive its now-fileless item, and write the
// undo journal row in one transaction. PID names the pending row the move was planned
// under (TrashPending), which the journal row takes over; empty mints a pid.
type TrashFileInput struct {
	PID          PID
	FilePID      PID
	Reason       string
	TrashPath    []byte
	TrashDisplay string
}

// TrashPending is a trash move recorded before the file moves, so an open after a crash
// between the move and its catalog write can finish it or drop it. The store fills in
// what the journal row needs from the file's row as it stands. PID becomes the journal
// row's pid, and JobPID names the run, when it has one.
type TrashPending struct {
	PID          PID
	FilePID      PID
	Reason       string
	TrashPath    []byte
	TrashDisplay string
	JobPID       PID
}

// PromotedFile is an alternate that took the place of a file its item lost. The caller
// re-reads it, so the item's metadata follows the tags of the file that now owns it.
type PromotedFile struct {
	FilePID   PID
	LibraryID int64
	Path      []byte
}

// DetachResult is what dropping a file's row did beyond the drop: the trash entry it
// recorded (empty for a detach that keeps no journal) and the alternates promoted in
// the file's place.
type DetachResult struct {
	TrashPID PID
	Promoted []PromotedFile
}

// MissingResult is what reconciling vanished files did: Marked items went missing with
// every file gone, Promoted alternates took the place of a gone primary or part, and
// Dropped gone files lost their rows with nothing taking their place (an alternate, or a
// row no item claimed).
type MissingResult struct {
	Marked   int
	Promoted []PromotedFile
	Dropped  int
}
