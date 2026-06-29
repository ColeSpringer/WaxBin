package model

// FingerprintInput is one analyzed file's fingerprint, written by the analyze
// pass. The store persists the vector, the min-hash terms, and stamps the file's
// analyzed_essence/analysis_version so the work is not repeated until the essence
// or the algorithm version changes.
type FingerprintInput struct {
	FilePID        PID
	EssenceHash    string
	AlgoVersion    int
	DurationBucket int64
	FP             []byte  // packed sub-fingerprint vector
	Terms          []int64 // min-hash index terms
}

// FingerprintCandidate is a possible alt-encoding match found via the inverted
// index: a file sharing min-hash terms with the query file, within its duration
// bucket. SharedTerms ranks candidates before full-vector verification; FP is the
// candidate's packed fingerprint, returned alongside so verification needs no
// extra per-candidate query.
type FingerprintCandidate struct {
	FilePID     PID
	ItemPID     PID // the item this file backs (so grouping yields items)
	SharedTerms int
	FP          []byte // packed sub-fingerprint vector for full verification
}
