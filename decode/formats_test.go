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
	// WaxFlow decodes these but encodes none of them; testaudio.Fixture says where
	// each checked-in file came from.
	"wma":         {file: "mono-8k.wma"},
	"wmalossless": {file: "lossless-s16.wma"},
	"wmapro":      {file: "pro-s16.wma"},
	"wmavoice":    {file: "voice-mono.wma"},
	"musepack":    {file: "ref-2s-sv8-chapters.mpc"},
	"alaw":        {file: "ref-1s-alaw.wav"},
	"mulaw":       {file: "ref-1s-mulaw.wav"},
	"ima-adpcm":   {file: "ref-1s-ima-adpcm.wav"},
	"ms-adpcm":    {file: "ref-1s-ms-adpcm.wav"},
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
	level := func(path string) float64 { return monoLevelDB(t, eng, path) }
	ref := level(writeBytes(t, "ref.wav", testaudio.EncodeAs(t, "wav", "", rate, testaudio.ReferenceSignal(rate, 2*time.Second))))
	for _, name := range []string{"ref-2s-sv7.mpc", "ref-2s-sv8-chapters.mpc"} {
		if got := level(writeBytes(t, name, testaudio.Fixture(t, name))); math.Abs(got-ref) > 1 {
			t.Errorf("%s decodes at %.2f dB, want within 1 dB of the wav's %.2f", name, got, ref)
		}
	}
}

// monoLevelDB is the RMS level of path's whole mono decode, the comparison the
// fixture tests make against the WAV of the same reference signal.
func monoLevelDB(tb testing.TB, eng *Engine, path string) float64 {
	tb.Helper()
	pcm, err := eng.Mono(context.Background(), path, 11025, 0)
	if err != nil {
		tb.Fatalf("%s: decode: %v", filepath.Base(path), err)
	}
	if pcm.Frames() == 0 {
		tb.Fatalf("%s: decoded to no frames", filepath.Base(path))
	}
	var sum float64
	for _, v := range pcm.Samples {
		sum += float64(v) * float64(v)
	}
	return 10 * math.Log10(sum/float64(len(pcm.Samples)))
}

// TestTelephonyCodecsDecodeToTheReferenceLevel: G.711 and both ADPCM families are
// checked-in fixtures, so the coverage test alone would only prove they are
// non-empty. Each was encoded from a ReferenceSignal WAV, so comparing the level
// of its decode with the level of that same WAV says the decode is right.
func TestTelephonyCodecsDecodeToTheReferenceLevel(t *testing.T) {
	eng := New(nil)
	refLevel := make(map[int]float64)
	for _, rate := range []int{8000, 44100} {
		sig := testaudio.ReferenceSignal(rate, time.Second)
		refLevel[rate] = monoLevelDB(t, eng, writeBytes(t, "ref.wav", testaudio.EncodeAs(t, "wav", "", rate, sig)))
	}
	for _, tc := range []struct {
		name string
		rate int
	}{
		{"ref-1s-alaw.wav", 8000},
		{"ref-1s-mulaw.wav", 8000},
		{"ref-1s-ima-adpcm.wav", 44100},
		{"ref-1s-ms-adpcm.wav", 44100},
	} {
		got := monoLevelDB(t, eng, writeBytes(t, tc.name, testaudio.Fixture(t, tc.name)))
		if ref := refLevel[tc.rate]; math.Abs(got-ref) > 1 {
			t.Errorf("%s decodes at %.2f dB, want within 1 dB of the wav's %.2f", tc.name, got, ref)
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
