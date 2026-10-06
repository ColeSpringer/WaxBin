package sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
)

// TestRebuildSearchIndexWritesNothingOnAFreshCatalog: every write path indexes what
// the repair builds, so a catalog the write paths built rebuilds no row. The book's
// author tag is spelled apart from the artist entity it resolves onto, the case where
// a rebuild reading entity names would disagree with the put.
func TestRebuildSearchIndexWritesNothingOnAFreshCatalog(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/0.flac", essence: "e0", content: "c0",
		title: "Carrie Theme", artist: "STEPHEN KING", album: "Readings"})
	tr := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "That’s Life", artist: "Jay-Z", albumArt: "Jay-Z", album: "東京スパイス",
		composer: "Shawn Carter", genre: "Hip-Hop"})
	if _, _, err := st.SetItemCredits(ctx, tr.ItemPID, model.RoleProducer, []string{"Kanye West"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set producer: %v", err)
	}
	putTrackCustom(t, st, lib.ID, "/lib/2.flac", "e2", "c2", "Tagged", map[string][]string{"MOOD": {"Calm"}}, false)
	// A plain composer edit that retires a composer credit, and a credit edit that
	// supersedes a plain composer.
	scored := putTrack(t, st, lib.ID, trackSpec{path: "/lib/5.flac", essence: "e5", content: "c5",
		title: "Scored", artist: "Orchestra", album: "Al"})
	if _, _, err := st.SetItemCredits(ctx, scored.ItemPID, model.RoleComposer, []string{"Hans Zimmer"},
		userAttr, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	if err := st.EditItemField(ctx, scored.ItemPID, "composer", "John Williams", userAttr, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit composer: %v", err)
	}
	if _, _, err := st.SetItemCredits(ctx, tr.ItemPID, model.RoleComposer, []string{"Shawn Carter", "Pharrell"},
		userAttr, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("set composer credit over the tagged composer: %v", err)
	}
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.m4b", essence: "b1", content: "bc1",
		title: "The Stand", author: "Stephen King", narrators: []string{"Grover Gardner"},
		series: "Gunslinger", seq: "1", genres: []string{"Horror"}})
	if err := st.EditItemField(ctx, book.ItemPID, "title", "The Stand (Uncut)",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit book title: %v", err)
	}
	// A series tagged in another case joins the series the first spelling made, one
	// edited onto it in a third case too, and a series of symbols alone makes none.
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/2.m4b", essence: "b2", content: "bc2",
		title: "The Drawing of the Three", author: "Stephen King", series: "gunslinger", seq: "2"})
	third := putBook(t, st, lib.ID, bookSpec{path: "/lib/b/3.m4b", essence: "b3", content: "bc3",
		title: "The Waste Lands", author: "Stephen King", series: "Other", seq: "3"})
	if err := st.EditItemField(ctx, third.ItemPID, "series", "GUNSLINGER",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit book series: %v", err)
	}
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/4.m4b", essence: "b4", content: "bc4",
		title: "Odd Book", author: "Someone", series: "!!!"})
	if _, err := st.UpsertFeed(ctx, model.UpsertFeedInput{
		FeedURL: "http://feed.example/x", IdentityKey: "podcast:feed.example/x",
		Feed: model.Feed{Title: "My Show", Author: "Host", Episodes: []model.FeedEpisode{
			{GUID: "g1", Title: "Pi'erre Episode", Description: "<p>Show &amp; tell</p>",
				EnclosureURL: "http://feed.example/1.mp3", EnclosureType: "audio/mpeg"},
		}},
		FetchedAtNS: 1,
	}); err != nil {
		t.Fatalf("upsert feed: %v", err)
	}
	n, err := st.RebuildSearchIndex(ctx)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if n != 0 {
		t.Errorf("rebuilt %d rows of a catalog its write paths just built, want 0", n)
	}
}

