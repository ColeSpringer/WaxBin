package decode

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/container"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
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
	t.Parallel()
	const rate = 8000
	flac := testaudio.EncodeAs(t, "flac", "", rate, testaudio.ReferenceSignal(rate, 4*time.Second))
	eng := New(nil)

	good, err := eng.Measure(context.Background(), writeBytes(t, "good.flac", flac), nil)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if good.Damaged() || good.DamageTruncated || good.Loss != "" || good.DamageDetail() != "" {
		t.Errorf("an intact file reads as damaged: %q, loss %q", good.InputDamage, good.Loss)
	}

	m, err := eng.Measure(context.Background(), writeBytes(t, "cut.flac", flac[:len(flac)*60/100]), nil)
	if err != nil {
		t.Fatalf("a cut flac must still measure: %v", err)
	}
	if len(m.InputDamage) == 0 || !m.Damaged() {
		t.Error("InputDamage is empty for a flac cut at 60%; the decoder worked around something")
	}
	if m.DamageTruncated {
		t.Errorf("DamageTruncated with %d findings, under WaxFlow's cap", len(m.InputDamage))
	}
	// What was measured and what the cut took add up to the length the header declares.
	if m.TruncatedFrames <= 0 || m.Frames+m.TruncatedFrames != good.Frames {
		t.Errorf("measured %d frames and counted %d truncated, want them to sum to the %d declared",
			m.Frames, m.TruncatedFrames, good.Frames)
	}
	if m.LostFrames != 0 {
		t.Errorf("LostFrames = %d for a file cut at its end, want 0", m.LostFrames)
	}
	// The line is WaxFlow's to word, so only the count is looked for.
	if !strings.Contains(m.Loss, strconv.FormatInt(m.TruncatedFrames, 10)) {
		t.Errorf("Loss = %q, want it to count the %d truncated frames", m.Loss, m.TruncatedFrames)
	}
	if d := m.DamageDetail(); !strings.HasPrefix(d, m.Loss) || !strings.Contains(d, m.InputDamage[0]) {
		t.Errorf("DamageDetail = %q, want the loss and then the findings", d)
	}
}

// TestMeasureCountsAudioLostToAHole: a FLAC with a damaged stretch inside it reads to
// its full length, with silence where the bad frames were, and says how much of the
// measurement is that silence. A read that stopped at the damage would measure the
// head of the file and call it the whole.
func TestMeasureCountsAudioLostToAHole(t *testing.T) {
	t.Parallel()
	const rate = 8000
	flac := testaudio.EncodeAs(t, "flac", "", rate, testaudio.ReferenceSignal(rate, 8*time.Second))
	eng := New(nil)
	good, err := eng.Measure(context.Background(), writeBytes(t, "good.flac", flac), nil)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	holed := bytes.Clone(flac)
	for i := len(holed) / 2; i < len(holed)/2+64; i++ {
		holed[i] ^= 0x5a
	}
	var tapped int64
	m, err := eng.Measure(context.Background(), writeBytes(t, "holed.flac", holed), func(chans [][]float32) {
		tapped += int64(len(chans[0]))
	})
	if err != nil {
		t.Fatalf("a flac with a damaged frame must still measure: %v", err)
	}
	if m.Frames != good.Frames || tapped != good.Frames {
		t.Errorf("measured %d frames and tapped %d, want the file's %d: a hole keeps the length", m.Frames, tapped, good.Frames)
	}
	if m.LostFrames <= 0 || m.LostFrames >= m.Frames {
		t.Errorf("LostFrames = %d of %d, want the damaged stretch and no more", m.LostFrames, m.Frames)
	}
	if m.TruncatedFrames != 0 {
		t.Errorf("TruncatedFrames = %d for a file whose end is intact, want 0", m.TruncatedFrames)
	}
	if !m.Damaged() || len(m.InputDamage) == 0 {
		t.Error("the hole was not reported as damage")
	}
	if !strings.Contains(m.Loss, strconv.FormatInt(m.LostFrames, 10)) {
		t.Errorf("Loss = %q, want it to count the %d lost frames", m.Loss, m.LostFrames)
	}
}

