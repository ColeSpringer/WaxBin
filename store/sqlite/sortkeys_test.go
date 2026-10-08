package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// storedKey reads one generated key column back.
func storedKey(t *testing.T, st *Store, query string, args ...any) string {
	t.Helper()
	var got string
	if err := st.rdb().QueryRowContext(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("read key (%s): %v", query, err)
	}
	return got
}

// changesSince returns the (entity_type, pid) of every delta appended after seq.
func changesSince(t *testing.T, st *Store, seq int64) [][2]string {
	t.Helper()
	rows, err := st.rdb().QueryContext(context.Background(),
		"SELECT entity_type, entity_pid FROM change_log WHERE seq > ? ORDER BY seq", seq)
	if err != nil {
		t.Fatalf("read change_log: %v", err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var kind, pid string
		if err := rows.Scan(&kind, &pid); err != nil {
			t.Fatalf("scan change_log: %v", err)
		}
		out = append(out, [2]string{kind, pid})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate change_log: %v", err)
	}
	return out
}

// TestRefreshSortKeysClearsDrift mirrors TestVerifyDetectsSortKeyDrift: the drift
// the check reports is exactly what the repair clears.
func TestRefreshSortKeysClearsDrift(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	seedTwoTracks(t, st, lib.ID)

	for _, stmt := range []string{
		"UPDATE artist SET sort_key = 'WRONG' WHERE name = 'Radiohead'",
		"UPDATE album SET sort_key = 'WRONG'",
		"UPDATE release_group SET sort_key = 'WRONG'",
		"UPDATE genre SET sort_key = 'WRONG'",
		"UPDATE playable_item SET sort_key = 'WRONG'",
	} {
		if _, err := st.wdb().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("corrupt (%s): %v", stmt, err)
		}
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SortKeyDrift != 6 { // 2 items, artist, album, release group, genre
		t.Fatalf("sort-key drift = %d, want 6", rep.SortKeyDrift)
	}
	if rep.Consistent() {
		t.Errorf("stale keys left the report consistent: %+v", rep)
	}

	n, err := st.RefreshSortKeys(ctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n != 6 {
		t.Errorf("rewrote %d rows, want 6", n)
	}
	rep, err = st.VerifyDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Consistent() {
		t.Fatalf("repair left drift: %+v", rep)
	}
	if got := storedKey(t, st, "SELECT sort_key FROM artist WHERE name = 'Radiohead'"); got != "radiohead" {
		t.Errorf("artist sort key = %q, want %q", got, "radiohead")
	}

	// Idempotent: nothing moved, so nothing is rewritten.
	if n, err := st.RefreshSortKeys(ctx); err != nil || n != 0 {
		t.Errorf("second refresh rewrote %d rows (err %v), want 0", n, err)
	}
}

// TestRefreshSortKeysFolds covers the change that motivates the repair: a key the
// ASCII-only implementation wrote buckets under E rather than after Z.
func TestRefreshSortKeysFolds(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/e/1.flac", essence: "e1", content: "c1", title: "Non, je ne regrette rien",
		artist: "Édith Piaf", album: "Éternelle", genre: "Chanson",
	})
	// What the pre-folding implementation stored: lowercased, codepoint-ordered.
	if _, err := st.wdb().ExecContext(ctx,
		"UPDATE artist SET sort_key = 'édith piaf' WHERE name = 'Édith Piaf'"); err != nil {
		t.Fatal(err)
	}
	if rep, _ := st.VerifyDerived(ctx); rep.SortKeyDrift != 1 {
		t.Fatalf("an unfolded key should report as drift, got %d", rep.SortKeyDrift)
	}
	if _, err := st.RefreshSortKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if got := storedKey(t, st, "SELECT sort_key FROM artist WHERE name = 'Édith Piaf'"); got != "edith piaf" {
		t.Errorf("folded artist key = %q, want %q", got, "edith piaf")
	}
}

