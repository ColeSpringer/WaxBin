package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/waxerr"
)

func ptrInt(v int) *int    { return &v }
func ptrNS(v int64) *int64 { return &v }

// TestSetPlayedUndoesAPlay covers the verb's reason for existing, across all
// three play-count modes: nil keeps the count, &0 resets it, &n sets it exactly.
func TestSetPlayedUndoesAPlay(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		playCount *int
		want      int
	}{
		{"count untouched", nil, 2},
		{"count reset", ptrInt(0), 0},
		{"count set exactly", ptrInt(7), 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, lib := entityFixture(t)
			item := seedItem(t, st, lib)
			if err := st.MarkPlayed(ctx, "", item, false); err != nil {
				t.Fatal(err)
			}
			if err := st.MarkPlayed(ctx, "", item, true); err != nil {
				t.Fatal(err)
			}

			changed, err := st.SetPlayed(ctx, "", item, false, false, tc.playCount, nil)
			if err != nil {
				t.Fatalf("SetPlayed: %v", err)
			}
			if !changed {
				t.Error("un-marking a played item reported changed=false")
			}
			got, _ := st.PlayStateFor(ctx, "", item)
			if got.Played || got.Finished {
				t.Errorf("played=%v finished=%v, want both false", got.Played, got.Finished)
			}
			if got.PlayCount != tc.want {
				t.Errorf("play count = %d, want %d", got.PlayCount, tc.want)
			}
			if got.PlayedChangedAt == 0 {
				t.Error("SetPlayed did not stamp played_changed_at")
			}
		})
	}
}

// TestSetPlayedKeepsCountConsistentWithPlayed covers marking an item played
// without naming a count, on both a fresh row and one a checkpoint already
// created. A zero count under played=1 would sort as never played in
// `browse most-played`, and MarkPlayed can never produce it.
func TestSetPlayedKeepsCountConsistentWithPlayed(t *testing.T) {
	ctx := context.Background()

	t.Run("no prior row", func(t *testing.T) {
		st, lib := entityFixture(t)
		item := seedItem(t, st, lib)
		if _, err := st.SetPlayed(ctx, "", item, true, false, nil, nil); err != nil {
			t.Fatal(err)
		}
		got, _ := st.PlayStateFor(ctx, "", item)
		if !got.Played || got.PlayCount != 1 {
			t.Errorf("played=%v count=%d, want played with count 1", got.Played, got.PlayCount)
		}
	})

	t.Run("row created by a checkpoint", func(t *testing.T) {
		st, lib := entityFixture(t)
		item := seedItem(t, st, lib)
		if err := st.SetProgress(ctx, "", item, 500); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetPlayed(ctx, "", item, true, true, nil, nil); err != nil {
			t.Fatal(err)
		}
		got, _ := st.PlayStateFor(ctx, "", item)
		if !got.Played || got.PlayCount != 1 {
			t.Errorf("played=%v count=%d, want played with count 1", got.Played, got.PlayCount)
		}
	})

	t.Run("an explicit count still wins", func(t *testing.T) {
		st, lib := entityFixture(t)
		item := seedItem(t, st, lib)
		if _, err := st.SetPlayed(ctx, "", item, true, false, ptrInt(9), nil); err != nil {
			t.Fatal(err)
		}
		got, _ := st.PlayStateFor(ctx, "", item)
		if got.PlayCount != 9 {
			t.Errorf("count = %d, want the caller's 9", got.PlayCount)
		}
	})

	t.Run("an existing count is untouched", func(t *testing.T) {
		st, lib := entityFixture(t)
		item := seedItem(t, st, lib)
		for i := 0; i < 3; i++ {
			if err := st.MarkPlayed(ctx, "", item, false); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.SetPlayed(ctx, "", item, true, true, nil, nil); err != nil {
			t.Fatal(err)
		}
		got, _ := st.PlayStateFor(ctx, "", item)
		if got.PlayCount != 3 {
			t.Errorf("count = %d, want the stored 3 left alone", got.PlayCount)
		}
	})
}

func TestSetPlayedRejectsInvalidInput(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	if _, err := st.SetPlayed(ctx, "", item, true, false, ptrInt(-1), nil); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("negative play count: want CodeInvalid, got %v (code %s)", err, waxerr.CodeOf(err))
	}
	// Unreachable through MarkPlayed, which hardcodes played=1; two independent
	// bools make it reachable for the first time, and both are query fields.
	if _, err := st.SetPlayed(ctx, "", item, false, true, nil, nil); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("finished without played: want CodeInvalid, got %v (code %s)", err, waxerr.CodeOf(err))
	}
	// The other half of the same invariant: played means at least one play.
	if _, err := st.SetPlayed(ctx, "", item, true, false, ptrInt(0), nil); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("played with a zero count: want CodeInvalid, got %v (code %s)", err, waxerr.CodeOf(err))
	}
	// Neither rejection wrote anything.
	got, _ := st.PlayStateFor(ctx, "", item)
	if got.Played || got.Finished || got.PlayCount != 0 {
		t.Errorf("a rejected call wrote state: %+v", got)
	}
}

