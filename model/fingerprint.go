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

// LoudnessData is a file's measured EBU R128 loudness and the ReplayGain track
// gain derived from it. Album gain/peak are filled by a separate album-aware
// aggregation, not here.
type LoudnessData struct {
	IntegratedLUFS float64
	TrackGainDB    float64
	TrackPeak      float64 // linear peak amplitude
}

// Loudness is the read shape for an item's stored ReplayGain, including the
// album-aware fields filled by the album-gain aggregation. HasAlbum reports
// whether album_gain_db is set (the item belongs to an album with measured gain).
type Loudness struct {
	IntegratedLUFS float64
	TrackGainDB    float64
	TrackPeak      float64
	AlbumGainDB    float64
	AlbumPeak      float64
	HasAlbum       bool
}

// PeaksData is a file's packed waveform overview. EssenceHash is the audio it was
// computed from, so a read can drop a waveform left over from superseded audio.
type PeaksData struct {
	Version     int
	Buckets     int
	Data        []byte
	EssenceHash string
}

// AnalysisInput is one analyzed file's full result, written atomically: the
// fingerprint, optional loudness and peaks, and the combined AnalysisVersion
// stamped onto the file so the work is not repeated until the essence or an
// analysis algorithm changes. Nil loudness or peaks means that part was not
// measured this run, for example after a transient ffmpeg error.
type AnalysisInput struct {
	Fingerprint     FingerprintInput
	AnalysisVersion int
	Loudness        *LoudnessData
	Peaks           *PeaksData
}
