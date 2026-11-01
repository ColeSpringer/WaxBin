package scan

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/meta"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(filepath.Join(dir, "song.lrc"), []byte("[00:00.00]Hello\n[00:01.50]World\n"), 0o644); err != nil {
		t.Fatalf("write lrc: %v", err)
	}

	embedded := &model.Lyrics{Source: model.SourceTag, Unsynced: "embedded block", Synced: []model.SyncedLine{{TimeMS: 99, Text: "old"}}}
	got, _, _ := scanSidecars(audio, embedded, newArtCache())
	if got.Source != model.SourceSidecar {
		t.Fatalf("source = %q, want sidecar (the sidecar is authoritative)", got.Source)
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
	t.Parallel()
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac") // no .lrc next to it
	embedded := &model.Lyrics{Source: model.SourceTag, Unsynced: "just text"}
	if got, _, _ := scanSidecars(audio, embedded, newArtCache()); got != embedded {
		t.Errorf("with no sidecar, expected the embedded lyrics unchanged, got %+v", got)
	}
	// And no lyrics at all stays nil.
	if got, _, _ := scanSidecars(audio, nil, newArtCache()); got != nil {
		t.Errorf("with no sidecar and no embedded, expected nil, got %+v", got)
	}
}

func TestBookInput(t *testing.T) {
	t.Parallel()
	tags := model.Tags{
		Title: "Chapter 1", Album: "The Way of Kings (Unabridged)",
		AlbumArtist: "Brandon Sanderson", Artist: "Brandon Sanderson, Narrator",
		Series: "Stormlight Archive", SeriesSeq: "1", ASIN: "B00ABC", Year: 2010,
		BookSignal: model.BookTagSignal, Narrators: []string{"Kate Reading", "Michael Kramer"},
		TrackNo: 2, DiscNo: 1,
		Genres: []string{"Fantasy"}, Genre: "Fantasy",
	}
	file := model.File{Path: []byte("/lib/x.m4b"), DurationMS: 5000}
	in := bookInput(7, file, tags, "ess1", nil, partPlace(&tags, "/lib", "/lib/x.m4b", false, nil))

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
	t.Parallel()
	tags := model.Tags{BookSignal: model.BookTagSignal} // no album, title, or ids
	in := bookInput(1, model.File{}, tags, "essX", nil, place{})
	if in.Item.IdentityKey != "essence:essX" {
		t.Errorf("untitled book key = %q, want essence fallback", in.Item.IdentityKey)
	}
}

