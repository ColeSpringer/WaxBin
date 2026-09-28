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
	AlgoVersion int    // the fingerprint algorithm (pure-Go vs Chromaprint); matches the query file's
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
//
// Frames and SampleRate are the span the buckets divide: bucket i covers frames
// [i*Frames/Buckets, (i+1)*Frames/Buckets), to within a quarter of a bucket. The rate
// is the decoded one the buckets were built at (48000 for every Opus file), which a
// lying header can make differ from ItemView.SampleRate. A cue window's start in CD
// frames lands at cdFrames*SampleRate/75 here, multiplied first since not every rate
// is a multiple of 75.
type PeaksData struct {
	Version     int
	Buckets     int
	Data        []byte
	EssenceHash string
	Frames      int64
	SampleRate  int
}

// DurationMS is the decoded length the waveform spans, or 0 with no rate.
func (p PeaksData) DurationMS() int64 {
	if p.SampleRate <= 0 {
		return 0
	}
	return p.Frames * 1000 / int64(p.SampleRate)
}

// ItemPeaks is one backing file's waveform within an item, carrying what a scrubber
// needs to place it on the item's timeline: which file it belongs to and its part
// position. A track has exactly one; a multi-file audiobook has one per part, which
// is why the per-item read exists at all, since the primary-file read answers for
// a single representative part only. Each part's Peaks carries its own span.
type ItemPeaks struct {
	FilePID  PID
	Position int
	Peaks    PeaksData
}

// AnalysisInput is one analyzed file's full result, written atomically: the
// fingerprint, optional loudness and peaks, and the combined AnalysisVersion
// stamped onto the file so the work is not repeated until the essence or an
// analysis algorithm changes. Nil loudness or peaks means that part was not
// measured this run, for example after a transient decode error.
type AnalysisInput struct {
	Fingerprint     FingerprintInput
	AnalysisVersion int
	Loudness        *LoudnessData
	Peaks           *PeaksData
	// MeasureCompleted says the measuring decode ran to the end of the file, whether
	// or not it produced anything to store. Silent and too-short material measures
	// fine and yields nil Loudness, which without this flag is indistinguishable from
	// a decode that fell over halfway. The store stamps the essence as the file's
	// measured_essence when this is set and clears the column when it is not, so the
	// retry predicate chases the second and leaves the first alone.
	MeasureCompleted bool
	// Diagnostics is what the measuring decode found wrong with the bytes, recorded
	// under OriginAnalyze for this essence, and Observed says whether it looked at them
	// at all. Only an observed decode replaces the prior verdict (an empty list clears
	// it); an unsupported input, an IO error or a cancel proves nothing about the audio,
	// though a verdict on an earlier essence goes either way. This differs from
	// MeasureCompleted on purpose: an unsupported input settles the measurement but
	// observed nothing.
	Diagnostics []FileDiagnostic
	Observed    bool
}
