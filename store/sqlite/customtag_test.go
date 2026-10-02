package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

func putTrackCustom(t *testing.T, st *Store, libID int64, path, essence, content, title string, custom map[string][]string, preserveLocks bool) *model.ScanItemResult {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 1,
			ContentHash: content, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence,
		},
		Track:         model.Track{Artist: "Artist", Album: "Album"},
		CustomTags:    custom,
		PreserveLocks: preserveLocks,
	}
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
	return res
}

func tagValues(t *testing.T, st *Store, pid model.PID, key string) []string {
	t.Helper()
	tags, err := st.ItemTags(context.Background(), pid)
	if err != nil {
		t.Fatalf("item tags: %v", err)
	}
	for _, tg := range tags {
		if tg.Key == key {
			return tg.Values
		}
	}
	return nil
}

func TestSetItemTag(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One", nil, true)
	pid := res.ItemPID

	// Set a multi-valued custom tag (key normalized to uppercase).
	stored, n, err := st.SetItemTag(ctx, pid, "mood", []string{"chill", "upbeat"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false)
	if err != nil {
		t.Fatalf("set tag: %v", err)
	}
	if stored != "MOOD" || n != 2 {
		t.Fatalf("SetItemTag = (%q,%d), want (MOOD,2)", stored, n)
	}
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 2 || got[0] != "chill" || got[1] != "upbeat" {
		t.Fatalf("MOOD values = %v", got)
	}
	// The tag.<KEY> lock is recorded.
	locked, err := st.IsFieldLocked(ctx, pid, "tag.MOOD")
	if err != nil || !locked {
		t.Fatalf("tag.MOOD should be locked: locked=%v err=%v", locked, err)
	}

	// A reserved key is rejected, directing the caller to the right surface.
	if _, _, err := st.SetItemTag(ctx, pid, "artist", []string{"x"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("reserved key should be CodeInvalid, got %v", err)
	}
	// An invalid key is rejected.
	if _, _, err := st.SetItemTag(ctx, pid, "bad=key", []string{"x"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("invalid key should be CodeInvalid, got %v", err)
	}

	// A whitespace-only value trims to empty, so it clears (reporting 0 stored) rather
	// than reading as a set of one value. Even with lock on (the default), a clear must
	// NOT leave a locked-empty tag behind (force is needed to clear the locked MOOD).
	if _, n, err := st.SetItemTag(ctx, pid, "MOOD", []string{"   "}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil || n != 0 {
		t.Fatalf("whitespace-only value should clear (n=0), got n=%d err=%v", n, err)
	}
	if got := tagValues(t, st, pid, "MOOD"); got != nil {
		t.Fatalf("MOOD should be cleared by a whitespace-only value, got %v", got)
	}
	if locked, _ := st.IsFieldLocked(ctx, pid, "tag.MOOD"); locked {
		t.Fatalf("a clear must drop the tag.MOOD lock, not leave a locked-empty tag")
	}
	// Because the clear dropped the lock, a re-set needs no force.
	if _, n, err := st.SetItemTag(ctx, pid, "MOOD", []string{"reset"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil || n != 1 {
		t.Fatalf("re-set after a clear should succeed without force: n=%d err=%v", n, err)
	}
}

// TestSetItemTagRefusesBookOwnedKeysOnBooks: a key the scanner reads into a book field
// is refused as a custom tag on a book, where the scalar edit owns it, and stays a
// plain custom tag on a track.
func TestSetItemTagRefusesBookOwnedKeysOnBooks(t *testing.T) {
	ctx := context.Background()
	st, lib := entityFixture(t)
	book := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/hobbit.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 1000, position: 1,
	})
	attr := model.Attribution{Source: model.SourceUser}
	for _, k := range []string{"SUBTITLE", "edition", "ASIN"} {
		_, _, err := st.SetItemTag(ctx, book.ItemPID, k, []string{"x"}, attr, model.LockOf(false), false)
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("SetItemTag(book, %s) = %v, want CodeInvalid", k, err)
		}
	}
	track := putTrackCustom(t, st, lib.ID, "/lib/song.mp3", "te1", "tc1", "Airbag", nil, false)
	if _, n, err := st.SetItemTag(ctx, track.ItemPID, "SUBTITLE", []string{"Live"}, attr, model.LockOf(false), false); err != nil || n != 1 {
		t.Errorf("SetItemTag(track, SUBTITLE) = (%d, %v), want the tag stored", n, err)
	}
}

func TestScanPreservesLockedCustomTag(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// First scan carries a custom MOOD frame; it is persisted.
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One",
		map[string][]string{"MOOD": {"chill"}}, true)
	pid := res.ItemPID
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "chill" {
		t.Fatalf("scan should persist custom MOOD, got %v", got)
	}

	// User curates and locks MOOD.
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"locked-mood"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil {
		t.Fatalf("set tag: %v", err)
	}

	// A forced rescan (PreserveLocks) must not clobber the locked tag, but a new
	// unlocked tag is still ingested.
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c2", "One",
		map[string][]string{"MOOD": {"from-file"}, "TEMPO": {"fast"}}, true)
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "locked-mood" {
		t.Fatalf("locked MOOD was clobbered by scan: %v", got)
	}
	if got := tagValues(t, st, pid, "TEMPO"); len(got) != 1 || got[0] != "fast" {
		t.Fatalf("unlocked TEMPO should be ingested: %v", got)
	}

	// An --ignore-locks rescan re-derives the tag from the file.
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c3", "One",
		map[string][]string{"MOOD": {"re-derived"}}, false)
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "re-derived" {
		t.Fatalf("--ignore-locks should re-derive MOOD: %v", got)
	}
	// TEMPO was not in the file this time and is not locked, so it is dropped.
	if got := tagValues(t, st, pid, "TEMPO"); got != nil {
		t.Fatalf("TEMPO should be dropped when absent from the file: %v", got)
	}
}

