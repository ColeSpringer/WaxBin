package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// entityPIDByName resolves an entity table row's pid by its display column, for
// asserting against the pids EntityByPID is looked up with.
func entityPIDByName(t *testing.T, st *Store, table, nameCol, name string) model.PID {
	t.Helper()
	var pid string
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT pid FROM "+table+" WHERE "+nameCol+" = ?", name).Scan(&pid); err != nil {
		t.Fatalf("pid of %s %q: %v", table, name, err)
	}
	return model.PID(pid)
}

// entityInfoFixture catalogs two Radiohead tracks (one artist, one release
// group, one album, one genre) plus a two-part series book by a different
// author, the members every kind's lookup is asserted against.
func entityInfoFixture(t *testing.T) (*Store, *model.Library) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Radiohead/OK Computer/01.flac", essence: "e1", content: "c1",
		title: "Airbag", artist: "Radiohead", album: "OK Computer", genre: "Rock",
		year: 1997, durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Radiohead/OK Computer/02.flac", essence: "e2", content: "c2",
		title: "Paranoid Android", artist: "Radiohead", album: "OK Computer", genre: "Rock",
		year: 1997, durationMS: 250,
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/Hobbit/hobbit.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", series: "Middle-earth", seq: "0",
		asin: "B0000000A1", durationMS: 3000,
	})
	return st, lib
}

func TestEntityByPIDAllKinds(t *testing.T) {
	st, lib := entityInfoFixture(t)
	ctx := context.Background()

	artistPID := entityPIDByName(t, st, "artist", "name", "Radiohead")
	rgPID := entityPIDByName(t, st, "release_group", "title", "OK Computer")
	albumPID := entityPIDByName(t, st, "album", "title", "OK Computer")
	genrePID := entityPIDByName(t, st, "genre", "name", "Rock")
	seriesPID := entityPIDByName(t, st, "series", "name", "Middle-earth")

	artist, err := st.EntityByPID(ctx, read.EntityArtist, artistPID)
	if err != nil {
		t.Fatalf("artist: %v", err)
	}
	if artist.Name != "Radiohead" || artist.ItemCount != 2 || artist.ReleaseGroupCount != 1 ||
		artist.TotalDurationMS != 350 {
		t.Errorf("artist = %+v, want 2 items, 1 release group, 350ms", artist)
	}
	if artist.SortKey == "" {
		t.Error("artist sort key missing")
	}

	rg, err := st.EntityByPID(ctx, read.EntityReleaseGroup, rgPID)
	if err != nil {
		t.Fatalf("release group: %v", err)
	}
	if rg.Name != "OK Computer" || rg.ItemCount != 2 || rg.TotalDurationMS != 350 {
		t.Errorf("release group = %+v, want 2 items, 350ms", rg)
	}
	if rg.ArtistPID != artistPID {
		t.Errorf("release group artist link = %s, want %s", rg.ArtistPID, artistPID)
	}
	if rg.Type != "album" {
		t.Errorf("release group type = %q, want album", rg.Type)
	}

	album, err := st.EntityByPID(ctx, read.EntityAlbum, albumPID)
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if album.Name != "OK Computer" || album.Year != 1997 {
		t.Errorf("album = %+v, want OK Computer 1997", album)
	}
	if album.ReleaseGroupPID != rgPID {
		t.Errorf("album release-group link = %s, want %s", album.ReleaseGroupPID, rgPID)
	}
	if album.ItemCount != 2 || album.TotalDurationMS != 350 {
		t.Errorf("album live counts = %d items %dms, want 2 items 350ms", album.ItemCount, album.TotalDurationMS)
	}

	genre, err := st.EntityByPID(ctx, read.EntityGenre, genrePID)
	if err != nil {
		t.Fatalf("genre: %v", err)
	}
	if genre.Name != "Rock" || genre.ItemCount != 2 || genre.TotalDurationMS != 350 {
		t.Errorf("genre = %+v, want 2 items, 350ms", genre)
	}
	if genre.MBID != "" {
		t.Errorf("genre mbid = %q, want empty (genres carry no external id)", genre.MBID)
	}

	series, err := st.EntityByPID(ctx, read.EntitySeries, seriesPID)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if series.Name != "Middle-earth" || series.ItemCount != 1 || series.TotalDurationMS != 3000 {
		t.Errorf("series = %+v, want 1 book, 3000ms (the maintained parts sum)", series)
	}

	// Every kind resolved members through the fixture library.
	for _, info := range []*read.EntityInfo{artist, rg, album, genre, series} {
		if len(info.LibraryPIDs) != 1 || info.LibraryPIDs[0] != lib.PID {
			t.Errorf("%s libraries = %v, want [%s]", info.Kind, info.LibraryPIDs, lib.PID)
		}
	}
}

