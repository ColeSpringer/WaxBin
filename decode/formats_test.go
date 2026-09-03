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

// codecFixture says how to get a decodable file for one codec: an EncodeAs format
// name where WaxFlow can encode the codec, or one of testaudio's checked-in fixtures
// where it only decodes it.
type codecFixture struct {
	format string
	file   string
}

// codecFixtures maps each codec ID Coverage() can report to its fixture. aiff is
// absent because it is not a codec; it is a container carrying pcm, exercised here
// through wav.
var codecFixtures = map[string]codecFixture{
	"pcm":     {format: "wav"},
	"flac":    {format: "flac"},
	"mp3":     {format: "mp3"},
	"alac":    {format: "alac"},
	"aac-lc":  {format: "aac"},
	"he-aac":  {format: "he-aac"},
	"vorbis":  {format: "vorbis"},
	"opus":    {format: "opus"},
	"wavpack": {format: "wavpack"},
	"ape":     {format: "ape"},
	// WaxFlow decodes WMA and Musepack but encodes neither; testaudio.Fixture says
	// where each checked-in file came from.
	"wma":      {file: "mono-8k.wma"},
	"musepack": {file: "ref-2s-sv8-chapters.mpc"},
}

// path returns a decodable file for the fixture, encoding one into dir when the
// codec has an encoder.
func (fx codecFixture) path(tb testing.TB, dir, name string, rate int, sig []float32) string {
	tb.Helper()
	data := testaudio.Fixture
	if fx.format != "" {
		data = func(tb testing.TB, _ string) []byte { return testaudio.EncodeAs(tb, fx.format, "", rate, sig) }
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data(tb, fx.file), 0o644); err != nil {
		tb.Fatal(err)
	}
	return p
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
		fx, ok := codecFixtures[fs.Codec]
		if !ok {
			t.Errorf("Coverage reports codec %q with no test fixture; add one or it is unverified", fs.Codec)
			continue
		}
		p := fx.path(t, dir, fs.Codec, rate, sig)
		pcm, err := eng.Mono(context.Background(), p, 11025, 120*time.Second)
		if err != nil {
			t.Errorf("codec %q (fixture %q) does not decode: %v", fs.Codec, filepath.Base(p), err)
			continue
		}
		if pcm.Frames() == 0 {
			t.Errorf("codec %q decoded to zero frames", fs.Codec)
		}
	}
}

// TestMusepackStreamVersionsDecode: the SV7 frame stream and the SV8 packet stream
// are separate demuxer paths upstream behind the one "musepack" codec the coverage
// table names, so both fixtures decode here. Each is compared with the WAV of the
// same signal by the level of its mono decode, which does not care that mppenc
// wrote the mono source as two channels; matching within a decibel says the decode
// is right rather than merely non-empty.
func TestMusepackStreamVersionsDecode(t *testing.T) {
	const rate = 44100
	eng := New(nil)
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	level := func(path string) float64 {
		pcm, err := eng.Mono(context.Background(), path, 11025, 0)
		if err != nil {
			t.Fatalf("%s: decode: %v", filepath.Base(path), err)
		}
		if pcm.Frames() == 0 {
			t.Fatalf("%s: decoded to no frames", filepath.Base(path))
		}
		var sum float64
		for _, v := range pcm.Samples {
			sum += float64(v) * float64(v)
		}
		return 10 * math.Log10(sum/float64(len(pcm.Samples)))
	}
	ref := level(write("ref.wav", testaudio.EncodeAs(t, "wav", "", rate, testaudio.ReferenceSignal(rate, 2*time.Second))))
	for _, name := range []string{"ref-2s-sv7.mpc", "ref-2s-sv8-chapters.mpc"} {
		if got := level(write(name, testaudio.Fixture(t, name))); math.Abs(got-ref) > 1 {
			t.Errorf("%s decodes at %.2f dB, want within 1 dB of the wav's %.2f", name, got, ref)
		}
	}
}

// TestFormatLoudnessParity: from one signal, the lossless formats decode to
// identical PCM and so measure bit-exact-equal, while the lossy ones perturb the
// signal and so measure only within tolerance. Splitting the assertion this way
// matters: one identical-LUFS assertion over every format would be wrong for the
// lossy half. Every format WaxFlow encodes belongs in one list or the other.
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
	for _, f := range []string{"aiff", "flac", "alac", "wavpack", "ape"} {
		m := measure(f)
		if m.IntegratedLUFS != ref.IntegratedLUFS || m.SamplePeakDB != ref.SamplePeakDB {
			t.Errorf("lossless %s: {LUFS %.6f, peak %.6f} != wav {LUFS %.6f, peak %.6f} (should be bit-exact)",
				f, m.IntegratedLUFS, m.SamplePeakDB, ref.IntegratedLUFS, ref.SamplePeakDB)
		}
	}
	for _, f := range []string{"mp3", "aac", "he-aac", "opus", "vorbis"} {
		m := measure(f)
		if math.Abs(m.IntegratedLUFS-ref.IntegratedLUFS) > 2 {
			t.Errorf("lossy %s: LUFS %.2f is not within 2 LU of wav's %.2f", f, m.IntegratedLUFS, ref.IntegratedLUFS)
		}
	}
}
