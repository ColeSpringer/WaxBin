package decode

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
)

// TestProbeReadsHeaderProperties pins what the scanner leans on: real properties
// for a container no tag parser reads, from the header alone.
func TestProbeReadsHeaderProperties(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	p := filepath.Join(dir, "a.wv")
	if err := os.WriteFile(p, testaudio.EncodeAs(t, "wavpack", "", rate,
		testaudio.ReferenceSignal(rate, 2*time.Second)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Probe(context.Background(), p)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got.SampleRate != rate || got.Channels != 1 || got.BitDepth != 16 {
		t.Errorf("Probe = {rate:%d channels:%d depth:%d}, want {%d 1 16}",
			got.SampleRate, got.Channels, got.BitDepth, rate)
	}
	if got.DurationMS < 1900 || got.DurationMS > 2100 {
		t.Errorf("durationMS = %d, want about 2000", got.DurationMS)
	}
	if got.Bitrate <= 0 {
		t.Errorf("bitrate = %d, want a real kbps figure", got.Bitrate)
	}
}

// TestProbeOmitsBitDepthWithoutOne: a codec that decodes to float stores no sample
// width, and the format's BitDepth is then the pipeline's own 32. Reporting that
// would put a number in the catalog the file does not hold, and rank a WMA above an
// MP3 in the upgrade scan on the strength of it.
func TestProbeOmitsBitDepthWithoutOne(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mono-8k.wma")
	if err := os.WriteFile(p, testaudio.Fixture(t, "mono-8k.wma"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Probe(context.Background(), p)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got.BitDepth != 0 {
		t.Errorf("bitDepth = %d, want 0 for a float-decoding codec", got.BitDepth)
	}
	if got.SampleRate != 8000 || got.Channels != 1 {
		t.Errorf("Probe = {rate:%d channels:%d}, want {8000 1}", got.SampleRate, got.Channels)
	}
}

// TestProbeUnsupportedInput: an input this build cannot open reports
// ErrUnsupported, which is how the scanner knows to leave the row's zeroes alone
// rather than write something invented.
func TestProbeUnsupportedInput(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mystery.wv")
	if err := os.WriteFile(p, []byte("not a container at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Probe(context.Background(), p); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Probe error = %v, want ErrUnsupported", err)
	}
}

// TestProbeDurationBounds pins the arithmetic on the untrusted header value: a
// sample count that would overflow the millisecond multiply, or claim more than
// the plausibility bound, reports no duration at all rather than a poisoned one.
func TestProbeDurationBounds(t *testing.T) {
	if got := probeDuration(8000*3, 8000); got != 3000 {
		t.Errorf("plain conversion = %d, want 3000", got)
	}
	if got := probeDuration(math.MaxInt64/500, 44100); got != 0 {
		t.Errorf("overflowing claim = %d, want 0", got)
	}
	if got := probeDuration(1e12, 44100); got != 0 {
		t.Errorf("implausible claim = %d, want 0", got)
	}
	if got := probeDuration(-1, 44100); got != 0 {
		t.Errorf("negative claim = %d, want 0", got)
	}
}
