package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
)

// drain collects changes from ch until it goes quiet for a short window.
func drain(ch <-chan model.Change) []model.Change {
	var out []model.Change
	timeout := time.After(time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, c)
		case <-timeout:
			return out
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
}

func TestSubscribePublishesDeltas(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	ch, cancel := st.Subscribe()
	defer cancel()

	// A mutation publishes its change_log rows after commit.
	r := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song", artist: "X", album: "Al",
	})
	changes := drain(ch)
	if len(changes) == 0 {
		t.Fatal("subscriber received no deltas after a mutation")
	}
	var sawItemCreate bool
	for _, c := range changes {
		if c.EntityType == "item" && c.EntityPID == r.ItemPID && c.Op == model.OpCreate {
			sawItemCreate = true
		}
	}
	if !sawItemCreate {
		t.Errorf("expected an item-create delta for %s, got %+v", r.ItemPID, changes)
	}

	// A play_state mutation publishes a play_state delta.
	if err := st.SetStar(ctx, "", r.ItemPID, true); err != nil {
		t.Fatal(err)
	}
	psChanges := drain(ch)
	var sawPlayState bool
	for _, c := range psChanges {
		if c.EntityType == "play_state" && c.EntityPID == r.ItemPID {
			sawPlayState = true
		}
	}
	if !sawPlayState {
		t.Errorf("expected a play_state delta, got %+v", psChanges)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	st, lib := entityFixture(t)
	ch, cancel := st.Subscribe()
	cancel()
	// The channel is closed by cancel; a closed channel reads zero values with ok=false.
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after cancel")
	}
	// Further mutations must not panic (no live subscribers).
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "A"})
}

func TestDataVersionMovesOnCommit(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	before, err := st.DataVersion(ctx)
	if err != nil {
		t.Fatalf("data version: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "S", artist: "X", album: "A"})
	after, err := st.DataVersion(ctx)
	if err != nil {
		t.Fatalf("data version: %v", err)
	}
	if after == before {
		t.Errorf("data_version did not move after a commit (%d == %d)", before, after)
	}
}
