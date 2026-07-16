package loudness

import (
	"math"
	"testing"
)

func TestFromMeasurement(t *testing.T) {
	r := FromMeasurement(-23.0, -6.0)
	if !r.Valid {
		t.Fatal("a gated measurement must be Valid")
	}
	if r.IntegratedLUFS != -23.0 {
		t.Errorf("IntegratedLUFS = %v, want -23", r.IntegratedLUFS)
	}
	// -6 dBFS is half amplitude, near enough.
	if math.Abs(r.SamplePeak-0.5011872336) > 1e-9 {
		t.Errorf("SamplePeak = %v, want ~0.5012 for -6 dBFS", r.SamplePeak)
	}
}

func TestFromMeasurementFullScalePeak(t *testing.T) {
	// 0 dBFS is exactly full scale, the value the catalog stores as 1.0.
	if r := FromMeasurement(-18.0, 0); math.Abs(r.SamplePeak-1.0) > 1e-12 {
		t.Errorf("SamplePeak = %v, want 1.0 for 0 dBFS", r.SamplePeak)
	}
}

func TestFromMeasurementSilence(t *testing.T) {
	// Silence never passes the absolute gate: both fields report -Inf.
	r := FromMeasurement(math.Inf(-1), math.Inf(-1))
	if r.Valid {
		t.Error("silence must not be Valid")
	}
	if r.SamplePeak != 0 {
		t.Errorf("SamplePeak = %v, want 0 for silence", r.SamplePeak)
	}
}

func TestFromMeasurementNonFiniteLoudnessIsInvalid(t *testing.T) {
	// Guard the values that would otherwise poison TrackGainDB and everything
	// derived from it.
	for _, lufs := range []float64{math.Inf(-1), math.Inf(1), math.NaN()} {
		if r := FromMeasurement(lufs, -3.0); r.Valid {
			t.Errorf("FromMeasurement(%v, -3) is Valid, want invalid", lufs)
		}
	}
}

func TestFromMeasurementPeakSurvivesInvalidLoudness(t *testing.T) {
	// The peak is measured independently of the gate, so an ungated track still
	// carries one.
	r := FromMeasurement(math.Inf(-1), -6.0)
	if r.Valid {
		t.Fatal("want invalid")
	}
	if math.Abs(r.SamplePeak-0.5011872336) > 1e-9 {
		t.Errorf("SamplePeak = %v, want the peak to survive an ungated measurement", r.SamplePeak)
	}
}

func TestFromMeasurementNonFinitePeak(t *testing.T) {
	// A non-finite sample peak (notably +Inf) must map to 0, not flow through to a
	// stored TrackPeak that breaks JSON marshaling. Loudness stays finite, so the
	// result is otherwise a valid measurement.
	for _, peak := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		r := FromMeasurement(-18.0, peak)
		if r.SamplePeak != 0 {
			t.Errorf("FromMeasurement(-18, %v).SamplePeak = %v, want 0", peak, r.SamplePeak)
		}
		if !r.Valid {
			t.Errorf("FromMeasurement(-18, %v).Valid = false, want true (loudness is finite)", peak)
		}
	}
}

func TestTrackGainDB(t *testing.T) {
	// A track quieter than the reference gets a positive gain.
	if g := TrackGainDB(-23.0); math.Abs(g-5.0) > 1e-12 {
		t.Errorf("TrackGainDB(-23) = %v, want 5", g)
	}
	// A track at the reference needs none.
	if g := TrackGainDB(ReferenceLUFS); g != 0 {
		t.Errorf("TrackGainDB(ReferenceLUFS) = %v, want 0", g)
	}
}
