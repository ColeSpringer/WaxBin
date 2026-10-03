package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// writeUnder writes an MP3 at a slash-separated path under root, creating its folders.
func writeUnder(t *testing.T, root, rel string, spec testaudio.MP3Spec) {
	t.Helper()
	p := filepath.Join(append([]string{root}, strings.Split(rel, "/")...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, testaudio.BuildMP3FromSpec(spec), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDiscFoldersNumberTheirDiscs: tracks in disc folders whose tags state no disc are
// one album, and each takes its disc from its folder, so the discs keep their order
// rather than interleaving by track number.
func TestDiscFoldersNumberTheirDiscs(t *testing.T) {
	t.Parallel()
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	for i, f := range []struct{ rel, title string }{
		{"Floyd/The Wall/CD1/01.mp3", "In the Flesh?"}, {"Floyd/The Wall/CD1/02.mp3", "The Thin Ice"},
		{"Floyd/The Wall/Disc 2/01.mp3", "Hey You"}, {"Floyd/The Wall/Disc 2/02.mp3", "Is There Anybody Out There?"},
	} {
		track := 1
		if strings.HasSuffix(f.rel, "02.mp3") {
			track = 2
		}
		writeUnder(t, root, f.rel, testaudio.MP3Spec{Title: f.title, Artist: "Pink Floyd", AlbumArtist: "Pink Floyd",
			Album: "The Wall", Track: track, Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	scanAll(t, sc, lib, false)
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 4 {
		t.Fatalf("items = %d (err %v), want 4", len(items), err)
	}
	want := map[string]int{"In the Flesh?": 1, "The Thin Ice": 1, "Hey You": 2, "Is There Anybody Out There?": 2}
	for _, it := range items {
		if it.DiscNo != want[it.Title] {
			t.Errorf("%s disc = %d, want %d", it.Title, it.DiscNo, want[it.Title])
		}
		if it.AlbumPID != items[0].AlbumPID {
			t.Errorf("%s album = %s, want every disc on %s", it.Title, it.AlbumPID, items[0].AlbumPID)
		}
	}
}

// TestDiscFolderRipTakesItsDisc: a cue rip in a disc folder carves tracks that carry
// the folder's disc, the rip file's own tags stating none.
func TestDiscFolderRipTakesItsDisc(t *testing.T) {
	t.Parallel()
	st, lib, sc, _, root := fastPathFixture(t)
	dir := filepath.Join(root, "Album", "CD2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMP3Raw(t, filepath.Join(dir, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole File", "Tagged Artist", "Tagged Album", 1, testaudio.AudioWithSeed(9)))
	writeCue(t, filepath.Join(dir, "album.cue"), twoTrackRipCue)
	scanAll(t, sc, lib, false)
	items := itemsByTrack(t, st)
	if len(items) != 2 {
		t.Fatalf("virtual tracks = %d, want 2", len(items))
	}
	for _, it := range items {
		if it.DiscNo != 2 {
			t.Errorf("%s disc = %d, want the folder's 2", it.Title, it.DiscNo)
		}
	}
}

// TestBookPartsInDiscFoldersKeepTheirOrder: a book's parts numbered from one in each
// disc folder take the folder's disc, so the second disc's parts follow the first's.
func TestBookPartsInDiscFoldersKeepTheirOrder(t *testing.T) {
	t.Parallel()
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	narrator := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	writeUnder(t, root, "Author/Tome/CD2/01.mp3", testaudio.MP3Spec{Title: "Part Three", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: narrator, Audio: testaudio.AudioWithSeed(2)})
	writeUnder(t, root, "Author/Tome/CD1/01.mp3", testaudio.MP3Spec{Title: "Part One", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: narrator, Audio: testaudio.AudioWithSeed(1)})
	scanAll(t, sc, lib, false)
	books, err := st.QueryItems(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("books = %d (err %v), want 1", len(books), err)
	}
	files, err := st.ItemFiles(ctx, books[0].PID)
	if err != nil || len(files) != 2 {
		t.Fatalf("parts = %d (err %v), want 2", len(files), err)
	}
	if !strings.Contains(files[0].DisplayPath, "CD1") || files[0].Position >= files[1].Position {
		t.Errorf("parts = %s @%d, %s @%d, want the CD1 part first", files[0].DisplayPath, files[0].Position,
			files[1].DisplayPath, files[1].Position)
	}
}

// TestResolveCoverClimbsFromADiscFolder: a file in a disc folder with no cover of its
// own takes the cover in its album folder, and a file in any other subfolder does not.
func TestResolveCoverClimbsFromADiscFolder(t *testing.T) {
	t.Parallel()
	album := t.TempDir()
	writeJPEG(t, filepath.Join(album, "cover.jpg"), 64, 64)
	for _, sub := range []string{"CD1", "Bonus"} {
		if err := os.MkdirAll(filepath.Join(album, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := resolveCover(filepath.Join(album, "CD1", "01.mp3"), nil, newArtCache()); got == nil || got.Width != 64 {
		t.Errorf("cover for a disc folder = %+v, want the album folder's", got)
	}
	if got := resolveCover(filepath.Join(album, "Bonus", "01.mp3"), nil, newArtCache()); got != nil {
		t.Errorf("cover for a non-disc subfolder = %+v, want none", got)
	}
	if !coverChangedFast(filepath.Join(album, "CD1", "01.mp3"), map[string]model.AuxObservation{}, newArtCache()) {
		t.Error("an album folder cover newly seen from a disc folder was not reported changed")
	}
}

// TestDiscFolderCoverScansOnce: the album folder's cover reaches a track in a disc
// folder on the full scan, and the next scan takes the fast path, since the cover the
// track uses is the one its observation records.
func TestDiscFolderCoverScansOnce(t *testing.T) {
	t.Parallel()
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	writeUnder(t, root, "Album/CD1/01.mp3", testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Album", Track: 1})
	writeJPEG(t, filepath.Join(root, "Album", "cover.jpg"), 64, 64)
	scanAll(t, sc, lib, false)
	pid := currentItemPID(t, st, "Song")
	prov, err := st.ArtProvenance(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront)
	if err != nil || prov.Width != 64 || prov.Source != model.SourceSidecar {
		t.Fatalf("cover = %+v (err %v), want the album folder's sidecar", prov, err)
	}
	if r := scanAll(t, sc, lib, false); r.Unchanged != 1 {
		t.Errorf("second scan = %+v, want the file unchanged", r)
	}
}

// TestLibraryRootNamedLikeADiscIsNoDiscFolder: only a folder inside the library can name a
// disc. Loose tracks in a root called "CD1" carry no disc, and the scan looks for their
// cover in the root alone, never in the folder beside it.
func TestLibraryRootNamedLikeADiscIsNoDiscFolder(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "CD1")
	st, lib, sc, _, _ := fastPathFixtureAt(t, root)
	ctx := context.Background()
	writeJPEG(t, filepath.Join(parent, "cover.jpg"), 64, 64)
	for i, title := range []string{"One", "Two"} {
		writeUnder(t, root, title+".mp3", testaudio.MP3Spec{Title: title, Artist: "Band", AlbumArtist: "Band",
			Album: "Album", Track: i + 1, Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	scanAll(t, sc, lib, false)
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %d (err %v), want 2", len(items), err)
	}
	for _, it := range items {
		if it.DiscNo != 0 {
			t.Errorf("%s disc = %d, want none: the library root is no disc folder", it.Title, it.DiscNo)
		}
		if prov, err := st.ArtProvenance(ctx, model.EntityRef{Type: model.ArtTrack, PID: it.PID}, model.ArtRoleFront); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Errorf("%s cover = %+v (err %v), want none: the folder beside the root is outside the library", it.Title, prov, err)
		}
	}
}

// TestPartPosition: a book part's place is its disc and place as its file states them: the
// tags, else a disc folder and the number its title or file name gives, and in a managed
// library the "Title - D-NN" name organize gives a part on a disc. A plain "Title - NN" is
// never read, being a reading-order index as often as a place, and nor is a staged file's
// name.
func TestPartPosition(t *testing.T) {
	t.Parallel()
	root := filepath.Join("/", "lib")
	for _, c := range []struct {
		rel           string
		title         string
		track, disc   int
		managed       bool
		want          int
		stated, given bool
	}{
		{"Author/Tome/x.mp3", "Chapter", 2, 0, true, 2, true, false},
		{"Author/Tome/x.mp3", "Chapter 3", 0, 0, true, 3, true, false},
		{"Author/Tome/Tome - 03.mp3", "Tome - 03", 0, 0, true, 0, false, false},
		{"Author/Tome/Tome - 2-03.mp3", "Tome - 2-03", 0, 0, true, 2*model.DiscStride + 3, true, false},
		{"Author/Tome/Tome - 2-03.mp3", "Tome - 2-03", 0, 0, false, 0, false, false},
		{"Author/Tome/Tome - 2-03.mp3", "Chapter", 3, 1, true, model.DiscStride + 3, true, false},
		{"Author/Tome/Tome - 07.mp3", "Chapter 1", 0, 0, true, 1, true, false},
		{"Author/Tome/CD2/01.mp3", "01", 0, 0, false, 2*model.DiscStride + 1, true, false},
		{"Author/Tome/Apollo - 13.mp3", "Apollo - 13", 0, 0, true, 0, false, false},
		{"Author/Tome/Lecture - 03-2021.mp3", "Lecture - 03-2021", 0, 0, true, 0, false, false},
		{"Author/Tome/Tome - 05.mp3", "Tome - 05", 0, 0, true, 2*model.DiscStride + 7, true, true},
	} {
		tags := &model.Tags{Title: c.title, TrackNo: c.track, DiscNo: c.disc}
		path := filepath.Join(append([]string{root}, strings.Split(c.rel, "/")...)...)
		var given *int
		if c.given {
			g := 2*model.DiscStride + 7
			given = &g
		}
		got := partPlace(tags, root, path, c.managed, given)
		if got.position != c.want || got.placeUnstated == c.stated {
			t.Errorf("partPlace(%s, title %q, track %d, disc %d, managed %v) = %+v, want %d stated %v",
				c.rel, c.title, c.track, c.disc, c.managed, got, c.want, c.stated)
		}
	}
	if pos, stated := PartPosition(&model.Tags{Title: "Tome - 2-03"}, root, filepath.Join(root, "Tome - 2-03.mp3")); pos != 0 || stated {
		t.Errorf("a staged part name read as %d (stated %v), want nothing", pos, stated)
	}
}

// TestRenamedBookPartsKeepTheirPlaces: a part whose new name no longer states the place
// its old one gave it keeps the place the catalog holds, and the disc in a part name
// orders the discs of parts whose tags state none.
func TestRenamedBookPartsKeepTheirPlaces(t *testing.T) {
	t.Parallel()
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	narrator := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	for i, p := range []struct {
		rel   string
		track int
	}{
		{"Author/Tome/01.mp3", 1}, {"Author/Tome/02.mp3", 2}, {"Author/Tome/Epilogue.mp3", 0},
		{"Author/Set/Set - 1-02.mp3", 2}, {"Author/Set/Set - 2-01.mp3", 1},
	} {
		album := "Tome"
		if strings.Contains(p.rel, "/Set/") {
			album = "Set"
		}
		writeUnder(t, root, p.rel, testaudio.MP3Spec{AlbumArtist: "Author", Album: album, Track: p.track,
			TXXX: narrator, Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	scanAll(t, sc, lib, false)
	// The epilogue's name gave it its place; the name organize would give it states none.
	if err := os.Rename(filepath.Join(root, "Author", "Tome", "Epilogue.mp3"), filepath.Join(root, "Author", "Tome", "Tome - 03.mp3")); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, true)
	books, err := st.QueryItems(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 2 {
		t.Fatalf("books = %d (err %v), want 2", len(books), err)
	}
	for _, b := range books {
		files, err := st.ItemFiles(ctx, b.PID)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range files {
			names = append(names, filepath.Base(f.DisplayPath))
		}
		want := map[string]string{"Tome": "01.mp3,02.mp3,Tome - 03.mp3", "Set": "Set - 1-02.mp3,Set - 2-01.mp3"}[b.Title]
		if got := strings.Join(names, ","); got != want {
			t.Errorf("%s reads %s, want %s", b.Title, got, want)
		}
	}
}

// TestManagedPartNamesGiveTheirDiscAndPlace: in a managed library a part named "Title -
// D-NN", the name organize gives a part when every part kept its place, reads back as that
// disc and place, so a rebuild orders the book as organize named it; an in-place library's
// names are its owner's, and a name like "Lecture - 03-2021" is no part name anywhere.
func TestManagedPartNamesGiveTheirDiscAndPlace(t *testing.T) {
	t.Parallel()
	for _, mode := range []model.Mode{model.ModeManaged, model.ModeInPlace} {
		st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
		lib.Mode = mode
		ctx := context.Background()
		for i, name := range []string{"Set - 2-01.mp3", "Set - 1-02.mp3", "Set - 1-01.mp3", "Set - 03-2021.mp3"} {
			writeUnder(t, root, "Author/Set/"+name, testaudio.MP3Spec{AlbumArtist: "Author", Album: "Set",
				Audio: testaudio.AudioWithSeed(byte(i + 1))})
		}
		scanAll(t, sc, lib, false)
		books := itemsOfKind(t, st, model.KindBook)
		if len(books) != 1 {
			t.Fatalf("%s: books = %d, want 1", mode, len(books))
		}
		files, err := st.ItemFiles(ctx, books[0].PID)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, f := range files {
			got[filepath.Base(f.DisplayPath)] = f.Position
		}
		want := map[string]int{"Set - 1-01.mp3": model.DiscStride + 1, "Set - 1-02.mp3": model.DiscStride + 2,
			"Set - 2-01.mp3": 2*model.DiscStride + 1, "Set - 03-2021.mp3": 0}
		if mode == model.ModeInPlace {
			want = map[string]int{"Set - 1-01.mp3": 0, "Set - 1-02.mp3": 0, "Set - 2-01.mp3": 0, "Set - 03-2021.mp3": 0}
		}
		for name, pos := range want {
			if got[name] != pos {
				t.Errorf("%s: %s at %d, want %d", mode, name, got[name], pos)
			}
		}
	}
}
