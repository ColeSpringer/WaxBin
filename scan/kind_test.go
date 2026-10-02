package scan

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
)

// kindFixture opens a store with one managed library declared media, rooted in a fresh
// folder.
func kindFixture(t *testing.T, media model.MediaType) (*sqlite.Store, *model.Library, *Scanner, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged, Media: media, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure lib: %v", err)
	}
	return st, lib, New(st, meta.NewReader(), slog.New(slog.NewTextHandler(io.Discard, nil))), root
}

// itemsOfKind lists the catalog's items of one kind.
func itemsOfKind(t *testing.T, st *sqlite.Store, kind model.Kind) []*model.ItemView {
	t.Helper()
	items, err := st.QueryItems(context.Background(), query.New(query.EntityItems).Where("kind", query.OpIs, string(kind)).Build(), "")
	if err != nil {
		t.Fatalf("query %s items: %v", kind, err)
	}
	return items
}

// partsOf counts an item's primary and part files.
func partsOf(t *testing.T, st *sqlite.Store, pid model.PID) int {
	t.Helper()
	files, err := st.ItemFiles(context.Background(), pid)
	if err != nil {
		t.Fatalf("item files: %v", err)
	}
	n := 0
	for _, f := range files {
		if f.Role != "alternate" {
			n++
		}
	}
	return n
}

// oneBook asserts the catalog holds exactly one book of parts parts and no track, and
// returns it.
func oneBook(t *testing.T, st *sqlite.Store, parts int) *model.ItemView {
	t.Helper()
	books := itemsOfKind(t, st, model.KindBook)
	if tracks := itemsOfKind(t, st, model.KindTrack); len(books) != 1 || len(tracks) != 0 {
		var titles []string
		for _, it := range append(books, tracks...) {
			titles = append(titles, string(it.Kind)+":"+it.Title)
		}
		t.Fatalf("items = %v, want one book and no track", titles)
	}
	if n := partsOf(t, st, books[0].PID); n != parts {
		t.Fatalf("book %q has %d parts, want %d", books[0].Title, n, parts)
	}
	return books[0]
}

