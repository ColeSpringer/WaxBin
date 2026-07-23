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

// playStateDeltas counts play_state rows in the change log, the observable a
// silent no-op must not move.
func playStateDeltas(t *testing.T, st *Store) int {
	t.Helper()
	return scalarInt(t, st, "SELECT COUNT(*) FROM change_log WHERE entity_type='play_state'")
}

// TestStarStampAndNoOp pins the star write semantics: a real flip bumps
// starred_changed_at (the stamp survives the unstar), a value-identical call is
// a silent no-op (no delta, starred_at preserved, stamp untouched), and
// unstarring an untouched item creates no row at all.
func TestStarStampAndNoOp(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	// Unstar on an untouched item: no row, no delta.
	if err := st.SetStar(ctx, "", item, false); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM play_state"); n != 0 {
		t.Fatalf("unstar-when-untouched created %d rows, want 0", n)
	}
	if n := playStateDeltas(t, st); n != 0 {
		t.Fatalf("unstar-when-untouched emitted %d deltas, want 0", n)
	}

	// First star: starred with a time, stamp set, one delta.
	if err := st.SetStar(ctx, "", item, true); err != nil {
		t.Fatal(err)
	}
	first, _ := st.PlayStateFor(ctx, "", item)
	if !first.Starred || first.StarredAt == 0 || first.StarredChangedAt == 0 {
		t.Fatalf("first star state = %+v, want starred with time and stamp", first)
	}
	if n := playStateDeltas(t, st); n != 1 {
		t.Fatalf("first star emitted %d deltas, want 1", n)
	}

	// Re-star: silent no-op. starred_at keeps the ORIGINAL time (no
	// refresh-on-restar), the stamp does not move, and no delta is emitted.
	if err := st.SetStar(ctx, "", item, true); err != nil {
		t.Fatal(err)
	}
	restar, _ := st.PlayStateFor(ctx, "", item)
	if restar.StarredAt != first.StarredAt {
		t.Errorf("re-star refreshed starred_at %d -> %d, want preserved", first.StarredAt, restar.StarredAt)
	}
	if restar.StarredChangedAt != first.StarredChangedAt {
		t.Errorf("re-star bumped the stamp %d -> %d, want untouched", first.StarredChangedAt, restar.StarredChangedAt)
	}
	if n := playStateDeltas(t, st); n != 1 {
		t.Errorf("re-star emitted a delta (%d total), want the silent no-op", n)
	}

	// Unstar: a real change. starred_at clears, the stamp advances past the
	// star's (the ordering an adapter-side replay guard compares), one new delta.
	if err := st.SetStar(ctx, "", item, false); err != nil {
		t.Fatal(err)
	}
	unstar, _ := st.PlayStateFor(ctx, "", item)
	if unstar.Starred || unstar.StarredAt != 0 {
		t.Fatalf("unstar state = %+v, want cleared star", unstar)
	}
	if unstar.StarredChangedAt <= first.StarredChangedAt {
		t.Errorf("unstar stamp %d not after star stamp %d (must survive and advance on the clear)",
			unstar.StarredChangedAt, first.StarredChangedAt)
	}
	if n := playStateDeltas(t, st); n != 2 {
		t.Errorf("unstar emitted %d total deltas, want 2", n)
	}

	// Re-unstar on the existing row: silent no-op again.
	if err := st.SetStar(ctx, "", item, false); err != nil {
		t.Fatal(err)
	}
	if n := playStateDeltas(t, st); n != 2 {
		t.Errorf("re-unstar emitted a delta (%d total), want the silent no-op", n)
	}
}

// TestRatingStampAndNoOp mirrors the star semantics for ratings: a value change
// (a clear included) bumps rating_changed_at, an identical re-rate is a silent
// no-op, and clearing a never-set rating creates no row.
func TestRatingStampAndNoOp(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	// Clearing an absent rating: no row, no delta.
	if err := st.SetRating(ctx, "", item, nil); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM play_state"); n != 0 {
		t.Fatalf("clear-when-unset created %d rows, want 0", n)
	}

	r := 80
	if err := st.SetRating(ctx, "", item, &r); err != nil {
		t.Fatal(err)
	}
	first, _ := st.PlayStateFor(ctx, "", item)
	if !first.HasRating || first.Rating != 80 || first.RatingChangedAt == 0 {
		t.Fatalf("rated state = %+v, want 80 with a stamp", first)
	}
	if n := playStateDeltas(t, st); n != 1 {
		t.Fatalf("first rate emitted %d deltas, want 1", n)
	}

	// Identical re-rate: silent no-op, stamp untouched.
	if err := st.SetRating(ctx, "", item, &r); err != nil {
		t.Fatal(err)
	}
	same, _ := st.PlayStateFor(ctx, "", item)
	if same.RatingChangedAt != first.RatingChangedAt {
		t.Errorf("re-rate bumped the stamp %d -> %d, want untouched", first.RatingChangedAt, same.RatingChangedAt)
	}
	if n := playStateDeltas(t, st); n != 1 {
		t.Errorf("identical re-rate emitted a delta (%d total), want the silent no-op", n)
	}

	// A different value bumps the stamp.
	r2 := 60
	if err := st.SetRating(ctx, "", item, &r2); err != nil {
		t.Fatal(err)
	}
	changed, _ := st.PlayStateFor(ctx, "", item)
	if changed.Rating != 60 || changed.RatingChangedAt <= first.RatingChangedAt {
		t.Fatalf("re-rate state = %+v, want 60 with an advanced stamp", changed)
	}

	// Clearing a set rating is a change: the value goes, the stamp survives and
	// advances.
	if err := st.SetRating(ctx, "", item, nil); err != nil {
		t.Fatal(err)
	}
	cleared, _ := st.PlayStateFor(ctx, "", item)
	if cleared.HasRating {
		t.Fatal("rating not cleared")
	}
	if cleared.RatingChangedAt <= changed.RatingChangedAt {
		t.Errorf("clear stamp %d not after set stamp %d (must survive the clear)",
			cleared.RatingChangedAt, changed.RatingChangedAt)
	}
	if n := playStateDeltas(t, st); n != 3 {
		t.Errorf("deltas after rate/re-rate/change/clear = %d, want 3", n)
	}
	// Re-clear: silent no-op on the existing row.
	if err := st.SetRating(ctx, "", item, nil); err != nil {
		t.Fatal(err)
	}
	if n := playStateDeltas(t, st); n != 3 {
		t.Errorf("re-clear emitted a delta (%d total), want the silent no-op", n)
	}
}

