package meta

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
)

// TestWriterRoundTripPreservesEssence writes a tag and confirms it reads back while
// the audio essence hash is unchanged (a tag edit must never alter audio).
func TestWriterRoundTripPreservesEssence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "song.mp3")
	if err := os.WriteFile(path, testaudio.BuildMP3("Song", "Artist", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewReader()
	before, err := r.Read(ctx, path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	w := NewWriter()
	res, err := w.Apply(ctx, path, []TagEdit{
		{Key: "ALBUMARTIST", Values: []string{"Various Artists"}},
		{Key: "REPLAYGAIN_TRACK_GAIN", Values: []string{"-6.35 dB"}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Changed || res.ContentHash == "" || res.Size == 0 {
		t.Fatalf("write result = %+v, want changed with size+hash", res)
	}

	after, err := r.Read(ctx, path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if after.Tags.AlbumArtist != "Various Artists" {
		t.Errorf("albumArtist = %q, want Various Artists", after.Tags.AlbumArtist)
	}
	if before.EssenceHash == "" {
		t.Fatal("test fixture has no essence hash")
	}
	if after.EssenceHash != before.EssenceHash {
		t.Errorf("essence changed by tag write: before=%s after=%s", before.EssenceHash, after.EssenceHash)
	}

	// A second identical write is a no-op.
	res2, err := w.Apply(ctx, path, []TagEdit{{Key: "ALBUMARTIST", Values: []string{"Various Artists"}}})
	if err != nil {
		t.Fatalf("apply no-op: %v", err)
	}
	if res2.Changed {
		t.Error("identical re-write reported Changed=true")
	}
}
