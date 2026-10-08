package sqlite

import (
	"context"
	"fmt"
	"slices"
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
			Genres: identity.SplitGenres(s.genre), Year: s.year, BPM: s.bpm,
		},
		PreserveLocks: preserveLocks,
	}
	if _, err := st.PutScannedTrack(context.Background(), in); err != nil {
		t.Fatalf("rescan %s: %v", s.path, err)
	}
}

// TestScanForcePreservesLockedBPM covers the numeric column the tags also carry: a
// curated bpm has to outlive a forced rescan whose file states a different number,
// and go back to the file's value once the lock is off.
func TestScanForcePreservesLockedBPM(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	orig := trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1",
		title: "Original", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock", bpm: 90,
	}
	putTrack(t, st, lib.ID, orig)
	pid := itemPID(t, st)

	if err := st.EditItemField(ctx, pid, "bpm", "128", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit bpm: %v", err)
	}

	retagged := orig
	retagged.content, retagged.bpm = "c2", 96
	rescanTrack(t, st, lib.ID, retagged, true)

	readBPM := func() int {
		t.Helper()
		var bpm int
		if err := st.rdb().QueryRowContext(ctx,
			"SELECT COALESCE(t.bpm,0) FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
			string(pid)).Scan(&bpm); err != nil {
			t.Fatalf("read bpm: %v", err)
		}
		return bpm
	}
	if got := readBPM(); got != 128 {
		t.Fatalf("bpm after preserving rescan = %d, want the locked 128", got)
	}

	ignore := retagged
	ignore.content = "c3"
	rescanTrack(t, st, lib.ID, ignore, false)
	if got := readBPM(); got != 96 {
		t.Fatalf("bpm after --ignore-locks = %d, want the file's 96", got)
	}
}