func TestEffectiveKindPrecedence(t *testing.T) {
	music := &model.Library{Media: model.MediaMusic}
	books := &model.Library{Media: model.MediaAudiobook}
	mixed := &model.Library{}
	plain := &model.Tags{}
	genre := &model.Tags{BookSignal: model.BookGenreSignal}
	cases := []struct {
		name         string
		tags         *model.Tags
		lib          *model.Library
		forced, lock model.Kind
		want         model.Kind
	}{
		{"plain file in a mixed library", plain, mixed, "", "", model.KindTrack},
		{"tags in a music library", genre, music, "", "", model.KindBook},
		{"audiobook library", plain, books, "", "", model.KindBook},
		{"lock over the library", plain, books, "", model.KindTrack, model.KindTrack},
		{"lock over the tags", genre, mixed, "", model.KindTrack, model.KindTrack},
		{"force over the lock", plain, mixed, model.KindBook, model.KindTrack, model.KindBook},
		{"no library", genre, nil, "", "", model.KindBook},
	}
	for _, c := range cases {
		if got := EffectiveKind(c.tags, c.lib, c.forced, c.lock); got != c.want {
			t.Errorf("%s: kind = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestAudiobookLibraryMakesABookOfAPlainFile: a library declared audiobook catalogs every
// file as a book whatever its tags say, and the book takes the spoken-word fields its tags
// carry (the narrator from the composer).
func TestAudiobookLibraryMakesABookOfAPlainFile(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	writeUnder(t, root, "Author/Tome/01 Chapter One.mp3", testaudio.MP3Spec{Title: "Chapter One", Artist: "Author",
		Album: "Tome", Track: 1, Genre: "Fiction", Composer: "Reader"})
	scanAll(t, sc, lib, false)
	book := oneBook(t, st, 1)
	if book.Title != "Tome" {
		t.Errorf("book title = %q, want Tome", book.Title)
	}
	detail, err := st.BookByPID(context.Background(), book.PID)
	if err != nil {
		t.Fatalf("book detail: %v", err)
	}
	if len(detail.Narrators) != 1 || detail.Narrators[0] != "Reader" {
		t.Errorf("narrators = %v, want the composer promoted", detail.Narrators)
	}
}

// TestMixedLibraryClassifiesByTags: a mixed library keeps tag classification, broadened
// by a spoken-word genre (spelled out or as ID3's numeric 183).
func TestMixedLibraryClassifiesByTags(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeUnder(t, root, "Band/Record/01.mp3", testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Record",
		Track: 1, Genre: "Fiction", Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter", Artist: "Author", Album: "Tome",
		Track: 1, Genre: "Audiobook", Audio: testaudio.AudioWithSeed(2)})
	writeUnder(t, root, "Writer/Saga/01.mp3", testaudio.MP3Spec{Title: "Chapter", Artist: "Writer", Album: "Saga",
		Track: 1, Genre: "(183)", Audio: testaudio.AudioWithSeed(3)})
	scanAll(t, sc, lib, false)
	if tracks := itemsOfKind(t, st, model.KindTrack); len(tracks) != 1 || tracks[0].Title != "Song" {
		t.Errorf("tracks = %d, want only the Fiction-tagged song", len(tracks))
	}
	if books := itemsOfKind(t, st, model.KindBook); len(books) != 2 {
		t.Errorf("books = %d, want the two spoken-word files", len(books))
	}
}

// TestM4BStaysABookInAMusicLibrary: a music library classifies by tags too, so an .m4b
// in it is still a book.
func TestM4BStaysABookInAMusicLibrary(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMusic)
	dst := filepath.Join(root, "Author", "Tome", "book.m4b")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, testaudio.Fixture(t, "sample.m4a"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)
	oneBook(t, st, 1)
}

// writeBookFolder writes three parts of "Tome" by Author under Author/Tome, the one at
// narrated (1-based) carrying a narrator credit and the rest no book signal at all.
func writeBookFolder(t *testing.T, root string, narrated int) {
	t.Helper()
	for i := 1; i <= 3; i++ {
		spec := testaudio.MP3Spec{Title: "Chapter " + string(rune('0'+i)), Artist: "Author", AlbumArtist: "Author",
			Album: "Tome", Track: i, Genre: "Fiction", Audio: testaudio.AudioWithSeed(byte(i))}
		if i == narrated {
			spec.TXXX = []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
		}
		writeUnder(t, root, "Author/Tome/0"+string(rune('0'+i))+".mp3", spec)
	}
}

// TestFolderConsensusJoinsUntaggedParts: in a mixed library, parts that state nothing
// about being a book join the book a narrated sibling in their folder makes, whichever
// order the walk reaches them in, and the counts read as one book created and two parts
// joining it.
func TestFolderConsensusJoinsUntaggedParts(t *testing.T) {
	for _, narrated := range []int{1, 3} {
		st, lib, sc, root := kindFixture(t, model.MediaMixed)
		writeBookFolder(t, root, narrated)
		res := scanAll(t, sc, lib, false)
		oneBook(t, st, 3)
		if res.ItemsCreated != 1 || res.ItemsUpdated != 2 {
			t.Errorf("narrated part %d: created %d, updated %d, want 1 and 2", narrated, res.ItemsCreated, res.ItemsUpdated)
		}
	}
}

// TestFolderBookTakesAPartWithNoAlbum: a part with no ALBUM joins its folder's book rather
// than keying a book of its own on its title, in an audiobook library and beside a
// narrated part in a mixed one, wherever it sorts.
func TestFolderBookTakesAPartWithNoAlbum(t *testing.T) {
	for _, media := range []model.MediaType{model.MediaAudiobook, model.MediaMixed} {
		st, lib, sc, root := kindFixture(t, media)
		writeUnder(t, root, "Author/Tome/00 Intro.mp3", testaudio.MP3Spec{Title: "Intro", Artist: "Author",
			Audio: testaudio.AudioWithSeed(9)})
		writeBookFolder(t, root, 2)
		scanAll(t, sc, lib, false)
		if book := oneBook(t, st, 4); book.Title != "Tome" {
			t.Errorf("%s: book = %q, want Tome", media, book.Title)
		}
	}
}

// TestSpokenWordTrackKeepsItsAlbumMusic: a Spoken Word genre names no audiobook in a mixed
// root, so a skit stays a track on its album.
func TestSpokenWordTrackKeepsItsAlbumMusic(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	for i, genre := range []string{"Rock", "Spoken Word", "Rock"} {
		writeUnder(t, root, "Band/Record/0"+string(rune('1'+i))+".mp3", testaudio.MP3Spec{Title: "Cut " + genre,
			Artist: "Band", AlbumArtist: "Band", Album: "Record", Track: i + 1, Genre: genre,
			Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	scanAll(t, sc, lib, false)
	if tracks, books := itemsOfKind(t, st, model.KindTrack), itemsOfKind(t, st, model.KindBook); len(tracks) != 3 || len(books) != 0 {
		t.Errorf("tracks %d, books %d, want the three cuts as tracks", len(tracks), len(books))
	}
}

// TestBookTitleFromItsFolder: book parts with neither an album nor a title tag key their
// book on the folder holding them, so an untagged book is one book named for its folder.
// A file straight under the root has no folder to take a name from.
func TestBookTitleFromItsFolder(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Author/Tome/02.mp3", testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	if book := oneBook(t, st, 2); book.Title != "Tome" {
		t.Errorf("book = %q, want the folder name", book.Title)
	}
	writeUnder(t, root, "Loose.mp3", testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(3)})
	scanAll(t, sc, lib, false)
	var titles []string
	for _, b := range itemsOfKind(t, st, model.KindBook) {
		titles = append(titles, b.Title)
	}
	if strings.Join(titles, ",") != "Loose,Tome" && strings.Join(titles, ",") != "Tome,Loose" {
		t.Errorf("books = %v, want Tome and a Loose book named for its file", titles)
	}
}

// TestFolderBookKeepsARetaggedPart: a part a folder's book took in keeps its place when it
// is retagged and read again on its own, the narrated part unchanged and not read.
func TestFolderBookKeepsARetaggedPart(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeBookFolder(t, root, 1)
	scanAll(t, sc, lib, false)
	before := oneBook(t, st, 3)
	writeUnder(t, root, "Author/Tome/02.mp3", testaudio.MP3Spec{Title: "Chapter Two, Revised", Artist: "Author",
		AlbumArtist: "Author", Album: "Tome", Track: 2, Genre: "Fiction", Audio: testaudio.AudioWithSeed(2)})
	res := scanAll(t, sc, lib, false)
	if after := oneBook(t, st, 3); after.PID != before.PID {
		t.Errorf("book pid moved from %s to %s", before.PID, after.PID)
	}
	if res.ItemsCreated != 0 {
		t.Errorf("created %d items, want the retagged part to stay in the book without a track in between", res.ItemsCreated)
	}
}

// TestFolderBookTakesANewPart: a part added later to a folder whose book has several parts
// joins it, the book's own files unchanged and not read.
func TestFolderBookTakesANewPart(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeBookFolder(t, root, 1)
	scanAll(t, sc, lib, false)
	writeUnder(t, root, "Author/Tome/04.mp3", testaudio.MP3Spec{Title: "Chapter 4", Artist: "Author",
		AlbumArtist: "Author", Album: "Tome", Track: 4, Genre: "Fiction", Audio: testaudio.AudioWithSeed(4)})
	res := scanAll(t, sc, lib, false)
	oneBook(t, st, 4)
	if res.ItemsCreated != 0 {
		t.Errorf("created %d items, want the new part to join the book", res.ItemsCreated)
	}
}

// TestNarratorAddedLaterGathersTheFolder: a folder cataloged as tracks becomes one book
// once one part gains a narrator credit, its unchanged siblings included.
func TestNarratorAddedLaterGathersTheFolder(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeBookFolder(t, root, 0)
	scanAll(t, sc, lib, false)
	tracks := itemsOfKind(t, st, model.KindTrack)
	if len(tracks) != 3 {
		t.Fatalf("tracks = %d before the narrator, want 3", len(tracks))
	}
	var first model.PID
	for _, tr := range tracks {
		if tr.Title == "Chapter 1" {
			first = tr.PID
		}
	}
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, Genre: "Fiction", TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}},
		Audio: testaudio.AudioWithSeed(1)})
	scanAll(t, sc, lib, false)
	if book := oneBook(t, st, 3); book.PID != first {
		t.Errorf("book pid = %s, want the narrated part's track %s turned into it", book.PID, first)
	}
}

