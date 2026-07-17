package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// rescanTrack re-persists a track under a fixed identity (same essence) with a fresh
// content hash so the store's entity-resolution gate fires, simulating a
// `scan --force` re-derive from disk. preserveLocks toggles the lock overlay.
func rescanTrack(t *testing.T, st *Store, libID int64, s trackSpec, preserveLocks bool) {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(s.path), DisplayPath: s.path, RelPath: []byte(s.path),
			Kind: model.FileAudio, Size: int64(len(s.content)), MTimeNS: 2,
			ContentHash: s.content, EssenceHash: s.essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: s.title,
			SortKey: model.SortKey(s.title), IdentityKey: "essence:" + s.essence,
		},
		Track: model.Track{
			Artist: s.artist, ArtistSort: model.SortKey(s.artist), Album: s.album,
			AlbumArtist: s.albumArt, Composer: s.composer, Genre: s.genre,
			Genres: identity.SplitGenres(s.genre), Year: s.year,
		},
		PreserveLocks: preserveLocks,
	}
	if _, err := st.PutScannedTrack(context.Background(), in); err != nil {
		t.Fatalf("rescan %s: %v", s.path, err)
	}
}

func TestScanForcePreservesLockedTrackFields(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	orig := trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1",
		title: "Original", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
	}
	putTrack(t, st, lib.ID, orig)
	pid := itemPID(t, st)

	// Curate a rename, re-artist, re-genre, and an identifier; all locked.
	edits := map[string]string{"title": "Renamed", "artist": "Beta", "genre": "Jazz", "isrc": "USRC17607839"}
	if err := st.EditItemFields(ctx, pid, edits, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	// A forced rescan re-derives from the ORIGINAL on-disk tags (fresh content hash so
	// the entity path fires). The locked fields must survive.
	forced := orig
	forced.content = "c2"
	rescanTrack(t, st, lib.ID, forced, true)

	var title, artist, genre, isrc, artistEntity string
	if err := st.read.QueryRowContext(ctx, `
		SELECT pi.title, t.artist, t.genre, t.isrc, COALESCE(a.name,'')
		FROM playable_item pi JOIN track t ON t.item_id=pi.id
		LEFT JOIN artist a ON a.id=t.artist_id WHERE pi.pid=?`, string(pid)).
		Scan(&title, &artist, &genre, &isrc, &artistEntity); err != nil {
		t.Fatalf("read: %v", err)
	}
	if title != "Renamed" || artist != "Beta" || genre != "Jazz" || isrc != "USRC17607839" {
		t.Fatalf("locked fields not preserved: title=%q artist=%q genre=%q isrc=%q", title, artist, genre, isrc)
	}
	// The denormalized column and the re-resolved entity FK agree (the delicate case).
	if artistEntity != "Beta" {
		t.Fatalf("artist entity FK = %q, want Beta (column/FK diverged)", artistEntity)
	}
	// The genre link points at the curated genre, not the re-derived one.
	var genreName string
	if err := st.read.QueryRowContext(ctx, `SELECT g.name FROM item_genre ig JOIN genre g ON g.id=ig.genre_id
		JOIN playable_item pi ON pi.id=ig.item_id WHERE pi.pid=?`, string(pid)).Scan(&genreName); err != nil {
		t.Fatalf("read genre link: %v", err)
	}
	if genreName != "Jazz" {
		t.Fatalf("genre link = %q, want Jazz", genreName)
	}
	// Derived data (rollups, FTS, sort keys) stays consistent after the preserving rescan.
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Fatalf("db verify not clean after preserving rescan: %+v (err %v)", r, err)
	}

	// --ignore-locks (PreserveLocks=false) re-derives everything from disk.
	ignore := orig
	ignore.content = "c3"
	rescanTrack(t, st, lib.ID, ignore, false)
	if err := st.read.QueryRowContext(ctx,
		"SELECT pi.title, t.artist, t.genre FROM playable_item pi JOIN track t ON t.item_id=pi.id WHERE pi.pid=?",
		string(pid)).Scan(&title, &artist, &genre); err != nil {
		t.Fatalf("read after ignore-locks: %v", err)
	}
	if title != "Original" || artist != "Alpha" || genre != "Rock" {
		t.Fatalf("ignore-locks did not re-derive: title=%q artist=%q genre=%q", title, artist, genre)
	}
}