func TestScanRefreshesFTSOnTagChangeWithoutAudioChange(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// First scan: the file carries MOOD=filevalue.
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One",
		map[string][]string{"MOOD": {"filevalue"}}, true)
	pid := res.ItemPID

	// User overrides MOOD (unlocked) to a searchable value.
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"editvalue"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set tag: %v", err)
	}
	sr, _ := st.Search(ctx, "editvalue", read.SearchOptions{})
	if len(sr.Tracks) != 1 {
		t.Fatalf("edit value should be searchable, got %+v", sr.Tracks)
	}

	// A rescan with IDENTICAL audio bytes (no content change) re-derives MOOD=filevalue
	// from the file (the tag is unlocked). The search row must follow even though the
	// entity/audio-change path did not run.
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One",
		map[string][]string{"MOOD": {"filevalue"}}, true)
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "filevalue" {
		t.Fatalf("MOOD should be re-derived to filevalue, got %v", got)
	}
	after, _ := st.Search(ctx, "filevalue", read.SearchOptions{})
	if len(after.Tracks) != 1 {
		t.Fatalf("re-derived value should be searchable after a no-audio-change rescan, got %+v", after.Tracks)
	}
	stale, _ := st.Search(ctx, "editvalue", read.SearchOptions{})
	if len(stale.Tracks) != 0 {
		t.Fatalf("stale edit value should no longer be searchable, got %+v", stale.Tracks)
	}
}

func TestItemTagsNotFound(t *testing.T) {
	st, _ := entityFixture(t)
	if _, err := st.ItemTags(context.Background(), "does-not-exist"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("ItemTags for a missing item should be CodeNotFound, got %v", err)
	}
}