// TestRebuildSearchIndexRepairsDrift: a row rewritten to stale text, a missing row and
// an orphan row are each put right, and a second run finds nothing left.
func TestRebuildSearchIndexRepairsDrift(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	stale := putTrack(t, st, lib.ID, trackSpec{path: "/lib/0.flac", essence: "e0", content: "c0",
		title: "Harbor Lights", artist: "Someone", album: "Al"})
	lost := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Paper Moon", artist: "Someone", album: "Al"})
	staleID := scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(stale.ItemPID))
	lostID := scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(lost.ItemPID))
	for _, q := range []struct {
		stmt string
		args []any
	}{
		{"UPDATE search_fts SET title = 'outdated words' WHERE rowid = ?", []any{staleID}},
		{"DELETE FROM search_fts WHERE rowid = ?", []any{lostID}},
		{"INSERT INTO search_fts(rowid, kind, title) VALUES (999999, 'track', 'orphan words')", nil},
	} {
		if _, err := st.write.ExecContext(ctx, q.stmt, q.args...); err != nil {
			t.Fatalf("%s: %v", q.stmt, err)
		}
	}
	n, err := st.RebuildSearchIndex(ctx)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if n != 3 {
		t.Errorf("rebuilt %d rows, want 3 (one stale, one missing, one orphan)", n)
	}
	for q, want := range map[string]int{"outdated": 0, "orphan": 0, "harbor": 1, "paper moon": 1} {
		res, err := st.Search(ctx, q, read.SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(res.Tracks) != want {
			t.Errorf("search %q = %+v, want %d tracks", q, res.Tracks, want)
		}
	}
	if n, err := st.RebuildSearchIndex(ctx); err != nil || n != 0 {
		t.Errorf("second rebuild = %d (err %v), want 0", n, err)
	}
	assertVerifyClean(t, st)
}

// TestRebuildSearchIndexSpansBatches exercises the mid-stream commit, with the items
// inserted directly (scanning past the batch size would take minutes) and no search
// row of their own.
func TestRebuildSearchIndexSpansBatches(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	const n = sortKeyBatch + 25
	if _, err := st.write.ExecContext(ctx, `
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < ?)
		INSERT INTO playable_item(pid, kind, state, title, sort_key, created_at, updated_at)
		SELECT 'pid' || i, 'track', 'present', 'Title ' || i, 'title ' || i, 1, 1 FROM seq`, n); err != nil {
		t.Fatalf("seed %d items: %v", n, err)
	}
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO track(item_id, artist) SELECT id, 'Batch Artist' FROM playable_item"); err != nil {
		t.Fatalf("seed tracks: %v", err)
	}
	got, err := st.RebuildSearchIndex(ctx)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got != n {
		t.Errorf("rebuilt %d rows, want all %d (the run crosses %d, the batch size)", got, n, sortKeyBatch)
	}
	if rows := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts WHERE search_fts MATCH 'batch'"); rows != n {
		t.Errorf("%d rows indexed, want %d, so a batch was dropped", rows, n)
	}
}

// TestRebuildSearchIndexInStepTakesNoWriteLock: the comparison runs on the read side,
// so a run over a catalog in step finishes while another writer holds the lock.
func TestRebuildSearchIndexInStepTakesNoWriteLock(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/0.flac", essence: "e0", content: "c0",
		title: "Harbor Lights", artist: "Someone", album: "Al"})
	held, release, writerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		writerDone <- st.writeTx(ctx, func(*sql.Tx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := st.RebuildSearchIndex(ctx)
		done <- result{n, err}
	}()
	waited := false
	select {
	case r := <-done:
		if r.err != nil || r.n != 0 {
			t.Errorf("rebuild = %d (err %v), want 0", r.n, r.err)
		}
	case <-time.After(10 * time.Second):
		waited = true
		t.Error("a rebuild with nothing to write waited on the write lock")
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatalf("holding writer: %v", err)
	}
	if waited {
		<-done
	}
}

