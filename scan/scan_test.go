package scan

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 70, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolveCoverSkipsUndecodableEmbedded(t *testing.T) {
	dir := t.TempDir()
	writeJPEG(t, filepath.Join(dir, "cover.jpg"), 80, 80)
	audio := filepath.Join(dir, "song.mp3")
	junk := &model.ArtImage{Data: []byte("this is not an image")}

	// A junk/undecodable embedded picture must not shadow the valid directory cover.
	got := resolveCover(audio, junk, newArtCache())
	if got == nil {
		t.Fatal("resolveCover returned nil; want the directory cover")
	}
	if string(got.Data) == string(junk.Data) {
		t.Fatal("undecodable embedded art shadowed the valid cover.jpg")
	}
	if got.Width != 80 || got.Format != "jpeg" {
		t.Errorf("resolved cover = %dx%d %s, want the 80x80 jpeg directory cover", got.Width, got.Height, got.Format)
	}
}

func TestResolveCoverKeepsUndecodableAsLastResort(t *testing.T) {
	dir := t.TempDir() // no directory cover here
	audio := filepath.Join(dir, "song.mp3")
	junk := &model.ArtImage{Data: []byte("exotic-but-real-bytes")}
	got := resolveCover(audio, junk, newArtCache())
	if got == nil || string(got.Data) != string(junk.Data) {
		t.Fatalf("with no directory cover, expected the embedded bytes as a last resort, got %v", got)
	}
	if got.Hash == "" {
		t.Error("last-resort embedded art must still carry a content hash for storage")
	}
}

func TestSidecarLyricsPrecedence(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(filepath.Join(dir, "song.lrc"), []byte("[00:00.00]Hello\n[00:01.50]World\n"), 0o644); err != nil {
		t.Fatalf("write lrc: %v", err)
	}

	embedded := &model.Lyrics{Source: "embedded", Unsynced: "embedded block", Synced: []model.SyncedLine{{TimeMS: 99, Text: "old"}}}
	got := sidecarLyrics(audio, embedded)
	if got.Source != "lrc" {
		t.Fatalf("source = %q, want lrc (sidecar is authoritative)", got.Source)
	}
	if len(got.Synced) != 2 || got.Synced[1].TimeMS != 1500 || got.Synced[1].Text != "World" {
		t.Errorf("synced = %+v, want the sidecar's 2 lines", got.Synced)
	}
	// The sidecar carries only timed lines, so the embedded unsynchronized block is
	// retained rather than dropped.
	if got.Unsynced != "embedded block" {
		t.Errorf("unsynced = %q, want the retained embedded block", got.Unsynced)
	}
}

func TestSidecarLyricsFallbackToEmbedded(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac") // no .lrc next to it
	embedded := &model.Lyrics{Source: "embedded", Unsynced: "just text"}
	if got := sidecarLyrics(audio, embedded); got != embedded {
		t.Errorf("with no sidecar, expected the embedded lyrics unchanged, got %+v", got)
	}
	// And no lyrics at all stays nil.
	if got := sidecarLyrics(audio, nil); got != nil {
		t.Errorf("with no sidecar and no embedded, expected nil, got %+v", got)
	}
}
