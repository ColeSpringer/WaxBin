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
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/waxerr"
)

// writeBytes writes data into a fresh temp dir and returns the path.
func writeBytes(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMeasureReportsToleratedDamage: a FLAC cut in half still measures, because the
// demuxer reads to the end of what is there and says what it worked around. That is
// damage, not failure, and the file is the audit pass's to report rather than the
// analyze pass's to refuse. A clean file reports none, which is the half that keeps
// the field from being noise.
func TestMeasureReportsToleratedDamage(t *testing.T) {
	const rate = 8000
	flac := testaudio.EncodeAs(t, "flac", "", rate, testaudio.ReferenceSignal(rate, 4*time.Second))
	eng := New(nil)

	m, err := eng.Measure(context.Background(), writeBytes(t, "cut.flac", flac[:len(flac)*60/100]), nil)
	if err != nil {
		t.Fatalf("a cut flac must still measure: %v", err)
	}
	if len(m.InputDamage) == 0 {
		t.Error("InputDamage is empty for a flac cut at 60%; the decoder worked around something")
	}

	m, err = eng.Measure(context.Background(), writeBytes(t, "good.flac", flac), nil)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if len(m.InputDamage) != 0 {
		t.Errorf("InputDamage = %q for an intact file, want none", m.InputDamage)
	}
}

// TestOpenClassifiesDamageApartFromUnsupported is the rule at the phase boundary. A
// file whose magic matched and whose headers then fail is damaged (CodeInvalid), and
// the analyze pass errors on it so audit can name it. Bytes whose magic matches
// nothing are a format this build does not cover (ErrUnsupported), and the pass skips
// them, even when the extension handed them to a demuxer that refused them.
func TestOpenClassifiesDamageApartFromUnsupported(t *testing.T) {
	eng := New(nil)

	// The fLaC marker matches the sniff table, so the FLAC demuxer takes the file
	// and finds no STREAMINFO behind the marker.
	_, err := eng.Measure(context.Background(), writeBytes(t, "truncated.flac", []byte("fLaC\x00\x00\x00")), nil)
	if err == nil {
		t.Fatal("a file with nothing behind its fLaC marker must not open")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want damage rather than ErrUnsupported", err)
	}
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalid {
		t.Errorf("code = %q, want %q", got, waxerr.CodeInvalid)
	}

	// A healthy Layer II stream matches no magic, so the .mp3 extension alone sends it
	// to the MP3 demuxer, which finds no Layer III frames. Calling that damage would
	// error on a good file every run and have audit report it as corrupt.
	for _, name := range []string{"layer2.mp3", "garbage.flac"} {
		data := []byte("nothing recognizes these bytes")
		if name == "layer2.mp3" {
			data = testaudio.Fixture(t, "ref-1s-layer2.mp2")
		}
		if _, err := eng.Measure(context.Background(), writeBytes(t, name, data), nil); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
}

// TestMeasureRemovesTheOggOpusHeaderGain: WaxFlow's decoder applies the OpusHead
// output gain, so a measurement that kept it would move every time the ReplayGain
// write-back moved the header, and the next pass would fold the same album gain in
// again. With the header taken back out, the catalog value is a property of the
// encoded audio and the header is a pure function of it, so a second pass is a no-op.
//
// The Mono comparison is the other half: it proves the decoder really did apply the
// header, which is what makes the subtraction necessary rather than cosmetic.
func TestMeasureRemovesTheOggOpusHeaderGain(t *testing.T) {
	const rate, headerQ78, headerDB = 48000, -1536, -6.0
	ctx := context.Background()
	dir := t.TempDir()
	opusBytes := testaudio.EncodeAs(t, "opus", "", rate, testaudio.ReferenceSignal(rate, 3*time.Second))
	plain := filepath.Join(dir, "plain.opus")
	gained := filepath.Join(dir, "gained.opus")
	for _, p := range []string{plain, gained} {
		if err := os.WriteFile(p, opusBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// meta does not import decode, so this test may import meta.
	if _, err := meta.NewWriter().Apply(ctx, gained, nil, meta.WithOutputGain(headerQ78)); err != nil {
		t.Fatalf("setting the header gain: %v", err)
	}

	eng := New(nil)
	// The waveform is stored against an essence hash that masks the header, so the
	// tap has to see the same signal the measurement describes.
	tapPeak := func(peak *float32) func([][]float32) {
		return func(chans [][]float32) {
			for _, ch := range chans {
				for _, v := range ch {
					*peak = max(*peak, float32(math.Abs(float64(v))))
				}
			}
		}
	}
	var peakA, peakB float32
	a, err := eng.Measure(ctx, plain, tapPeak(&peakA))
	if err != nil {
		t.Fatalf("measure plain: %v", err)
	}
	b, err := eng.Measure(ctx, gained, tapPeak(&peakB))
	if err != nil {
		t.Fatalf("measure gained: %v", err)
	}
	if math.Abs(float64(peakA-peakB)) > 1e-4 {
		t.Errorf("tap peak %.6f vs %.6f; the waveform must not move with the header", peakA, peakB)
	}
	if a.HeaderGainDB != 0 {
		t.Errorf("plain HeaderGainDB = %v, want 0", a.HeaderGainDB)
	}
	if b.HeaderGainDB != headerDB {
		t.Errorf("gained HeaderGainDB = %v, want %v", b.HeaderGainDB, headerDB)
	}
	if math.Abs(a.IntegratedLUFS-b.IntegratedLUFS) > 0.01 {
		t.Errorf("LUFS %.4f vs %.4f; the header must not reach the stored measurement",
			a.IntegratedLUFS, b.IntegratedLUFS)
	}
	if math.Abs(a.SamplePeakDB-b.SamplePeakDB) > 0.01 {
		t.Errorf("peak %.4f vs %.4f; the header must not reach the stored measurement",
			a.SamplePeakDB, b.SamplePeakDB)
	}

	// What a player hears did change, by the header's own amount.
	if got := monoLevelDB(t, eng, gained) - monoLevelDB(t, eng, plain); math.Abs(got-headerDB) > 0.1 {
		t.Errorf("decoded level moved by %.2f dB, want %.1f; the decoder must apply the header", got, headerDB)
	}
}
