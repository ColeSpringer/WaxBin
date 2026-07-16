package decode

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
)

// tone returns a mono sine at the given amplitude.
func tone(rate int, dur time.Duration, freq, amp float64) []float32 {
	n := int(dur.Seconds() * float64(rate))
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

func writeWAV(t *testing.T, dir, name string, rate int, samples []float32) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, testaudio.EncodeWAV16(rate, samples), 0o644); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	return p
}

// writeStereoWAV writes interleaved 16-bit stereo. testaudio.EncodeWAV16 is
// mono-only, and the limiter guard below needs two correlated channels.
func writeStereoWAV(t *testing.T, dir, name string, rate int, left, right []float32) string {
	t.Helper()
	if len(left) != len(right) {
		t.Fatalf("channel lengths differ: %d vs %d", len(left), len(right))
	}
	dataLen := len(left) * 4 // 2 channels * 2 bytes
	buf := make([]byte, 0, 44+dataLen)
	buf = append(buf, "RIFF"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(36+dataLen))
	buf = append(buf, "WAVE"...)
	buf = append(buf, "fmt "...)
	buf = binary.LittleEndian.AppendUint32(buf, 16)
	buf = binary.LittleEndian.AppendUint16(buf, 1) // PCM
	buf = binary.LittleEndian.AppendUint16(buf, 2) // stereo
	buf = binary.LittleEndian.AppendUint32(buf, uint32(rate))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(rate*4)) // byte rate
	buf = binary.LittleEndian.AppendUint16(buf, 4)              // block align
	buf = binary.LittleEndian.AppendUint16(buf, 16)
	buf = append(buf, "data"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(dataLen))
	clamp := func(s float32) uint16 {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		return uint16(int16(math.Round(float64(s) * 32767)))
	}
	for i := range left {
		buf = binary.LittleEndian.AppendUint16(buf, clamp(left[i]))
		buf = binary.LittleEndian.AppendUint16(buf, clamp(right[i]))
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatalf("write stereo wav: %v", err)
	}
	return p
}

func TestMonoRoundTrip(t *testing.T) {
	const rate = 22050
	samples := tone(rate, time.Second, 440, 0.4)
	p := writeWAV(t, t.TempDir(), "tone.wav", rate, samples)

	pcm, err := New(nil).Mono(context.Background(), p, 0, 0)
	if err != nil {
		t.Fatalf("Mono: %v", err)
	}
	if pcm.SampleRate != rate || pcm.Channels != 1 {
		t.Fatalf("decoded %d Hz / %d ch, want %d / 1", pcm.SampleRate, pcm.Channels, rate)
	}
	if pcm.Frames() != len(samples) {
		t.Fatalf("decoded %d frames, want %d", pcm.Frames(), len(samples))
	}
	// 16-bit quantization error is < 1/32767; values must round-trip.
	for i := 0; i < len(samples); i += 137 {
		if d := math.Abs(float64(pcm.Samples[i] - samples[i])); d > 1e-3 {
			t.Fatalf("sample %d off by %g (got %g want %g)", i, d, pcm.Samples[i], samples[i])
		}
	}
}

func TestMonoMaxDuration(t *testing.T) {
	const rate = 22050
	p := writeWAV(t, t.TempDir(), "long.wav", rate, tone(rate, 4*time.Second, 300, 0.1))

	pcm, err := New(nil).Mono(context.Background(), p, 0, time.Second)
	if err != nil {
		t.Fatalf("Mono: %v", err)
	}
	// The cap is exact: Mono truncates the chunk that crosses it.
	if got := pcm.Frames(); got != rate {
		t.Errorf("capped decode returned %d frames, want exactly %d (1s)", got, rate)
	}
}

func TestMonoZeroMaxDecodesWholeFile(t *testing.T) {
	const rate = 22050
	samples := tone(rate, 2*time.Second, 300, 0.2)
	p := writeWAV(t, t.TempDir(), "whole.wav", rate, samples)

	pcm, err := New(nil).Mono(context.Background(), p, 0, 0)
	if err != nil {
		t.Fatalf("Mono: %v", err)
	}
	if pcm.Frames() != len(samples) {
		t.Errorf("decoded %d frames, want the whole %d", pcm.Frames(), len(samples))
	}
}

