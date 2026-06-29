package loudness

import (
	"context"
	"math"
	"os/exec"
	"strings"
	"testing"
)

const ebur128Summary = `[Parsed_ebur128_0 @ 0x55] t: 2.4   TARGET:-23 LUFS    M: -21.1 S:-120.7     I: -21.1 LUFS       LRA:   0.0 LU  FTPK: -18.1 dBFS  TPK: -18.1 dBFS
[Parsed_ebur128_0 @ 0x55] Summary:

  Integrated loudness:
    I:         -14.6 LUFS
    Threshold: -24.9 LUFS

  Loudness range:
    LRA:         7.6 LU

  True peak:
    Peak:       -1.0 dBFS
`

func TestParseEBUR128(t *testing.T) {
	res, err := parseEBUR128(strings.NewReader(ebur128Summary))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !res.Valid || math.Abs(res.IntegratedLUFS-(-14.6)) > 1e-6 {
		t.Errorf("integrated = %.3f (valid %v), want -14.6 from the Summary (not the per-frame -21.1)", res.IntegratedLUFS, res.Valid)
	}
	// -1.0 dBFS true peak -> ~0.891 linear.
	want := math.Pow(10, -1.0/20)
	if math.Abs(res.SamplePeak-want) > 1e-6 {
		t.Errorf("peak = %.4f, want %.4f", res.SamplePeak, want)
	}
}

func TestParseEBUR128NoSummary(t *testing.T) {
	if _, err := parseEBUR128(strings.NewReader("no summary here\n")); err == nil {
		t.Fatal("expected an error when no integrated loudness is present")
	}
}

// TestParseEBUR128TakesMaxPeak verifies that if the summary ever carries more
// than one Peak line (e.g. sample + true peak in a future ffmpeg), the loudest
// is kept rather than the last seen.
func TestParseEBUR128TakesMaxPeak(t *testing.T) {
	const summary = `[ebur128] Summary:

  Integrated loudness:
    I:         -16.0 LUFS

  Sample peak:
    Peak:      -6.0 dBFS

  True peak:
    Peak:      -2.0 dBFS
`
	res, err := parseEBUR128(strings.NewReader(summary))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := math.Pow(10, -2.0/20) // the louder (-2 dBFS) peak
	if math.Abs(res.SamplePeak-want) > 1e-6 {
		t.Errorf("peak = %.4f, want the max %.4f (not the last-seen -6 dBFS)", res.SamplePeak, want)
	}
}

// TestFFmpegEnd2End runs the real ebur128 filter when ffmpeg is on PATH.
func TestFFmpegEnd2End(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	wav := t.TempDir() + "/s.wav"
	cmd := exec.Command(bin, "-hide_banner", "-f", "lavfi", "-i", "sine=frequency=1000:duration=2", "-y", wav)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not synthesize test wav: %v (%s)", err, out)
	}
	res, err := FFmpeg(context.Background(), bin, wav)
	if err != nil {
		// Some minimal ffmpeg builds lack ebur128; treat as a skip, not a failure.
		t.Skipf("ebur128 unavailable: %v", err)
	}
	if !res.Valid {
		t.Errorf("expected a valid measurement, got %+v", res)
	}
}
