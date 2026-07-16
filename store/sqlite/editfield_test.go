package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// editFixture scans one track and returns its item pid.
func editFixture(t *testing.T) (*Store, model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1",
		title: "Original", artist: "Alpha", albumArt: "Alpha", album: "One",
		genre: "Rock", year: 2001, composer: "Writer",
	})
	return st, itemPID(t, st)
}

func TestEditPlainFieldAndProvenance(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "comment", "hello world", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit comment: %v", err)
	}

	var comment string
	if err := st.read.QueryRowContext(ctx,
		"SELECT comment FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?", string(pid)).Scan(&comment); err != nil {
		t.Fatalf("read comment: %v", err)
	}
	if comment != "hello world" {
		t.Fatalf("comment = %q, want %q", comment, "hello world")
	}

	rows, err := st.FieldProvenance(ctx, pid)
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("provenance rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Field != "comment" || got.Source != model.SourceUser || !got.Locked ||
		got.Value != "hello world" || got.Provider != "" {
		t.Fatalf("provenance = %+v, want user+locked comment with empty provider", got)
	}
}

func TestEditTitleRebuildsFTSAndSortKey(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "title", "Renamed Song", model.SourceUser, false, false); err != nil {
		t.Fatalf("edit title: %v", err)
	}

	v, err := st.ItemByPID(ctx, pid)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if v.Title != "Renamed Song" {
		t.Fatalf("title = %q, want %q", v.Title, "Renamed Song")
	}

	// sort_key follows the new title.
	var sortKey string
	if err := st.read.QueryRowContext(ctx, "SELECT sort_key FROM playable_item WHERE pid=?", string(pid)).Scan(&sortKey); err != nil {
		t.Fatalf("read sort_key: %v", err)
	}
	if want := model.SortKey("Renamed Song"); sortKey != want {
		t.Fatalf("sort_key = %q, want %q", sortKey, want)
	}

	// FTS finds the new title and not the old one.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts WHERE search_fts MATCH 'renamed'"); n != 1 {
		t.Errorf("FTS match for new title = %d, want 1", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts WHERE search_fts MATCH 'original'"); n != 0 {
		t.Errorf("FTS still matches old title (%d), want 0", n)
	}

	// --no-lock recorded a user row that is not locked.
	rows, _ := st.FieldProvenance(ctx, pid)
	if len(rows) != 1 || rows[0].Locked {
		t.Fatalf("provenance = %+v, want one unlocked user row", rows)
	}
}

func TestEditArtistReResolvesEntitiesAndRollups(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "artist", "Beta", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit artist: %v", err)
	}

	v, err := st.ItemByPID(ctx, pid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.Artist != "Beta" {
		t.Fatalf("artist = %q, want Beta", v.Artist)
	}

	// A new Beta artist exists and the track's FK points at it.
	var artistName string
	if err := st.read.QueryRowContext(ctx, `SELECT a.name FROM artist a
		JOIN track t ON t.artist_id = a.id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid=?`, string(pid)).Scan(&artistName); err != nil {
		t.Fatalf("read artist entity: %v", err)
	}
	if artistName != "Beta" {
		t.Fatalf("linked artist = %q, want Beta", artistName)
	}

	// Every artist keeps a rollup row (the recompute LEFT JOINs from artist), so an
	// artist that lost its last track is a harmless zero row, not db-verify drift.
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.Consistent() {
		t.Fatalf("db verify not clean after artist edit: %+v", rep)
	}
}

// TestEditOrphansEntityKeepsVerifyClean edits both artist and album_artist away
// from Alpha, fully orphaning it, and asserts Alpha survives as a zero-rollup ghost
// with db verify still clean (the edit adds no in-transaction entity GC).
func TestEditOrphansEntityKeepsVerifyClean(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	err := st.EditItemFields(ctx, pid, map[string]string{
		"artist": "Beta", "album_artist": "Beta",
	}, model.SourceUser, true, false)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	// Alpha is now unreferenced but keeps a zero rollup row.
	if n := scalarInt(t, st,
		"SELECT track_count FROM artist_rollup ar JOIN artist a ON a.id=ar.artist_id WHERE a.name='Alpha'"); n != 0 {
		t.Errorf("orphaned Alpha rollup track_count = %d, want 0", n)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean after orphaning edit: %+v (err %v)", rep, err)
	}
}