func TestCustomTagIsSearchable(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One", nil, true)
	pid := res.ItemPID

	// A distinctive custom-tag value should surface the item in full-text search.
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"melancholic"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set tag: %v", err)
	}
	sr, err := st.Search(ctx, "melancholic", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(sr.Tracks) != 1 || sr.Tracks[0].PID != pid {
		t.Fatalf("custom tag value should be searchable, got %+v", sr.Tracks)
	}
}

// TestScanDropsALockedRowUnderANewlyReservedKey: newly reserving a key (MEDIA moved to
// album.media) takes effect on the next rescan, and a lock cannot exempt it, since
// SetItemTag rejects a reserved key and a surviving row would be uneditable. The fixture
// is written directly: only a catalog predating the reservation can hold such a row.
func TestScanDropsALockedRowUnderANewlyReservedKey(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One",
		map[string][]string{"MOOD": {"chill"}}, true)
	pid := res.ItemPID
	// The control: a curated, locked tag under a key that stays custom.
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"chill"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil {
		t.Fatalf("lock MOOD: %v", err)
	}

	var itemID int64
	if err := st.read.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE pid = ?", string(pid)).Scan(&itemID); err != nil {
		t.Fatalf("resolve item: %v", err)
	}
	err := st.writeTx(ctx, func(tx *sql.Tx) error {
		if err := writeItemTagTx(ctx, tx, itemID, "MEDIA", []string{"CD"}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, updated_at)
			VALUES (?, 'tag.MEDIA', 'user', 1, ?)`, itemID, nowNS())
		return err
	})
	if err != nil {
		t.Fatalf("seed the legacy row: %v", err)
	}
	if got := tagValues(t, st, pid, "MEDIA"); len(got) != 1 {
		t.Fatalf("fixture did not store the legacy row: %v", got)
	}

	// A lock-preserving rescan: MOOD keeps, MEDIA does not.
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c2", "One",
		map[string][]string{"MOOD": {"from-file"}}, true)
	if got := tagValues(t, st, pid, "MEDIA"); got != nil {
		t.Errorf("locked tag.MEDIA survived the sync as %v; a reserved key has no custom-tag surface", got)
	}
	// The lock row goes with the values. IsCuratableField refuses a reserved tag.<KEY>,
	// so a surviving row would be unreachable by SetItemTag and UnlockField alike.
	var orphans int
	if err := st.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM field_provenance WHERE item_id=? AND field='tag.MEDIA'", itemID).
		Scan(&orphans); err != nil {
		t.Fatalf("count provenance: %v", err)
	}
	if orphans != 0 {
		t.Errorf("tag.MEDIA provenance rows = %d, want 0 (nothing could remove them)", orphans)
	}
	if err := st.UnlockField(ctx, pid, "tag.MEDIA"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("unlocking a reserved key should be CodeInvalid, got %v", err)
	}
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "chill" {
		t.Errorf("locked MOOD = %v, want it still curated (the drop must be specific to reserved keys)", got)
	}
}

// TestScanDropsAnUnlockedRowUnderANewlyReservedKey is the same sweep for a row that was
// never locked. It is just as unreachable once reserved (IsCuratableField refuses the
// field whatever its lock bit), so iterating only the locked keys would leave it behind.
func TestScanDropsAnUnlockedRowUnderANewlyReservedKey(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One",
		map[string][]string{"MOOD": {"chill"}}, true)
	pid := res.ItemPID

	var itemID int64
	if err := st.read.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE pid = ?", string(pid)).Scan(&itemID); err != nil {
		t.Fatalf("resolve item: %v", err)
	}
	err := st.writeTx(ctx, func(tx *sql.Tx) error {
		if err := writeItemTagTx(ctx, tx, itemID, "RELEASECOUNTRY", []string{"GB"}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, updated_at)
			VALUES (?, 'tag.RELEASECOUNTRY', 'user', 0, ?)`, itemID, nowNS())
		return err
	})
	if err != nil {
		t.Fatalf("seed the legacy row: %v", err)
	}

	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c2", "One",
		map[string][]string{"MOOD": {"chill"}}, true)
	if got := tagValues(t, st, pid, "RELEASECOUNTRY"); got != nil {
		t.Errorf("reserved tag survived the sync as %v", got)
	}
	var orphans int
	if err := st.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM field_provenance WHERE item_id=? AND field='tag.RELEASECOUNTRY'", itemID).
		Scan(&orphans); err != nil {
		t.Fatalf("count provenance: %v", err)
	}
	if orphans != 0 {
		t.Errorf("unlocked tag.RELEASECOUNTRY provenance rows = %d, want 0", orphans)
	}
}