// TestFolderBookKeepsAMovedPart: a part the folder rule took in stays in its book when it
// moves to a folder of its own, as a part whose tags name the book would.
func TestFolderBookKeepsAMovedPart(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeBookFolder(t, root, 1)
	scanAll(t, sc, lib, false)
	before := oneBook(t, st, 3)
	dst := filepath.Join(root, "Author", "Elsewhere", "02.mp3")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Author", "Tome", "02.mp3"), dst); err != nil {
		t.Fatal(err)
	}
	res := scanAll(t, sc, lib, false)
	if after := oneBook(t, st, 3); after.PID != before.PID {
		t.Errorf("book pid moved from %s to %s", before.PID, after.PID)
	}
	if res.Relinked != 1 || res.ItemsCreated != 0 {
		t.Errorf("relinked %d, created %d, want the moved part relinked into its book", res.Relinked, res.ItemsCreated)
	}
}

// TestFastPathReadsATrackInAnAudiobookLibrary: once a library is declared audiobook, an
// unchanged file cataloged there as a track is read again so the next plain scan makes it
// a book; a track whose kind is locked stays one and is not read.
func TestFastPathReadsATrackInAnAudiobookLibrary(t *testing.T) {
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter", Artist: "Author", Album: "Tome",
		Track: 1, Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Band/Record/01.mp3", testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Record",
		Track: 1, Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	var song model.PID
	for _, it := range itemsOfKind(t, st, model.KindTrack) {
		if it.Title == "Song" {
			song = it.PID
		}
	}
	if err := st.LockField(ctx, song, model.KindLockField); err != nil {
		t.Fatalf("lock kind: %v", err)
	}
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: lib.Root, DisplayRoot: lib.DisplayRoot, Mode: lib.Mode,
		Media: model.MediaAudiobook, Profile: lib.Profile})
	if err != nil {
		t.Fatalf("declare audiobook: %v", err)
	}
	res := scanAll(t, sc, lib, false)
	if books, tracks := itemsOfKind(t, st, model.KindBook), itemsOfKind(t, st, model.KindTrack); len(books) != 1 || len(tracks) != 1 || tracks[0].PID != song {
		t.Errorf("books %d, tracks %d, want Tome a book and the locked song a track", len(books), len(tracks))
	}
	if res.Reread != 1 {
		t.Errorf("re-read %d files, want only the unlocked track", res.Reread)
	}
}

