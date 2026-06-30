package model

// DupPolicy is how an import treats a file whose audio essence already exists in
// the catalog.
type DupPolicy string

const (
	// DupSkip leaves a duplicate in the inbox and does not import it (the default).
	DupSkip DupPolicy = "skip"
	// DupAllow imports a duplicate as a separate copy (the store keeps both file
	// rows; exact-hash dedup is the scanner's job, not the importer's).
	DupAllow DupPolicy = "allow"
)

// Valid reports whether p is a known policy.
func (p DupPolicy) Valid() bool { return p == DupSkip || p == DupAllow }

// ImportBatchState is the lifecycle of an import batch.
type ImportBatchState string

const (
	ImportRunning ImportBatchState = "running"
	ImportDone    ImportBatchState = "done"
	ImportFailed  ImportBatchState = "failed"
)

// ImportBatch records one import of a staging folder into a managed library, with
// source attribution and per-outcome tallies for later review.
type ImportBatch struct {
	ID          int64
	PID         PID
	Source      string
	LibraryID   int64
	State       ImportBatchState
	Imported    int
	Duplicates  int
	Quarantined int
	Errored     int
	Bytes       int64
	StartedAt   int64 // unix nanoseconds
	FinishedAt  int64 // unix nanoseconds, 0 while running
}