// TestScanRetiresTheFoldedWireSpellings is the MEDIA precedent applied to the frame
// spellings WaxLabel now folds onto canonical keys on every format's read path. A
// catalog scanned before the bump holds these rows from an ID3 TXXX, MP4 freeform,
// APEv2, or Vorbis frame, and a Matroska one cannot be the fixture: those already
// folded a release earlier, so they left no rows behind. The rows are written
// directly here for the same reason the MEDIA fixture is.
func TestScanRetiresTheFoldedWireSpellings(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.mp3", "e1", "c1", "One",
		map[string][]string{"MOOD": {"chill"}}, true)
	pid := res.ItemPID

	var itemID int64
	if err := st.read.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE pid = ?", string(pid)).Scan(&itemID); err != nil {
		t.Fatalf("resolve item: %v", err)
	}
	// CATALOG_NUMBER locked, PART_NUMBER not: neither lock state exempts a key that
	// has become reserved.
	seeded := map[string]int{"CATALOG_NUMBER": 1, "PART_NUMBER": 0}
	err := st.writeTx(ctx, func(tx *sql.Tx) error {
		for key, locked := range seeded {
			if err := writeItemTagTx(ctx, tx, itemID, key, []string{"legacy"}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, updated_at)
				VALUES (?, ?, 'user', ?, ?)`, itemID, model.TagLockField(key), locked, nowNS()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed the legacy rows: %v", err)
	}
	for key := range seeded {
		if got := tagValues(t, st, pid, key); len(got) != 1 {
			t.Fatalf("fixture did not store %s: %v", key, got)
		}
	}

	putTrackCustom(t, st, lib.ID, "/lib/1.mp3", "e1", "c2", "One",
		map[string][]string{"MOOD": {"chill"}}, true)

	for key := range seeded {
		if got := tagValues(t, st, pid, key); got != nil {
			t.Errorf("%s survived the sync as %v; a reserved key has no custom-tag surface", key, got)
		}
		var orphans int
		if err := st.read.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM field_provenance WHERE item_id=? AND field=?",
			itemID, model.TagLockField(key)).Scan(&orphans); err != nil {
			t.Fatalf("count provenance: %v", err)
		}
		if orphans != 0 {
			t.Errorf("tag.%s provenance rows = %d, want 0 (nothing could remove them)", key, orphans)
		}
		if _, _, err := st.SetItemTag(ctx, pid, key, []string{"x"},
			model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("SetItemTag(%s) = %v, want CodeInvalid for a reserved key", key, err)
		}
	}
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "chill" {
		t.Errorf("MOOD = %v, want it untouched (the drop must be specific to reserved keys)", got)
	}
}

// TestVerifyReclaimsUnreachableReservedTagProvenance covers the row the scan can never
// reach. syncItemTagsTx returns on its fast path when an item holds no custom tags and
// the file carries none, which is before the reserved sweep, so a lone tag.<RESERVED>
// provenance row on an otherwise plain file survives every rescan. Widening the fast
// path would cost each plain file a second query per scan, so db verify counts the row
// as reclaimable and --fix deletes it.
func TestVerifyReclaimsUnreachableReservedTagProvenance(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/plain.mp3", "e1", "c1", "One", nil, true)
	pid := res.ItemPID

	var itemID int64
	if err := st.read.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE pid = ?", string(pid)).Scan(&itemID); err != nil {
		t.Fatalf("resolve item: %v", err)
	}
	err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, updated_at)
			VALUES (?, 'tag.PUBLISHER', 'user', 1, ?)`, itemID, nowNS())
		return err
	})
	if err != nil {
		t.Fatalf("seed the stranded row: %v", err)
	}
	// A rescan of the plain file leaves it: this is the gap, not an oversight in the test.
	putTrackCustom(t, st, lib.ID, "/lib/plain.mp3", "e1", "c2", "One", nil, true)

	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.OrphanReservedTagProvenance != 1 {
		t.Errorf("orphan reserved tag provenance = %d, want 1", rep.OrphanReservedTagProvenance)
	}
	// Garbage, not corruption: the same split the orphan-art counts get.
	if !rep.Consistent() {
		t.Errorf("a stranded provenance row made the catalog report inconsistent: %+v", rep)
	}
	if !rep.Reclaimable() {
		t.Error("a stranded provenance row should be reported as reclaimable")
	}

	n, err := st.GCReservedTagProvenance(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n != 1 {
		t.Errorf("GCReservedTagProvenance = %d, want 1", n)
	}
	rep, err = st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify after gc: %v", err)
	}
	if rep.OrphanReservedTagProvenance != 0 || rep.Reclaimable() {
		t.Errorf("row survived the sweep: %+v", rep)
	}
}