// TestEntityByPIDMatchesFacet pins the lookup's counts to the facet the pid came
// from: an artist bucket's count and its EntityByPID ItemCount must agree.
func TestEntityByPIDMatchesFacet(t *testing.T) {
	st, _ := entityInfoFixture(t)
	ctx := context.Background()

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupArtist, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	var checked int
	for _, b := range res.Buckets {
		if b.EntityPID == "" || b.Display != "Radiohead" {
			continue
		}
		info, err := st.EntityByPID(ctx, read.EntityArtist, b.EntityPID)
		if err != nil {
			t.Fatalf("lookup of facet pid %s: %v", b.EntityPID, err)
		}
		if info.ItemCount != b.Count {
			t.Errorf("artist %s: facet count %d, entity count %d; must agree", b.Display, b.Count, info.ItemCount)
		}
		checked++
	}
	if checked != 1 {
		t.Fatalf("checked %d artist buckets, want the Radiohead one", checked)
	}
}

// TestEntityLibraryPIDsSpanLibraries verifies membership resolves libraries per
// member: an artist backing items in two libraries reports both, and a book
// counts under its author (the facet membership rule).
func TestEntityLibraryPIDsSpanLibraries(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/other"), DisplayRoot: "/other", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("second library: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1",
		title: "One", artist: "Spread", album: "Alp"})
	putTrack(t, st, lib2.ID, trackSpec{path: "/other/b.flac", essence: "e2", content: "c2",
		title: "Two", artist: "Spread", album: "Bet"})
	putBook(t, st, lib2.ID, bookSpec{path: "/other/book.m4b", essence: "be1", content: "bc1",
		title: "Memoir", author: "Lone Author", durationMS: 100})

	spread, err := st.EntityByPID(ctx, read.EntityArtist, entityPIDByName(t, st, "artist", "name", "Spread"))
	if err != nil {
		t.Fatalf("artist: %v", err)
	}
	if len(spread.LibraryPIDs) != 2 || spread.LibraryPIDs[0] != lib.PID || spread.LibraryPIDs[1] != lib2.PID {
		t.Errorf("spread libraries = %v, want [%s %s] in library order", spread.LibraryPIDs, lib.PID, lib2.PID)
	}

	// The author artist has no tracks, so the rollup count is zero, but the
	// authored book still places the artist in its library (facet membership).
	author, err := st.EntityByPID(ctx, read.EntityArtist, entityPIDByName(t, st, "artist", "name", "Lone Author"))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if len(author.LibraryPIDs) != 1 || author.LibraryPIDs[0] != lib2.PID {
		t.Errorf("author libraries = %v, want [%s] via the authored book", author.LibraryPIDs, lib2.PID)
	}
}

func TestEntityByPIDUnknown(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	if _, err := st.EntityByPID(ctx, "podcast", "x"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown kind = %v, want CodeInvalid", err)
	}
	if _, err := st.EntityByPID(ctx, read.EntityArtist, "missing"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown pid = %v, want CodeNotFound", err)
	}
}
