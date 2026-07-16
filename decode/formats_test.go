package decode

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
)

// codecFixtures maps each codec ID Coverage() can report to the EncodeAs format
// that produces a decodable fixture for it. aiff is absent because it is not a
// codec; it is a container carrying pcm, exercised here through wav.
var codecFixtures = map[string]string{
	"pcm":    "wav",
	"flac":   "flac",
	"mp3":    "mp3",
	"alac":   "alac",
	"aac-lc": "aac",
	"vorbis": "vorbis",
	"opus":   "opus",
}

// TestCoverageDecodesEveryCodec is the honesty check with teeth: every codec
// Coverage() claims as decodable must actually decode a real fixture. A WaxFlow
// codec rename, or a new codec ID with no fixture, fails here loudly rather than
// mislabeling doctor's coverage table.
func TestCoverageDecodesEveryCodec(t *testing.T) {
	const rate = 44100
	sig := testaudio.ReferenceSignal(rate, 3*time.Second)
	eng := New(nil)
	dir := t.TempDir()
	for _, fs := range Coverage() {
		format, ok := codecFixtures[fs.Codec]
		if !ok {
			t.Errorf("Coverage reports codec %q with no test fixture; add one or it is unverified", fs.Codec)
			continue
		}
		p := filepath.Join(dir, fs.Codec)
		if err := os.WriteFile(p, testaudio.EncodeAs(t, format, "", rate, sig), 0o644); err != nil {
			t.Fatal(err)
		}
		pcm, err := eng.Mono(context.Background(), p, 11025, 120*time.Second)
		if err != nil {
			t.Errorf("codec %q (fixture format %q) does not decode: %v", fs.Codec, format, err)
			continue
		}
		if pcm.Frames() == 0 {
			t.Errorf("codec %q decoded to zero frames", fs.Codec)
		}
	}
}

// TestFormatLoudnessParity: from one signal, the lossless formats decode to
// identical PCM and so measure bit-exact-equal, while the lossy ones perturb the
// signal and so measure only within tolerance. Splitting the assertion this way
// matters: "identical LUFS across all eight" would be wrong for the lossy half.
func TestFormatLoudnessParity(t *testing.T) {
	const rate = 44100
	sig := testaudio.ReferenceSignal(rate, 4*time.Second)
	eng := New(nil)
	dir := t.TempDir()
	measure := func(format string) *Measurement {
		p := filepath.Join(dir, format)
		if err := os.WriteFile(p, testaudio.EncodeAs(t, format, "", rate, sig), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := eng.Measure(context.Background(), p, nil)
		if err != nil {
			t.Fatalf("%s: measure: %v", format, err)
		}
		return m
	}
	ref := measure("wav")
	for _, f := range []string{"aiff", "flac", "alac"} {
		m := measure(f)
		if m.IntegratedLUFS != ref.IntegratedLUFS || m.SamplePeakDB != ref.SamplePeakDB {
			t.Errorf("lossless %s: {LUFS %.6f, peak %.6f} != wav {LUFS %.6f, peak %.6f} (should be bit-exact)",
				f, m.IntegratedLUFS, m.SamplePeakDB, ref.IntegratedLUFS, ref.SamplePeakDB)
		}
	}
	for _, f := range []string{"mp3", "aac", "opus", "vorbis"} {
		m := measure(f)
		if math.Abs(m.IntegratedLUFS-ref.IntegratedLUFS) > 2 {
			t.Errorf("lossy %s: LUFS %.2f is not within 2 LU of wav's %.2f", f, m.IntegratedLUFS, ref.IntegratedLUFS)
		}
	}
}
