package model

import "github.com/colespringer/waxbin/waxerr"

// JobState is the lifecycle state of a job row.
type JobState string

const (
	JobRunning  JobState = "running"
	JobDone     JobState = "done"
	JobFailed   JobState = "failed"
	JobCrashed  JobState = "crashed" // owner died holding it (reclaimed on Open)
	JobCanceled JobState = "canceled"
)

// FSMutateScope is the lease scope every job that scans a library or moves its files runs
// under, so a reader of the lease table can tell whether one is running.
const FSMutateScope = "fs-mutate"

// Job is a unit of mutating background work, such as scan, organize, or
// analysis.
// Heartbeats prove liveness; flock-based reclaim on Open turns orphaned running
// jobs into crashed without PID checks.
type Job struct {
	ID       int64
	PID      PID
	Kind     string // "scan", "organize", ...
	Scope    string // lease scope this job ran under
	State    JobState
	Owner    string  // write-owner identity that created the job
	Progress float64 // 0..1
	Message  string
	Error    string
	// ErrorCode and ErrorReason are the class and reason of the error that ended a failed
	// or canceled job, so a client tailing the row answers as the run would have; empty
	// for a job that finished, and for one reclaimed after a crash (internal).
	ErrorCode   waxerr.Code
	ErrorReason waxerr.Reason
	// Params is what the job was asked to do, as JSON, written when it starts; empty for a
	// job that records none.
	Params string
	// Result is a JSON summary a job records for a client tailing it (a server-run
	// scan/analyze/enrich/organize the client did not run in-process), empty for a job
	// that records none. A job that checkpoints (a kind change) holds its result so far
	// while it runs, and one reclaimed after a crash keeps its last checkpoint.
	Result string
	// TargetType and TargetPID name what a targeted job ran on: item, artist,
	// release_group or album for a scoped enrichment, library for a single-library scan,
	// trash for one entry's restore or purge. Both are empty for a whole-catalog pass.
	TargetType  string
	TargetPID   PID
	StartedAt   int64 // unix nanoseconds
	HeartbeatAt int64 // unix nanoseconds
	FinishedAt  int64 // unix nanoseconds, 0 while running
}

// Lease is a scoped advisory lock ensuring at most one mutating job per scope.
type Lease struct {
	Scope       string
	Owner       string
	JobID       int64
	AcquiredAt  int64 // unix nanoseconds
	HeartbeatAt int64 // unix nanoseconds
}
