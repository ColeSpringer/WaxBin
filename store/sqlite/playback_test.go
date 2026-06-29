package sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func seedItem(t *testing.T, st *Store, lib *model.Library) model.PID {
	t.Helper()
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song", artist: "X", album: "Al",
	})
	return res.ItemPID
}

func TestDefaultUserSeeded(t *testing.T) {
	st, _ := entityFixture(t)
	u, err := st.DefaultUser(context.Background())
	if err != nil {
		t.Fatalf("default user: %v", err)
	}
	if u.Name != DefaultUserName || !u.IsDefault {
		t.Errorf("default user = %+v, want %q/default", u, DefaultUserName)
	}
}

func TestPlayStateLifecycle(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	// A never-touched item returns a zero state, not an error.
	got, err := st.PlayStateFor(ctx, "", item)
	if err != nil {
		t.Fatalf("initial state: %v", err)
	}
	if got.Played || got.PlayCount != 0 || got.HasRating || got.Starred {
		t.Fatalf("fresh state not zero: %+v", got)
	}

	// Progress, two plays (one finishing), a rating, and a star accumulate.
	if err := st.SetProgress(ctx, "", item, 42000); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPlayed(ctx, "", item, false); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPlayed(ctx, "", item, true); err != nil {
		t.Fatal(err)
	}
	r := 80
	if err := st.SetRating(ctx, "", item, &r); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStar(ctx, "", item, true); err != nil {
		t.Fatal(err)
	}

	got, _ = st.PlayStateFor(ctx, "", item)
	if got.PositionMS != 42000 {
		t.Errorf("position = %d, want 42000", got.PositionMS)
	}
	if !got.Played || got.PlayCount != 2 {
		t.Errorf("play count = %d (played %v), want 2/true", got.PlayCount, got.Played)
	}
	if !got.Finished {
		t.Error("finished flag should stay set once finished, even after a non-finishing play earlier")
	}
	if !got.HasRating || got.Rating != 80 {
		t.Errorf("rating = %d (has %v), want 80", got.Rating, got.HasRating)
	}
	if !got.Starred || got.StarredAt == 0 {
		t.Error("star not recorded with a time")
	}

	// Clearing the rating and unstarring.
	if err := st.SetRating(ctx, "", item, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStar(ctx, "", item, false); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.HasRating {
		t.Error("rating not cleared")
	}
	if got.Starred {
		t.Error("star not cleared")
	}
}

func TestRatingClamped(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)
	over := 250
	if err := st.SetRating(ctx, "", item, &over); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if got.Rating != 100 {
		t.Errorf("rating = %d, want clamped to 100", got.Rating)
	}
}

func TestBookmarksAndQueue(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	bm, err := st.AddBookmark(ctx, "", item, 60000, "chapter 2")
	if err != nil {
		t.Fatalf("add bookmark: %v", err)
	}
	bms, _ := st.Bookmarks(ctx, "", item)
	if len(bms) != 1 || bms[0].Label != "chapter 2" || bms[0].PositionMS != 60000 {
		t.Fatalf("bookmarks = %+v", bms)
	}
	if err := st.DeleteBookmark(ctx, bm); err != nil {
		t.Fatalf("delete bookmark: %v", err)
	}
	if bms, _ := st.Bookmarks(ctx, "", item); len(bms) != 0 {
		t.Errorf("bookmark survived delete: %+v", bms)
	}

	if err := st.SetQueue(ctx, "", []model.PID{item}); err != nil {
		t.Fatalf("set queue: %v", err)
	}
	q, _ := st.Queue(ctx, "")
	if len(q) != 1 || q[0].PID != item {
		t.Fatalf("queue = %+v", q)
	}
	// An unknown item in the queue is rejected (no silent drop).
	if err := st.SetQueue(ctx, "", []model.PID{"01J0NONEXISTENT0000000000"}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("queue with an unknown item: want CodeNotFound, got %v", err)
	}
}

func TestSessions(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)
	sess, err := st.StartSession(ctx, "", item, "test-client")
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := st.EndSession(ctx, sess, 123000); err != nil {
		t.Fatalf("end session: %v", err)
	}
	var msPlayed int64
	var ended sql.NullInt64
	if err := st.read.QueryRowContext(ctx,
		"SELECT ms_played, ended_at FROM play_session WHERE pid = ?", string(sess)).Scan(&msPlayed, &ended); err != nil {
		t.Fatal(err)
	}
	if msPlayed != 123000 || !ended.Valid {
		t.Errorf("session not closed: ms=%d ended=%v", msPlayed, ended.Valid)
	}
	if err := st.EndSession(ctx, "01J0NONEXISTENT0000000000", 1); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("ending an unknown session: want CodeNotFound, got %v", err)
	}
}

func TestPlayStateCascadesWithItem(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Re-key the file essence so the prior item is orphaned and deleted.
	spec := trackSpec{path: "/lib/a/1.mp3", essence: "e1", content: "c1", title: "First", artist: "A", album: "Al"}
	r := putTrack(t, st, lib.ID, spec)
	if err := st.SetStar(ctx, "", r.ItemPID, true); err != nil {
		t.Fatal(err)
	}
	spec.essence, spec.content, spec.title = "e2", "c2", "Second"
	putTrack(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM play_state"); n != 0 {
		t.Errorf("orphaned play_state rows = %d, want 0 (cascaded with the item)", n)
	}
}