// TestStampsUntouchedByProgressAndPlays pins the stamp scope: checkpoints and
// play counts never move the star/rating change stamps.
func TestStampsUntouchedByProgressAndPlays(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	if err := st.SetStar(ctx, "", item, true); err != nil {
		t.Fatal(err)
	}
	r := 90
	if err := st.SetRating(ctx, "", item, &r); err != nil {
		t.Fatal(err)
	}
	before, _ := st.PlayStateFor(ctx, "", item)

	if err := st.SetProgress(ctx, "", item, 42000); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPlayed(ctx, "", item, true); err != nil {
		t.Fatal(err)
	}
	after, _ := st.PlayStateFor(ctx, "", item)
	if after.StarredChangedAt != before.StarredChangedAt || after.RatingChangedAt != before.RatingChangedAt {
		t.Errorf("progress/play moved the stamps: %+v -> %+v", before, after)
	}
	if after.PositionMS != 42000 || after.PlayCount != 1 {
		t.Errorf("progress/play state = %+v, want position 42000 and one play", after)
	}
}

// TestPlayStatesForItems covers the bulk read: multi-user states keyed by item,
// per-item ordering by user pid, untouched and unknown pids absent, duplicate
// input collapsed, and the stamps carried through.
func TestPlayStatesForItems(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item1 := seedItem(t, st, lib)
	res2 := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/2.flac", essence: "e2", content: "c2", title: "Song2", artist: "X", album: "Al",
	})
	item2 := res2.ItemPID
	res3 := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/3.flac", essence: "e3", content: "c3", title: "Song3", artist: "X", album: "Al",
	})
	item3 := res3.ItemPID

	def, err := st.DefaultUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}

	// Default user stars item1 and rates item2; bob stars item2. item3 untouched.
	if err := st.SetStar(ctx, "", item1, true); err != nil {
		t.Fatal(err)
	}
	r := 70
	if err := st.SetRating(ctx, "", item2, &r); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStar(ctx, bob.PID, item2, true); err != nil {
		t.Fatal(err)
	}

	got, err := st.PlayStatesForItems(ctx, []model.PID{item1, item2, item2, item3, "01J0NONEXISTENT0000000000"})
	if err != nil {
		t.Fatalf("PlayStatesForItems: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("map has %d items, want 2 (untouched and unknown absent): %+v", len(got), got)
	}
	if s1 := got[item1]; len(s1) != 1 || s1[0].UserPID != def.PID || !s1[0].Starred || s1[0].StarredChangedAt == 0 {
		t.Errorf("item1 states = %+v, want the default user's star with its stamp", s1)
	}
	s2 := got[item2]
	if len(s2) != 2 {
		t.Fatalf("item2 states = %+v, want both users", s2)
	}
	if !(s2[0].UserPID < s2[1].UserPID) {
		t.Errorf("item2 states not ordered by user pid: %s then %s", s2[0].UserPID, s2[1].UserPID)
	}
	for _, s := range s2 {
		switch s.UserPID {
		case def.PID:
			if !s.HasRating || s.Rating != 70 || s.RatingChangedAt == 0 {
				t.Errorf("default state on item2 = %+v, want rating 70 with stamp", s)
			}
		case bob.PID:
			if !s.Starred || s.StarredChangedAt == 0 {
				t.Errorf("bob state on item2 = %+v, want star with stamp", s)
			}
		default:
			t.Errorf("unexpected user %s on item2", s.UserPID)
		}
	}

	// Empty input reads nothing.
	if out, err := st.PlayStatesForItems(ctx, nil); err != nil || out != nil {
		t.Errorf("empty input = %+v (err %v), want nil", out, err)
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