// TestMonoResamplesToInternalRate covers the fingerprint's route: decode
// straight to the analysis rate rather than box-averaging afterwards.
func TestMonoResamplesToInternalRate(t *testing.T) {
	const srcRate, dstRate = 44100, 11025
	p := writeWAV(t, t.TempDir(), "hq.wav", srcRate, tone(srcRate, 2*time.Second, 440, 0.5))

	pcm, err := New(nil).Mono(context.Background(), p, dstRate, 0)
	if err != nil {
		t.Fatalf("Mono: %v", err)
	}
	if pcm.SampleRate != dstRate || pcm.Channels != 1 {
		t.Fatalf("decoded %d Hz / %d ch, want %d / 1", pcm.SampleRate, pcm.Channels, dstRate)
	}
	// 2 seconds at the target rate, within a chunk's slack for filter latency.
	if got, want := pcm.Frames(), 2*dstRate; math.Abs(float64(got-want)) > 4096 {
		t.Errorf("decoded %d frames, want ~%d", got, want)
	}
	// The 440 Hz tone survives resampling: it is far below the new Nyquist.
	var peak float32
	for _, s := range pcm.Samples {
		if a := float32(math.Abs(float64(s))); a > peak {
			peak = a
		}
	}
	if math.Abs(float64(peak)-0.5) > 0.05 {
		t.Errorf("resampled peak = %.3f, want ~0.5", peak)
	}
}

// TestMonoUnsupportedInput pins the open phase: an input nothing recognizes is
// ErrUnsupported, which the analyze pass reads as "skip and retry later".
func TestMonoUnsupportedInput(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.wav")
	if err := os.WriteFile(p, []byte("not a wav file at all, nor anything else"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(nil).Mono(context.Background(), p, 0, 0)
	if err == nil {
		t.Fatal("want an error decoding an unrecognized file")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported (open phase)", err)
	}
}

func TestMonoMissingFile(t *testing.T) {
	_, err := New(nil).Mono(context.Background(), filepath.Join(t.TempDir(), "nope.wav"), 0, 0)
	if err == nil {
		t.Fatal("want an error for a missing file")
	}
	// A missing file is not an unsupported format; it must not be skipped-and-retried.
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want a plain IO error, not ErrUnsupported", err)
	}
}

// TestMonoStereoIsUnlimitedAmplitudeAverage is the limiter regression guard.
// Setting ChainSpec.Channels to mix down would engage WaxFlow's true-peak
// limiter (its stereo->mono matrix gain is 1.414 > 1), clamping at -1 dBTP
// (~0.891) and flattening exactly what the fingerprint and waveform read. A
// full-scale correlated stereo pair must come back at ~1.0.
func TestMonoStereoIsUnlimitedAmplitudeAverage(t *testing.T) {
	const rate = 44100
	full := tone(rate, time.Second, 440, 1.0)
	p := writeStereoWAV(t, t.TempDir(), "loud.wav", rate, full, full)

	pcm, err := New(nil).Mono(context.Background(), p, 0, 0)
	if err != nil {
		t.Fatalf("Mono: %v", err)
	}
	if pcm.Channels != 1 {
		t.Fatalf("Channels = %d, want 1", pcm.Channels)
	}
	var peak float64
	for _, s := range pcm.Samples {
		if a := math.Abs(float64(s)); a > peak {
			peak = a
		}
	}
	if peak < 0.95 {
		t.Errorf("mono peak = %.4f, want ~1.0; 0.891 means the true-peak limiter engaged "+
			"(did someone set ChainSpec.Channels or .Dynamics?)", peak)
	}
	// L==R, so the average is the same signal, not an energy-preserving +3 dB mix.
	if peak > 1.01 {
		t.Errorf("mono peak = %.4f, want an amplitude average (~1.0), not an energy mix", peak)
	}
}

// TestMixMonoMatchesPCMMono is the parity contract: MixMono (planar, streaming)
// and PCM.Mono (interleaved, buffered) feed the same waveform code, so a
// streamed waveform must equal a buffered one sample for sample.
func TestMixMonoMatchesPCMMono(t *testing.T) {
	for _, nch := range []int{1, 2, 6} {
		const frames = 500
		inter := make([]float32, frames*nch)
		chans := make([][]float32, nch)
		for c := range chans {
			chans[c] = make([]float32, frames)
		}
		for i := 0; i < frames; i++ {
			for c := 0; c < nch; c++ {
				v := float32(math.Sin(float64(i)*0.01*float64(c+1)) * 0.7)
				inter[i*nch+c] = v
				chans[c][i] = v
			}
		}
		want := (&PCM{SampleRate: 44100, Channels: nch, Samples: inter}).Mono()
		got := MixMono(nil, chans)
		if len(got) != len(want) {
			t.Fatalf("%d ch: len = %d, want %d", nch, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%d ch: sample %d = %v, want %v (bit-for-bit parity required)",
					nch, i, got[i], want[i])
			}
		}
	}
}

func TestMixMonoEmpty(t *testing.T) {
	if got := MixMono(nil, nil); len(got) != 0 {
		t.Errorf("MixMono(nil, nil) = %v, want empty", got)
	}
}

