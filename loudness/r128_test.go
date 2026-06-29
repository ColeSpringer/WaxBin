package loudness

import (
	"math"
	"testing"

	"github.com/colespringer/waxbin/decode"
)

// sine builds a mono PCM sine of the given amplitude and duration.
func sine(rate int, freq, amp, durSec float64) *decode.PCM {
	n := int(durSec * float64(rate))
	s := make([]float32, n)
	for i := range s {
		s[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return &decode.PCM{SampleRate: rate, Channels: 1, Samples: s}
}

func TestR128Silence(t *testing.T) {
	silent := &decode.PCM{SampleRate: 44100, Channels: 1, Samples: make([]float32, 44100*2)}
	if r := R128(silent); r.Valid {
		t.Errorf("silence measured as valid loudness: %+v", r)
	}
}

func TestR128PlausibleAndMonotonic(t *testing.T) {
	loud := R128(sine(44100, 1000, 0.5, 3))
	quiet := R128(sine(44100, 1000, 0.05, 3))
	if !loud.Valid || !quiet.Valid {
		t.Fatalf("expected valid measurements, got loud=%+v quiet=%+v", loud, quiet)
	}
	// A half-scale 1 kHz sine sits in a sane LUFS range (not silence, not absurd).
	if loud.IntegratedLUFS < -30 || loud.IntegratedLUFS > 0 {
		t.Errorf("integrated loudness %.2f LUFS outside a plausible range", loud.IntegratedLUFS)
	}
	// A 20 dB quieter signal must measure quieter.
	if quiet.IntegratedLUFS >= loud.IntegratedLUFS {
		t.Errorf("quieter signal measured louder: quiet=%.2f loud=%.2f", quiet.IntegratedLUFS, loud.IntegratedLUFS)
	}
	// ~20 dB amplitude drop should move loudness by roughly that much.
	if d := loud.IntegratedLUFS - quiet.IntegratedLUFS; d < 15 || d > 25 {
		t.Errorf("loudness delta %.1f dB, want ~20 for a 10x amplitude drop", d)
	}
	if loud.SamplePeak < 0.49 || loud.SamplePeak > 0.51 {
		t.Errorf("sample peak %.3f, want ~0.5", loud.SamplePeak)
	}
}

func TestResampleUpsampleInterpolates(t *testing.T) {
	// Upsampling a ramp must interpolate between samples, not duplicate them: the
	// output should be (weakly) monotone with intermediate values, and never just
	// repeat an input sample at a fractional position.
	in := []float64{0, 1, 2, 3, 4}
	out := resampleTo(in, 4, 8) // ratio 0.5: ~2x as many samples
	if len(out) < len(in) {
		t.Fatalf("upsample produced %d samples, want >= %d", len(out), len(in))
	}
	for i := 1; i < len(out); i++ {
		if out[i] < out[i-1]-1e-9 {
			t.Fatalf("upsampled ramp not monotone at %d: %v", i, out)
		}
	}
	// A midpoint sample should be a true interpolation (e.g. ~0.5), not 0 or 1.
	mid := out[1]
	if mid <= 0 || mid >= 1 {
		t.Errorf("interpolated sample out[1] = %.3f, want strictly between in[0]=0 and in[1]=1", mid)
	}
}

func TestResampleDownsampleAverages(t *testing.T) {
	// Downsampling still averages windows (anti-aliasing), unchanged behavior.
	in := []float64{0, 2, 0, 2, 0, 2, 0, 2}
	out := resampleTo(in, 8, 4) // ratio 2: average pairs -> all 1.0
	for i, v := range out {
		if v < 0.9 || v > 1.1 {
			t.Errorf("downsampled out[%d] = %.3f, want ~1.0 (pair average)", i, v)
		}
	}
}

func TestTrackGainDB(t *testing.T) {
	// A track measured at the reference loudness needs no gain.
	if g := TrackGainDB(ReferenceLUFS); math.Abs(g) > 1e-9 {
		t.Errorf("gain at reference = %.4f, want 0", g)
	}
	// A track 6 dB louder than reference needs -6 dB.
	if g := TrackGainDB(ReferenceLUFS + 6); math.Abs(g+6) > 1e-9 {
		t.Errorf("gain = %.4f, want -6", g)
	}
}
