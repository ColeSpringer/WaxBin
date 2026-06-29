package decode

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
)

// writeWAV16 writes mono 16-bit PCM as a RIFF/WAVE file using the shared encoder.
func writeWAV16(t *testing.T, path string, rate int, samples []float32) {
	t.Helper()
	if err := os.WriteFile(path, testaudio.EncodeWAV16(rate, samples), 0o644); err != nil {
		t.Fatalf("write wav: %v", err)
	}
}

func TestWAVDecodeRoundTrip(t *testing.T) {
	rate := 22050
	samples := make([]float32, rate) // 1 second
	for i := range samples {
		samples[i] = float32(0.4 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
	}
	path := filepath.Join(t.TempDir(), "tone.wav")
	writeWAV16(t, path, rate, samples)

	pcm, err := (WAVDecoder{}).Decode(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pcm.SampleRate != rate || pcm.Channels != 1 {
		t.Fatalf("decoded format = %d Hz / %d ch, want %d / 1", pcm.SampleRate, pcm.Channels, rate)
	}
	if pcm.Frames() != len(samples) {
		t.Fatalf("decoded %d frames, want %d", pcm.Frames(), len(samples))
	}
	// 16-bit quantization error is < 1/32767; sample values must round-trip.
	for i := 0; i < len(samples); i += 137 {
		if d := math.Abs(float64(pcm.Samples[i] - samples[i])); d > 1e-3 {
			t.Fatalf("sample %d off by %g (got %g want %g)", i, d, pcm.Samples[i], samples[i])
		}
	}
}

func TestWAVDecodeMaxDuration(t *testing.T) {
	rate := 22050
	samples := make([]float32, rate*4) // 4 seconds
	for i := range samples {
		samples[i] = float32(0.1 * math.Sin(float64(i)))
	}
	path := filepath.Join(t.TempDir(), "long.wav")
	writeWAV16(t, path, rate, samples)

	pcm, err := (WAVDecoder{}).Decode(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The cap is honored within a frame's worth of samples.
	if got := pcm.Frames(); got < rate-2 || got > rate+2 {
		t.Errorf("capped decode returned %d frames, want ~%d (1s)", got, rate)
	}
}

func TestWAVTruncatedDataChunk(t *testing.T) {
	rate := 22050
	samples := make([]float32, 1000)
	for i := range samples {
		samples[i] = float32(0.5 * math.Sin(float64(i)*0.1)) // all non-zero, varied
	}
	full := encodeWAV16Bytes(t, rate, samples)
	// Keep the 44-byte header (which still claims 2000 data bytes) but drop half
	// the audio, simulating a truncated/corrupt file.
	truncated := full[:44+1000] // 500 samples * 2 bytes present
	path := filepath.Join(t.TempDir(), "trunc.wav")
	if err := os.WriteFile(path, truncated, 0o644); err != nil {
		t.Fatal(err)
	}

	pcm, err := (WAVDecoder{}).Decode(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pcm.Frames() != 500 {
		t.Fatalf("decoded %d frames, want 500 (only the bytes actually present)", pcm.Frames())
	}
	// The decoder must not have appended phantom silent samples: the last decoded
	// sample matches the original, it is not a zero tail.
	if d := math.Abs(float64(pcm.Samples[499] - samples[499])); d > 1e-3 {
		t.Errorf("last decoded sample = %g, want ~%g (no zero padding)", pcm.Samples[499], samples[499])
	}
}

// encodeWAV16Bytes returns the encoded WAV bytes for truncation/corruption tests.
func encodeWAV16Bytes(t *testing.T, rate int, samples []float32) []byte {
	t.Helper()
	return testaudio.EncodeWAV16(rate, samples)
}

func TestWAVOversizedChunkIsBounded(t *testing.T) {
	// A tiny file whose data chunk header claims ~4 GiB must not trigger a 4 GiB
	// allocation: the read is capped at the file size, so it returns a small
	// result (or an error) rather than OOMing or panicking.
	var buf []byte
	put := func(s string) { buf = append(buf, s...) }
	put32 := func(v uint32) { buf = binary.LittleEndian.AppendUint32(buf, v) }
	put16 := func(v uint16) { buf = binary.LittleEndian.AppendUint16(buf, v) }
	put("RIFF")
	put32(0xFFFFFFFF)
	put("WAVE")
	put("fmt ")
	put32(16)
	put16(1)
	put16(1)
	put32(44100)
	put32(88200)
	put16(2)
	put16(16)
	put("data")
	put32(0xFFFFFFF0) // claims ~4 GiB of audio in a ~50-byte file
	buf = append(buf, 0, 0, 0, 0)
	path := filepath.Join(t.TempDir(), "bomb.wav")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	pcm, err := (WAVDecoder{}).Decode(context.Background(), path, 0)
	// Either outcome is acceptable; what matters is it didn't allocate ~4 GiB.
	if err == nil && pcm.Frames() > len(buf) {
		t.Fatalf("decoded %d frames from a ~%d-byte file; allocation was not bounded", pcm.Frames(), len(buf))
	}
}

func TestWAVRejectsAbsurdSampleRate(t *testing.T) {
	// A sample rate near 4e9 would overflow the frame arithmetic; it must be
	// rejected as invalid rather than panicking.
	rate := 22050
	samples := make([]float32, 100)
	full := encodeWAV16Bytes(t, rate, samples)
	binary.LittleEndian.PutUint32(full[24:28], 0xFFFFFFF0) // clobber sample rate
	path := filepath.Join(t.TempDir(), "fast.wav")
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (WAVDecoder{}).Decode(context.Background(), path, time.Second); err == nil {
		t.Fatal("expected an error for an absurd sample rate")
	}
}

func TestWAVRejectsNonRIFF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.wav")
	if err := os.WriteFile(path, []byte("not a wav file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (WAVDecoder{}).Decode(context.Background(), path, 0); err == nil {
		t.Fatal("expected an error decoding a non-RIFF file")
	}
}

func TestDefaultRegistryHasWAV(t *testing.T) {
	r := Default()
	if _, ok := r.For("pcm"); !ok {
		t.Fatal("default registry must always provide a WAV (pcm) decoder")
	}
	// Coverage always lists pcm as decodable (pure-go), regardless of ffmpeg.
	var sawPCM bool
	for _, fs := range r.Coverage() {
		if fs.Codec == "pcm" {
			sawPCM = true
			if !fs.Analysis || fs.Decoder != "pure-go (wav)" {
				t.Errorf("pcm coverage = %+v, want pure-go analysis", fs)
			}
		}
	}
	if !sawPCM {
		t.Error("coverage should include pcm")
	}
}
