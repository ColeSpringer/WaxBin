package meta_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/meta"
)

// TestReadTagsSurvivesCorruptFrameSize feeds an ID3v2 frame whose size field is
// near 2^31 (which would overflow pos+frameSize on a 32-bit int) and asserts the
// reader neither panics nor reads out of bounds. It falls back gracefully.
func TestReadTagsSurvivesCorruptFrameSize(t *testing.T) {
	frame := []byte("TIT2")
	frame = append(frame, 0x7F, 0xFF, 0xFF, 0xFF) // absurd frame size
	frame = append(frame, 0, 0)                   // flags
	frame = append(frame, 0x03, 'X')              // tiny body

	sz := len(frame)
	out := []byte{'I', 'D', '3', 3, 0, 0,
		byte(sz >> 21 & 0x7f), byte(sz >> 14 & 0x7f), byte(sz >> 7 & 0x7f), byte(sz & 0x7f)}
	out = append(out, frame...)
	out = append(out, 0xFF, 0xFB, 0, 0) // a little "audio"

	dir := t.TempDir()
	path := filepath.Join(dir, "fallback.mp3")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}

	tags, err := meta.NewReader().ReadTags(path) // must not panic
	if err != nil {
		t.Fatalf("ReadTags: %v", err)
	}
	if tags.Title != "fallback" {
		t.Fatalf("expected filename-fallback title, got %q", tags.Title)
	}
}
