package meta

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
)

func TestReadWAVProperties(t *testing.T) {
	const rate = 22050
	samples := make([]float32, rate*3) // 3 seconds, mono
	path := filepath.Join(t.TempDir(), "tone.wav")
	if err := os.WriteFile(path, testaudio.EncodeWAV16(rate, samples), 0o644); err != nil {
		t.Fatal(err)
	}

	tags, err := DefaultReader{}.ReadTags(path)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if tags.SampleRate != rate || tags.Channels != 1 || tags.BitDepth != 16 {
		t.Errorf("audio props = %d Hz / %d ch / %d-bit, want %d / 1 / 16",
			tags.SampleRate, tags.Channels, tags.BitDepth, rate)
	}
	// 3s within a frame's rounding; duration must be populated (not 0) so the
	// analyze pass buckets WAV by true length, not a decode-cap fallback.
	if tags.DurationMS < 2990 || tags.DurationMS > 3010 {
		t.Errorf("duration = %d ms, want ~3000", tags.DurationMS)
	}
}