func TestScanForcePreservesLockedTrackFields(t *testing.T) {
	t.Parallel()
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
	if err := st.EditItemFields(ctx, pid, edits, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	// A forced rescan re-derives from the ORIGINAL on-disk tags (fresh content hash so
	// the entity path fires). The locked fields must survive.
	forced := orig
	forced.content = "c2"
	rescanTrack(t, st, lib.ID, forced, true)

	var title, artist, genre, isrc, artistEntity string
	if err := st.rdb().QueryRowContext(ctx, `
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
	if err := st.rdb().QueryRowContext(ctx, `SELECT g.name FROM item_genre ig JOIN genre g ON g.id=ig.genre_id
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
	if err := st.rdb().QueryRowContext(ctx,
		"SELECT pi.title, t.artist, t.genre FROM playable_item pi JOIN track t ON t.item_id=pi.id WHERE pi.pid=?",
		string(pid)).Scan(&title, &artist, &genre); err != nil {
		t.Fatalf("read after ignore-locks: %v", err)
	}
	if title != "Original" || artist != "Alpha" || genre != "Rock" {
		t.Fatalf("ignore-locks did not re-derive: title=%q artist=%q genre=%q", title, artist, genre)
	}
}

func TestScanForcePreservesLockedCredits(t *testing.T) {
	t.Parallel()
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
	if _, _, err := st.SetItemCredits(ctx, tpid, model.RoleComposer, []string{"Curated Composer"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	forced := orig
	forced.content = "c2"
	rescanTrack(t, st, lib.ID, forced, true)
	var composer string
	if err := st.rdb().QueryRowContext(ctx,
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
	if err := st.rdb().QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='book' LIMIT 1").Scan(&bpid); err != nil {
		t.Fatalf("book pid: %v", err)
	}
	if _, _, err := st.SetItemCredits(ctx, model.PID(bpid), model.RoleTranslator, []string{"Terry Translator"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
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
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"},
		series: "The Series", seq: "1", genres: []string{"Fantasy"}, year: 2010,
	})
	var pid string
	if err := st.rdb().QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='book' LIMIT 1").Scan(&pid); err != nil {
		t.Fatalf("book pid: %v", err)
	}
	bpid := model.PID(pid)

	if err := st.EditItemFields(ctx, bpid, map[string]string{"author": "Mary Writer", "publisher": "Recorded Books"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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
	if err := st.rdb().QueryRowContext(ctx,
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

// forcedRescan re-puts a track exactly as putTrack first wrote it, under the same
// content hash, the way `scan --force` re-reads a file that has not changed.
func forcedRescan(t *testing.T, st *Store, libID int64, s trackSpec) *model.ScanItemResult {
	t.Helper()
	in := trackSpecInput(libID, s)
	in.PreserveLocks = true
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("forced rescan %s: %v", s.path, err)
	}
	return res
}

// itemChangesSince counts the item delta rows written for pid after seq.
func itemChangesSince(t *testing.T, st *Store, pid model.PID, seq int64) int {
	t.Helper()
	return scalarInt(t, st, `SELECT COUNT(*) FROM change_log
		WHERE seq > ? AND entity_type = 'item' AND entity_pid = ?`, seq, string(pid))
}

func provenanceRows(t *testing.T, st *Store, pid model.PID, field string) int {
	t.Helper()
	return scalarInt(t, st, `SELECT COUNT(*) FROM field_provenance fp
		JOIN playable_item pi ON pi.id = fp.item_id WHERE pi.pid = ? AND fp.field = ?`, string(pid), field)
}

func itemGenreNames(t *testing.T, st *Store, pid model.PID) []string {
	t.Helper()
	rows, err := st.rdb().QueryContext(context.Background(), `SELECT g.name FROM item_genre ig
		JOIN genre g ON g.id = ig.genre_id JOIN playable_item pi ON pi.id = ig.item_id
		WHERE pi.pid = ? ORDER BY g.name`, string(pid))
	if err != nil {
		t.Fatalf("genre links: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan genre link: %v", err)
		}
		out = append(out, name)
	}
	return out
}

func trackColumn(t *testing.T, st *Store, pid model.PID, col string) string {
	t.Helper()
	var v string
	if err := st.rdb().QueryRowContext(context.Background(), "SELECT COALESCE(t."+col+", '') FROM track t "+
		"JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?", string(pid)).Scan(&v); err != nil {
		t.Fatalf("read track.%s: %v", col, err)
	}
	return v
}

func assertDerivedClean(t *testing.T, st *Store) {
	t.Helper()
	if r, err := st.VerifyDerived(context.Background()); err != nil || !r.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", r, err)
	}
}

// TestForcedRescanRederivesUnlockedEditedField: an unlocked catalog-only edit is
// re-derived from the file by a forced rescan of the unchanged file, and everything that
// hangs off the field follows the column: the genre links, the provenance row, and one
// item delta.
func TestForcedRescanRederivesUnlockedEditedField(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Hip-Hop/Rap",
	}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	if err := st.EditItemField(ctx, pid, "genre", "Hip Hop", model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	seq, _ := st.LatestChangeSeq(ctx)

	res := forcedRescan(t, st, lib.ID, spec)

	if got := trackColumn(t, st, pid, "genre"); got != "Hip-Hop/Rap" {
		t.Fatalf("track.genre = %q, want the file's Hip-Hop/Rap", got)
	}
	if got := itemGenreNames(t, st, pid); !slices.Equal(got, []string{"Hip-Hop", "Rap"}) {
		t.Fatalf("genre links = %v, want [Hip-Hop Rap] from the file", got)
	}
	if n := provenanceRows(t, st, pid, "genre"); n != 0 {
		t.Fatalf("genre provenance rows = %d, want 0 once the edit is re-derived", n)
	}
	if n := itemChangesSince(t, st, pid, seq); n != 1 {
		t.Fatalf("item deltas = %d, want 1", n)
	}
	if !res.MetadataChanged {
		t.Fatal("MetadataChanged = false, want true for a re-derived edit")
	}
	assertDerivedClean(t, st)
}

// TestForcedRescanKeepsWrittenBackEdit: an edit the write-back put into the file is what
// the file now says, so a forced rescan has nothing to re-derive and stays silent.
func TestForcedRescanKeepsWrittenBackEdit(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Hip-Hop/Rap",
	}
	first := putTrack(t, st, lib.ID, spec)
	pid := first.ItemPID
	if err := st.EditItemField(ctx, pid, "genre", "Hip Hop", model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	// The write-back rewrote the file and recorded its new state, as writeBackFiles does.
	if ok, err := st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID: first.FilePID, ExpectedSize: 2, ExpectedMTimeNS: 1,
		NewSize: 2, NewMTimeNS: 1, NewContentHash: "c2",
	}); err != nil || !ok {
		t.Fatalf("record written file: ok=%v err=%v", ok, err)
	}
	written := spec
	written.content, written.genre = "c2", "Hip Hop"
	seq, _ := st.LatestChangeSeq(ctx)

	res := forcedRescan(t, st, lib.ID, written)

	if res.MetadataChanged {
		t.Fatal("MetadataChanged = true, want false when the file carries the edit")
	}
	if seq2, _ := st.LatestChangeSeq(ctx); seq2 != seq {
		t.Fatalf("forced rescan of a written-back file emitted %d change rows", seq2-seq)
	}
	prov, err := st.FieldProvenance(ctx, pid)
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	var kept bool
	for _, p := range prov {
		if p.Field == "genre" && p.Source == model.SourceUser && p.Value == "Hip Hop" {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("genre provenance = %+v, want the user row kept", prov)
	}
}

// TestForcedRescanHealsStaleProvenance: a column that already holds the file's value
// under a provenance row naming an older edit (what a forced rescan left before the
// re-derive rule) is healed: the row goes and the links are resolved from the column.
func TestForcedRescanHealsStaleProvenance(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Hip-Hop/Rap",
	}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	if err := st.EditItemField(ctx, pid, "genre", "Hip Hop", model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	if _, err := st.wdb().ExecContext(ctx, `UPDATE track SET genre = 'Hip-Hop/Rap'
		WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)`, string(pid)); err != nil {
		t.Fatalf("stage the stale column: %v", err)
	}
	seq, _ := st.LatestChangeSeq(ctx)

	res := forcedRescan(t, st, lib.ID, spec)

	if n := provenanceRows(t, st, pid, "genre"); n != 0 {
		t.Fatalf("genre provenance rows = %d, want the stale row deleted", n)
	}
	if got := itemGenreNames(t, st, pid); !slices.Equal(got, []string{"Hip-Hop", "Rap"}) {
		t.Fatalf("genre links = %v, want [Hip-Hop Rap] resolved from the column", got)
	}
	if n := itemChangesSince(t, st, pid, seq); n != 1 {
		t.Fatalf("item deltas = %d, want 1", n)
	}
	if !res.MetadataChanged {
		t.Fatal("MetadataChanged = false, want true for a healed row")
	}
	assertDerivedClean(t, st)
}

// TestForcedRescanClearsStaleCustomTagProvenance: an unlocked custom tag the scan
// replaces or drops loses its provenance row with it, since the row would otherwise
// describe a value the item no longer holds.
func TestForcedRescanClearsStaleCustomTagProvenance(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	onDisk := map[string][]string{"RELEASESTATUS": {"official"}}
	pid := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One", onDisk, true).ItemPID
	user := model.Attribution{Source: model.SourceUser}
	if _, _, err := st.SetItemTag(ctx, pid, "RELEASESTATUS", []string{"unofficial"}, user, model.LockUnchanged, false); err != nil {
		t.Fatalf("set RELEASESTATUS: %v", err)
	}
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"chill"}, user, model.LockUnchanged, false); err != nil {
		t.Fatalf("set MOOD: %v", err)
	}

	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One", onDisk, true)

	if got := tagValues(t, st, pid, "RELEASESTATUS"); !slices.Equal(got, []string{"official"}) {
		t.Fatalf("RELEASESTATUS = %v, want the file's [official]", got)
	}
	if got := tagValues(t, st, pid, "MOOD"); got != nil {
		t.Fatalf("MOOD = %v, want it dropped with the file lacking it", got)
	}
	for _, field := range []string{"tag.RELEASESTATUS", "tag.MOOD"} {
		if n := provenanceRows(t, st, pid, field); n != 0 {
			t.Fatalf("%s provenance rows = %d, want 0", field, n)
		}
	}
}

// TestForcedRescanClearsCatalogOnlyComposerCredit: a composer credit set without a lock
// is re-derived like the scalar it denormalizes into, so the credit list, its provenance
// row, and the column agree with the file afterwards.
func TestForcedRescanClearsCatalogOnlyComposerCredit(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One", composer: "Disk Composer",
	}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Curated Composer"},
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	seq, _ := st.LatestChangeSeq(ctx)

	res := forcedRescan(t, st, lib.ID, spec)

	if got := trackColumn(t, st, pid, "composer"); got != "Disk Composer" {
		t.Fatalf("track.composer = %q, want Disk Composer", got)
	}
	credits, err := st.ItemCredits(ctx, pid)
	if err != nil {
		t.Fatalf("credits: %v", err)
	}
	for _, c := range credits {
		if c.Role == model.RoleComposer {
			t.Fatalf("composer credit %q survived the re-derive: %+v", c.Name, credits)
		}
	}
	if n := provenanceRows(t, st, pid, "credit.composer"); n != 0 {
		t.Fatalf("credit.composer provenance rows = %d, want 0", n)
	}
	if n := itemChangesSince(t, st, pid, seq); n != 1 {
		t.Fatalf("item deltas = %d, want 1", n)
	}
	if !res.MetadataChanged {
		t.Fatal("MetadataChanged = false, want true")
	}
	assertDerivedClean(t, st)
}

