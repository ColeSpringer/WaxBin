package identity_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/testaudio"
)

// TestEssenceFirstChangeDetection is the core identity guarantee: retagging a
// file (same audio, different ID3) changes the content hash but leaves the
// essence hash stable.
func TestEssenceFirstChangeDetection(t *testing.T) {
	dir := t.TempDir()
	audio := testaudio.DefaultAudio()

	v1 := filepath.Join(dir, "v1.mp3")
	v2 := filepath.Join(dir, "v2.mp3")
	write(t, v1, testaudio.BuildMP3WithAudio("Old Title", "Artist", "Album", 1, audio))
	write(t, v2, testaudio.BuildMP3WithAudio("A Much Longer New Title", "Different Artist", "Album", 1, audio))

	c1 := contentHash(t, v1)
	c2 := contentHash(t, v2)
	if c1 == c2 {
		t.Fatal("content hashes should differ after retag")
	}

	e1 := essenceHash(t, v1, c1)
	e2 := essenceHash(t, v2, c2)
	if e1 != e2 {
		t.Fatalf("essence hashes should be stable across retag: %s vs %s", e1, e2)
	}
}

// TestEssenceIndependentOfID3Presence: a file with no ID3 tag at all has the
// same essence as the tagged version (the tag is stripped either way).
func TestEssenceIndependentOfID3Presence(t *testing.T) {
	dir := t.TempDir()
	audio := testaudio.DefaultAudio()

	tagged := filepath.Join(dir, "tagged.mp3")
	bare := filepath.Join(dir, "bare.mp3")
	write(t, tagged, testaudio.BuildMP3WithAudio("Title", "Artist", "Album", 1, audio))
	write(t, bare, audio) // raw frames, no ID3

	et := essenceHash(t, tagged, contentHash(t, tagged))
	eb := essenceHash(t, bare, contentHash(t, bare))
	if et != eb {
		t.Fatalf("essence should ignore ID3 presence: %s vs %s", et, eb)
	}
}

// TestEssenceFallbackForUnknownFormat verifies that a non-MP3/FLAC file falls
// back to the content hash.
func TestEssenceFallbackForUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audio.wav")
	write(t, p, []byte("RIFF....WAVEfmt fake wav bytes"))
	c := contentHash(t, p)
	e := essenceHash(t, p, c)
	if e != c {
		t.Fatalf("fallback essence should equal content hash: %s vs %s", e, c)
	}
}

// TestEssenceADTSAACFallsBack: an ADTS .aac file starts with 0xFFF1, which
// overlaps the MPEG audio sync word. Extension-based routing must keep it on the
// content-hash fallback, not the MP3 essence path (which strips a trailing 128
// bytes and could collide two distinct files).
func TestEssenceADTSAACFallsBack(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "clip.aac")
	data := append([]byte{0xFF, 0xF1, 0x50, 0x80}, make([]byte, 256)...)
	write(t, p, data)

	c := contentHash(t, p)
	e := essenceHash(t, p, c)
	if e != c {
		t.Fatalf("ADTS .aac essence should equal the content hash, got %s vs %s", e, c)
	}
}

func contentHash(t *testing.T, path string) string {
	t.Helper()
	h, err := identity.ContentHash(path)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	return h
}

func essenceHash(t *testing.T, path, content string) string {
	t.Helper()
	h, err := identity.EssenceHash(path, content)
	if err != nil {
		t.Fatalf("EssenceHash: %v", err)
	}
	return h
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
