package model

// OrganizeMove is one committed move from an organize job's journal, as an undo reads it:
// where the job found the file and where it put it, and the file as the catalog holds it
// now. FilePID, ItemPID, Path and Root are empty when the catalog no longer holds the file,
// and for a companion step, which names no file.
type OrganizeMove struct {
	Kind    JournalKind
	FilePID PID
	ItemPID PID
	Src     []byte // where the job found the file
	Dst     []byte // where the job put it
	Path    []byte // where the catalog has the file now
	Root    []byte // the root of the library holding it
}

// JournalKind is what an organize journal row records: a file the job moved, or a step it
// took with a folder companion (a cover, a booklet) after its moves, which an undo takes
// back exactly.
type JournalKind string

const (
	JournalFile          JournalKind = "file"           // an audio file moved from Src to Dst
	JournalCompanion     JournalKind = "companion"      // a companion moved from Src to Dst
	JournalCompanionCopy JournalKind = "companion_copy" // a companion at Src copied to Dst
	JournalCompanionDrop JournalKind = "companion_drop" // the copy at Dst of the one at Src removed
)

// CompanionMove is one companion step an organize job took, as its journal records it.
type CompanionMove struct {
	Kind     JournalKind
	Src, Dst []byte
}

// OrganizeBatch is one job's organize journal: the job, and what its moves came to.
// Planned counts moves a crash left between the disk and the catalog, which the next
// read-write open settles.
type OrganizeBatch struct {
	JobPID     PID
	Kind       string // organize or organize-undo
	State      JobState
	StartedAt  int64 // unix nanoseconds
	Committed  int
	RolledBack int
	Planned    int
}