func TestScanForcePreservesLockedCredits(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// A track composer set via the credit API locks credit.composer and writes the
	// track.composer denorm. A forced rescan re-derives the denorm from disk, so the
	// overlay must preserve it or show/credit would diverge.
	orig := trackSpec{
		path: "/lib/A/1/01.flac", essence: "e1", content: "c1",
		title: "One", artist: "Alpha", albumArt: "Alpha", album: "One", composer: "Disk Composer",
	}
	putTrack(t, st, lib.ID, orig)
	tpid := itemPID(t, st)
	if _, err := st.SetItemCredits(ctx, tpid, model.RoleComposer, []string{"Curated Composer"}, model.SourceUser, true, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	forced := orig
	forced.content = "c2"
	rescanTrack(t, st, lib.ID, forced, true)
	var composer string
	if err := st.read.QueryRowContext(ctx,
		"SELECT composer FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?", string(tpid)).Scan(&composer); err != nil {
		t.Fatalf("read composer: %v", err)
	}
	if composer != "Curated Composer" {
		t.Fatalf("track.composer after rescan = %q, want Curated Composer (credit.composer lock)", composer)
	}

	// A book translator set via the credit API locks credit.translator. A forced rescan
	// runs resolveContributors (which wipes all roles); the overlay must re-supply the
	// translator or it vanishes entirely.
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"},
	})
	var bpid string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='book' LIMIT 1").Scan(&bpid); err != nil {
		t.Fatalf("book pid: %v", err)
	}
	if _, err := st.SetItemCredits(ctx, model.PID(bpid), model.RoleTranslator, []string{"Terry Translator"}, model.SourceUser, true, false); err != nil {
		t.Fatalf("set translator credit: %v", err)
	}
	rescanBookForce(t, st, lib.ID, "be1", "bc2")
	credits, err := st.ItemCredits(ctx, model.PID(bpid))
	if err != nil {
		t.Fatalf("read credits: %v", err)
	}
	found := false
	for _, c := range credits {
		if c.Role == model.RoleTranslator && c.Name == "Terry Translator" {
			found = true
		}
	}
	if !found {
		t.Fatalf("translator credit lost after rescan: %+v", credits)
	}
}

func TestScanForcePreservesLockedBookFields(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"},
		series: "The Series", seq: "1", genres: []string{"Fantasy"}, year: 2010,
	})
	var pid string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='book' LIMIT 1").Scan(&pid); err != nil {
		t.Fatalf("book pid: %v", err)
	}
	bpid := model.PID(pid)

	if err := st.EditItemFields(ctx, bpid, map[string]string{"author": "Mary Writer", "publisher": "Recorded Books"},
		model.SourceUser, true, false); err != nil {
		t.Fatalf("edit book: %v", err)
	}

	// Forced rescan with the original tags and a fresh content hash.
	rescanBookForce(t, st, lib.ID, "be1", "bc2")

	v, err := st.ItemByPID(ctx, bpid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.Artist != "Mary Writer" {
		t.Fatalf("locked author not preserved: %q", v.Artist)
	}
	var publisher string
	if err := st.read.QueryRowContext(ctx,
		"SELECT publisher FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?", pid).Scan(&publisher); err != nil {
		t.Fatalf("read publisher: %v", err)
	}
	if publisher != "Recorded Books" {
		t.Fatalf("locked publisher not preserved: %q", publisher)
	}
}

// rescanBookForce re-persists the single-file book under a fresh content hash with
// its original tags and PreserveLocks on.
func rescanBookForce(t *testing.T, st *Store, libID int64, essence, content string) {
	t.Helper()
	in := model.PutScannedBookInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte("/lib/Author/Book/book.m4b"), DisplayPath: "/lib/Author/Book/book.m4b",
			RelPath: []byte("book.m4b"), Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 2,
			ContentHash: content, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: "The Book",
			SortKey: model.SortKey("The Book"), IdentityKey: identity.BookKey("", "", "Jane Author", "The Book", ""),
		},
		Book: model.Book{
			Author: "Jane Author", Authors: []string{"Jane Author"},
			Narrators: []string{"Ned Narrator"}, Narrator: "Ned Narrator",
			Series: "The Series", SeriesSeq: "1", Genre: "Fantasy",
			Genres: []string{"Fantasy"}, Year: 2010,
		},
		PreserveLocks: true,
	}
	if _, err := st.PutScannedBook(context.Background(), in); err != nil {
		t.Fatalf("rescan book: %v", err)
	}
}