// inspectingReader counts full reads apart from tag-only ones.
type inspectingReader struct {
	inner           *meta.Adapter
	reads, inspects int
}

func (r *inspectingReader) Read(ctx context.Context, path string) (*meta.FileMeta, error) {
	r.reads++
	return r.inner.Read(ctx, path)
}

func (r *inspectingReader) Inspect(ctx context.Context, path string) (*meta.FileMeta, error) {
	r.inspects++
	return r.inner.Inspect(ctx, path)
}

// TestUntaggedBooksInAnAuthorFolderStayApart: files with no album whose names are not part
// numbers are books of their own, named for their files, rather than one book named for
// the folder they share or parts of a tagged book beside them.
func TestUntaggedBooksInAnAuthorFolderStayApart(t *testing.T) {
	for _, tagged := range []bool{false, true} {
		st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
		writeUnder(t, root, "Frank Herbert/Dune.mp3", testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(1)})
		children := testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(2)}
		if tagged {
			children.Title, children.Album, children.Artist = "Children of Dune", "Children of Dune", "Frank Herbert"
		}
		writeUnder(t, root, "Frank Herbert/Children of Dune.mp3", children)
		scanAll(t, sc, lib, false)
		if books := itemsOfKind(t, st, model.KindBook); len(books) != 2 {
			t.Errorf("tagged %v: books = %d, want Dune and Children of Dune apart", tagged, len(books))
		}
	}
}

// TestTitledPartsWithoutAnAlbumAreOneBook: numbered parts that carry chapter titles and
// no album are one book named for their folder.
func TestTitledPartsWithoutAnAlbumAreOneBook(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	for i := 1; i <= 3; i++ {
		writeUnder(t, root, "Author/Tome/0"+string(rune('0'+i))+".mp3", testaudio.MP3Spec{Title: "Chapter " + string(rune('0'+i)),
			Artist: "Author", Track: i, Audio: testaudio.AudioWithSeed(byte(i))})
	}
	scanAll(t, sc, lib, false)
	if book := oneBook(t, st, 3); book.Title != "Tome" {
		t.Errorf("book = %q, want Tome", book.Title)
	}
}