// TestSetPlayedNoOpIsSilent pins the changed bool against the delta feed, which
// is the observable it claims to mirror.
func TestSetPlayedNoOpIsSilent(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	// Clearing flags on an item with no state at all creates no row.
	before := playStateDeltas(t, st)
	changed, err := st.SetPlayed(ctx, "", item, false, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed || playStateDeltas(t, st) != before {
		t.Errorf("clearing an untouched item reported changed=%v and moved the delta count", changed)
	}

	if err := st.MarkPlayed(ctx, "", item, true); err != nil {
		t.Fatal(err)
	}
	stamped, _ := st.PlayStateFor(ctx, "", item)
	before = playStateDeltas(t, st)

	changed, err = st.SetPlayed(ctx, "", item, true, true, ptrInt(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a value-identical SetPlayed reported changed=true")
	}
	if playStateDeltas(t, st) != before {
		t.Error("a value-identical SetPlayed appended a delta")
	}
	after, _ := st.PlayStateFor(ctx, "", item)
	if after.PlayedChangedAt != stamped.PlayedChangedAt {
		t.Errorf("a value-identical SetPlayed moved the stamp (%d -> %d)",
			stamped.PlayedChangedAt, after.PlayedChangedAt)
	}
}

func TestSetPlayedAsOfRecordedTime(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	// A NULL prior stamp carries no ordering, so the first recorded-time change
	// applies and lands in recorded time.
	changed, err := st.SetPlayed(ctx, "", item, true, true, ptrInt(1), ptrNS(1000))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first recorded-time change did not apply")
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if got.PlayedChangedAt != 1000 {
		t.Fatalf("stamp = %d, want the recorded time 1000", got.PlayedChangedAt)
	}

	// An older replay loses, and so does a same-time one.
	for _, at := range []int64{500, 1000} {
		changed, err := st.SetPlayed(ctx, "", item, false, false, nil, ptrNS(at))
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Errorf("replay at %d was applied over a change recorded at 1000", at)
		}
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if !got.Played || !got.Finished {
		t.Errorf("a stale replay undid the flags: %+v", got)
	}

	// A newer one wins.
	if changed, err = st.SetPlayed(ctx, "", item, false, false, nil, ptrNS(2000)); err != nil || !changed {
		t.Fatalf("newer replay: changed=%v err=%v", changed, err)
	}
	got, _ = st.PlayStateFor(ctx, "", item)
	if got.Played || got.PlayedChangedAt != 2000 {
		t.Errorf("state after the newer replay = %+v, want cleared and stamped at 2000", got)
	}
}

// TestSetPlayedStampScope pins what played_changed_at describes. A count-only
// change must leave it alone, or a count reset would outrank a genuine flag change
// recorded earlier, and it must never move backwards, or an interactive un-mark
// stamping at server-now would undercut a future stamp a replay already stored.
func TestSetPlayedStampScope(t *testing.T) {
	ctx := context.Background()

	t.Run("a count-only change leaves the stamp", func(t *testing.T) {
		st, lib := entityFixture(t)
		item := seedItem(t, st, lib)
		if _, err := st.SetPlayed(ctx, "", item, true, true, ptrInt(3), ptrNS(1000)); err != nil {
			t.Fatal(err)
		}
		changed, err := st.SetPlayed(ctx, "", item, true, true, ptrInt(5), ptrNS(2000))
		if err != nil || !changed {
			t.Fatalf("count-only change: changed=%v err=%v", changed, err)
		}
		got, _ := st.PlayStateFor(ctx, "", item)
		if got.PlayCount != 5 {
			t.Errorf("count = %d, want 5", got.PlayCount)
		}
		if got.PlayedChangedAt != 1000 {
			t.Errorf("stamp = %d, want the flag-change time 1000 left in place", got.PlayedChangedAt)
		}
		// So a flag change recorded between the two still applies.
		if changed, err := st.SetPlayed(ctx, "", item, false, false, nil, ptrNS(1500)); err != nil || !changed {
			t.Errorf("a flag change at 1500 was refused: changed=%v err=%v", changed, err)
		}
	})

	t.Run("the stamp never regresses", func(t *testing.T) {
		st, lib := entityFixture(t)
		item := seedItem(t, st, lib)
		const future = int64(1) << 62
		if _, err := st.SetPlayed(ctx, "", item, true, true, nil, ptrNS(future)); err != nil {
			t.Fatal(err)
		}
		// An interactive un-mark stamps at server-now, far below the future stamp.
		if changed, err := st.SetPlayed(ctx, "", item, false, false, nil, nil); err != nil || !changed {
			t.Fatalf("interactive un-mark: changed=%v err=%v", changed, err)
		}
		got, _ := st.PlayStateFor(ctx, "", item)
		if got.Played {
			t.Error("the un-mark did not apply")
		}
		if got.PlayedChangedAt != future {
			t.Errorf("stamp = %d, want the future stamp %d held", got.PlayedChangedAt, future)
		}
	})
}

// TestMarkPlayedOrdersAgainstSetPlayed is why MarkPlayed stamps at all, and pins
// the trailing-clock case SetPlayed's doc warns about.
func TestMarkPlayedOrdersAgainstSetPlayed(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	item := seedItem(t, st, lib)

	if err := st.MarkPlayed(ctx, "", item, true); err != nil {
		t.Fatal(err)
	}
	played, _ := st.PlayStateFor(ctx, "", item)
	if played.PlayedChangedAt == 0 {
		t.Fatal("MarkPlayed did not stamp played_changed_at on a row it created")
	}

	changed, err := st.SetPlayed(ctx, "", item, false, false, nil, ptrNS(played.PlayedChangedAt-1))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("an un-mark recorded before the play it undoes was applied")
	}
	got, _ := st.PlayStateFor(ctx, "", item)
	if !got.Played {
		t.Error("a stale un-mark cleared the played flag")
	}
}

// TestMarkPlayedStampIsMonotonic covers the two ways the stamp fails silently:
// the COALESCE trap on a first play, and a future asOf regressed by the next play.
func TestMarkPlayedStampIsMonotonic(t *testing.T) {
	ctx := context.Background()

	t.Run("first play on a fresh row", func(t *testing.T) {
		st, lib := entityFixture(t)
		item := seedItem(t, st, lib)
		if err := st.MarkPlayed(ctx, "", item, false); err != nil {
			t.Fatal(err)
		}
		got, _ := st.PlayStateFor(ctx, "", item)
		if got.PlayedChangedAt == 0 {
			t.Error("the first play left played_changed_at NULL")
		}
	})

	t.Run("first play on an existing row", func(t *testing.T) {
		st, lib := entityFixture(t)
		item := seedItem(t, st, lib)
		// A progress checkpoint creates the row with played_changed_at still NULL,
		// which is where the bare MAX() would wipe the stamp.
		if err := st.SetProgress(ctx, "", item, 1000); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkPlayed(ctx, "", item, false); err != nil {
			t.Fatal(err)
		}
		got, _ := st.PlayStateFor(ctx, "", item)
		if got.PlayedChangedAt == 0 {
			t.Error("a play over a checkpoint-created row left played_changed_at NULL")
		}
	})

	t.Run("future stamp is not regressed", func(t *testing.T) {
		st, lib := entityFixture(t)
		item := seedItem(t, st, lib)
		const future = int64(1) << 62
		if _, err := st.SetPlayed(ctx, "", item, true, false, nil, ptrNS(future)); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkPlayed(ctx, "", item, true); err != nil {
			t.Fatal(err)
		}
		got, _ := st.PlayStateFor(ctx, "", item)
		if got.PlayedChangedAt != future {
			t.Errorf("stamp = %d, want the future stamp %d left in place", got.PlayedChangedAt, future)
		}
		// The play itself still applied; only the stamp is pinned. Count 2: the
		// SetPlayed above set the flag with no count, which lands at 1, and the play
		// increments it.
		if !got.Finished || got.PlayCount != 2 {
			t.Errorf("state = %+v, want the play recorded regardless", got)
		}
	})
}
