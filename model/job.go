package model

// JobState is the lifecycle state of a job row.
type JobState string

const (
	JobRunning  JobState = "running"
	JobDone     JobState = "done"
	JobFailed   JobState = "failed"
	JobCrashed  JobState = "crashed" // owner died holding it (reclaimed on Open)
	JobCanceled JobState = "canceled"
)

// Job is a unit of mutating background work, such as scan, organize, or
// analysis.
// Heartbeats prove liveness; flock-based reclaim on Open turns orphaned running
// jobs into crashed without PID checks.
type Job struct {
	ID          int64
	PID         PID
	Kind        string // "scan", "organize", ...
	Scope       string // lease scope this job ran under
	State       JobState
	Owner       string  // write-owner identity that created the job
	Progress    float64 // 0..1
	Message     string
	Error       string
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
