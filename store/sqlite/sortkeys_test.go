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
	if err := st.read.QueryRowContext(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("read key (%s): %v", query, err)
	}
	return got
}

// changesSince returns the (entity_type, pid) of every delta appended after seq.
func changesSince(t *testing.T, st *Store, seq int64) [][2]string {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(),
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
		if _, err := st.write.ExecContext(ctx, stmt); err != nil {
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
	if !rep.SortKeyDriftOnly() {
		t.Errorf("stale keys should be the only inconsistency: %+v", rep)
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
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/e/1.flac", essence: "e1", content: "c1", title: "Non, je ne regrette rien",
		artist: "Édith Piaf", album: "Éternelle", genre: "Chanson",
	})
	// What the pre-folding implementation stored: lowercased, codepoint-ordered.
	if _, err := st.write.ExecContext(ctx,
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

	if _, err := st.write.ExecContext(ctx, "UPDATE artist SET sort_key = 'WRONG'"); err != nil {
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

// TestRefreshSortKeysRefoldsTagDerivedColumns covers the columns whose input was a
// sort tag the catalog does not keep.
func TestRefreshSortKeysRefoldsTagDerivedColumns(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	tr := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "The Beatles", album: "Help", composer: "Antonín Dvořák",
	})
	bk := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/bk/1.m4b", essence: "be1", content: "bc1", title: "Kafka on the Shore",
		author: "Haruki Murakami", series: "Standalone", seq: "1",
	})
	// The keys the ASCII-only implementation wrote from the sort tags: lowercased and
	// article-preserving, but unfolded.
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{"UPDATE track SET artist_sort = ?, composer_sort = ? WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)",
			[]any{"beatles, thé", "dvořák, antonín", string(tr.ItemPID)}},
		{"UPDATE book SET author_sort = ? WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)",
			[]any{"murakami, haruki ٢", string(bk.ItemPID)}},
	} {
		if _, err := st.write.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
			t.Fatalf("seed tag-derived key: %v", err)
		}
	}

	if _, err := st.RefreshSortKeys(ctx); err != nil {
		t.Fatal(err)
	}

	// The comma form survives: a recompute from "The Beatles" would have written
	// "beatles" and destroyed the tag.
	if got := storedKey(t, st, "SELECT artist_sort FROM track"); got != "beatles, the" {
		t.Errorf("artist_sort = %q, want %q", got, "beatles, the")
	}
	if got := storedKey(t, st, "SELECT composer_sort FROM track"); got != "dvorak, antonin" {
		t.Errorf("composer_sort = %q, want %q", got, "dvorak, antonin")
	}
	// A non-ASCII digit run comes out padded, the case Fold alone gets wrong.
	if got, want := storedKey(t, st, "SELECT author_sort FROM book"), "murakami, haruki 0000000002"; got != want {
		t.Errorf("author_sort = %q, want %q", got, want)
	}

	if n, err := st.RefreshSortKeys(ctx); err != nil || n != 0 {
		t.Errorf("second refold rewrote %d rows (err %v), want 0", n, err)
	}
}

// TestRefreshSortKeysSkipsLockedSortFields proves a locked composer_sort or
// author_sort is left alone: an explicit edit stores the literal string the user
// typed, which refolding would rewrite.
func TestRefreshSortKeysSkipsLockedSortFields(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	locked := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "A", album: "Help", composer: "Antonín Dvořák",
	})
	unlocked := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "B", album: "Help", composer: "Antonín Dvořák",
	})
	bk := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/bk/1.m4b", essence: "be1", content: "bc1", title: "Kafka on the Shore",
		author: "Haruki Murakami",
	})

	const userText = "Dvořák, Antonín"
	if err := st.EditItemFields(ctx, locked.ItemPID,
		map[string]string{"composer_sort": userText}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit composer_sort: %v", err)
	}
	if err := st.EditItemFields(ctx, bk.ItemPID,
		map[string]string{"author_sort": userText}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit author_sort: %v", err)
	}
	if _, err := st.write.ExecContext(ctx,
		"UPDATE track SET composer_sort = 'dvořák, antonín' WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)",
		string(unlocked.ItemPID)); err != nil {
		t.Fatal(err)
	}

	if _, err := st.RefreshSortKeys(ctx); err != nil {
		t.Fatal(err)
	}

	if got := storedKey(t, st,
		"SELECT composer_sort FROM track WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)",
		string(locked.ItemPID)); got != userText {
		t.Errorf("locked composer_sort = %q, want the user's %q untouched", got, userText)
	}
	if got := storedKey(t, st, "SELECT author_sort FROM book"); got != userText {
		t.Errorf("locked author_sort = %q, want the user's %q untouched", got, userText)
	}
	if got := storedKey(t, st,
		"SELECT composer_sort FROM track WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)",
		string(unlocked.ItemPID)); got != "dvorak, antonin" {
		t.Errorf("unlocked composer_sort = %q, want it refolded", got)
	}
}

// TestRefreshSortKeysCoversAliasAndSeriesSeq covers the two columns that were in
// neither the drift check nor any repair before.
func TestRefreshSortKeysCoversAliasAndSeriesSeq(t *testing.T) {
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
	if _, err := st.write.ExecContext(ctx, `INSERT INTO artist_alias(artist_id, name, sort_key, is_primary)
		SELECT id, 'Piaf, Édith', 'piaf, édith', 0 FROM artist WHERE pid = ?`, artistPID); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	if _, err := st.write.ExecContext(ctx, "UPDATE book SET series_seq_sort = 'WRONG'"); err != nil {
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
	st, lib := entityFixture(t)
	ctx := context.Background()
	seedTwoTracks(t, st, lib.ID)

	before, _ := st.LatestChangeSeq(ctx)
	if _, err := st.write.ExecContext(ctx, "UPDATE artist SET sort_key = 'WRONG'"); err != nil {
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
	st, _ := entityFixture(t)
	ctx := context.Background()
	const n = sortKeyBatch + 25
	if _, err := st.write.ExecContext(ctx, `
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
	if err := st.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM artist WHERE sort_key = 'WRONG'").Scan(&stale); err != nil {
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