// TestRefreshSortKeysUsesCuratedOverride proves a curated entity is recomputed from
// the override the user typed, not from the display name.
func TestRefreshSortKeysUsesCuratedOverride(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/e/1.flac", essence: "e1", content: "c1", title: "La Vie en rose",
		artist: "Édith Piaf", album: "Éternelle",
	})
	artistPID := storedKey(t, st, "SELECT pid FROM artist WHERE name = 'Édith Piaf'")
	if _, err := st.EditEntityFields(ctx, model.MergeArtist, model.PID(artistPID),
		map[string]string{"sort": "Piaf, Édith"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("curate sort: %v", err)
	}
	// A curated key is not drift, because the column holds SortKey(override).
	if rep, _ := st.VerifyDerived(ctx); rep.SortKeyDrift != 0 {
		t.Fatalf("a curated override should not read as drift, got %d", rep.SortKeyDrift)
	}

	if _, err := st.wdb().ExecContext(ctx, "UPDATE artist SET sort_key = 'WRONG'"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RefreshSortKeys(ctx); err != nil {
		t.Fatal(err)
	}
	got := storedKey(t, st, "SELECT sort_key FROM artist WHERE name = 'Édith Piaf'")
	if want := model.SortKey("Piaf, Édith"); got != want {
		t.Errorf("curated key recomputed to %q, want %q (the override, folded)", got, want)
	}
	if got == model.SortKey("Édith Piaf") {
		t.Error("curated key was recomputed from the display name, discarding the override")
	}
	if rep, _ := st.VerifyDerived(ctx); rep.SortKeyDrift != 0 {
		t.Errorf("repair left curated drift: %d", rep.SortKeyDrift)
	}
}

// TestSortKeysAreRecomputedFromSpellings: db verify counts a key that does not fold from
// its spelling (or, with none, from the name it sorts by), and the repair writes exactly
// that key back, leaving every spelling, a locked one included, as it was.
func TestSortKeysAreRecomputedFromSpellings(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	tr := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "The Beatles", artistSort: "Beatles, Thé", album: "A", composer: "Antonín Dvořák",
	}).ItemPID
	locked := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", album: "A",
		composer: "Antonín Dvořák",
	}).ItemPID
	bk := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/1.m4b", essence: "be1", content: "bc1", title: "Kafka on the Shore",
		author: "Haruki Murakami", authorSort: "Murakami, Haruki ٢",
	}).ItemPID
	user := model.Attribution{Source: model.SourceUser}
	if err := st.EditItemField(ctx, bk, "author_sort", "Murakami, Haruki ٢", user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	if err := st.EditItemField(ctx, locked, "composer_sort", "Dvořák, Antonín", user, model.LockOf(true), false); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE track SET artist_sort_key = 'stale', composer_sort_key = 'stale'",
		"UPDATE book SET author_sort_key = 'stale'",
	} {
		if _, err := st.wdb().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SortKeyDrift != 5 {
		t.Fatalf("sort key drift = %d, want the five stale keys", rep.SortKeyDrift)
	}
	if n, err := st.RefreshSortKeys(ctx); err != nil || n != 5 {
		t.Fatalf("RefreshSortKeys rewrote %d (err %v), want 5", n, err)
	}
	if sp, k := sortCols(t, st, tr, "track", "artist_sort", "artist_sort_key"); sp != "Beatles, Thé" || k != "beatles, the" {
		t.Errorf("artist sort after the repair = (%q, %q)", sp, k)
	}
	if sp, k := sortCols(t, st, tr, "track", "composer_sort", "composer_sort_key"); sp != "" || k != model.SortKey("Antonín Dvořák") {
		t.Errorf("composer sort after the repair = (%q, %q)", sp, k)
	}
	if sp, k := sortCols(t, st, locked, "track", "composer_sort", "composer_sort_key"); sp != "Dvořák, Antonín" || k != "dvorak, antonin" {
		t.Errorf("locked composer sort after the repair = (%q, %q), want the spelling kept and its key", sp, k)
	}
	// A non-ASCII digit run comes out padded, and the locked spelling keeps its bytes.
	if sp, k := sortCols(t, st, bk, "book", "author_sort", "author_sort_key"); sp != "Murakami, Haruki ٢" || k != "murakami, haruki 0000000002" {
		t.Errorf("author sort after the repair = (%q, %q)", sp, k)
	}
	if n, err := st.RefreshSortKeys(ctx); err != nil || n != 0 {
		t.Errorf("second repair rewrote %d (err %v), want 0", n, err)
	}
	assertVerifyClean(t, st)
}

// TestRefreshSortKeysCoversAliasAndSeriesSeq covers the two columns that were in
// neither the drift check nor any repair before.
func TestRefreshSortKeysCoversAliasAndSeriesSeq(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/e/1.flac", essence: "e1", content: "c1", title: "One", artist: "Édith Piaf", album: "A",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/bk/1.m4b", essence: "be1", content: "bc1", title: "The Hobbit",
		author: "Tolkien", series: "Middle-earth", seq: "2",
	})
	artistPID := storedKey(t, st, "SELECT pid FROM artist WHERE name = 'Édith Piaf'")
	if _, err := st.wdb().ExecContext(ctx, `INSERT INTO artist_alias(artist_id, name, sort_key, is_primary)
		SELECT id, 'Piaf, Édith', 'piaf, édith', 0 FROM artist WHERE pid = ?`, artistPID); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	if _, err := st.wdb().ExecContext(ctx, "UPDATE book SET series_seq_sort = 'WRONG'"); err != nil {
		t.Fatal(err)
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SortKeyDrift != 2 {
		t.Fatalf("alias + series-sequence drift = %d, want 2", rep.SortKeyDrift)
	}

	before, _ := st.LatestChangeSeq(ctx)
	if n, err := st.RefreshSortKeys(ctx); err != nil || n != 2 {
		t.Fatalf("refresh rewrote %d rows (err %v), want 2", n, err)
	}
	if rep, _ := st.VerifyDerived(ctx); rep.SortKeyDrift != 0 {
		t.Errorf("repair left drift: %d", rep.SortKeyDrift)
	}
	if got := storedKey(t, st, "SELECT sort_key FROM artist_alias"); got != "piaf, edith" {
		t.Errorf("alias key = %q, want %q", got, "piaf, edith")
	}
	if got, want := storedKey(t, st, "SELECT series_seq_sort FROM book"), model.SortKey("2"); got != want {
		t.Errorf("series_seq_sort = %q, want %q", got, want)
	}

	// An alias has no pid, so its delta names the artist it belongs to.
	var sawAlias bool
	for _, c := range changesSince(t, st, before) {
		if c[0] == "artist" && c[1] == artistPID {
			sawAlias = true
		}
	}
	if !sawAlias {
		t.Errorf("a rewritten alias should emit its artist's delta, got %v", changesSince(t, st, before))
	}
}