// TestForcedRescanRederivesBookTitle: a book's title is rewritten by every put, so an
// unlocked catalog-only title edit reverts on a forced rescan, and the search row, the
// provenance row and the delta have to revert with it.
func TestForcedRescanRederivesBookTitle(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := bookSpec{
		path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"},
	}
	pid := putBook(t, st, lib.ID, spec).ItemPID
	if err := st.EditItemField(ctx, pid, "title", "Edited Title", model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit title: %v", err)
	}
	seq, _ := st.LatestChangeSeq(ctx)

	spec.preserveLocks = true
	res := putBook(t, st, lib.ID, spec)

	var title, ftsTitle string
	if err := st.rdb().QueryRowContext(ctx, `SELECT pi.title, f.title FROM playable_item pi
		JOIN search_fts f ON f.rowid = pi.id WHERE pi.pid = ?`, string(pid)).Scan(&title, &ftsTitle); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if title != "The Book" || ftsTitle != "The Book" {
		t.Fatalf("title = %q, search title = %q, want both The Book", title, ftsTitle)
	}
	if n := provenanceRows(t, st, pid, "title"); n != 0 {
		t.Fatalf("title provenance rows = %d, want 0", n)
	}
	if n := itemChangesSince(t, st, pid, seq); n != 1 {
		t.Fatalf("item deltas = %d, want 1", n)
	}
	if !res.MetadataChanged {
		t.Fatal("MetadataChanged = false, want true")
	}
}

// TestIgnoreLocksRescanKeepsLockDropsValue: --ignore-locks re-derives a locked field
// from the file, discarding the curated value. The lock itself stays, now over the
// file's value, so the row must stop naming the value it no longer holds.
func TestIgnoreLocksRescanKeepsLockDropsValue(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	spec := trackSpec{
		path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock",
	}
	in := trackSpecInput(lib.ID, spec)
	in.CustomTags = map[string][]string{"MOOD": {"happy"}}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	pid := res.ItemPID
	if err := st.EditItemField(ctx, pid, "genre", "Jazz", user, model.LockOn, false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"chill"}, user, model.LockOn, false); err != nil {
		t.Fatalf("set MOOD: %v", err)
	}

	in.PreserveLocks = false
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("ignore-locks rescan: %v", err)
	}

	if got := trackColumn(t, st, pid, "genre"); got != "Rock" {
		t.Fatalf("genre = %q, want the file's Rock", got)
	}
	prov, err := st.FieldProvenance(ctx, pid)
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	want := map[string]bool{"genre": true, "tag.MOOD": true}
	for _, p := range prov {
		if !want[p.Field] {
			continue
		}
		delete(want, p.Field)
		if !p.Locked || p.Source != model.SourceTag || p.Value != "" {
			t.Fatalf("%s row = %+v, want a lock-only row", p.Field, p)
		}
	}
	if len(want) != 0 {
		t.Fatalf("rows missing after --ignore-locks: %v (all: %+v)", want, prov)
	}
}