func TestEditGenreUpdatesLinksAndVerifyClean(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "genre", "Jazz; Blues", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}

	// item_genre now links Jazz and Blues, not Rock.
	names := map[string]bool{}
	rows, err := st.read.QueryContext(ctx, `SELECT g.name FROM item_genre ig
		JOIN genre g ON g.id = ig.genre_id
		JOIN playable_item pi ON pi.id = ig.item_id WHERE pi.pid=?`, string(pid))
	if err != nil {
		t.Fatalf("read genres: %v", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names[n] = true
	}
	rows.Close()
	if !names["Jazz"] || !names["Blues"] || names["Rock"] {
		t.Fatalf("genres = %v, want Jazz+Blues and not Rock", names)
	}

	// The denormalized column reflects the edit too.
	v, _ := st.ItemByPID(ctx, pid)
	if v.Genre != "Jazz; Blues" {
		t.Errorf("denormalized genre = %q, want %q", v.Genre, "Jazz; Blues")
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean after genre edit: %+v (err %v)", rep, err)
	}
}

func TestEditYearReResolvesAlbum(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "year", "1999", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit year: %v", err)
	}
	v, _ := st.ItemByPID(ctx, pid)
	if v.Year != 1999 {
		t.Fatalf("year = %d, want 1999", v.Year)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean after year edit: %+v (err %v)", rep, err)
	}
}

func TestEditMultipleFieldsOneDelta(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	seq0, _ := st.LatestChangeSeq(ctx)
	err := st.EditItemFields(ctx, pid, map[string]string{
		"artist": "Gamma", "title": "New Title", "composer": "New Writer",
	}, model.SourceUser, true, false)
	if err != nil {
		t.Fatalf("edit fields: %v", err)
	}
	// Exactly one item delta for the whole multi-field edit.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM change_log WHERE seq>? AND entity_type='item'", seq0); n != 1 {
		t.Errorf("item deltas = %d, want 1 for a multi-field edit", n)
	}
	// All three fields recorded provenance.
	rows, _ := st.FieldProvenance(ctx, pid)
	if len(rows) != 3 {
		t.Fatalf("provenance rows = %d, want 3", len(rows))
	}
}

// TestEditTrimsValueEverywhere verifies a store edit trims surrounding whitespace so
// the denormalized column, the resolved entity, and the recorded provenance all store
// the same value (not just the CLI-facing input).
func TestEditTrimsValueEverywhere(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "artist", "  Spaced Artist  ", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	// Denormalized column is trimmed.
	v, _ := st.ItemByPID(ctx, pid)
	if v.Artist != "Spaced Artist" {
		t.Errorf("view artist = %q, want %q", v.Artist, "Spaced Artist")
	}
	// The resolved artist entity name is trimmed (and matches the column).
	var name string
	if err := st.read.QueryRowContext(ctx, `SELECT a.name FROM artist a
		JOIN track t ON t.artist_id = a.id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid=?`, string(pid)).Scan(&name); err != nil {
		t.Fatalf("read entity: %v", err)
	}
	if name != "Spaced Artist" {
		t.Errorf("entity name = %q, want %q", name, "Spaced Artist")
	}
	// Provenance records the trimmed curated value.
	rows, _ := st.FieldProvenance(ctx, pid)
	if len(rows) != 1 || rows[0].Value != "Spaced Artist" {
		t.Errorf("provenance value = %+v, want trimmed %q", rows, "Spaced Artist")
	}
}

func TestEditRespectsLock(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.LockField(ctx, pid, "artist"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	// Without force, editing a locked field is refused with CodeLocked.
	err := st.EditItemField(ctx, pid, "artist", "Delta", model.SourceUser, true, false)
	if !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("edit locked field: want CodeLocked, got %v", err)
	}
	v, _ := st.ItemByPID(ctx, pid)
	if v.Artist != "Alpha" {
		t.Fatalf("artist changed despite lock: %q", v.Artist)
	}
	// With force it goes through.
	if err := st.EditItemField(ctx, pid, "artist", "Delta", model.SourceUser, true, true); err != nil {
		t.Fatalf("forced edit: %v", err)
	}
	v, _ = st.ItemByPID(ctx, pid)
	if v.Artist != "Delta" {
		t.Fatalf("forced artist = %q, want Delta", v.Artist)
	}
}