// TestRefreshSortKeysEmitsDeltas checks the change feed: a rewritten entity emits
// its own delta and nothing else, and a run that moves nothing stays silent.
func TestRefreshSortKeysEmitsDeltas(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	seedTwoTracks(t, st, lib.ID)

	before, _ := st.LatestChangeSeq(ctx)
	if _, err := st.wdb().ExecContext(ctx, "UPDATE artist SET sort_key = 'WRONG'"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RefreshSortKeys(ctx); err != nil || n != 1 {
		t.Fatalf("refresh rewrote %d rows (err %v), want 1", n, err)
	}
	got := changesSince(t, st, before)
	artistPID := storedKey(t, st, "SELECT pid FROM artist WHERE name = 'Radiohead'")
	// EditEntityFields fans an item delta out to every member; a bulk refold
	// deliberately does not.
	if len(got) != 1 || got[0] != [2]string{"artist", artistPID} {
		t.Errorf("deltas = %v, want one artist delta for %s", got, artistPID)
	}

	before, _ = st.LatestChangeSeq(ctx)
	if _, err := st.RefreshSortKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.LatestChangeSeq(ctx); after != before {
		t.Errorf("a refresh that moved nothing emitted %d deltas", after-before)
	}
}

// TestRefreshSortKeysSpansBatches exercises the mid-stream commit. The rows are
// inserted directly because scanning past the batch size would take minutes.
func TestRefreshSortKeysSpansBatches(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	const n = sortKeyBatch + 25
	if _, err := st.wdb().ExecContext(ctx, `
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < ?)
		INSERT INTO artist(pid, name, sort_key, match_key)
		SELECT 'pid' || i, 'Édith ' || i, 'WRONG', 'edith ' || i FROM seq`, n); err != nil {
		t.Fatalf("seed %d artists: %v", n, err)
	}

	before, _ := st.LatestChangeSeq(ctx)
	rewritten, err := st.RefreshSortKeys(ctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if rewritten != n {
		t.Errorf("rewrote %d rows, want all %d (the run crosses %d, the batch size)", rewritten, n, sortKeyBatch)
	}
	var stale int
	if err := st.rdb().QueryRowContext(ctx, "SELECT COUNT(*) FROM artist WHERE sort_key = 'WRONG'").Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Errorf("%d rows kept their stale key, so a batch was dropped", stale)
	}
	if after, _ := st.LatestChangeSeq(ctx); after-before != int64(n) {
		t.Errorf("emitted %d deltas across the batches, want %d", after-before, n)
	}
}

// TestMergeAppliesInheritedSortOverride guards the invariant against the one other
// path that moves a sort override: a merge inherits the loser's curation row, so
// the survivor's column has to follow or the merge leaves the catalog failing
// db verify.
func TestMergeAppliesInheritedSortOverride(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "Edith Piaf", album: "X"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "Piaf", album: "Y"})
	survivor := storedKey(t, st, "SELECT pid FROM artist WHERE name = 'Edith Piaf'")
	loser := storedKey(t, st, "SELECT pid FROM artist WHERE name = 'Piaf'")

	if _, err := st.EditEntityFields(ctx, model.MergeArtist, model.PID(loser),
		map[string]string{"sort": "Piaf, Edith"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeArtist, model.PID(survivor), model.PID(loser)); err != nil {
		t.Fatal(err)
	}

	if got, want := storedKey(t, st, "SELECT sort_key FROM artist WHERE pid = ?", survivor), model.SortKey("Piaf, Edith"); got != want {
		t.Errorf("survivor sort_key = %q, want the inherited override %q", got, want)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SortKeyDrift != 0 {
		t.Errorf("merge left %d sort-key drift", rep.SortKeyDrift)
	}
	if n, err := st.RefreshSortKeys(ctx); err != nil || n != 0 {
		t.Errorf("refresh rewrote %d rows (err %v) after a merge, want 0", n, err)
	}
}