// TestScanForcePreservesLockedTotals: a curated track or disc total outlives a forced
// rescan of a file that states another, like every other locked column.
func TestScanForcePreservesLockedTotals(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	in := trackSpecInput(lib.ID, spec)
	in.Track.TrackNo, in.Track.TrackTotal, in.Track.DiscNo, in.Track.DiscTotal = 1, 9, 1, 1
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.EditItemFields(ctx, res.ItemPID, map[string]string{"track_total": "10", "disc_total": "2"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("edit totals: %v", err)
	}
	in.PreserveLocks = true
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	if got := trackColumn(t, st, res.ItemPID, "track_total"); got != "10" {
		t.Fatalf("track_total = %q, want the locked 10", got)
	}
	if got := trackColumn(t, st, res.ItemPID, "disc_total"); got != "2" {
		t.Fatalf("disc_total = %q, want the locked 2", got)
	}
}

// TestScanForceKeepsLockedNumberPastTheFileTotal: a renumbering locked past the total
// the file still states (a catalog-only edit) keeps the cleared total through a forced
// rescan, rather than reading the file's stale pair back as "7 of 1".
func TestScanForceKeepsLockedNumberPastTheFileTotal(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	in := trackSpecInput(lib.ID, spec)
	in.Track.TrackNo, in.Track.TrackTotal, in.Track.DiscNo, in.Track.DiscTotal = 1, 1, 1, 1
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.EditItemFields(ctx, res.ItemPID, map[string]string{"track_no": "7", "disc_no": "2"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("renumber: %v", err)
	}
	in.PreserveLocks = true
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	if no, total := trackColumn(t, st, res.ItemPID, "track_no"), trackColumn(t, st, res.ItemPID, "track_total"); no != "7" || total != "" {
		t.Fatalf("track = %s/%s, want 7 with no total", no, total)
	}
	if no, total := trackColumn(t, st, res.ItemPID, "disc_no"), trackColumn(t, st, res.ItemPID, "disc_total"); no != "2" || total != "" {
		t.Fatalf("disc = %s/%s, want 2 with no total", no, total)
	}
}

// TestBookScanSettlesOwedRows: a content change that re-derives a book's unlocked fields
// from its primary part settles their owed rows, the title's row settles once the file
// states the catalog's title, and a locked field's row stands while the file disagrees.
func TestBookScanSettlesOwedRows(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	spec := bookSpec{path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"}}
	pid := putBook(t, st, lib.ID, spec).ItemPID
	if err := st.EditItemField(ctx, pid, "narrator", "Other Reader", user, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit narrator: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "publisher", "House", user, model.LockOn, false); err != nil {
		t.Fatalf("edit publisher: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "title", "Edited Title", user, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit title: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM file_diagnostic WHERE code = 'tag_write_owed'"); n != 3 {
		t.Fatalf("owed rows after the edits = %d, want title, narrator and publisher", n)
	}

	spec.content, spec.preserveLocks = "bc2", true
	putBook(t, st, lib.ID, spec)

	diags, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{ItemPID: pid, Code: model.DiagTagWriteOwed})
	if err != nil {
		t.Fatalf("owed rows: %v", err)
	}
	if len(diags) != 1 || diags[0].TagKey != "publisher" {
		t.Fatalf("owed rows = %+v, want only the locked publisher left", diags)
	}
}

// TestContentChangedBookRescanRetiresReDerivedProvenance: a book's put rewrites every
// unlocked field from its primary part when the bytes change, so a catalog-only edit loses
// its provenance row with its value, and so does an enrichment fill once the part states
// a value of its own. The enrichment write-back's drift on every part goes once nothing
// stays owed, and a locked field keeps its row.
func TestContentChangedBookRescanRetiresReDerivedProvenance(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	spec := bookSpec{path: "/lib/Author/Book/01.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"}, position: 1}
	res := putBook(t, st, lib.ID, spec)
	pid := res.ItemPID
	part2 := spec
	part2.path, part2.essence, part2.content, part2.position = "/lib/Author/Book/02.m4b", "be2", "bc2", 2
	res2 := putBook(t, st, lib.ID, part2)

	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	var fileNarrator string
	if err := st.rdb().QueryRowContext(ctx, "SELECT narrator FROM book WHERE item_id = ?", itemID).Scan(&fileNarrator); err != nil {
		t.Fatalf("narrator: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "narrator", "Other Reader", user, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit narrator: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "subtitle", "Kept", user, model.LockOn, false); err != nil {
		t.Fatalf("edit subtitle: %v", err)
	}
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: itemID, PID: pid, Matched: true, Provider: "mb", Fields: map[string]string{"publisher": "House"},
	}); err != nil {
		t.Fatalf("fill publisher: %v", err)
	}
	for _, f := range []model.PID{res.FilePID, res2.FilePID} {
		if err := st.AddFileDiagnostic(ctx, f, model.OriginEnrichment, model.FileDiagnostic{
			Code: model.DiagTagWriteUnsynced, Severity: model.SeverityWarn, Detail: "permission denied",
		}); err != nil {
			t.Fatalf("drift: %v", err)
		}
	}

	spec.content, spec.preserveLocks, spec.publisher = "bc1b", true, "Pressed"
	out := putBook(t, st, lib.ID, spec)

	var narrator, publisher, subtitle string
	if err := st.rdb().QueryRowContext(ctx, "SELECT narrator, publisher, subtitle FROM book WHERE item_id = ?", itemID).
		Scan(&narrator, &publisher, &subtitle); err != nil {
		t.Fatalf("book row: %v", err)
	}
	if narrator != fileNarrator || publisher != "Pressed" || subtitle != "Kept" {
		t.Fatalf("narrator/publisher/subtitle = %q/%q/%q, want the file's narrator and publisher, the locked subtitle",
			narrator, publisher, subtitle)
	}
	for _, field := range []string{"narrator", "publisher"} {
		if n := provenanceRows(t, st, pid, field); n != 0 {
			t.Fatalf("%s provenance rows = %d, want the re-derived row retired", field, n)
		}
	}
	if n := provenanceRows(t, st, pid, "subtitle"); n != 1 {
		t.Fatalf("subtitle provenance rows = %d, want the locked row kept", n)
	}
	if !out.MetadataChanged {
		t.Fatal("MetadataChanged = false, want true")
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM file_diagnostic WHERE origin = 'enrichment'"); n != 0 {
		t.Fatalf("enrichment drift rows = %d, want none on either part once nothing is owed", n)
	}
	assertDerivedClean(t, st)
}

// TestForcedRescanRederivesBookFields: a forced rescan of a book's unchanged primary part
// re-derives an unlocked catalog-only edit the way a track's does, and keeps a locked one.
func TestForcedRescanRederivesBookFields(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	spec := bookSpec{path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"}, series: "Saga"}
	pid := putBook(t, st, lib.ID, spec).ItemPID
	if err := st.EditItemField(ctx, pid, "series", "Other Saga", user, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit series: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "subtitle", "Kept", user, model.LockOn, false); err != nil {
		t.Fatalf("edit subtitle: %v", err)
	}
	seq, _ := st.LatestChangeSeq(ctx)

	spec.preserveLocks = true
	res := putBook(t, st, lib.ID, spec)

	v, err := st.BookByPID(ctx, pid)
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if v.Series != "Saga" || v.Subtitle != "Kept" {
		t.Fatalf("series/subtitle = %q/%q, want the file's Saga and the locked Kept", v.Series, v.Subtitle)
	}
	if n := provenanceRows(t, st, pid, "series"); n != 0 {
		t.Fatalf("series provenance rows = %d, want the re-derived row retired", n)
	}
	if !res.MetadataChanged {
		t.Fatal("MetadataChanged = false, want true")
	}
	if n := itemChangesSince(t, st, pid, seq); n != 1 {
		t.Fatalf("item deltas = %d, want 1", n)
	}
	assertDerivedClean(t, st)
}

// TestBookScanSettlesOwedLockedFieldTheFileAgreesWith: a locked book field's owed row
// stands while the file disagrees and goes once the file carries the catalog's value,
// the rule a track's locked field already follows.
func TestBookScanSettlesOwedLockedFieldTheFileAgreesWith(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	spec := bookSpec{path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"}, series: "Saga"}
	pid := putBook(t, st, lib.ID, spec).ItemPID
	if err := st.EditItemField(ctx, pid, "series", "Other Saga", user, model.LockOn, false); err != nil {
		t.Fatalf("edit series: %v", err)
	}
	spec.preserveLocks = true
	putBook(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM file_diagnostic WHERE code = 'tag_write_owed'"); n != 1 {
		t.Fatalf("owed rows = %d, want the locked series kept while the file disagrees", n)
	}
	spec.series, spec.content = "Other Saga", "bc2"
	putBook(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM file_diagnostic WHERE code = 'tag_write_owed'"); n != 0 {
		t.Fatalf("owed rows = %d, want none once the file carries the series", n)
	}
}

// TestForcedBookRescanLeavesEntitySpellingsAlone: a book's series and its translator and
// editor credits read back as entity names, which keep their first-seen spelling or a merge
// survivor's name. A forced rescan of an unchanged file must not read that difference as a
// re-derive: it would rewrite the book on every pass and fork a merged series back out.
func TestForcedBookRescanLeavesEntitySpellingsAlone(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	first := bookSpec{path: "/lib/A/One/one.m4b", essence: "be1", content: "bc1",
		title: "Leviathan Wakes", author: "James Corey", series: "The Expanse"}
	second := bookSpec{path: "/lib/A/Two/two.m4b", essence: "be2", content: "bc2",
		title: "Caliban's War", author: "James Corey", series: "the expanse"}
	putBook(t, st, lib.ID, first)
	pid := putBook(t, st, lib.ID, second).ItemPID
	seq, _ := st.LatestChangeSeq(ctx)

	second.preserveLocks = true
	for i := range 2 {
		if res := putBook(t, st, lib.ID, second); res.MetadataChanged {
			t.Fatalf("forced rescan #%d re-derived a series spelled differently from its entity", i+1)
		}
	}
	if n := itemChangesSince(t, st, pid, seq); n != 0 {
		t.Fatalf("item deltas = %d, want none from rescans of an unchanged book", n)
	}

	third := bookSpec{path: "/lib/B/Three/three.m4b", essence: "be3", content: "bc3",
		title: "The Eye of the World", author: "Robert Jordan", series: "Wheel of Time"}
	putBook(t, st, lib.ID, third)
	putBook(t, st, lib.ID, bookSpec{path: "/lib/B/Four/four.m4b", essence: "be4", content: "bc4",
		title: "The Great Hunt", author: "Robert Jordan", series: "The Wheel of Time"})
	survivor := model.PID(scalarStr(t, st, "SELECT pid FROM series WHERE name = 'The Wheel of Time'"))
	loser := model.PID(scalarStr(t, st, "SELECT pid FROM series WHERE name = 'Wheel of Time'"))
	if _, err := st.MergeEntity(ctx, model.MergeSeries, survivor, loser); err != nil {
		t.Fatalf("merge series: %v", err)
	}
	third.preserveLocks = true
	putBook(t, st, lib.ID, third)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM series WHERE name LIKE '%Wheel of Time'"); n != 1 {
		t.Fatalf("series rows = %d, want the merge to survive a forced rescan", n)
	}
}

// TestBookPartLeavesTheTitleToThePrimary: the primary part owns a book's metadata, the
// title included, so a later part tagged with another title (one ASIN groups them) neither
// renames the book nor reports a change, whatever order the parts are read in.
func TestBookPartLeavesTheTitleToThePrimary(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	one := bookSpec{path: "/lib/A/Book/01.m4b", essence: "be1", content: "bc1",
		title: "Book One", author: "Jane Author", asin: "B000000001", position: 1}
	two := bookSpec{path: "/lib/A/Book/02.m4b", essence: "be2", content: "bc2",
		title: "Book One Part Two", author: "Jane Author", asin: "B000000001", position: 2}
	pid := putBook(t, st, lib.ID, one).ItemPID
	if res := putBook(t, st, lib.ID, two); res.ItemPID != pid || res.MetadataChanged {
		t.Fatalf("part two = %+v, want it joined to the book without a metadata change", res)
	}
	two.preserveLocks = true
	putBook(t, st, lib.ID, two)
	if got := scalarStr(t, st, "SELECT title FROM playable_item WHERE pid = ?", string(pid)); got != "Book One" {
		t.Fatalf("title = %q, want the primary part's Book One", got)
	}
}

// TestNonPrimaryBookPartKeepsOwedRowsItsFileStillLacks: a part that does not own the
// book's metadata compares its file against the book the catalog holds, so a changed
// part whose tags still lack a catalog-only edit keeps that edit owed.
func TestNonPrimaryBookPartKeepsOwedRowsItsFileStillLacks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	one := bookSpec{path: "/lib/A/Book/01.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", position: 1}
	two := bookSpec{path: "/lib/A/Book/02.m4b", essence: "be2", content: "bc2",
		title: "The Book", author: "Jane Author", position: 2}
	pid := putBook(t, st, lib.ID, one).ItemPID
	partTwo := putBook(t, st, lib.ID, two).FilePID
	if err := st.EditItemField(ctx, pid, "series", "Curated Saga", model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit series: %v", err)
	}

	two.content, two.preserveLocks = "bc2b", true
	putBook(t, st, lib.ID, two)

	diags, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: partTwo, Code: model.DiagTagWriteOwed})
	if err != nil {
		t.Fatalf("owed rows: %v", err)
	}
	if len(diags) != 1 || diags[0].TagKey != "series" {
		t.Fatalf("part two owed = %+v, want series kept while its file lacks the catalog's series", diags)
	}
}

