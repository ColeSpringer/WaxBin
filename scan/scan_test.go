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

func TestBookInput(t *testing.T) {
	tags := model.Tags{
		Title: "Chapter 1", Album: "The Way of Kings (Unabridged)",
		AlbumArtist: "Brandon Sanderson", Artist: "Brandon Sanderson, Narrator",
		Series: "Stormlight Archive", SeriesSeq: "1", ASIN: "B00ABC", Year: 2010,
		IsAudiobook: true, Narrators: []string{"Kate Reading", "Michael Kramer"},
		TrackNo: 2, DiscNo: 1,
		Genres: []string{"Fantasy"}, Genre: "Fantasy",
	}
	file := model.File{Path: []byte("/lib/x.m4b"), DurationMS: 5000}
	in := bookInput(7, file, tags, "ess1", nil)

	if in.Item.Kind != model.KindBook {
		t.Errorf("kind = %s, want book", in.Item.Kind)
	}
	// Book title is the album, with the abridged marker stripped.
	if in.Item.Title != "The Way of Kings" {
		t.Errorf("title = %q, want The Way of Kings", in.Item.Title)
	}
	// Author is the album artist; the book key is the ASIN.
	if in.Book.Author != "Brandon Sanderson" {
		t.Errorf("author = %q, want Brandon Sanderson", in.Book.Author)
	}
	if in.Item.IdentityKey != "asin:b00abc" {
		t.Errorf("key = %q, want asin:b00abc", in.Item.IdentityKey)
	}
	if len(in.Book.Narrators) != 2 {
		t.Errorf("narrators = %v, want 2", in.Book.Narrators)
	}
	// Disc/track drive the part position.
	if in.Position != 100002 {
		t.Errorf("position = %d, want 100002 (disc 1, track 2)", in.Position)
	}
	// No embedded chapters: a single whole-file chapter is synthesized.
	if len(in.Chapters) != 1 || in.Chapters[0].Title != "Chapter 1" {
		t.Errorf("synthesized chapters = %v, want one titled by the file", in.Chapters)
	}
}

func TestBookInputUntitledFallsBackToEssence(t *testing.T) {
	tags := model.Tags{IsAudiobook: true} // no album, title, or ids
	in := bookInput(1, model.File{}, tags, "essX", nil)
	if in.Item.IdentityKey != "essence:essX" {
		t.Errorf("untitled book key = %q, want essence fallback", in.Item.IdentityKey)
	}
}

func TestCleanBookTitle(t *testing.T) {
	cases := map[string]string{
		"The Hobbit (Unabridged)": "The Hobbit",
		"Dune [Abridged]":         "Dune",
		"Plain Title":             "Plain Title",
		"Unabridged":              "Unabridged", // never strip the whole title away
	}
	for in, want := range cases {
		if got := cleanBookTitle(in); got != want {
			t.Errorf("cleanBookTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
