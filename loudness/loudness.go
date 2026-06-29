// Package loudness computes EBU R128 / ReplayGain 2.0 loudness. It exposes one
// Result for two implementations: ffmpeg's whole-file ebur128 filter when
// available, and a pure-Go ITU-R BS.1770 measurement over decoded PCM as the
// fallback. The analyze pass chooses the implementation.
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

// dbToLin converts dBFS to a linear amplitude (used to render ffmpeg's dBFS peak).
func dbToLin(db float64) float64 { return math.Pow(10, db/20) }