// TestValuelessProvenanceIsNoClaim: the enrichment genre fill records its source with no
// value, since genres are many, so the row states nothing a file could contradict. Once
// the write-back put the genre in the file, a forced rescan has nothing to re-derive.
func TestValuelessProvenanceIsNoClaim(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One", genre: "Rock"}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	if _, err := st.wdb().ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, provider, locked, updated_at)
		VALUES ((SELECT id FROM playable_item WHERE pid = ?), 'genre', 'enrichment', 'mb', 0, 1)`, string(pid)); err != nil {
		t.Fatalf("stage the fill's row: %v", err)
	}
	seq, _ := st.LatestChangeSeq(ctx)

	if res := forcedRescan(t, st, lib.ID, spec); res.MetadataChanged {
		t.Fatal("MetadataChanged = true for a file that carries the filled genre")
	}
	if n := itemChangesSince(t, st, pid, seq); n != 0 {
		t.Fatalf("item deltas = %d, want none", n)
	}
	if n := provenanceRows(t, st, pid, "genre"); n != 1 {
		t.Fatalf("genre provenance rows = %d, want the enrichment row kept", n)
	}
}

// TestValuelessEnrichmentRowRetiresWithItsDrift: a rescan of a file that states its own
// value for a field a valueless enrichment row filled retires the row and, with nothing
// left owed, the drift a failed write-back left on the file.
func TestValuelessEnrichmentRowRetiresWithItsDrift(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	res := putTrack(t, st, lib.ID, spec)
	if _, err := st.wdb().ExecContext(ctx, "UPDATE track SET genre = 'Rock' WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)", string(res.ItemPID)); err != nil {
		t.Fatalf("stage the filled column: %v", err)
	}
	if _, err := st.wdb().ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, provider, locked, updated_at)
		VALUES ((SELECT id FROM playable_item WHERE pid = ?), 'genre', 'enrichment', 'mb', 0, 1)`, string(res.ItemPID)); err != nil {
		t.Fatalf("stage the fill's row: %v", err)
	}
	if err := st.AddFileDiagnostic(ctx, res.FilePID, model.OriginEnrichment, model.FileDiagnostic{
		Code: model.DiagTagWriteUnsynced, Severity: model.SeverityWarn, Detail: "permission denied",
	}); err != nil {
		t.Fatalf("drift: %v", err)
	}

	spec.genre = "Jazz"
	forcedRescan(t, st, lib.ID, spec)

	if n := provenanceRows(t, st, res.ItemPID, "genre"); n != 0 {
		t.Fatalf("genre provenance rows = %d, want the re-derived fill retired", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM file_diagnostic WHERE origin = 'enrichment'"); n != 0 {
		t.Fatalf("enrichment drift rows = %d, want none once nothing is owed", n)
	}
}

