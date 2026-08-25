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

	if err := st.EditItemField(ctx, pid, "comment", "hello world", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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

	if err := st.EditItemField(ctx, pid, "title", "Renamed Song", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
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

	if err := st.EditItemField(ctx, pid, "artist", "Beta", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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

// TestEditOrphansEntityKeepsVerifyClean fully orphans Alpha with a genuinely
// non-uniform batch (its two tracks move to two different artists, so the whole-set
// rename-in-place path pinned in editrename_test.go does not apply) and asserts Alpha
// survives as a zero-rollup ghost with db verify still clean (the edit adds no
// in-transaction entity GC).
func TestEditOrphansEntityKeepsVerifyClean(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1",
		title: "S1", artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/02.flac", essence: "e2", content: "c2",
		title: "S2", artist: "Alpha", albumArt: "Alpha", album: "One", year: 2001,
	})
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S1'"))
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S2'"))

	_, err := st.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: pid1, Fields: map[string]string{"artist": "Beta", "album_artist": "Beta"}},
		{ItemPID: pid2, Fields: map[string]string{"artist": "Gamma", "album_artist": "Gamma"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false)
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

	if err := st.EditItemField(ctx, pid, "genre", "Jazz; Blues", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating genres: %v", err)
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

	if err := st.EditItemField(ctx, pid, "year", "1999", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false)
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

	if err := st.EditItemField(ctx, pid, "artist", "  Spaced Artist  ", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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
	err := st.EditItemField(ctx, pid, "artist", "Delta", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false)
	if !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("edit locked field: want CodeLocked, got %v", err)
	}
	v, _ := st.ItemByPID(ctx, pid)
	if v.Artist != "Alpha" {
		t.Fatalf("artist changed despite lock: %q", v.Artist)
	}
	// With force it goes through.
	if err := st.EditItemField(ctx, pid, "artist", "Delta", model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil {
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

	if err := st.EditItemField(ctx, pid, "genre", "Jazz", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	// Enrichment (fill-when-empty, lock-respecting) must not overwrite the locked field.
	err := st.SetFieldProvenance(ctx, pid, "genre",
		model.Attribution{Source: model.SourceEnrichment, Provider: "musicbrainz"}, "Pop", false)
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("enrichment over user-locked field: want CodeConflict, got %v", err)
	}
}

func TestEditRejectsBadInput(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "not_a_field", "x", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown field: want CodeInvalid, got %v", err)
	}
	if err := st.EditItemField(ctx, pid, "year", "not-a-number", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("bad year: want CodeInvalid, got %v", err)
	}
	for _, f := range []string{"year", "track_no", "disc_no"} {
		if err := st.EditItemField(ctx, pid, f, "-5", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("negative %s: want CodeInvalid, got %v", f, err)
		}
	}
	if err := st.EditItemField(ctx, "01J0NONEXISTENT0000000000", "title", "x", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeNotFound) {
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
		"INSERT INTO item_file(item_id, file_id, role, position, start_frames, end_frames) VALUES (?,?,'primary',0,0,75)", vid, fileID); err != nil {
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

// TestEditCarriesTheCallersAttribution: an edit records the source and provider the
// caller supplied rather than a source the store invented, which is what lets a program
// that fetched a value say so.
func TestEditCarriesTheCallersAttribution(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "comment", "fetched",
		model.Attribution{Source: model.SourceEnrichment, Provider: "itunes"}, model.LockOf(false), false); err != nil {
		t.Fatalf("stamped edit: %v", err)
	}
	var source, provider string
	if err := st.read.QueryRowContext(ctx,
		"SELECT source, COALESCE(provider,'') FROM field_provenance WHERE field='comment'").
		Scan(&source, &provider); err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if source != string(model.SourceEnrichment) || provider != "itunes" {
		t.Errorf("row = %q/%q, want enrichment/itunes", source, provider)
	}

	// And an edit that names no origin is still a user edit, which is what keeps every
	// existing caller writing exactly what it always did.
	if err := st.EditItemField(ctx, pid, "comment", "typed", model.Attribution{}, model.LockOf(false), false); err != nil {
		t.Fatalf("unstamped edit: %v", err)
	}
	if err := st.read.QueryRowContext(ctx,
		"SELECT source, COALESCE(provider,'') FROM field_provenance WHERE field='comment'").
		Scan(&source, &provider); err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if source != string(model.SourceUser) || provider != "" {
		t.Errorf("row = %q/%q, want user with no provider", source, provider)
	}
}

// TestScalarEditRefusesWhatAScalarRowCannotHold: the artifact-only sources have no
// meaning on a scalar field, and a scalar row has no column for a fetch URL, so both
// are refused rather than dropped.
func TestScalarEditRefusesWhatAScalarRowCannotHold(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	for _, attr := range []model.Attribution{
		{Source: model.SourceSidecar},
		{Source: model.SourceFeed},
		{Source: model.SourceEnrichment},
		{Source: model.SourceUser, SourceURL: "https://example/cover.png"},
	} {
		if err := st.EditItemField(ctx, pid, "comment", "x", attr, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("edit with %+v = %v, want CodeInvalid", attr, err)
		}
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance WHERE field='comment'"); n != 0 {
		t.Errorf("%d comment provenance rows after refused edits, want 0", n)
	}
}

// TestEditWithLockUnchangedLeavesTheLockStanding is the fix for a caller that only meant
// to change a value: it no longer has to state a lock intent it never formed, and a
// forced edit no longer releases a lock it never read.
func TestEditWithLockUnchangedLeavesTheLockStanding(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "comment", "first", model.Attribution{}, model.LockOn, false); err != nil {
		t.Fatalf("locking edit: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "comment", "second", model.Attribution{}, model.LockUnchanged, true); err != nil {
		t.Fatalf("forced edit leaving the lock alone: %v", err)
	}
	if n := scalarInt(t, st, "SELECT locked FROM field_provenance WHERE field='comment'"); n != 1 {
		t.Errorf("locked = %d after an unchanged-lock edit, want the lock still standing", n)
	}
	var value string
	if err := st.read.QueryRowContext(ctx,
		"SELECT COALESCE(value,'') FROM field_provenance WHERE field='comment'").Scan(&value); err != nil {
		t.Fatalf("read value: %v", err)
	}
	if value != "second" {
		t.Errorf("value = %q, want the edit to have applied", value)
	}

	// An unlocked field left unchanged stays unlocked, and a fresh row inserts unlocked.
	if err := st.EditItemField(ctx, pid, "title", "T", model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("fresh unchanged-lock edit: %v", err)
	}
	if n := scalarInt(t, st, "SELECT locked FROM field_provenance WHERE field='title'"); n != 0 {
		t.Errorf("locked = %d on a fresh row, want 0", n)
	}
	if err := st.EditItemField(ctx, pid, "comment", "third", model.Attribution{}, model.LockOff, true); err != nil {
		t.Fatalf("unlocking edit: %v", err)
	}
	if n := scalarInt(t, st, "SELECT locked FROM field_provenance WHERE field='comment'"); n != 0 {
		t.Errorf("locked = %d after an explicit unlock, want 0", n)
	}
}

// TestEditRefusesAnUnknownLockInstruction keeps the lock vocabulary closed the way the
// art-role one is.
func TestEditRefusesAnUnknownLockInstruction(t *testing.T) {
	st, pid := editFixture(t)
	if err := st.EditItemField(context.Background(), pid, "comment", "x",
		model.Attribution{}, model.LockChange("yes"), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown lock instruction = %v, want CodeInvalid", err)
	}
}

// TestEditKeepsMBIDKeyedAlbum pins the no-fork contract for an MBID-identified album:
// an item-level edit re-resolves with the release ids the entity rows actually own, so
// no edit re-keys a member with a heuristic key no row owns. Without the carryover in
// loadTrackForEditTx, every one of these edits forked the member off the album.
func TestEditKeepsMBIDKeyedAlbum(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const rgMBID = "11111111-1111-1111-1111-111111111111"
	const relMBID = "22222222-2222-2222-2222-222222222222"
	for i, title := range []string{"T1", "T2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Alpha/One/0" + string(rune('1'+i)) + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "Alpha", albumArt: "Alpha", album: "One",
			genre: "Rock", year: 2001, mbReleaseGroup: rgMBID, mbRelease: relMBID,
		})
	}
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T1'"))
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T2'"))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1", n)
	}
	albumID := scalarInt(t, st, "SELECT id FROM album")
	wantKey := "mbid:" + relMBID
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != wantKey {
		t.Fatalf("album match_key = %q, want %q", k, wantKey)
	}

	check := func(step string) {
		t.Helper()
		if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
			t.Fatalf("%s: album rows = %d, want 1", step, n)
		}
		if k := scalarStr(t, st, "SELECT match_key FROM album"); k != wantKey {
			t.Fatalf("%s: album match_key = %q, want %q", step, k, wantKey)
		}
		for _, pid := range []model.PID{pid1, pid2} {
			if id := scalarInt(t, st, `SELECT t.album_id FROM track t
				JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(pid)); id != albumID {
				t.Fatalf("%s: album_id for %s = %d, want %d", step, pid, id, albumID)
			}
		}
		rep, err := st.VerifyDerived(ctx)
		if err != nil || !rep.Consistent() {
			t.Fatalf("%s: db verify not clean: %+v (err %v)", step, rep, err)
		}
	}

	attr := model.Attribution{Source: model.SourceUser}
	if err := st.EditItemField(ctx, pid1, "genre", "Jazz", attr, model.LockOf(true), false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	check("genre edit")
	if err := st.EditItemField(ctx, pid2, "album", "Renamed", attr, model.LockOf(true), false); err != nil {
		t.Fatalf("edit album: %v", err)
	}
	check("album edit")
	if v, _ := st.ItemByPID(ctx, pid2); v.Album != "Renamed" {
		t.Fatalf("denormalized album = %q, want Renamed", v.Album)
	}
	if err := st.EditItemField(ctx, pid1, "year", "1999", attr, model.LockOf(true), false); err != nil {
		t.Fatalf("edit year: %v", err)
	}
	check("year edit")
	if v, _ := st.ItemByPID(ctx, pid1); v.Year != 1999 {
		t.Fatalf("denormalized year = %d, want 1999", v.Year)
	}
}

// TestEditYearUnderMBIDReleaseGroup is the heuristic-album sibling: with only the
// release-group mbid set, a partial-member year edit re-keys that member's album (the
// year is part of the heuristic album key) but stays under the same mbid-keyed RG row.
func TestEditYearUnderMBIDReleaseGroup(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const rgMBID = "33333333-3333-3333-3333-333333333333"
	for i, title := range []string{"T1", "T2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Alpha/One/0" + string(rune('1'+i)) + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "Alpha", albumArt: "Alpha", album: "One",
			year: 2001, mbReleaseGroup: rgMBID,
		})
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("rg rows = %d, want 1", n)
	}
	rgID := scalarInt(t, st, "SELECT id FROM release_group")

	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T1'"))
	if err := st.EditItemField(ctx, pid1, "year", "1999",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit year: %v", err)
	}
	// The member re-keyed onto a different album row, still under the same RG.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("rg rows after year edit = %d, want 1", n)
	}
	if id := scalarInt(t, st, `SELECT al.release_group_id FROM track t
		JOIN album al ON al.id = t.album_id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid=?`, string(pid1)); id != rgID {
		t.Fatalf("member's release group = %d, want %d", id, rgID)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", rep, err)
	}
}

// TestEditKeepsSplitCreditAnchor pins the derived-credit anchor: a single-frame
// "A feat. B" credit with no album_artist anchors its release group on the raw string
// at scan time, and an unrelated edit must not move that anchor onto the split primary.
// A stated multi-value list, by contrast, anchors on its first artist and keeps doing
// so across an edit (the curated/stated branch keeps the loaded list).
func TestEditKeepsSplitCreditAnchor(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/one/01.flac", essence: "e1", content: "c1",
		title: "S1", artist: "A feat. B", album: "One",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/two/01.flac", essence: "e2", content: "c2",
		title: "S2", artist: "X", artists: []string{"X", "Y"}, album: "Two",
	})
	keyOne := scalarStr(t, st, "SELECT match_key FROM album WHERE title='One'")
	keyTwo := scalarStr(t, st, "SELECT match_key FROM album WHERE title='Two'")

	attr := model.Attribution{Source: model.SourceUser}
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S1'"))
	if err := st.EditItemField(ctx, pid1, "genre", "Jazz", attr, model.LockOf(true), false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE title='One'"); n != 1 {
		t.Fatalf("album rows for One = %d, want 1 (derived credit re-anchored on the raw string)", n)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE title='One'"); k != keyOne {
		t.Fatalf("album One match_key = %q, want unchanged %q", k, keyOne)
	}

	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S2'"))
	if err := st.EditItemField(ctx, pid2, "genre", "Jazz", attr, model.LockOf(true), false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE title='Two'"); n != 1 {
		t.Fatalf("album rows for Two = %d, want 1 (stated list keeps its first-artist anchor)", n)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE title='Two'"); k != keyTwo {
		t.Fatalf("album Two match_key = %q, want unchanged %q", k, keyTwo)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", rep, err)
	}
}

// TestEditKeepsCaseDriftedDerivedCredit: the contributor list carries entity display
// names in the first-seen casing, so a derived credit whose spelling differs only in
// case from the entities it resolved onto must still read as derived; a genre edit
// then keeps the raw-credit anchor and the chain does not fork.
func TestEditKeepsCaseDriftedDerivedCredit(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Track 1 spells the entity "Alpha"; track 2's credit folds onto it lowercase.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/solo/01.flac", essence: "d1", content: "d1",
		title: "Solo", artist: "Alpha", album: "Solo", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/duet/01.flac", essence: "d2", content: "d2",
		title: "Duet", artist: "alpha feat. beta", album: "Duet", year: 2001,
	})
	keyDuet := scalarStr(t, st, "SELECT match_key FROM album WHERE title='Duet'")

	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='Duet'"))
	if err := st.EditItemField(ctx, pid, "genre", "Jazz",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album WHERE title='Duet'"); n != 1 {
		t.Fatalf("album rows for Duet = %d, want 1 (case drift must not re-anchor)", n)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE title='Duet'"); k != keyDuet {
		t.Fatalf("Duet match_key = %q, want unchanged %q", k, keyDuet)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", rep, err)
	}
}

// TestEditArtistSameValueKeepsRawAnchor: an artist edit stores the credit raw and
// lets resolution re-split it, so setting the field to the value it already holds on
// one member of a raw-anchored album computes the same keys a scan would and does
// not fork the member off its chain.
func TestEditArtistSameValueKeepsRawAnchor(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, title := range []string{"F1", "F2"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/feat/0" + string(rune('1'+i)) + ".flac", essence: "f" + title, content: "c" + title,
			title: title, artist: "A feat. B", album: "One", year: 2001,
		})
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1", n)
	}
	key0 := scalarStr(t, st, "SELECT match_key FROM album")

	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='F1'"))
	if err := st.EditItemField(ctx, pid, "artist", "A feat. B",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit artist: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want 1 (a value-preserving credit edit must not fork)", n)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album"); k != key0 {
		t.Fatalf("album match_key = %q, want unchanged %q", k, key0)
	}
	// The credit still splits into two contributor entities.
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN playable_item pi ON pi.id = ic.item_id
		WHERE pi.pid=? AND ic.role='artist'`, string(pid)); n != 2 {
		t.Errorf("contributor rows = %d, want the split pair", n)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", rep, err)
	}
}
