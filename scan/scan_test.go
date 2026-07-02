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
	got, _ := scanSidecars(audio, embedded, newArtCache())
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
	if got, _ := scanSidecars(audio, embedded, newArtCache()); got != embedded {
		t.Errorf("with no sidecar, expected the embedded lyrics unchanged, got %+v", got)
	}
	// And no lyrics at all stays nil.
	if got, _ := scanSidecars(audio, nil, newArtCache()); got != nil {
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

// exoticAVIF returns minimal AVIF-branded ISOBMFF bytes: recognized by SniffExotic
// but not decodable by the pure-Go decoders, so finalizeArt leaves Width/Height 0.
func exoticAVIF() []byte {
	b := append([]byte{0, 0, 0, 0x20}, []byte("ftypavif")...)
	return append(b, make([]byte, 16)...)
}

func TestResolveCoverExoticEmbeddedYieldsToDirCover(t *testing.T) {
	dir := t.TempDir()
	writeJPEG(t, filepath.Join(dir, "cover.jpg"), 64, 64)
	audio := filepath.Join(dir, "song.m4a")
	// An exotic embedded cover is recognized but has unknown dimensions (no pure-Go
	// decoder). It must NOT shadow the genuinely-decodable directory cover.jpg.
	got := resolveCover(audio, &model.ArtImage{Data: exoticAVIF()}, newArtCache())
	if got == nil {
		t.Fatal("resolveCover returned nil; want the directory cover")
	}
	if got.Format != "jpeg" || got.Width != 64 {
		t.Errorf("resolved = %s %dx%d, want the 64x64 jpeg dir cover (exotic embedded with unknown dims must yield)", got.Format, got.Width, got.Height)
	}
}

func TestResolveCoverExoticEmbeddedKeptWithoutDirCover(t *testing.T) {
	dir := t.TempDir() // no directory cover
	audio := filepath.Join(dir, "song.m4a")
	got := resolveCover(audio, &model.ArtImage{Data: exoticAVIF()}, newArtCache())
	if got == nil || got.Format != "avif" {
		t.Fatalf("with no dir cover, the exotic embedded cover should be kept as last resort, got %v", got)
	}
	if got.Hash == "" {
		t.Error("last-resort exotic embedded art must carry a content hash for storage")
	}
}

func TestScanSidecarsRecordsUndecodableCoverObs(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	// A cover file present on disk but neither decodable nor a recognized exotic.
	if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), []byte("not really a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, aux := scanSidecars(audio, nil, newArtCache())
	var cover *model.AuxObservation
	for i := range aux {
		if aux[i].Kind == model.AuxCover {
			cover = &aux[i]
		}
	}
	// Without an existence-based fallback the full scan records nothing, and the
	// fast-path (which detects covers by existence) would see it as newly-appeared and
	// force a full reprocess on every scan.
	if cover == nil {
		t.Fatal("no cover observation recorded for a present-but-undecodable cover")
	}
	if want := filepath.Join(dir, "cover.jpg"); string(cover.Path) != want {
		t.Errorf("cover obs path = %q, want %q", cover.Path, want)
	}
	if cover.Size == 0 {
		t.Error("cover obs should carry the stat size so the fast-path can compare it")
	}
}

func TestScanCueSidecarReadableButEmpty(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "book.m4b")
	// A readable .cue that yields no chapters (no TRACK/INDEX entries).
	if err := os.WriteFile(filepath.Join(dir, "book.cue"), []byte("REM just a comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chapters, obs, ok := scanCueSidecar(audio)
	if !ok {
		t.Fatal("scanCueSidecar reported not-readable for a readable .cue; its observation must be recorded so the fast-path does not re-parse it forever")
	}
	if len(chapters) != 0 {
		t.Errorf("chapters = %v, want none from a chapterless cue", chapters)
	}
	if obs.Kind != model.AuxCue || string(obs.Path) != filepath.Join(dir, "book.cue") || obs.Size == 0 {
		t.Errorf("obs = %+v, want a populated AuxCue observation", obs)
	}
	// A truly-missing .cue still reports not-readable.
	if _, _, ok := scanCueSidecar(filepath.Join(dir, "missing.m4b")); ok {
		t.Error("scanCueSidecar should report ok=false when there is no .cue")
	}
}

func TestFindDirCoverAVIF(t *testing.T) {
	dir := t.TempDir()
	// A minimal AVIF-branded ISOBMFF header (undecodable by pure-Go, but recognized).
	avif := append([]byte{0, 0, 0, 0x20}, []byte("ftypavif")...)
	avif = append(avif, make([]byte, 16)...)
	if err := os.WriteFile(filepath.Join(dir, "cover.avif"), avif, 0o644); err != nil {
		t.Fatal(err)
	}
	img, path := findDirCover(dir)
	if img == nil {
		t.Fatal("cover.avif not found (exotic covers must still be discovered)")
	}
	if img.Format != "avif" || img.Hash == "" {
		t.Errorf("avif cover: format=%q hash=%q, want avif + a hash", img.Format, img.Hash)
	}
	if filepath.Base(path) != "cover.avif" {
		t.Errorf("cover path = %q, want cover.avif", path)
	}
}
