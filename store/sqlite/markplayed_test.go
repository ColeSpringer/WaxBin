package sqlite

import (
	"context"
	"testing"
)

// TestMarkPlayedAsOfRecordedTime pins the recorded-time contract of a play: the
// count always increments, the three stamps land in recorded time and never move
// backwards, and a live play after a recorded one stamps at server-now.
func TestMarkPlayedAsOfRecordedTime(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	if err := st.MarkPlayed(ctx, "", item, false, ptrNS(1000)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if got.PlayCount != 1 || !got.Played || got.Finished ||
		got.LastPlayedAt != 1000 || got.LastProgressAt != 1000 || got.PlayedChangedAt != 1000 {
		t.Fatalf("first recorded play = %+v, want count 1, played, every stamp at 1000", got)
	}

	if err := st.MarkPlayed(ctx, "", item, true, ptrNS(3000)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.PlayCount != 2 || !got.Finished ||
		got.LastPlayedAt != 3000 || got.LastProgressAt != 3000 || got.PlayedChangedAt != 3000 {
		t.Fatalf("newer recorded play = %+v, want count 2, finished, every stamp at 3000", got)
	}

	// An older play fills in history: it is counted, and no stamp moves backwards.
	if err := st.MarkPlayed(ctx, "", item, false, ptrNS(2000)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.PlayCount != 3 || !got.Finished ||
		got.LastPlayedAt != 3000 || got.LastProgressAt != 3000 || got.PlayedChangedAt != 3000 {
		t.Fatalf("older recorded play = %+v, want count 3 with every stamp still at 3000", got)
	}

	if err := st.MarkPlayed(ctx, "", item, false, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.PlayCount != 4 || got.LastPlayedAt <= 3000 || got.LastProgressAt <= 3000 || got.PlayedChangedAt <= 3000 {
		t.Fatalf("live play = %+v, want count 4 with every stamp past 3000", got)
	}
}

// TestMarkPlayedAsOfDoesNotResurrectAnUndo covers the hazard played_changed_at
// exists for: a play recorded before an un-mark that came later is counted and
// keeps its play time, but does not re-set the flags the user cleared. The delta
// still fires, since the count moved.
func TestMarkPlayedAsOfDoesNotResurrectAnUndo(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	if err := st.MarkPlayed(ctx, "", item, true, ptrNS(1000)); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetPlayed(ctx, "", item, false, false, nil, ptrNS(2000)); err != nil || !changed {
		t.Fatalf("un-mark: changed=%v err=%v", changed, err)
	}

	before := playStateDeltas(t, st)
	if err := st.MarkPlayed(ctx, "", item, true, ptrNS(1500)); err != nil {
		t.Fatal(err)
	}
	if playStateDeltas(t, st) != before+1 {
		t.Error("a counted play emitted no play_state delta")
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if got.Played || got.Finished {
		t.Errorf("a play recorded before the un-mark re-set the flags: %+v", got)
	}
	if got.PlayCount != 2 || got.LastPlayedAt != 1500 || got.PlayedChangedAt != 2000 {
		t.Errorf("state = %+v, want count 2, last played 1500, change stamp kept at 2000", got)
	}

	// A play recorded after the un-mark is the newer change and sets the flags.
	if err := st.MarkPlayed(ctx, "", item, true, ptrNS(2500)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if !got.Played || !got.Finished || got.PlayCount != 3 || got.LastPlayedAt != 2500 || got.PlayedChangedAt != 2500 {
		t.Errorf("state = %+v, want the newer play to set the flags and stamps at 2500", got)
	}
}

// TestMarkPlayedAsOfZeroIsServerNow pins the shared sentinel: a pointer to 0 is
// "no recorded time", not the epoch (see asOfRecorded).
func TestMarkPlayedAsOfZeroIsServerNow(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)
	before := nowNS()

	if err := st.MarkPlayed(ctx, "", item, false, ptrNS(0)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if got.LastPlayedAt < before || got.PlayedChangedAt < before {
		t.Fatalf("play with as-of 0 = %+v, want stamps at server-now (>= %d), not the epoch", got, before)
	}
}

// TestSetProgressAsOfRecordedTime pins the checkpoint half: the position always
// applies, the stamp lands in recorded time and never moves backwards, and a
// live checkpoint stamps at server-now.
func TestSetProgressAsOfRecordedTime(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	if err := st.SetProgress(ctx, "", item, 5000, ptrNS(1000)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if got.PositionMS != 5000 || got.LastProgressAt != 1000 || got.LastPlayedAt != 0 {
		t.Fatalf("recorded checkpoint = %+v, want position 5000 stamped at 1000 and no play", got)
	}

	// An older checkpoint still lands its position (the engine does not order resume
	// points), but must not move the stamp back.
	if err := st.SetProgress(ctx, "", item, 9000, ptrNS(500)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.PositionMS != 9000 || got.LastProgressAt != 1000 {
		t.Fatalf("older checkpoint = %+v, want position 9000 with the stamp still at 1000", got)
	}

	if err := st.SetProgress(ctx, "", item, 7000, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.PositionMS != 7000 || got.LastProgressAt <= 1000 {
		t.Fatalf("live checkpoint = %+v, want position 7000 stamped past 1000", got)
	}
}

// TestMarkPlayedSameTimeAsFlagChangeApplies pins the strict ordering: a play
// recorded at the same time as a flag change is not stale, so a clear and a play
// composed at one recorded time (the CLI's --reset-count --played --as-of) land
// the play.
func TestMarkPlayedSameTimeAsFlagChangeApplies(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	if err := st.MarkPlayed(ctx, "", item, true, ptrNS(1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetPlayed(ctx, "", item, false, false, ptrInt(0), ptrNS(2000)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPlayed(ctx, "", item, false, ptrNS(2000)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if !got.Played || got.Finished || got.PlayCount != 1 || got.PlayedChangedAt != 2000 || got.LastPlayedAt != 2000 {
		t.Fatalf("state = %+v, want played, unfinished, count 1, stamped at 2000", got)
	}
}

// TestPlaybackRecencyClampsFutureStamp pins the clamp on the two recency columns:
// a future-skewed recorded time lands at server-now there, so the next live play
// or checkpoint moves past it, while played_changed_at keeps the raw stamp for
// ordering.
func TestPlaybackRecencyClampsFutureStamp(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)
	const future = int64(1) << 62
	before := nowNS()

	if err := st.MarkPlayed(ctx, "", item, false, ptrNS(future)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if got.LastPlayedAt < before || got.LastPlayedAt >= future || got.LastProgressAt < before || got.LastProgressAt >= future {
		t.Fatalf("future play = %+v, want the recency stamps clamped to now", got)
	}
	if got.PlayedChangedAt != future {
		t.Errorf("played_changed_at = %d, want the raw future stamp %d", got.PlayedChangedAt, future)
	}

	if err := st.MarkPlayed(ctx, "", item, false, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := st.PlayStateFor(ctx, "", item)
	if after.LastPlayedAt <= got.LastPlayedAt || after.LastProgressAt <= got.LastProgressAt {
		t.Errorf("a live play after a future-stamped one did not advance recency: %+v -> %+v", got, after)
	}

	if err := st.SetProgress(ctx, "", item, 100, ptrNS(future)); err != nil {
		t.Fatal(err)
	}
	prog, _ := st.PlayStateFor(ctx, "", item)
	if prog.LastProgressAt >= future || prog.LastProgressAt < after.LastProgressAt {
		t.Errorf("future checkpoint stamped last_progress_at = %d, want clamped to now and not regressed", prog.LastProgressAt)
	}
}