// TestMixMonoReusesScratch checks the streaming path does not allocate per chunk.
func TestMixMonoReusesScratch(t *testing.T) {
	chans := [][]float32{make([]float32, 128), make([]float32, 128)}
	scratch := make([]float32, 0, 128)
	first := MixMono(scratch, chans)
	second := MixMono(first[:0], chans)
	if &first[0] != &second[0] {
		t.Error("MixMono reallocated instead of reusing the scratch capacity")
	}
}

func TestMeasureWAV(t *testing.T) {
	const rate = 44100
	samples := tone(rate, 2*time.Second, 1000, 1.0)
	p := writeWAV(t, t.TempDir(), "m.wav", rate, samples)

	var tapped int
	m, err := New(nil).Measure(context.Background(), p, func(chans [][]float32) {
		tapped += len(chans[0])
	})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	// The tap must see every frame of the file, exactly once.
	if tapped != len(samples) {
		t.Errorf("tap saw %d frames, want %d", tapped, len(samples))
	}
	// A full-scale tone sits at ~0 dBFS.
	if math.Abs(m.SamplePeakDB) > 0.1 {
		t.Errorf("SamplePeakDB = %.3f, want ~0 for a full-scale tone", m.SamplePeakDB)
	}
	// A full-scale 1 kHz sine measures around -3 LUFS; just pin it as a real,
	// gated measurement rather than silence.
	if math.IsInf(m.IntegratedLUFS, 0) || m.IntegratedLUFS > 0 || m.IntegratedLUFS < -20 {
		t.Errorf("IntegratedLUFS = %v, want a plausible gated measurement", m.IntegratedLUFS)
	}
}

func TestMeasureNilTap(t *testing.T) {
	const rate = 22050
	p := writeWAV(t, t.TempDir(), "n.wav", rate, tone(rate, time.Second, 440, 0.5))
	if _, err := New(nil).Measure(context.Background(), p, nil); err != nil {
		t.Fatalf("Measure with a nil tap: %v", err)
	}
}

func TestMeasureUnsupportedInput(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.flac")
	if err := os.WriteFile(p, []byte("definitely not audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(nil).Measure(context.Background(), p, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported (open phase)", err)
	}
}

func TestMeasureCanceled(t *testing.T) {
	const rate = 44100
	p := writeWAV(t, t.TempDir(), "c.wav", rate, tone(rate, 5*time.Second, 440, 0.5))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(nil).Measure(ctx, p, nil)
	if err == nil {
		t.Fatal("want an error for a canceled measure")
	}
	// Cancellation must not read as "this build cannot decode this", which would
	// stamp the file skipped rather than retrying it.
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want a canceled error, not ErrUnsupported", err)
	}
}

// TestCoverageIsHonest guards the display table: everything it claims decodable
// must name the real decoder, and the set must be derived rather than a stale
// hand-maintained list.
func TestCoverageIsHonest(t *testing.T) {
	cov := Coverage()
	if len(cov) == 0 {
		t.Fatal("Coverage is empty")
	}
	seen := map[string]bool{}
	for _, fs := range cov {
		if !fs.Analysis {
			t.Errorf("%+v: Coverage lists only decodable codecs, so Analysis must be true", fs)
		}
		if fs.Decoder != "waxflow" {
			t.Errorf("%+v: Decoder = %q, want waxflow", fs, fs.Decoder)
		}
		if fs.Codec == "" {
			t.Error("Coverage has an entry with no codec")
		}
		seen[fs.Codec] = true
	}
	// The codecs the migration exists to cover. aac-lc is WaxFlow's ID for what
	// WaxLabel calls "AAC", the exact vocabulary gap that makes a codec
	// pre-filter a trap, and why this table is display-only.
	for _, want := range []string{"pcm", "flac", "mp3", "alac", "aac-lc", "vorbis", "opus"} {
		if !seen[want] {
			t.Errorf("Coverage is missing %q; it reports %v", want, seen)
		}
	}
	// aiff is a PCM container, not a codec: it must not appear as one.
	if seen["aiff"] {
		t.Error("Coverage lists aiff as a codec; it is a container carrying pcm")
	}
}

// TestCoverageCodecsDecode is the honesty check with teeth: a codec string the
// table claims must actually decode a real file. WAV is the fixture this
// package can build without an encoder; the full eight-format sweep is Phase 8's.
func TestCoverageCodecsDecode(t *testing.T) {
	const rate = 22050
	p := writeWAV(t, t.TempDir(), "cov.wav", rate, tone(rate, time.Second, 440, 0.3))
	pcm, err := New(nil).Mono(context.Background(), p, 0, 0)
	if err != nil {
		t.Fatalf("the pcm codec Coverage claims does not decode: %v", err)
	}
	if pcm.Frames() == 0 {
		t.Error("decoded no frames")
	}
}
