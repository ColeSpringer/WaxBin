package sqlite

import (
	"context"
	"reflect"
	"testing"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

func TestFacetByTagValue(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// A: MOOD=[happy, sad], a multi-value item counted once per distinct value.
	putTrackCustom(t, st, lib.ID, "/lib/a.flac", "ea", "ca", "A",
		map[string][]string{"MOOD": {"happy", "sad"}}, true)
	// B: MOOD=[happy].
	putTrackCustom(t, st, lib.ID, "/lib/b.flac", "eb", "cb", "B",
		map[string][]string{"MOOD": {"happy"}}, true)
	// C: no MOOD, so an untagged item is excluded from the value dimension (INNER JOIN).
	putTrackCustom(t, st, lib.ID, "/lib/c.flac", "ec", "cc", "C", nil, true)

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupBy("tag.MOOD"), "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	if b, ok := bucketByDisplay(res, "happy"); !ok || b.Count != 2 {
		t.Errorf("happy bucket = %+v, want count 2 (A and B)", b)
	}
	if b, ok := bucketByDisplay(res, "sad"); !ok || b.Count != 1 {
		t.Errorf("sad bucket = %+v, want count 1 (A once)", b)
	}
	if len(res.Buckets) != 2 {
		t.Errorf("tag.MOOD facet has %d buckets, want 2 (untagged C excluded): %+v", len(res.Buckets), res.Buckets)
	}
	// A tag facet groups by value, not an entity, so buckets carry no drilldown pid.
	for _, b := range res.Buckets {
		if b.EntityPID != "" || b.IsUnknown {
			t.Errorf("tag bucket %+v should be a plain value bucket (no entity pid, no unknown)", b)
		}
	}
}

// TestFacetByTagWithUserFilter is the arg-ordering assertion: a facet over a custom-tag
// dimension AND a per-user filter binds args in clause order user-id -> tag key ->
// WHERE args. Any swap makes the tag key bind a non-key value and the facet returns the
// wrong (typically empty) result, so a correct happy:1 proves the ordering.
func TestFacetByTagWithUserFilter(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrackCustom(t, st, lib.ID, "/lib/a.flac", "ea", "ca", "A",
		map[string][]string{"MOOD": {"happy"}}, true)
	putTrackCustom(t, st, lib.ID, "/lib/b.flac", "eb", "cb", "B",
		map[string][]string{"MOOD": {"happy"}}, true)

	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.SetStar(ctx, bob.PID, a.ItemPID, true); err != nil {
		t.Fatalf("star: %v", err)
	}

	q := query.New(query.EntityItems).Where("starred", query.OpIs, 1).Build()
	res, err := st.Facet(ctx, q, read.GroupBy("tag.MOOD"), bob.PID)
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	if b, ok := bucketByDisplay(res, "happy"); !ok || b.Count != 1 {
		t.Fatalf("happy bucket = %+v, want count 1 (only A is starred by bob); arg order likely wrong", b)
	}
	if len(res.Buckets) != 1 {
		t.Errorf("facet buckets = %+v, want just happy:1", res.Buckets)
	}
}

func TestTagKeys(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrackCustom(t, st, lib.ID, "/lib/a.flac", "ea", "ca", "A",
		map[string][]string{"MOOD": {"happy", "sad"}, "MYKEY": {"foo"}}, true)
	putTrackCustom(t, st, lib.ID, "/lib/b.flac", "eb", "cb", "B",
		map[string][]string{"MOOD": {"calm"}}, true)

	keys, err := st.TagKeys(ctx)
	if err != nil {
		t.Fatalf("tag keys: %v", err)
	}
	// MOOD is on 2 distinct items (A counted once despite two values); MYKEY on 1.
	// Ordered by count desc, then key.
	want := []read.TagKeyCount{{Key: "MOOD", Count: 2}, {Key: "MYKEY", Count: 1}}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("TagKeys = %+v, want %+v", keys, want)
	}
}

func TestTagKeysEmpty(t *testing.T) {
	st, lib := entityFixture(t)
	putTrackCustom(t, st, lib.ID, "/lib/a.flac", "ea", "ca", "A", nil, true)
	keys, err := st.TagKeys(context.Background())
	if err != nil {
		t.Fatalf("tag keys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("TagKeys with no custom tags = %+v, want empty", keys)
	}
}