// TestNewBookDoesNotRereadItsSiblings: a new book in a folder of single-file books is the
// only file a plain scan reads in full; its siblings name books of their own.
func TestNewBookDoesNotRereadItsSiblings(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged,
		Media: model.MediaAudiobook, Profile: "waxbin-native"})
	if err != nil {
		t.Fatalf("ensure lib: %v", err)
	}
	r := &inspectingReader{inner: meta.NewReader()}
	sc := New(st, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i, title := range []string{"Alpha", "Beta", "Gamma", "Delta", "Epsilon"} {
		writeUnder(t, root, "Author/"+title+".mp3", testaudio.MP3Spec{Title: title, Artist: "Author", Album: title,
			Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	scanAll(t, sc, lib, false)
	before := r.reads
	writeUnder(t, root, "Author/Zeta.mp3", testaudio.MP3Spec{Title: "Zeta", Artist: "Author", Album: "Zeta",
		Audio: testaudio.AudioWithSeed(9)})
	scanAll(t, sc, lib, false)
	if got := r.reads - before; got != 1 {
		t.Errorf("full reads = %d after adding one book, want 1", got)
	}
	if books := itemsOfKind(t, st, model.KindBook); len(books) != 6 {
		t.Errorf("books = %d, want 6", len(books))
	}
}

// TestOnePartBookTakesALaterPart: a narrated book of one part takes in a plain part that
// arrives in a later scan.
func TestOnePartBookTakesALaterPart(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(1)})
	scanAll(t, sc, lib, false)
	writeUnder(t, root, "Author/Tome/02.mp3", testaudio.MP3Spec{Title: "Chapter 2", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 2, Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	oneBook(t, st, 2)
}

// TestDiscFoldersJoinAcrossSubPathScans: disc folders scanned one at a time, as the
// watcher rescans a folder, still make one book.
func TestDiscFoldersJoinAcrossSubPathScans(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	ctx := context.Background()
	writeUnder(t, root, "Author/Tome/CD1/01.mp3", testaudio.MP3Spec{Title: "Part 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, Audio: testaudio.AudioWithSeed(1)})
	if _, err := sc.Scan(ctx, Request{Library: lib, SubPath: filepath.Join(root, "Author", "Tome", "CD1")}, nil); err != nil {
		t.Fatal(err)
	}
	writeUnder(t, root, "Author/Tome/CD2/01.mp3", testaudio.MP3Spec{Title: "Part 2", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(2)})
	if _, err := sc.Scan(ctx, Request{Library: lib, SubPath: filepath.Join(root, "Author", "Tome", "CD2")}, nil); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)
	oneBook(t, st, 2)
}

// TestMusicLibraryKeepsANarratedAlbumApart: a library declared music classifies by tags,
// so a narrated intro is a book of its own and its album's songs stay tracks.
func TestMusicLibraryKeepsANarratedAlbumApart(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMusic)
	for i := 1; i <= 4; i++ {
		spec := testaudio.MP3Spec{Title: "Song " + string(rune('0'+i)), Artist: "Band", AlbumArtist: "Band", Album: "Record",
			Track: i, Genre: "Rock", Audio: testaudio.AudioWithSeed(byte(i))}
		if i == 1 {
			spec.TXXX = []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Guest"}}
		}
		writeUnder(t, root, "Band/Record/0"+string(rune('0'+i))+".mp3", spec)
	}
	scanAll(t, sc, lib, false)
	if tracks := itemsOfKind(t, st, model.KindTrack); len(tracks) != 3 {
		t.Errorf("tracks = %d, want the three songs kept as tracks", len(tracks))
	}
}

// TestRipBesideABookStaysARip: a single-file rip whose sheet carves it stays a rip beside
// a narrated book in its folder, though its file names no album and a number.
func TestRipBesideABookStaysARip(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeUnder(t, root, "Shelf/a-book.mp3", testaudio.MP3Spec{Title: "Ch 1", Artist: "Author", Album: "Tome",
		TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Shelf/CD1.mp3", testaudio.MP3Spec{Title: "Whole", Artist: "Band", Audio: testaudio.AudioWithSeed(2)})
	writeCue(t, filepath.Join(root, "Shelf", "CD1.cue"), strings.Replace(twoTrackRipCue, "album.mp3", "CD1.mp3", 1))
	scanAll(t, sc, lib, false)
	if tracks := itemsOfKind(t, st, model.KindTrack); len(tracks) != 2 {
		t.Errorf("tracks = %d, want the rip's two virtual tracks", len(tracks))
	}
}

// TestFolderSettleLeavesAnErroredFile: a file the walk could not read is not read again
// by a folder settle, so its error is its only count.
func TestFolderSettleLeavesAnErroredFile(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeUnder(t, root, "Author/Tome/02.mp3", testaudio.MP3Spec{Title: "Chapter 2", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 2, Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(1)})
	failing := filepath.Join(root, "Author", "Tome", "02.mp3")
	once := true
	sc.stat = func(path string) (fs.FileInfo, error) {
		if path == failing && once {
			once = false
			return nil, fs.ErrPermission
		}
		return os.Stat(path)
	}
	res := scanAll(t, sc, lib, false)
	if res.Errored != 1 || res.Unchanged < 0 {
		t.Errorf("errored %d, unchanged %d, want the failed read counted once", res.Errored, res.Unchanged)
	}
	if tracks := itemsOfKind(t, st, model.KindTrack); len(tracks) != 1 {
		t.Errorf("tracks = %d, want the unread part left as it was", len(tracks))
	}
}

// TestPromotedPartLeavesTheBookTitle: a part the folder rule took in, promoted to primary
// when the narrated part is trashed, owns the book's metadata but never its title, since
// its tags name no book.
func TestPromotedPartLeavesTheBookTitle(t *testing.T) {
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(1)})
	for i := 2; i <= 3; i++ {
		writeUnder(t, root, "Author/Tome/0"+string(rune('0'+i))+".mp3", testaudio.MP3Spec{Title: "Chapter " + string(rune('0'+i)),
			Artist: "Author", Track: i, Audio: testaudio.AudioWithSeed(byte(i))})
	}
	scanAll(t, sc, lib, false)
	book := oneBook(t, st, 3)
	first, err := st.FileByPath(ctx, []byte(filepath.Join(root, "Author", "Tome", "01.mp3")))
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	res, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: first.PID, TrashPath: []byte("/trash/01.mp3"), TrashDisplay: "/trash/01.mp3"})
	if err != nil || len(res.Promoted) != 1 {
		t.Fatalf("trash = %+v (err %v), want a part promoted", res, err)
	}
	sc.RereadPromoted(ctx, res.Promoted)
	if after := oneBook(t, st, 2); after.PID != book.PID || after.Title != "Tome" {
		t.Errorf("book = %s %q, want %s still titled Tome", after.PID, after.Title, book.PID)
	}
}

// TestFolderRuleIgnoresWalkOrder: a numbered part kept in the one-part book its folder
// named still joins the book a tagged part read after it in the same walk makes, as it
// would had the walk reached the tagged part first.
func TestFolderRuleIgnoresWalkOrder(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	writeUnder(t, root, "Tome/01.mp3", testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(1)})
	scanAll(t, sc, lib, false)
	writeUnder(t, root, "Tome/01.mp3", testaudio.MP3Spec{Track: 1, Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Tome/02.mp3", testaudio.MP3Spec{Title: "Chapter 2", Artist: "Writer", Album: "Tome", Track: 2,
		Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	oneBook(t, st, 2)
}

// TestFolderSettleSkipsPartsAlreadyIn: a forced rescan of a book whose narrated part sorts
// last reads each part once; the numbered parts with no album it kept in the book are not
// read again when the narrated part names the same book.
func TestFolderSettleSkipsPartsAlreadyIn(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged, Profile: "waxbin-native"})
	if err != nil {
		t.Fatalf("ensure lib: %v", err)
	}
	r := &inspectingReader{inner: meta.NewReader()}
	sc := New(st, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := 1; i <= 2; i++ {
		writeUnder(t, root, "Author/Tome/0"+string(rune('0'+i))+".mp3", testaudio.MP3Spec{Title: "Chapter " + string(rune('0'+i)),
			Artist: "Author", Track: i, Audio: testaudio.AudioWithSeed(byte(i))})
	}
	writeUnder(t, root, "Author/Tome/03.mp3", testaudio.MP3Spec{Title: "Chapter 3", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 3, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(3)})
	scanAll(t, sc, lib, false)
	oneBook(t, st, 3)
	before := r.reads
	scanAll(t, sc, lib, true)
	if got := r.reads - before; got != 3 {
		t.Errorf("full reads = %d on a forced rescan of three parts, want 3", got)
	}
	oneBook(t, st, 3)
}

// TestSplitPartsNumberedAlikeStayParts: an audiobook a splitter cut into equal parts and
// tagged track 1 on every file is still one book of every part, not one part and its
// "encodings".
func TestSplitPartsNumberedAlikeStayParts(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	for i := 1; i <= 4; i++ {
		writeUnder(t, root, "Austen/Emma/Emma - Part "+string(rune('0'+i))+".mp3", testaudio.MP3Spec{
			Title: "Emma", Artist: "Jane Austen", AlbumArtist: "Jane Austen", Album: "Emma", Track: 1,
			Audio: testaudio.AudioWithSeed(byte(20 + i))})
	}
	scanAll(t, sc, lib, false)
	oneBook(t, st, 4)
}