// TestRescanKeepsEnrichmentFillsTheFileLacks: a value enrichment filled stays through a
// rescan while the file says nothing for that field, --ignore-locks included, since it
// holds nothing against the file, and the rescan changes nothing. A value the file states
// replaces the fill for that field alone.
func TestRescanKeepsEnrichmentFillsTheFileLacks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemID, PID: pid, Matched: true, Provider: "mb",
		Fields: map[string]string{"bpm": "120", "isrc": "USRC17607839", "composer": "Roger Waters"}}); err != nil {
		t.Fatalf("fill fields: %v", err)
	}
	rgID := int64(scalarInt(t, st, "SELECT id FROM release_group"))
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{ReleaseGroupID: rgID,
		PID: model.PID(scalarStr(t, st, "SELECT pid FROM release_group")), Matched: true,
		Genres: []string{"Progressive Rock"}, GenreProvider: "mb"}); err != nil {
		t.Fatalf("fill genre: %v", err)
	}
	filled := func(step string, want map[string]string) {
		t.Helper()
		for col, v := range want {
			if got := trackColumn(t, st, pid, col); got != v {
				t.Errorf("%s: %s = %q, want %q", step, col, got, v)
			}
		}
		if n := scalarInt(t, st, "SELECT COUNT(*) FROM item_genre WHERE item_id = ?", itemID); n != 1 {
			t.Errorf("%s: genre links = %d, want the filled genre's", step, n)
		}
	}
	want := map[string]string{"bpm": "120", "isrc": "USRC17607839", "composer": "Roger Waters", "genre": "Progressive Rock"}
	seq, _ := st.LatestChangeSeq(ctx)
	for _, preserve := range []bool{true, false} {
		spec.preserveLocks = preserve
		if res := putTrack(t, st, lib.ID, spec); res.MetadataChanged {
			t.Errorf("preserve locks %v: MetadataChanged = true, want the fills kept as they were", preserve)
		}
		filled(fmt.Sprintf("preserve locks %v", preserve), want)
	}
	if n := itemChangesSince(t, st, pid, seq); n != 0 {
		t.Errorf("item deltas = %d, want none", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance WHERE item_id = ? AND source = 'enrichment'", itemID); n != 4 {
		t.Errorf("enrichment rows = %d, want the four fills kept", n)
	}

	spec.bpm, spec.content, spec.preserveLocks = 98, "c2", true
	putTrack(t, st, lib.ID, spec)
	want["bpm"] = "98"
	filled("file states a bpm", want)
	if n := provenanceRows(t, st, pid, "bpm"); n != 0 {
		t.Errorf("bpm provenance rows = %d, want the fill retired for the file's value", n)
	}
}

// TestBookRescanKeepsEnrichmentFillsTheFileLacks: the book twin. A content change that
// rewrites the book from its primary part keeps what enrichment filled while the part says
// nothing for it, and takes the part's own value where it states one.
func TestBookRescanKeepsEnrichmentFillsTheFileLacks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := bookSpec{path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", narrators: []string{"Ned Narrator"}}
	pid := putBook(t, st, lib.ID, spec).ItemPID
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemID, PID: pid, Matched: true, Provider: "mb",
		Fields: map[string]string{"publisher": "Tor", "description": "A long tale.", "year": "1999"}}); err != nil {
		t.Fatalf("fill fields: %v", err)
	}
	column := func(col string) string {
		t.Helper()
		return scalarStr(t, st, "SELECT COALESCE(CAST("+col+" AS TEXT), '') FROM book WHERE item_id = ?", itemID)
	}

	spec.content, spec.preserveLocks = "bc2", true
	putBook(t, st, lib.ID, spec)
	for col, want := range map[string]string{"publisher": "Tor", "description": "A long tale.", "year": "1999"} {
		if got := column(col); got != want {
			t.Errorf("%s = %q after a rescan, want the fill %q kept", col, got, want)
		}
	}

	spec.content, spec.publisher = "bc3", "Gollancz"
	putBook(t, st, lib.ID, spec)
	if got := column("publisher"); got != "Gollancz" {
		t.Errorf("publisher = %q, want the part's own Gollancz", got)
	}
	if n := provenanceRows(t, st, pid, "publisher"); n != 0 {
		t.Errorf("publisher provenance rows = %d, want the fill retired", n)
	}
	if got := column("description"); got != "A long tale." {
		t.Errorf("description = %q, want the fill kept", got)
	}
}

// TestKeptEnrichmentFillStaysOwed: a value applied as enrichment through the edit surface
// without a write-back is owed to the file, and a rescan that keeps it over a file still
// silent on it pays nothing, for a book's primary part as for a track.
func TestKeptEnrichmentFillStaysOwed(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	fill := model.Attribution{Source: model.SourceEnrichment, Provider: "mb"}

	book := bookSpec{path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author"}
	bookPID := putBook(t, st, lib.ID, book).ItemPID
	if err := st.EditItemField(ctx, bookPID, "publisher", "Tor", fill, model.LockUnchanged, false); err != nil {
		t.Fatalf("fill publisher: %v", err)
	}
	book.content, book.preserveLocks = "bc2", true
	putBook(t, st, lib.ID, book)
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM file_diagnostic d JOIN item_file itf ON itf.file_id = d.file_id
		JOIN playable_item pi ON pi.id = itf.item_id WHERE pi.pid = ? AND d.code = 'tag_write_owed' AND d.tag_key = 'publisher'`,
		string(bookPID)); n != 1 {
		t.Errorf("book publisher owed rows = %d, want the kept fill still owed", n)
	}

	track := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	trackPID := putTrack(t, st, lib.ID, track).ItemPID
	if err := st.EditItemField(ctx, trackPID, "bpm", "120", fill, model.LockUnchanged, false); err != nil {
		t.Fatalf("fill bpm: %v", err)
	}
	forcedRescan(t, st, lib.ID, track)
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM file_diagnostic d JOIN item_file itf ON itf.file_id = d.file_id
		JOIN playable_item pi ON pi.id = itf.item_id WHERE pi.pid = ? AND d.code = 'tag_write_owed' AND d.tag_key = 'bpm'`,
		string(trackPID)); n != 1 {
		t.Errorf("track bpm owed rows = %d, want the kept fill still owed", n)
	}
}