// TestEditLocksAgainstEnrichment checks that once a user edit auto-locks a field, a
// later enrichment write to it is refused.
func TestEditLocksAgainstEnrichment(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "genre", "Jazz", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	// Enrichment (fill-when-empty, lock-respecting) must not overwrite the locked field.
	err := st.SetFieldProvenance(ctx, pid, "genre", model.SourceEnrichment, "Pop", false)
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("enrichment over user-locked field: want CodeConflict, got %v", err)
	}
}

func TestEditRejectsBadInput(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "not_a_field", "x", model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown field: want CodeInvalid, got %v", err)
	}
	if err := st.EditItemField(ctx, pid, "year", "not-a-number", model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("bad year: want CodeInvalid, got %v", err)
	}
	for _, f := range []string{"year", "track_no", "disc_no"} {
		if err := st.EditItemField(ctx, pid, f, "-5", model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("negative %s: want CodeInvalid, got %v", f, err)
		}
	}
	if err := st.EditItemField(ctx, "01J0NONEXISTENT0000000000", "title", "x", model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown item: want CodeNotFound, got %v", err)
	}
}

// TestLockIsKindAware verifies lock/unlock reject a field that does not apply to the
// item's kind (the whitelist is the track∪book union, but a track cannot carry an
// author lock, nor a book an album_artist lock), while a valid same-kind field works.
func TestLockIsKindAware(t *testing.T) {
	ctx := context.Background()
	track, trackPID := editFixture(t)
	book, bookPID := bookEditFixture(t)

	// A track field on a track and a book field on a book are lockable.
	if err := track.LockField(ctx, trackPID, "artist"); err != nil {
		t.Errorf("lock track artist: %v", err)
	}
	if err := book.LockField(ctx, bookPID, "author"); err != nil {
		t.Errorf("lock book author: %v", err)
	}
	// Cross-kind fields are rejected as invalid, not stored as junk provenance.
	if err := track.LockField(ctx, trackPID, "author"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("lock track author: want CodeInvalid, got %v", err)
	}
	if err := book.LockField(ctx, bookPID, "album_artist"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("lock book album_artist: want CodeInvalid, got %v", err)
	}
	// No junk rows landed for the rejected cross-kind locks.
	if n := scalarInt(t, track, "SELECT COUNT(*) FROM field_provenance WHERE field='author'"); n != 0 {
		t.Errorf("track author provenance rows = %d, want 0", n)
	}
}

func TestFileSharedOrVirtual(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "A"})

	var filePID string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM file LIMIT 1").Scan(&filePID); err != nil {
		t.Fatalf("read file pid: %v", err)
	}
	// A normal single-item file is not shared.
	shared, err := st.FileSharedOrVirtual(ctx, model.PID(filePID))
	if err != nil {
		t.Fatalf("shared check: %v", err)
	}
	if shared {
		t.Fatalf("single-item file reported as shared")
	}

	// Give the file an offset-bearing edge to a second item, which makes it virtual.
	var fileID int64
	_ = st.read.QueryRowContext(ctx, "SELECT id FROM file WHERE pid=?", filePID).Scan(&fileID)
	_, err = st.write.ExecContext(ctx, `INSERT INTO playable_item(pid, kind, state, title, sort_key, identity_key, created_at, updated_at)
		VALUES ('01J0VIRTUAL00000000000000','track','present','V','v','virt:1',1,1)`)
	if err != nil {
		t.Fatalf("insert virtual item: %v", err)
	}
	var vid int64
	_ = st.read.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE pid='01J0VIRTUAL00000000000000'").Scan(&vid)
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO item_file(item_id, file_id, role, position, start_ms, end_ms) VALUES (?,?,'primary',0,0,1000)", vid, fileID); err != nil {
		t.Fatalf("insert virtual edge: %v", err)
	}
	shared, err = st.FileSharedOrVirtual(ctx, model.PID(filePID))
	if err != nil {
		t.Fatalf("shared check 2: %v", err)
	}
	if !shared {
		t.Fatalf("multi-item offset-bearing file not reported as shared")
	}
}