// TestMeasureMarksAFullDamageList: WaxFlow stops listing findings at
// container.MaxWarnings and keeps no count of distinct ones past it, so a full list is
// reported as at least that much damage.
func TestMeasureMarksAFullDamageList(t *testing.T) {
	t.Parallel()
	const rate, holes = 8000, 2 * container.MaxWarnings
	flac := testaudio.EncodeAs(t, "flac", "", rate, testaudio.ReferenceSignal(rate, 6*time.Minute))
	// One flipped byte fails one frame's CRC. Spaced this far apart no two share a
	// frame or its neighbor, so each is a hole of its own.
	step := len(flac) / (holes + 2)
	for i := 1; i <= holes; i++ {
		flac[i*step] ^= 0xff
	}
	m, err := New(nil).Measure(context.Background(), writeBytes(t, "holes.flac", flac), nil)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if len(m.InputDamage) != container.MaxWarnings || !m.DamageTruncated {
		t.Fatalf("listed %d findings, truncated %v; want WaxFlow's cap of %d, marked full",
			len(m.InputDamage), m.DamageTruncated, container.MaxWarnings)
	}
	d := m.DamageDetail()
	if len(d) > model.MaxDetailBytes || !strings.Contains(d, "; and at least ") || !strings.HasSuffix(d, " more") {
		t.Errorf("DamageDetail = %q (%d bytes), want it within the cap and ending on how many more", d, len(d))
	}
}

// TestDamageDetail pins the one line a damaged read is reported in: the loss first,
// then the findings that fit, whole, then a count of the rest that survives the cap.
func TestDamageDetail(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200)
	for _, tc := range []struct {
		name string
		m    Measurement
		want string
	}{
		{"clean", Measurement{}, ""},
		{"one finding", Measurement{InputDamage: []string{"frame sync lost"}}, "frame sync lost"},
		{"loss alone", Measurement{TruncatedFrames: 1, Loss: "one frame short"}, "one frame short"},
		{"loss then findings", Measurement{LostFrames: 588, Loss: "588 lost", InputDamage: []string{"a", "b"}}, "588 lost; a; b"},
		{"the rest counted", Measurement{InputDamage: []string{long, long, long, "d"}},
			long + "; " + long + "; and 2 more"},
		{"a full list", Measurement{InputDamage: []string{long, long, long}, DamageTruncated: true},
			long + "; " + long + "; and at least 1 more"},
	} {
		got := tc.m.DamageDetail()
		if got != tc.want {
			t.Errorf("%s: DamageDetail = %q, want %q", tc.name, got, tc.want)
		}
		if len(got) > model.MaxDetailBytes {
			t.Errorf("%s: %d bytes, over the %d a detail holds", tc.name, len(got), model.MaxDetailBytes)
		}
	}
	// A first finding too long to fit is cut, and the count still closes the line.
	huge := Measurement{InputDamage: []string{strings.Repeat("y", 2*model.MaxDetailBytes), "b"}}
	if got := huge.DamageDetail(); len(got) != model.MaxDetailBytes || !strings.HasSuffix(got, "; and 1 more") {
		t.Errorf("an oversized finding gave %d bytes ending %q, want the cap and the count", len(got), got[max(len(got)-16, 0):])
	}
}

// TestOpenClassifiesDamageApartFromUnsupported is the rule at the phase boundary. A
// file whose magic matched and whose headers then fail is damaged (CodeInvalid), and
// the analyze pass errors on it so audit can name it. Bytes whose magic matches
// nothing are a format this build does not cover (ErrUnsupported), and the pass skips
// them, even when the extension handed them to a demuxer that refused them.
func TestOpenClassifiesDamageApartFromUnsupported(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