// TestGCReservedTagProvenanceKeepsLiveLocks: the sweep is keyed on the reserved set, so
// a curated lock under a key that is still a custom tag must be left alone.
func TestGCReservedTagProvenanceKeepsLiveLocks(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.mp3", "e1", "c1", "One",
		map[string][]string{"MOOD": {"chill"}}, true)
	pid := res.ItemPID
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"chill"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil {
		t.Fatalf("lock MOOD: %v", err)
	}
	n, err := st.GCReservedTagProvenance(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n != 0 {
		t.Errorf("GCReservedTagProvenance = %d, want 0 with nothing reserved to sweep", n)
	}
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "chill" {
		t.Errorf("MOOD = %v, want it still curated", got)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.OrphanReservedTagProvenance != 0 {
		t.Errorf("a live tag.MOOD lock counted as orphaned (%d)", rep.OrphanReservedTagProvenance)
	}
}

// TestVerifyReclaimsStrandedTagKeys: tightening the key rule (WaxLabel 1.6 dropped
// '~') leaves rows under a key nothing can query, edit, or unlock, and the scan keeps a
// locked one on purpose. db verify reports them as reclaimable garbage, not corruption,
// and --fix removes them without touching a live curated tag.
func TestVerifyReclaimsStrandedTagKeys(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.mp3", "e1", "c1", "One",
		map[string][]string{"MOOD": {"chill"}}, true)
	pid := res.ItemPID

	var itemID int64
	if err := st.read.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE pid = ?", string(pid)).Scan(&itemID); err != nil {
		t.Fatalf("resolve item: %v", err)
	}
	// Seeded raw: SetItemTag refuses the key now, which is the point.
	err := st.writeTx(ctx, func(tx *sql.Tx) error {
		for i, v := range []string{"a", "b"} {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO item_tag(item_id, key, value, position) VALUES (?,?,?,?)", itemID, "ODD~KEY", v, i); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, updated_at)
			VALUES (?, 'tag.ODD~KEY', 'user', 1, ?)`, itemID, nowNS())
		return err
	})
	if err != nil {
		t.Fatalf("seed the stranded rows: %v", err)
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.StrandedTagKeyRows != 3 {
		t.Errorf("stranded tag key rows = %d, want 3 (two values and a lock)", rep.StrandedTagKeyRows)
	}
	if !rep.Consistent() {
		t.Errorf("stranded rows made the catalog report inconsistent: %+v", rep)
	}
	if !rep.Reclaimable() {
		t.Error("stranded rows should be reported as reclaimable")
	}

	n, err := st.GCStrandedTagKeys(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n != 3 {
		t.Errorf("GCStrandedTagKeys = %d, want 3", n)
	}
	rep, err = st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify after gc: %v", err)
	}
	if rep.StrandedTagKeyRows != 0 || rep.Reclaimable() {
		t.Errorf("after gc: stranded = %d, reclaimable = %t; want 0 and false", rep.StrandedTagKeyRows, rep.Reclaimable())
	}
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "chill" {
		t.Errorf("MOOD = %v, want the live tag untouched", got)
	}
}

// TestTagEditOwesItsFiles: a custom tag set in the catalog alone owes the item's file the
// value, a set that changes nothing owes nothing, and an alias spelling owes nothing,
// since a tag write under it lands on another field and so no write-back can pay it.
func TestTagEditOwesItsFiles(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One", map[string][]string{"MOOD": {"calm"}}, true)
	user := model.Attribution{Source: model.SourceUser}
	owed := func() []string {
		t.Helper()
		keys, err := owedKeysOf(ctx, st, res.FilePID)
		if err != nil {
			t.Fatalf("owed rows: %v", err)
		}
		slices.Sort(keys)
		return keys
	}

	if _, _, err := st.SetItemTag(ctx, res.ItemPID, "MOOD", []string{" calm "}, user, model.LockUnchanged, false); err != nil {
		t.Fatalf("set unchanged: %v", err)
	}
	if got := owed(); got != nil {
		t.Fatalf("owed after a set that changed nothing = %v, want none", got)
	}
	if _, _, err := st.SetItemTag(ctx, res.ItemPID, "mood", []string{"happy"}, user, model.LockUnchanged, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := owed(); !slices.Equal(got, []string{"tag.MOOD"}) {
		t.Fatalf("owed after a catalog-only set = %v, want [tag.MOOD]", got)
	}
	if _, _, err := st.SetItemTag(ctx, res.ItemPID, "YEAR", []string{"1999"}, user, model.LockUnchanged, false); err != nil {
		t.Fatalf("set an alias spelling: %v", err)
	}
	if got := owed(); !slices.Equal(got, []string{"tag.MOOD"}) {
		t.Fatalf("owed after an alias spelling = %v, want [tag.MOOD] alone", got)
	}
}

// TestScanSettlesOwedTagRows: a scan pays a custom tag's owed row once the file states
// what the catalog holds. A locked edit stays owed while the file differs and goes once
// the file carries it, and an unlocked edit the scan re-derives away owes nothing.
func TestScanSettlesOwedTagRows(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	calm := map[string][]string{"MOOD": {"calm"}}
	happy := map[string][]string{"MOOD": {"happy"}}
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One", calm, true)
	user := model.Attribution{Source: model.SourceUser}
	owed := func() []string {
		t.Helper()
		keys, err := owedKeysOf(ctx, st, res.FilePID)
		if err != nil {
			t.Fatalf("owed rows: %v", err)
		}
		return keys
	}

	if _, _, err := st.SetItemTag(ctx, res.ItemPID, "MOOD", []string{"happy"}, user, model.LockOn, false); err != nil {
		t.Fatalf("set locked: %v", err)
	}
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One", calm, true)
	if got := owed(); !slices.Equal(got, []string{"tag.MOOD"}) {
		t.Fatalf("owed after a rescan of a file still saying calm = %v, want [tag.MOOD]", got)
	}
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c2", "One", happy, true)
	if got := owed(); got != nil {
		t.Fatalf("owed once the file carries the value = %v, want none", got)
	}

	if _, _, err := st.SetItemTag(ctx, res.ItemPID, "MOOD", []string{"sad"}, user, model.LockOff, true); err != nil {
		t.Fatalf("set unlocked: %v", err)
	}
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c2", "One", happy, true)
	if got := tagValues(t, st, res.ItemPID, "MOOD"); !slices.Equal(got, []string{"happy"}) {
		t.Fatalf("MOOD = %v, want the unlocked edit re-derived to the file's [happy]", got)
	}
	if got := owed(); got != nil {
		t.Fatalf("owed after the edit was re-derived away = %v, want none", got)
	}
}

// TestScanSettlesAReDerivedBookTagOnEveryPart: a book's custom tags are its primary
// part's, so once that part's scan re-derives a catalog-only edit away, no part owes it,
// the parts the scan did not read included.
func TestScanSettlesAReDerivedBookTagOnEveryPart(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	partOne := bookSpec{path: "/lib/B/1.m4b", essence: "b1", content: "b1", title: "Book", author: "Auth", position: 1}
	one := putBook(t, st, lib.ID, partOne)
	two := putBook(t, st, lib.ID, bookSpec{path: "/lib/B/2.m4b", essence: "b2", content: "b2", title: "Book", author: "Auth", position: 2})
	if one.ItemPID != two.ItemPID {
		t.Fatalf("parts landed on %s and %s, want one book", one.ItemPID, two.ItemPID)
	}
	if _, _, err := st.SetItemTag(ctx, one.ItemPID, "MOOD", []string{"tense"}, model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	for _, f := range []model.PID{one.FilePID, two.FilePID} {
		if keys, err := owedKeysOf(ctx, st, f); err != nil || !slices.Equal(keys, []string{"tag.MOOD"}) {
			t.Fatalf("owed on %s after the set = %v (err %v), want [tag.MOOD]", f, keys, err)
		}
	}

	partOne.preserveLocks = true
	putBook(t, st, lib.ID, partOne)
	if got := tagValues(t, st, one.ItemPID, "MOOD"); got != nil {
		t.Fatalf("MOOD = %v, want the unlocked edit re-derived away", got)
	}
	for _, f := range []model.PID{one.FilePID, two.FilePID} {
		if keys, err := owedKeysOf(ctx, st, f); err != nil || keys != nil {
			t.Errorf("owed on %s after the re-derive = %v (err %v), want none", f, keys, err)
		}
	}
}

// TestScanSettlesABookPartThatCarriesTheTag: a part that is not the book's primary still
// pays its own owed row once its file carries the catalog's values, and only its own.
func TestScanSettlesABookPartThatCarriesTheTag(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	one := putBook(t, st, lib.ID, bookSpec{path: "/lib/B/1.m4b", essence: "b1", content: "b1", title: "Book", author: "Auth", position: 1})
	partTwo := bookSpec{path: "/lib/B/2.m4b", essence: "b2", content: "b2", title: "Book", author: "Auth", position: 2}
	two := putBook(t, st, lib.ID, partTwo)
	if _, _, err := st.SetItemTag(ctx, one.ItemPID, "MOOD", []string{"tense"}, model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("set: %v", err)
	}

	partTwo.content, partTwo.preserveLocks = "b2-tagged", true
	partTwo.custom = map[string][]string{"MOOD": {"tense"}}
	putBook(t, st, lib.ID, partTwo)
	if keys, err := owedKeysOf(ctx, st, two.FilePID); err != nil || keys != nil {
		t.Errorf("owed on the part carrying the tag = %v (err %v), want none", keys, err)
	}
	if keys, err := owedKeysOf(ctx, st, one.FilePID); err != nil || !slices.Equal(keys, []string{"tag.MOOD"}) {
		t.Errorf("owed on the primary = %v (err %v), want [tag.MOOD] still", keys, err)
	}
}

// TestIgnoreLocksRescanSettlesALockedTagEverywhere: a rescan that ignores locks replaces even
// a locked tag with the primary's file, so the edit the owed rows were about is gone from
// every part.
func TestIgnoreLocksRescanSettlesALockedTagEverywhere(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	partOne := bookSpec{path: "/lib/B/1.m4b", essence: "b1", content: "b1", title: "Book", author: "Auth", position: 1}
	one := putBook(t, st, lib.ID, partOne)
	two := putBook(t, st, lib.ID, bookSpec{path: "/lib/B/2.m4b", essence: "b2", content: "b2", title: "Book", author: "Auth", position: 2})
	if _, _, err := st.SetItemTag(ctx, one.ItemPID, "MOOD", []string{"tense"}, model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("set: %v", err)
	}

	putBook(t, st, lib.ID, partOne) // preserveLocks false: the --ignore-locks rescan
	if got := tagValues(t, st, one.ItemPID, "MOOD"); got != nil {
		t.Fatalf("MOOD = %v, want the locked tag replaced by the file's nothing", got)
	}
	for _, f := range []model.PID{one.FilePID, two.FilePID} {
		if keys, err := owedKeysOf(ctx, st, f); err != nil || keys != nil {
			t.Errorf("owed on %s = %v (err %v), want none", f, keys, err)
		}
	}
}

// TestVerifyReclaimsUnpayableTagOwedRows: an owed row under a key no write-back can carry,
// one the key rule has since stopped accepting, one WaxBin has since reserved, or one a
// tag write puts onto another field, is stranded with the tag it was about, so db verify
// counts it and --fix reclaims it, while a live key's row stays.
func TestVerifyReclaimsUnpayableTagOwedRows(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.mp3", "e1", "c1", "One", nil, true)
	if _, _, err := st.SetItemTag(ctx, res.ItemPID, "MOOD", []string{"calm"}, model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Seeded raw: each was owed before the rule that strands it.
	for _, key := range []string{"tag.ODD~KEY", "tag.BPM", "tag.YEAR"} {
		if err := st.AddFileDiagnostic(ctx, res.FilePID, model.OriginEdit, model.FileDiagnostic{
			Code: model.DiagTagWriteOwed, Severity: model.SeverityInfo, TagKey: key, Detail: key + " owed",
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.StrandedTagKeyRows != 3 || !rep.Reclaimable() {
		t.Fatalf("stranded rows = %d (reclaimable %v), want the three unpayable owed rows", rep.StrandedTagKeyRows, rep.Reclaimable())
	}
	if n, err := st.GCStrandedTagKeys(ctx); err != nil || n != 3 {
		t.Fatalf("gc = %d (err %v), want 3", n, err)
	}
	if keys, err := owedKeysOf(ctx, st, res.FilePID); err != nil || !slices.Equal(keys, []string{"tag.MOOD"}) {
		t.Errorf("owed after gc = %v (err %v), want the live key alone", keys, err)
	}
}

// TestForgetItemTagPaysEveryFile: ForgetItemTag follows a write-back that cleared the tag
// from every file, so once the catalog drops it too no file owes it.
func TestForgetItemTagPaysEveryFile(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	one := putBook(t, st, lib.ID, bookSpec{path: "/lib/B/1.m4b", essence: "b1", content: "b1", title: "Book", author: "Auth", position: 1})
	two := putBook(t, st, lib.ID, bookSpec{path: "/lib/B/2.m4b", essence: "b2", content: "b2", title: "Book", author: "Auth", position: 2})
	if _, _, err := st.SetItemTag(ctx, one.ItemPID, "TITLESORT", []string{"Curated"}, model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.ForgetItemTag(ctx, one.ItemPID, "TITLESORT"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got := tagValues(t, st, one.ItemPID, "TITLESORT"); got != nil {
		t.Errorf("TITLESORT = %v, want it dropped", got)
	}
	for _, f := range []model.PID{one.FilePID, two.FilePID} {
		if keys, err := owedKeysOf(ctx, st, f); err != nil || keys != nil {
			t.Errorf("owed on %s = %v (err %v), want none", f, keys, err)
		}
	}
}
