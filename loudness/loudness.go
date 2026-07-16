// Package loudness models EBU R128 / ReplayGain 2.0 loudness for the catalog:
// the Result a track's measurement is stored as, the reference level its gain is
// computed against, and the conversion from a measured dB domain into that
// Result.
//
// The measurement itself belongs to WaxFlow, whose BS.1770 meter rides the
// analyze pass's single decode. This package deliberately owns no BS.1770
// implementation of its own: two implementations that can disagree is a
// liability, and the measuring one is conformance-tested.
package loudness

import "math"

// ReferenceLUFS is the ReplayGain 2.0 / EBU R128 reference loudness. Track gain
// is the dB adjustment that would bring a track's integrated loudness to it.
const ReferenceLUFS = -18.0

// AnalysisVersion identifies the loudness algorithm. The analyze pass keys its
// per-file stamp on the combined analyze version; bumping this is one reason to
// force re-analysis.
const AnalysisVersion = 1

// Result is a track's measured loudness.
type Result struct {
	IntegratedLUFS float64 // EBU R128 integrated loudness (LUFS)
	SamplePeak     float64 // linear peak amplitude (1.0 == full scale)
	Valid          bool    // false when the track is silent/too short to gate
}

// TrackGainDB returns the ReplayGain track gain in dB for an integrated loudness:
// the adjustment that brings the track to the reference loudness.
func TrackGainDB(integratedLUFS float64) float64 { return ReferenceLUFS - integratedLUFS }

// FromMeasurement builds a Result from a whole-file measurement in the dB
// domain: integrated loudness in LUFS and the maximum sample magnitude in dBFS.
// It takes plain floats rather than a decoder type so this package stays
// independent of the one that produces them.
//
// Material that never passes the absolute gate (silence, or a track too short to
// form one) measures as math.Inf(-1), which reads as unmeasured: Valid stays
// false and the caller stores nothing. The peak is converted independently of
// the gate, so it is still present on an invalid Result.
//
// Note this is a sample peak, not a true peak. The distinction is why the
// conversion lives here rather than at the call site: an oversampled true peak
// exceeds full scale on material a sample peak reports at 1.0, and the catalog's
// track_peak column means the latter.
func FromMeasurement(integratedLUFS, samplePeakDB float64) Result {
	r := Result{SamplePeak: peakFromDB(samplePeakDB)}
	// A non-finite loudness of any kind has no usable gain: -Inf is the gate
	// reporting silence, and +Inf/NaN would poison TrackGainDB and every value
	// derived from it downstream. Both read as unmeasured rather than stored.
	if math.IsInf(integratedLUFS, 0) || math.IsNaN(integratedLUFS) {
		return r
	}
	r.IntegratedLUFS = integratedLUFS
	r.Valid = true
	return r
}

// peakFromDB converts a dBFS peak to the linear amplitude the catalog stores. Any
// non-finite input maps to 0: silence reports -Inf (zero amplitude, which dbToLin
// would also yield), while +Inf and NaN are nonsensical for a sample peak and would
// otherwise store a value (notably +Inf) that breaks JSON marshaling downstream.
func peakFromDB(db float64) float64 {
	if math.IsInf(db, 0) || math.IsNaN(db) {
		return 0
	}
	return dbToLin(db)
}

// dbToLin converts dBFS to a linear amplitude.
func dbToLin(db float64) float64 { return math.Pow(10, db/20) }