// TestAlbumFillsStayWithTheirAlbum: a year and a genre enrichment filled for an album
// belong to that album, so a member whose file is retagged onto another album leaves them
// behind, while a silent rescan of a member that stayed keeps them.
func TestAlbumFillsStayWithTheirAlbum(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	one := trackSpec{path: "/lib/A/Mixed/01.flac", essence: "e1", content: "c1", title: "One", artist: "Alpha", albumArt: "Alpha", album: "Hits"}
	two := trackSpec{path: "/lib/A/Mixed/02.flac", essence: "e2", content: "c2", title: "Two", artist: "Alpha", albumArt: "Alpha", album: "Hits"}
	onePID := putTrack(t, st, lib.ID, one).ItemPID
	twoPID := putTrack(t, st, lib.ID, two).ItemPID
	if err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{AlbumID: int64(scalarInt(t, st, "SELECT id FROM album")),
		PID: model.PID(scalarStr(t, st, "SELECT pid FROM album")), Matched: true, Provider: "mb",
		Fields: map[string]string{"year": "1990"}}); err != nil {
		t.Fatalf("fill year: %v", err)
	}
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{ReleaseGroupID: int64(scalarInt(t, st, "SELECT id FROM release_group")),
		PID: model.PID(scalarStr(t, st, "SELECT pid FROM release_group")), Matched: true,
		Genres: []string{"Synthpop"}, GenreProvider: "mb"}); err != nil {
		t.Fatalf("fill genre: %v", err)
	}
	for _, pid := range []model.PID{onePID, twoPID} {
		if y, g := trackColumn(t, st, pid, "year"), trackColumn(t, st, pid, "genre"); y != "1990" || g != "Synthpop" {
			t.Fatalf("%s before = year %q genre %q, want the fills", pid, y, g)
		}
	}

	one.content, one.album, one.preserveLocks = "c1b", "Live", true
	putTrack(t, st, lib.ID, one)
	if y, g := trackColumn(t, st, onePID, "year"), trackColumn(t, st, onePID, "genre"); (y != "" && y != "0") || g != "" {
		t.Errorf("retagged member = year %q genre %q, want the old album's fills left behind", y, g)
	}
	two.content, two.preserveLocks = "c2b", true
	putTrack(t, st, lib.ID, two)
	if y, g := trackColumn(t, st, twoPID, "year"), trackColumn(t, st, twoPID, "genre"); y != "1990" || g != "Synthpop" {
		t.Errorf("member that stayed = year %q genre %q, want the fills kept", y, g)
	}
}