func TestCleanBookTitle(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.flac")
	// A cover file present on disk but neither decodable nor a recognized exotic.
	if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), []byte("not really a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, aux, _ := scanSidecars(audio, nil, newArtCache())
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
	t.Parallel()
	dir := t.TempDir()
	audio := filepath.Join(dir, "book.m4b")
	// A readable .cue that yields no chapters (no TRACK/INDEX entries).
	if err := os.WriteFile(filepath.Join(dir, "book.cue"), []byte("REM just a comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sheet, obs, _, _, unread, ok := scanCueSidecar(audio)
	if !ok {
		t.Fatal("scanCueSidecar reported not-readable for a readable .cue; its observation must be recorded so the fast-path does not re-parse it forever")
	}
	if sheet != nil || unread {
		t.Errorf("sheet = %v, unread = %v; want nil from a trackless cue that was read", sheet, unread)
	}
	if obs.Kind != model.AuxCue || string(obs.Path) != filepath.Join(dir, "book.cue") || obs.Size == 0 {
		t.Errorf("obs = %+v, want a populated AuxCue observation", obs)
	}
	// A sheet whose every TRACK line is misspelled has no tracks either, but it was
	// read, and its warnings are what the caller reports.
	if err := os.WriteFile(filepath.Join(dir, "book.cue"), []byte("TRCK 01 AUDIO\n  INDEX 01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sheet, _, diags, refusal, unread, ok := scanCueSidecar(audio)
	if !ok || unread || refusal != "" || len(diags) != 0 {
		t.Errorf("ok = %v, unread = %v, refusal = %q, diags = %+v; want a read sheet the caller reports on", ok, unread, refusal, diags)
	}
	if sheet == nil || len(sheet.Tracks) != 0 || len(sheet.Warnings) != 1 {
		t.Errorf("sheet = %+v, want no tracks and one warning", sheet)
	}
	// A truly-missing .cue still reports not-readable.
	if _, _, _, _, _, ok := scanCueSidecar(filepath.Join(dir, "missing.m4b")); ok {
		t.Error("scanCueSidecar should report ok=false when there is no .cue")
	}
}

// TestCueSheetDiagKeepsItsEnding: a detail is capped, and what became of the sheet is
// the part a reader needs most, so long chapter titles give way rather than the ending.
func TestCueSheetDiagKeepsItsEnding(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("A Very Long Chapter Title ", 12)
	dropped := []string{`TRACK 02 ("` + long + `") has no usable INDEX 01`, `TRACK 05 ("` + long + `") has no usable INDEX 01`}
	ds := cueSheetDiag(nil, dropped, "", "the readable lines were applied")
	if len(ds) != 1 || len(ds[0].Detail) > 512 || !strings.HasSuffix(ds[0].Detail, "; the readable lines were applied") {
		t.Errorf("detail = %q, want at most 512 bytes ending with what became of the sheet", ds[0].Detail)
	}
}

// TestCueSheetDiagCountsTruncatedWarnings: past upstream's cap the count of unread
// lines is a floor, and the detail says so.
func TestCueSheetDiagCountsTruncatedWarnings(t *testing.T) {
	t.Parallel()
	sheet, err := meta.ParseCueSheet(strings.Repeat("INDEX 01 00:00:00\n", 70))
	if err != nil {
		t.Fatal(err)
	}
	ds := cueSheetDiag(sheet, nil, "", "the sheet was not applied")
	if len(ds) != 1 || !strings.HasPrefix(ds[0].Detail, "at least 64 line(s) of the cue sheet could not be read: ") ||
		!strings.Contains(ds[0].Detail, " (and at least 61 more); the sheet was not applied") {
		t.Errorf("detail = %q, want a floor on the count", ds[0].Detail)
	}
}

func TestFindDirCoverAVIF(t *testing.T) {
	t.Parallel()
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

// TestCreditArtistsReportsOnlyAStatedList pins what the scanner passes down: the
// values a file repeated in its ARTIST frame, or nil to let the store split the
// combined one. The distinction is what keeps a tagged artist id from collapsing a
// list the tagger stated.
func TestCreditArtistsReportsOnlyAStatedList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tags model.Tags
		want []string
	}{
		{
			"a repeated frame is taken verbatim",
			model.Tags{Artist: "A vs. B", Artists: []string{"A vs. B", "Sunn O)))"}},
			[]string{"A vs. B", "Sunn O)))"},
		},
		{
			"one value states nothing",
			model.Tags{Artist: "Jay-Z feat. Alicia Keys", Artists: []string{"Jay-Z feat. Alicia Keys"}},
			nil,
		},
		{
			"no Artists at all states nothing",
			model.Tags{Artist: "Jay-Z feat. Alicia Keys"},
			nil,
		},
		{"untagged", model.Tags{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := creditArtists(c.tags)
			if len(got) != len(c.want) {
				t.Fatalf("creditArtists = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("creditArtists = %v, want %v", got, c.want)
				}
			}
		})
	}

	// The returned slice must not alias the tags', or a later mutation of one would
	// surface in the other.
	tags := model.Tags{Artist: "A", Artists: []string{"A", "B"}}
	got := creditArtists(tags)
	got[0] = "mutated"
	if tags.Artists[0] != "A" {
		t.Error("creditArtists aliased the tag slice")
	}
}

// TestIsAudioMatroska pins which Matroska extensions the scanner picks up. The
// decoder handles the container whatever the extension says, so audioExts is the
// only gate, and .mkv and .webm stay out because they routinely carry video.
func TestIsAudioMatroska(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"/lib/live.mka", "/lib/LIVE.MKA"} {
		if !IsAudio(p) {
			t.Errorf("IsAudio(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/lib/show.mkv", "/lib/clip.webm"} {
		if IsAudio(p) {
			t.Errorf("IsAudio(%q) = true, want false (video containers stay out)", p)
		}
	}
}

// TestProbePropertiesInterruptedScanFails pins the cancellation contract: the
// size+mtime fast path never revisits an unchanged file, so a probe interrupted
// mid-rebuild must fail the file's scan rather than commit zeroed properties the
// next scan will trust. A probe that merely cannot read the file keeps the zeroes
// and lets the file catalog, so audit can still name it.
func TestProbePropertiesInterruptedScanFails(t *testing.T) {
	t.Parallel()
	s := &Scanner{log: slog.New(slog.DiscardHandler)}
	missing := filepath.Join(t.TempDir(), "gone.wv")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var tags model.Tags
	if err := s.probeProperties(canceled, missing, &tags); err == nil {
		t.Fatal("an interrupted probe must fail the file's scan, or its zeroes freeze on the fast path")
	}

	if err := s.probeProperties(context.Background(), missing, &tags); err != nil {
		t.Fatalf("an unreadable file must still catalog with zeroes, got %v", err)
	}
	if tags.DurationMS != 0 || tags.SampleRate != 0 {
		t.Fatalf("tags = %+v, want untouched zeroes when the probe reports nothing", tags)
	}
}

// TestScanInputsCarryTheTitleSpelling: a book's title spelling is its ALBUMSORT, since
// its title is the album tag, a track's is its TITLESORT, and a track a cue sheet carves
// out of a rip takes none, the rip's own spelling naming the whole file.
func TestScanInputsCarryTheTitleSpelling(t *testing.T) {
	t.Parallel()
	tags := model.Tags{Title: "Part 1", TitleSort: "Part One", Album: "The Hobbit", AlbumSort: "Hobbit, The",
		AlbumArtist: "J.R.R. Tolkien", BookSignal: model.BookTagSignal}
	in := bookInput(1, model.File{}, tags, "e", nil, place{})
	if in.Item.TitleSort != "Hobbit, The" {
		t.Errorf("book TitleSort = %q, want its ALBUMSORT", in.Item.TitleSort)
	}
	tr := trackInput(1, model.File{}, model.Tags{Title: "東京", TitleSort: "Tokyo", AlbumSort: "Album, The",
		AlbumArtistSort: "Artist, The"}, "e")
	if tr.Item.TitleSort != "Tokyo" || tr.Track.AlbumSort != "Album, The" || tr.Track.AlbumArtistSort != "Artist, The" {
		t.Errorf("track spellings = %q/%q/%q, want TITLESORT, ALBUMSORT and ALBUMARTISTSORT",
			tr.Item.TitleSort, tr.Track.AlbumSort, tr.Track.AlbumArtistSort)
	}
	rip := model.Tags{Title: "Whole Rip", TitleSort: "Rip, Whole", Album: "Live"}
	sheet := &meta.CueSheet{Title: "Live"}
	windows := []meta.CueWindow{{Track: meta.CueTrack{Number: 1, Title: "Opening"}}}
	for _, vt := range carvedTracks("e", rip, sheet, windows) {
		if vt.Item.TitleSort != "" {
			t.Errorf("carved %q TitleSort = %q, want none", vt.Item.Title, vt.Item.TitleSort)
		}
	}
}

// TestABookTakesTheSpellingOfItsTitlesTag: a book titled by its TITLE, for want of an
// ALBUM, is spelled by TITLESORT; an ALBUMSORT outranks it, since that is where a written
// back spelling lands, and a book titled by its ALBUM never takes the part's TITLESORT.
func TestABookTakesTheSpellingOfItsTitlesTag(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		tags model.Tags
		want string
	}{
		{model.Tags{Title: "東京物語", TitleSort: "Tokyo Monogatari"}, "Tokyo Monogatari"},
		{model.Tags{Title: "東京物語", TitleSort: "Tokyo Monogatari", AlbumSort: "Tokyo Story"}, "Tokyo Story"},
		{model.Tags{Title: "Part 1", TitleSort: "Part One", Album: "The Hobbit"}, ""},
	} {
		c.tags.AlbumArtist, c.tags.BookSignal = "Author", model.BookTagSignal
		if got := bookInput(1, model.File{}, c.tags, "e", nil, place{}).Item.TitleSort; got != c.want {
			t.Errorf("book from %+v: TitleSort = %q, want %q", c.tags, got, c.want)
		}
	}
}

// TestCarvedTracksTakeTheRipsAlbumSpellings: a carved track takes the rip's ALBUMSORT and
// ALBUMARTISTSORT when its album and album artist are the ones the rip's tags name, as a
// plain rip's tracks do, and none for an album the sheet names otherwise.
func TestCarvedTracksTakeTheRipsAlbumSpellings(t *testing.T) {
	t.Parallel()
	tags := model.Tags{Album: "The Wall", AlbumSort: "Wall, The", AlbumArtist: "Pink Floyd",
		AlbumArtistSort: "Floyd, Pink", Artist: "Pink Floyd"}
	windows := []meta.CueWindow{{Track: meta.CueTrack{Number: 1, Title: "In the Flesh?"}}}
	for _, c := range []struct {
		name                       string
		sheet                      meta.CueSheet
		wantAlbum, wantAlbumArtist string
	}{
		{"from the tags", meta.CueSheet{}, "Wall, The", "Floyd, Pink"},
		{"the sheet agrees", meta.CueSheet{Title: "The Wall", Performer: "Pink Floyd"}, "Wall, The", "Floyd, Pink"},
		{"the sheet differs", meta.CueSheet{Title: "Live", Performer: "Someone"}, "", ""},
	} {
		sheet := c.sheet
		vt := carvedTracks("e", tags, &sheet, windows)[0]
		if vt.Track.AlbumSort != c.wantAlbum || vt.Track.AlbumArtistSort != c.wantAlbumArtist {
			t.Errorf("%s: album/album-artist spellings = %q/%q, want %q/%q", c.name,
				vt.Track.AlbumSort, vt.Track.AlbumArtistSort, c.wantAlbum, c.wantAlbumArtist)
		}
	}
}

// TestVerdictOfPartitionsEveryOutcome: every put outcome takes one verdict, and the scan
// counts each verdict under exactly one of its counters, so an import's report and a
// scan's totals read one rule.
func TestVerdictOfPartitionsEveryOutcome(t *testing.T) {
	t.Parallel()
	created := func(r *Result) int { return r.ItemsCreated }
	updated := func(r *Result) int { return r.ItemsUpdated }
	copies := func(r *Result) int { return r.Copies }
	sidecars := func(r *Result) int { return r.SidecarsUpdated }
	unchanged := func(r *Result) int { return r.Unchanged }
	cases := []struct {
		name    string
		out     model.ScanItemResult
		verdict Verdict
		counter func(*Result) int
	}{
		{"a new item", model.ScanItemResult{ItemCreated: true, FileCreated: true}, VerdictCreated, created},
		{"a part or a takeover", model.ScanItemResult{FileCreated: true, Joined: true}, VerdictJoined, updated},
		{"a new copy", model.ScanItemResult{FileCreated: true, AttachedAsCopy: true, Joined: true}, VerdictCopy, copies},
		{"a missing item back", model.ScanItemResult{Revived: true}, VerdictRevived, updated},
		{"a takeover of a missing item", model.ScanItemResult{FileCreated: true, Joined: true, Revived: true}, VerdictRevived, updated},
		{"a copy read again brings its item back", model.ScanItemResult{AttachedAsCopy: true, Revived: true}, VerdictRevived, updated},
		{"new bytes", model.ScanItemResult{ContentChanged: true}, VerdictUpdated, updated},
		{"a re-derived field", model.ScanItemResult{MetadataChanged: true}, VerdictUpdated, updated},
		{"a file row new to a standing item", model.ScanItemResult{FileCreated: true}, VerdictUpdated, updated},
		{"a copy with new bytes", model.ScanItemResult{AttachedAsCopy: true, ContentChanged: true}, VerdictUpdated, updated},
		{"a sidecar alone", model.ScanItemResult{SidecarsChanged: true}, VerdictSidecars, sidecars},
		{"a copy read again", model.ScanItemResult{AttachedAsCopy: true}, VerdictUnchanged, unchanged},
		{"nothing", model.ScanItemResult{}, VerdictUnchanged, unchanged},
	}
	seen := map[Verdict]bool{}
	for _, c := range cases {
		out := c.out
		if got := VerdictOf(&out); got != c.verdict {
			t.Errorf("%s: verdict %q, want %q", c.name, got, c.verdict)
		}
		seen[c.verdict] = true
		res := &Result{}
		counted := countOutcome(res, &out)
		if sum := res.ItemsCreated + res.ItemsUpdated + res.SidecarsUpdated + res.Copies + res.Unchanged; sum != 1 || *counted != 1 || c.counter(res) != 1 {
			t.Errorf("%s: counted %+v, want one file under the verdict's counter", c.name, *res)
		}
	}
	for _, v := range []Verdict{VerdictCreated, VerdictJoined, VerdictCopy, VerdictRevived, VerdictUpdated, VerdictSidecars, VerdictUnchanged} {
		if !seen[v] {
			t.Errorf("verdict %q has no case", v)
		}
	}
}
