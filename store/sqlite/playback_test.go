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
	if _, err := st.SetRating(ctx, "", item, &r, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStar(ctx, "", item, true, nil); err != nil {
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
	if _, err := st.SetRating(ctx, "", item, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStar(ctx, "", item, false, nil); err != nil {
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
	if _, err := st.SetRating(ctx, "", item, &over, nil); err != nil {
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

// TestStarAsOfRecordedTime pins the recorded-time (as-of) guard on SetStar: a flip
// whose recorded time is not newer than the stored change is skipped as a stale
// replay, a newer one applies and stamps in recorded time, and a value-identical
// call stays a no-op regardless of as-of.
func TestStarAsOfRecordedTime(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)
	ns := func(v int64) *int64 { return &v }

	// Star recorded at time 100: starred_at and the stamp both land in recorded time.
	if _, err := st.SetStar(ctx, "", item, true, ns(100)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if !got.Starred || got.StarredAt != 100 || got.StarredChangedAt != 100 {
		t.Fatalf("star@100 = %+v, want starred with recorded time 100", got)
	}
	if n := playStateDeltas(t, st); n != 1 {
		t.Fatalf("star@100 emitted %d deltas, want 1", n)
	}

	// Stale replay: an unstar recorded at 50 (not newer than the stored 100) is
	// skipped, leaving the item starred with its stamp untouched and no new delta.
	if _, err := st.SetStar(ctx, "", item, false, ns(50)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if !got.Starred || got.StarredChangedAt != 100 {
		t.Errorf("stale unstar@50 = %+v, want still starred with stamp 100", got)
	}
	if n := playStateDeltas(t, st); n != 1 {
		t.Errorf("stale unstar emitted a delta (%d total), want the silent skip", n)
	}

	// Equal recorded time is also stale (not strictly newer): an unstar at exactly
	// 100 loses to the star already recorded at 100.
	if _, err := st.SetStar(ctx, "", item, false, ns(100)); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.PlayStateFor(ctx, "", item); !got.Starred {
		t.Errorf("unstar@100 (equal) = %+v, want skipped (not strictly newer)", got)
	}

	// A value-identical re-star with any as-of stays a no-op: the stamp keeps 100.
	if _, err := st.SetStar(ctx, "", item, true, ns(999)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.StarredChangedAt != 100 || got.StarredAt != 100 {
		t.Errorf("identical re-star@999 = %+v, want the stored stamp 100 preserved", got)
	}
	if n := playStateDeltas(t, st); n != 1 {
		t.Errorf("identical re-star emitted a delta (%d total), want the silent no-op", n)
	}

	// A newer recorded time wins: unstar at 200 (> 100) applies, clearing the star
	// and advancing the stamp, with a new delta.
	if _, err := st.SetStar(ctx, "", item, false, ns(200)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.Starred || got.StarredAt != 0 || got.StarredChangedAt != 200 {
		t.Errorf("unstar@200 = %+v, want cleared star with stamp 200", got)
	}
	if n := playStateDeltas(t, st); n != 2 {
		t.Errorf("unstar@200 emitted %d total deltas, want 2", n)
	}
}

// TestStarAsOfNullPriorApplies verifies a star flip applies when the stored star
// stamp is NULL (no ordering info) even with an old recorded time: a rating-only
// row carries a NULL starred_changed_at.
func TestStarAsOfNullPriorApplies(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)
	old := int64(50)

	// A rating creates the row but leaves starred_changed_at NULL.
	r := 80
	if _, err := st.SetRating(ctx, "", item, &r, nil); err != nil {
		t.Fatal(err)
	}
	// Star recorded at an old time: with no prior star stamp to lose to, it applies.
	if _, err := st.SetStar(ctx, "", item, true, &old); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if !got.Starred || got.StarredChangedAt != 50 {
		t.Errorf("star@50 over a NULL prior stamp = %+v, want starred with stamp 50", got)
	}
}

// TestStarAsOfZeroIsServerNow verifies a zero as-of is treated as "no recorded
// time" (like nil), stamping at server-now instead of the epoch. Unix-ns 0 is the
// not-provided sentinel the proxy wire uses, so the direct store path must agree:
// a lone &0 must not stamp at the epoch and then lose every staleness comparison.
func TestStarAsOfZeroIsServerNow(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)
	zero := int64(0)
	before := nowNS()

	if _, err := st.SetStar(ctx, "", item, true, &zero); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if !got.Starred || got.StarredChangedAt < before {
		t.Fatalf("star with as-of 0 = %+v, want starred at server-now (>= %d), not the epoch", got, before)
	}
}

// TestRatingAsOfRecordedTime mirrors the star as-of guard for ratings: an older
// recorded time is skipped, a newer applies and stamps in recorded time, and a
// value-identical re-rate stays a no-op preserving the stamp.
func TestRatingAsOfRecordedTime(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)
	ns := func(v int64) *int64 { return &v }
	r80, r60 := 80, 60

	if _, err := st.SetRating(ctx, "", item, &r80, ns(100)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if got.Rating != 80 || got.RatingChangedAt != 100 {
		t.Fatalf("rate@100 = %+v, want 80 with recorded stamp 100", got)
	}

	// Stale replay: a different value recorded at 50 (not newer than 100) is skipped,
	// with no delta beyond the first write.
	if _, err := st.SetRating(ctx, "", item, &r60, ns(50)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.Rating != 80 || got.RatingChangedAt != 100 {
		t.Errorf("stale rate@50 = %+v, want unchanged 80 with stamp 100", got)
	}
	if n := playStateDeltas(t, st); n != 1 {
		t.Errorf("stale rate emitted %d deltas, want 1 (only the first write)", n)
	}

	// Value-identical re-rate with any as-of is a no-op preserving the stamp.
	if _, err := st.SetRating(ctx, "", item, &r80, ns(999)); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.PlayStateFor(ctx, "", item); got.RatingChangedAt != 100 {
		t.Errorf("identical re-rate@999 stamp = %d, want preserved 100", got.RatingChangedAt)
	}

	// A newer recorded time wins.
	if _, err := st.SetRating(ctx, "", item, &r60, ns(200)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.Rating != 60 || got.RatingChangedAt != 200 {
		t.Errorf("rate@200 = %+v, want 60 with stamp 200", got)
	}
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
	if _, err := st.SetStar(ctx, "", item, false, nil); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM play_state"); n != 0 {
		t.Fatalf("unstar-when-untouched created %d rows, want 0", n)
	}
	if n := playStateDeltas(t, st); n != 0 {
		t.Fatalf("unstar-when-untouched emitted %d deltas, want 0", n)
	}

	// First star: starred with a time, stamp set, one delta.
	if _, err := st.SetStar(ctx, "", item, true, nil); err != nil {
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
	if _, err := st.SetStar(ctx, "", item, true, nil); err != nil {
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
	if _, err := st.SetStar(ctx, "", item, false, nil); err != nil {
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
	if _, err := st.SetStar(ctx, "", item, false, nil); err != nil {
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
	if _, err := st.SetRating(ctx, "", item, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM play_state"); n != 0 {
		t.Fatalf("clear-when-unset created %d rows, want 0", n)
	}

	r := 80
	if _, err := st.SetRating(ctx, "", item, &r, nil); err != nil {
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
	if _, err := st.SetRating(ctx, "", item, &r, nil); err != nil {
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
	if _, err := st.SetRating(ctx, "", item, &r2, nil); err != nil {
		t.Fatal(err)
	}
	changed, _ := st.PlayStateFor(ctx, "", item)
	if changed.Rating != 60 || changed.RatingChangedAt <= first.RatingChangedAt {
		t.Fatalf("re-rate state = %+v, want 60 with an advanced stamp", changed)
	}

	// Clearing a set rating is a change: the value goes, the stamp survives and
	// advances.
	if _, err := st.SetRating(ctx, "", item, nil, nil); err != nil {
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
	if _, err := st.SetRating(ctx, "", item, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := playStateDeltas(t, st); n != 3 {
		t.Errorf("re-clear emitted a delta (%d total), want the silent no-op", n)
	}
}

// TestPlayStateChangedAgreesWithDelta pins the whole contract of the changed bool
// SetStar and SetRating return: it is true exactly when a play_state delta was
// appended. Every no-op case is covered (a value-identical write, a clear of
// something never set, and a stale as-of replay) alongside the real changes, and
// each step asserts the bool against the delta count rather than against a
// hardcoded expectation, so the two can never drift apart. That agreement is the
// reason the bool exists: a follow-up state read cannot tell "applied" from
// "duplicate skipped" for a replay, which lands the stamp on the value it already
// held.
func TestPlayStateChangedAgreesWithDelta(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)
	ns := func(v int64) *int64 { return &v }

	before := playStateDeltas(t, st)
	// step runs one mutation and asserts its bool against the observable it claims to
	// mirror: whether the play_state delta count moved.
	step := func(what string, mut func() (bool, error)) {
		t.Helper()
		changed, err := mut()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		now := playStateDeltas(t, st)
		if wrote := now > before; wrote != changed {
			t.Errorf("%s reported changed=%v but the delta count went %d -> %d", what, changed, before, now)
		}
		before = now
	}

	r80, r60 := 80, 60
	// Clearing a never-set rating and unstarring an untouched item create no row at
	// all, so there is no stamp for the recorded-time steps below to trip over.
	step("clear unset rating", func() (bool, error) { return st.SetRating(ctx, "", item, nil, nil) })
	step("unstar untouched", func() (bool, error) { return st.SetStar(ctx, "", item, false, nil) })

	// Stars, in recorded time. A stale replay is the case with no read-back tell:
	// afterwards the row is exactly what an applied write would have left, so the bool
	// is the only thing that distinguishes applied from skipped.
	step("star @100", func() (bool, error) { return st.SetStar(ctx, "", item, true, ns(100)) })
	step("older replay @50", func() (bool, error) { return st.SetStar(ctx, "", item, false, ns(50)) })
	step("same-time replay @100", func() (bool, error) { return st.SetStar(ctx, "", item, false, ns(100)) })
	step("unstar @999", func() (bool, error) { return st.SetStar(ctx, "", item, false, ns(999)) })

	// Ratings, on the row the stars left behind: its rating stamp is still NULL, which
	// carries no ordering, so the first recorded-time rating applies.
	step("rate 80 @100", func() (bool, error) { return st.SetRating(ctx, "", item, &r80, ns(100)) })
	step("older replay @50", func() (bool, error) { return st.SetRating(ctx, "", item, &r60, ns(50)) })
	step("identical re-rate @999", func() (bool, error) { return st.SetRating(ctx, "", item, &r80, ns(999)) })
	step("rate 60 @999", func() (bool, error) { return st.SetRating(ctx, "", item, &r60, ns(999)) })
	// Clearing a set rating is a change; re-clearing it is not.
	step("clear set rating", func() (bool, error) { return st.SetRating(ctx, "", item, nil, ns(1500)) })
	step("re-clear rating", func() (bool, error) { return st.SetRating(ctx, "", item, nil, ns(2000)) })

	// Played/finished, whose stamp is still NULL on this row.
	step("mark played @100", func() (bool, error) { return st.SetPlayed(ctx, "", item, true, true, nil, ns(100)) })
	step("identical re-set @999", func() (bool, error) { return st.SetPlayed(ctx, "", item, true, true, nil, ns(999)) })
	step("older un-mark @50", func() (bool, error) { return st.SetPlayed(ctx, "", item, false, false, nil, ns(50)) })
	step("un-mark @999", func() (bool, error) { return st.SetPlayed(ctx, "", item, false, false, nil, ns(999)) })
	// A count-only change is still a change.
	step("reset count @1500", func() (bool, error) { return st.SetPlayed(ctx, "", item, false, false, ptrInt(0), ns(1500)) })
}

// TestStampsUntouchedByProgressAndPlays pins the stamp scope: checkpoints, play
// counts, and a played/finished change never move the star/rating change stamps.
func TestStampsUntouchedByProgressAndPlays(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	if _, err := st.SetStar(ctx, "", item, true, nil); err != nil {
		t.Fatal(err)
	}
	r := 90
	if _, err := st.SetRating(ctx, "", item, &r, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := st.PlayStateFor(ctx, "", item)

	if err := st.SetProgress(ctx, "", item, 42000); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPlayed(ctx, "", item, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetPlayed(ctx, "", item, false, false, ptrInt(0), nil); err != nil {
		t.Fatal(err)
	}
	after, _ := st.PlayStateFor(ctx, "", item)
	if after.StarredChangedAt != before.StarredChangedAt || after.RatingChangedAt != before.RatingChangedAt {
		t.Errorf("progress/play moved the stamps: %+v -> %+v", before, after)
	}
	if after.PositionMS != 42000 || after.PlayCount != 0 || after.Played {
		t.Errorf("progress/play state = %+v, want position 42000 and the play un-marked", after)
	}
}

// TestLastProgressStampedByPlaybackWritesOnly pins which writes move
// last_progress_at. The two playback writes stamp it; a star and a rating move
// updated_at and leave it alone, which is what keeps a star off the head of the
// in-progress list.
func TestLastProgressStampedByPlaybackWritesOnly(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	if err := st.SetProgress(ctx, "", item, 42000); err != nil {
		t.Fatal(err)
	}
	afterProgress, _ := st.PlayStateFor(ctx, "", item)
	if afterProgress.LastProgressAt == 0 {
		t.Fatal("SetProgress did not stamp last_progress_at")
	}
	if afterProgress.LastPlayedAt != 0 {
		t.Errorf("SetProgress stamped last_played_at = %d, want 0", afterProgress.LastPlayedAt)
	}

	if err := st.MarkPlayed(ctx, "", item, false); err != nil {
		t.Fatal(err)
	}
	afterPlay, _ := st.PlayStateFor(ctx, "", item)
	if afterPlay.LastProgressAt <= afterProgress.LastProgressAt {
		t.Errorf("MarkPlayed did not advance last_progress_at (%d -> %d)",
			afterProgress.LastProgressAt, afterPlay.LastProgressAt)
	}
	if afterPlay.LastPlayedAt == 0 {
		t.Error("MarkPlayed did not stamp last_played_at")
	}

	r := 90
	if _, err := st.SetRating(ctx, "", item, &r, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStar(ctx, "", item, true, nil); err != nil {
		t.Fatal(err)
	}
	// SetPlayed belongs with the star and the rating, not the playback writes:
	// un-marking a play must not push the item back up the in-progress list.
	if _, err := st.SetPlayed(ctx, "", item, false, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	afterMeta, _ := st.PlayStateFor(ctx, "", item)
	if afterMeta.LastProgressAt != afterPlay.LastProgressAt {
		t.Errorf("a star, rating, or un-mark moved last_progress_at (%d -> %d)",
			afterPlay.LastProgressAt, afterMeta.LastProgressAt)
	}
	if afterMeta.LastPlayedAt != afterPlay.LastPlayedAt {
		t.Errorf("an un-mark moved last_played_at (%d -> %d)",
			afterPlay.LastPlayedAt, afterMeta.LastPlayedAt)
	}
	if afterMeta.UpdatedAt <= afterPlay.UpdatedAt {
		t.Errorf("a star, rating, or un-mark did not move updated_at (%d -> %d)",
			afterPlay.UpdatedAt, afterMeta.UpdatedAt)
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
	if _, err := st.SetStar(ctx, "", item1, true, nil); err != nil {
		t.Fatal(err)
	}
	r := 70
	if _, err := st.SetRating(ctx, "", item2, &r, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStar(ctx, bob.PID, item2, true, nil); err != nil {
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
	if _, err := st.SetStar(ctx, "", r.ItemPID, true, nil); err != nil {
		t.Fatal(err)
	}
	spec.essence, spec.content, spec.title = "e2", "c2", "Second"
	putTrack(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM play_state"); n != 0 {
		t.Errorf("orphaned play_state rows = %d, want 0 (cascaded with the item)", n)
	}
}