// TestEnrichmentCreditsSurviveASilentRescan: a credit set as enrichment stays through a
// rescan of a file that names nobody in that role, a track's composer and a book's
// narrator alike.
func TestEnrichmentCreditsSurviveASilentRescan(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	fill := model.Attribution{Source: model.SourceEnrichment, Provider: "mb"}
	track := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1", title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	tpid := putTrack(t, st, lib.ID, track).ItemPID
	if _, _, err := st.SetItemCredits(ctx, tpid, model.RoleComposer, []string{"Roger Waters"}, fill, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("credit composer: %v", err)
	}
	if res := forcedRescan(t, st, lib.ID, track); res.MetadataChanged {
		t.Error("MetadataChanged = true, want the credit kept as it was")
	}
	if got := trackColumn(t, st, tpid, "composer"); got != "Roger Waters" {
		t.Errorf("composer = %q, want the credited Roger Waters", got)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic JOIN playable_item pi ON pi.id = ic.item_id
		WHERE pi.pid = ? AND ic.role = 'composer'`, string(tpid)); n != 1 {
		t.Errorf("composer credits = %d, want 1", n)
	}

	book := bookSpec{path: "/lib/Author/Book/book.m4b", essence: "be1", content: "bc1", title: "The Book", author: "Jane Author"}
	bpid := putBook(t, st, lib.ID, book).ItemPID
	if _, _, err := st.SetItemCredits(ctx, bpid, model.RoleNarrator, []string{"Ned Narrator"}, fill, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("credit narrator: %v", err)
	}
	book.content, book.preserveLocks = "bc2", true
	putBook(t, st, lib.ID, book)
	if got := scalarStr(t, st, `SELECT COALESCE(b.narrator, '') FROM book b JOIN playable_item pi ON pi.id = b.item_id
		WHERE pi.pid = ?`, string(bpid)); got != "Ned Narrator" {
		t.Errorf("narrator = %q, want the credited Ned Narrator", got)
	}
}

// TestFillGivesWayToALaterEdit: once a later edit owns the column, the fill's row no
// longer describes it, so a rescan of a silent file re-derives the column under the
// edit's own rule in one pass, leaving the column and the credits agreeing.
func TestFillGivesWayToALaterEdit(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1", title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemID, PID: pid, Matched: true, Provider: "mb",
		Fields: map[string]string{"composer": "Roger Waters"}}); err != nil {
		t.Fatalf("fill composer: %v", err)
	}
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Roger Waters", "David Gilmour"},
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("credit composers: %v", err)
	}
	forcedRescan(t, st, lib.ID, spec)
	if got := trackColumn(t, st, pid, "composer"); got != "" {
		t.Errorf("composer = %q, want the unlocked edit re-derived from the silent file", got)
	}
	if n := provenanceRows(t, st, pid, "composer") + provenanceRows(t, st, pid, "credit.composer"); n != 0 {
		t.Errorf("composer provenance rows = %d, want none", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM item_contributor WHERE item_id = ? AND role = 'composer'", itemID); n != 0 {
		t.Errorf("composer credits = %d, want none beside an empty column", n)
	}
	if res := forcedRescan(t, st, lib.ID, spec); res.MetadataChanged {
		t.Error("second rescan MetadataChanged = true, want the first to have settled it")
	}
}

// TestFillBeatsAFallbackGuess: a field a display fallback guessed from the file name is
// one the file does not state, so a value set as enrichment stays over the guess, the
// title included.
func TestFillBeatsAFallbackGuess(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	in := trackSpecInput(lib.ID, trackSpec{path: "/lib/A/One/07 Guess.flac", essence: "e1", content: "c1",
		title: "Guess", artist: "Alpha", albumArt: "Alpha", album: "One"})
	in.Track.TrackNo = 7
	in.Derived = []string{"title", "track_no"}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.EditItemFields(ctx, res.ItemPID, map[string]string{"title": "Provider Title", "track_no": "3"},
		model.Attribution{Source: model.SourceEnrichment, Provider: "mb"}, model.LockUnchanged, false); err != nil {
		t.Fatalf("fill: %v", err)
	}
	in.PreserveLocks = true
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if got := scalarStr(t, st, "SELECT title FROM playable_item WHERE pid = ?", string(res.ItemPID)); got != "Provider Title" {
		t.Errorf("title = %q, want the fill kept over the file-name guess", got)
	}
	if got := trackColumn(t, st, res.ItemPID, "track_no"); got != "3" {
		t.Errorf("track_no = %q, want the fill kept over the file-name guess", got)
	}
}

// TestFillGivesWayToAZeroTheFileStates: a string the file states is a value even when it
// reads "0", and a total kept beside a number the file now states past it is cleared.
func TestFillGivesWayToAZeroTheFileStates(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1", title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	if err := st.EditItemFields(ctx, pid, map[string]string{"comment": "Provider note", "track_total": "10"},
		model.Attribution{Source: model.SourceEnrichment, Provider: "mb"}, model.LockUnchanged, false); err != nil {
		t.Fatalf("fill: %v", err)
	}
	in := trackSpecInput(lib.ID, spec)
	in.PreserveLocks, in.File.ContentHash = true, "c2"
	in.Track.Comment, in.Track.TrackNo = "0", 12
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if got := trackColumn(t, st, pid, "comment"); got != "0" {
		t.Errorf("comment = %q, want the file's own 0", got)
	}
	if got := trackColumn(t, st, pid, "track_total"); got != "" && got != "0" {
		t.Errorf("track_total = %q beside track 12, want the kept 10 cleared", got)
	}
}

// TestLostFillReopensTheWriteBack: a fill the write-back settled on the file, which the
// file no longer carries, is owed again, while a file whose write reported the value
// lost is not reopened, since it would only lose it again.
func TestLostFillReopensTheWriteBack(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1", title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	res := putTrack(t, st, lib.ID, spec)
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(res.ItemPID)))
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemID, PID: res.ItemPID, Matched: true, Provider: "mb",
		Fields: map[string]string{"bpm": "120"}}); err != nil {
		t.Fatalf("fill bpm: %v", err)
	}
	at := int64(scalarInt(t, st, "SELECT updated_at FROM field_provenance WHERE item_id = ? AND field = 'bpm'", itemID))
	owed := func(step string, want int) {
		t.Helper()
		rows, err := st.EnrichmentWriteback(ctx, nil)
		if err != nil {
			t.Fatalf("%s: writeback: %v", step, err)
		}
		if len(rows) != want {
			t.Errorf("%s: owed files = %d, want %d", step, len(rows), want)
		}
	}
	if err := st.SettleEnrichmentWrite(ctx, res.FilePID, at); err != nil {
		t.Fatalf("settle: %v", err)
	}
	owed("settled", 0)

	spec.content, spec.preserveLocks = "c2", true
	putTrack(t, st, lib.ID, spec)
	owed("after the file lost the bpm", 1)

	if err := st.SettleEnrichmentWrite(ctx, res.FilePID, at); err != nil {
		t.Fatalf("settle again: %v", err)
	}
	if err := st.AddFileDiagnostic(ctx, res.FilePID, model.OriginEnrichment, model.FileDiagnostic{
		Code: model.DiagTagWriteLost, Severity: model.SeverityWarn, Detail: "BPM not stored"}); err != nil {
		t.Fatalf("lost: %v", err)
	}
	spec.content = "c3"
	putTrack(t, st, lib.ID, spec)
	owed("a value the file could not store", 0)
}

// TestNormalizedFillSurvivesASilentRescan: a normalize pass respelling an enrichment value
// leaves a value the file never carried, so a rescan of a silent file keeps it.
func TestNormalizedFillSurvivesASilentRescan(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1", title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemID, PID: pid, Matched: true, Provider: "mb",
		Fields: map[string]string{"composer": "roger waters"}}); err != nil {
		t.Fatalf("fill composer: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "composer", "Roger Waters", model.Attribution{Source: model.SourceNormalize},
		model.LockUnchanged, false); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	forcedRescan(t, st, lib.ID, spec)
	if got := trackColumn(t, st, pid, "composer"); got != "Roger Waters" {
		t.Errorf("composer = %q, want the normalized fill kept", got)
	}
}

// TestLockedFillUnderIgnoreLocks: a locked enrichment value over a silent file stays even
// under --ignore-locks, which re-derives what the file states, and the file's own value
// replaces it.
func TestLockedFillUnderIgnoreLocks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1", title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{ItemID: itemID, PID: pid, Matched: true, Provider: "mb",
		Fields: map[string]string{"bpm": "120"}}); err != nil {
		t.Fatalf("fill bpm: %v", err)
	}
	if err := st.LockField(ctx, pid, "bpm"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	spec.content = "c2"
	putTrack(t, st, lib.ID, spec)
	if got := trackColumn(t, st, pid, "bpm"); got != "120" {
		t.Errorf("bpm under --ignore-locks over a silent file = %q, want 120 kept", got)
	}
	spec.content, spec.bpm = "c3", 98
	putTrack(t, st, lib.ID, spec)
	if got := trackColumn(t, st, pid, "bpm"); got != "98" {
		t.Errorf("bpm = %q, want the file's own 98", got)
	}
}

// TestEnrichmentCustomTagSurvivesASilentRescan: a custom tag set as enrichment stays
// through a rescan of a file without the key, and the file's own value replaces it.
func TestEnrichmentCustomTagSurvivesASilentRescan(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putTrackCustom(t, st, lib.ID, "/lib/a.mp3", "ess-a", "c1", "Song", nil, true).ItemPID
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"Calm"}, model.Attribution{Source: model.SourceEnrichment, Provider: "mb"},
		model.LockUnchanged, false); err != nil {
		t.Fatalf("set tag: %v", err)
	}
	putTrackCustom(t, st, lib.ID, "/lib/a.mp3", "ess-a", "c2", "Song", nil, true)
	if got := scalarStr(t, st, "SELECT COALESCE(GROUP_CONCAT(value), '') FROM item_tag it JOIN playable_item pi ON pi.id = it.item_id WHERE pi.pid = ? AND it.key = 'MOOD'", string(pid)); got != "Calm" {
		t.Errorf("MOOD after a silent rescan = %q, want Calm kept", got)
	}
	putTrackCustom(t, st, lib.ID, "/lib/a.mp3", "ess-a", "c3", "Song", map[string][]string{"MOOD": {"Angry"}}, true)
	if got := scalarStr(t, st, "SELECT COALESCE(GROUP_CONCAT(value), '') FROM item_tag it JOIN playable_item pi ON pi.id = it.item_id WHERE pi.pid = ? AND it.key = 'MOOD'", string(pid)); got != "Angry" {
		t.Errorf("MOOD = %q, want the file's own Angry", got)
	}
}

// TestEnrichmentLyricsSurviveASilentRescan: lyrics enrichment fetched stay through a
// rescan of a file with none, and the file's own lyrics replace them.
func TestEnrichmentLyricsSurviveASilentRescan(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/A/One/01.flac", essence: "e1", content: "c1", title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One"}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	if err := st.ApplyLyricsEnrichment(ctx, model.LyricsEnrichment{ItemID: itemID, PID: pid, Matched: true, Provider: "lrclib",
		Lyrics: &model.Lyrics{Unsynced: "fetched words", Source: model.SourceEnrichment, Provider: "lrclib"}}); err != nil {
		t.Fatalf("fill lyrics: %v", err)
	}
	forcedRescan(t, st, lib.ID, spec)
	if ly, err := st.LyricsByItem(ctx, pid); err != nil || ly.Unsynced != "fetched words" {
		t.Fatalf("lyrics after a silent rescan = %+v (err %v), want the fetched words kept", ly, err)
	}
	in := trackSpecInput(lib.ID, spec)
	in.PreserveLocks, in.File.ContentHash = true, "c2"
	in.Lyrics = &model.Lyrics{Unsynced: "file words", Source: model.SourceTag}
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("rescan with lyrics: %v", err)
	}
	if ly, err := st.LyricsByItem(ctx, pid); err != nil || ly.Unsynced != "file words" {
		t.Errorf("lyrics = %+v (err %v), want the file's own", ly, err)
	}
}