// TestGCStrandedTagKeysReindexes: the stranded values sat in the search row's extra
// column, so the GC that deletes them rebuilds the row instead of leaving them
// searchable until the item's next scan.
func TestGCStrandedTagKeysReindexes(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.mp3", "e1", "c1", "One", map[string][]string{"MOOD": {"chill"}}, true)
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(res.ItemPID)))
	// Seeded raw: SetItemTag refuses the key now, which is the point.
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO item_tag(item_id, key, value, position) VALUES (?, 'ODD~KEY', 'zanzibar', 0)", itemID); err != nil {
			return err
		}
		return rebuildItemSearchFTSTx(ctx, tx, itemID, string(model.KindTrack))
	}); err != nil {
		t.Fatalf("seed the stranded row: %v", err)
	}
	found := func(q string) int {
		t.Helper()
		r, err := st.Search(ctx, q, read.SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		return len(r.Tracks)
	}
	if found("zanzibar") != 1 {
		t.Fatal("the seeded value is not searchable, so the test proves nothing")
	}
	if _, err := st.GCStrandedTagKeys(ctx); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if found("zanzibar") != 0 || found("chill") != 1 {
		t.Error("after the gc the stranded value is still searchable, or the kept one is not")
	}
}

// TestUpsertShowTitleChangeReindexesEpisodes: an episode's search row and item view
// carry its show's title, which a show edit can change outside a feed sync, and a feed
// need not list every episode the catalog holds. A rename reindexes every episode and
// emits one update for each, one the same sync changed included, and a sync counts each
// of them as updated once.
func TestUpsertShowTitleChangeReindexesEpisodes(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	show := model.UpsertShowInput{IdentityKey: "show:x", FeedURL: "http://feed.example/x", Title: "Old Show Name"}
	pid, _, err := st.UpsertShow(ctx, show)
	if err != nil {
		t.Fatalf("upsert show: %v", err)
	}
	if _, err := st.UpsertEpisode(ctx, model.UpsertEpisodeInput{PodcastPID: pid,
		Episode: model.FeedEpisode{GUID: "g1", Title: "Pilot", EnclosureURL: "http://feed.example/1.mp3"}}); err != nil {
		t.Fatalf("upsert episode: %v", err)
	}
	show.Title = "Brand New Name"
	seq, _ := st.LatestChangeSeq(ctx)
	if _, _, err := st.UpsertShow(ctx, show); err != nil {
		t.Fatalf("rename show: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM change_log WHERE seq > ? AND entity_type = 'item' AND op = 'update'", seq); n != 1 {
		t.Errorf("show rename emitted %d episode updates, want 1", n)
	}
	for q, want := range map[string]int{"brand new": 1, "old show": 0} {
		res, err := st.Search(ctx, q, read.SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(res.Episodes) != want {
			t.Errorf("search %q episodes = %+v, want %d", q, res.Episodes, want)
		}
	}

	// A feed sync renames the show too, and its feed need not list every episode the
	// catalog holds.
	feed := model.UpsertFeedInput{FeedURL: "http://feed.example/y", IdentityKey: "podcast:feed.example/y",
		Feed: model.Feed{Title: "First Title", Episodes: []model.FeedEpisode{
			{GUID: "a", Title: "Older", EnclosureURL: "http://feed.example/a.mp3"},
			{GUID: "b", Title: "Newer", EnclosureURL: "http://feed.example/b.mp3"},
		}}, FetchedAtNS: 1}
	if _, err := st.UpsertFeed(ctx, feed); err != nil {
		t.Fatalf("upsert feed: %v", err)
	}
	feed.Feed.Title, feed.Feed.Episodes, feed.FetchedAtNS = "Second Title", feed.Feed.Episodes[1:], 2
	feed.Feed.Episodes[0].Description = "now with notes"
	seq, _ = st.LatestChangeSeq(ctx)
	synced, err := st.UpsertFeed(ctx, feed)
	if err != nil {
		t.Fatalf("resync feed: %v", err)
	}
	if synced.EpisodesUpdated != 2 {
		t.Errorf("rename sync updated %d episodes, want both", synced.EpisodesUpdated)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM change_log WHERE seq > ? AND entity_type = 'item' AND op = 'update'", seq); n != 2 {
		t.Errorf("feed rename emitted %d episode updates, want one for each of the two", n)
	}
	res, err := st.Search(ctx, "second title", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Episodes) != 2 {
		t.Errorf("search second title = %+v, want both episodes, the one the feed dropped included", res.Episodes)
	}
}
