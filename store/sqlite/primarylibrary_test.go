package sqlite

import (
	"context"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// placementFixture catalogs an item whose primary sits in the first library with a copy
// in the second (the same essence read again from the second root attaches as a copy and
// the first read keeps the item), an item placed in the second alone, and a fileless
// episode. It returns the store, the two libraries and the three item pids in that order.
func placementFixture(t *testing.T) (*Store, *model.Library, *model.Library, model.PID, model.PID, model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	lib2, err := st.EnsureLibrary(context.Background(), &model.Library{
		Root: []byte("/lib2"), DisplayRoot: "/lib2", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure lib2: %v", err)
	}
	copied := putTrack(t, st, lib.ID, trackSpec{path: "/lib/song.flac", essence: "esame", content: "c1",
		title: "Song", artist: "X", album: "Al"})
	second := putTrack(t, st, lib2.ID, trackSpec{path: "/lib2/song.flac", essence: "esame", content: "c2",
		title: "Song", artist: "X", album: "Al"})
	if !second.AttachedAsCopy || second.ItemPID != copied.ItemPID {
		t.Fatalf("second read = %+v, want a copy attached to %s", second, copied.ItemPID)
	}
	placed := putTrack(t, st, lib2.ID, trackSpec{path: "/lib2/other.flac", essence: "eother", content: "c3",
		title: "Other", artist: "Y", album: "Bl"})
	putFeed(t, st, "http://cast.example/f", "Ep1")
	eps, err := st.QueryItems(context.Background(), query.New(query.EntityItems).Where("kind", query.OpIs, "episode").Build(), "")
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %d (err %v), want the one fileless episode", len(eps), err)
	}
	return st, lib, lib2, copied.ItemPID, placed.ItemPID, eps[0].PID
}

func pidsOf(items []*model.ItemView) []model.PID {
	out := make([]model.PID, len(items))
	for i, it := range items {
		out[i] = it.PID
	}
	return out
}

// TestPrimaryLibraryFieldReadsThePlacement: library matches an item through any of its
// files, primary_library through the primary alone, and the two agree on a fileless item.
func TestPrimaryLibraryFieldReadsThePlacement(t *testing.T) {
	t.Parallel()
	st, lib, lib2, copied, placed, episode := placementFixture(t)
	ctx := context.Background()
	a, b := string(lib.PID), string(lib2.PID)

	for _, tc := range []struct {
		field string
		op    query.Op
		value any
		want  int
	}{
		{"library", query.OpIs, a, 1},
		{"library", query.OpIs, b, 2},
		{"primary_library", query.OpIs, a, 1},
		{"primary_library", query.OpIs, b, 1},
		{"primary_library", query.OpIsMissing, nil, 1},
		{"primary_library", query.OpIsPresent, nil, 2},
		// isNot is a deny-list, like notIn: a fileless item is kept, and a pid no library
		// holds excludes nothing.
		{"primary_library", query.OpIsNot, a, 2},
		{"primary_library", query.OpIsNot, "no-such-library", 3},
	} {
		if n := countWhere(t, st, tc.field, tc.op, tc.value); n != tc.want {
			t.Errorf("%s %s %v = %d, want %d", tc.field, tc.op, tc.value, n, tc.want)
		}
	}
	countValues := func(op query.Op, vals ...any) int {
		t.Helper()
		n, err := st.CountItems(ctx, query.New(query.EntityItems).
			WhereValues("primary_library", op, vals...).Build(), "")
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if n := countValues(query.OpIn, a, b); n != 2 {
		t.Errorf("primary_library in [A, B] = %d, want 2 (each placed item once)", n)
	}
	// notIn is a deny-list: it keeps the fileless episode, as library's does.
	if n := countValues(query.OpNotIn, a); n != 2 {
		t.Errorf("primary_library notIn [A] = %d, want 2 (the item placed in B and the episode)", n)
	}
	if n := countValues(query.OpNotIn, b); n != 2 {
		t.Errorf("primary_library notIn [B] = %d, want 2 (the item placed in A and the episode)", n)
	}

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Where("primary_library", query.OpIs, b).Build(), "")
	if err != nil || len(items) != 1 || items[0].PID != placed || items[0].LibraryPID != lib2.PID {
		t.Fatalf("primary_library is B = %v (err %v), want %s placed in B", pidsOf(items), err, placed)
	}
	items, err = st.QueryItems(ctx, query.New(query.EntityItems).Where("primary_library", query.OpIs, a).Build(), "")
	if err != nil || len(items) != 1 || items[0].PID != copied || items[0].LibraryPID != lib.PID {
		t.Fatalf("primary_library is A = %v (err %v), want %s placed in A", pidsOf(items), err, copied)
	}
	items, err = st.QueryItems(ctx, query.New(query.EntityItems).WherePresence("primary_library", query.OpIsMissing).Build(), "")
	if err != nil || len(items) != 1 || items[0].PID != episode || items[0].LibraryPID != "" {
		t.Fatalf("primary_library isMissing = %v (err %v), want the episode %s", pidsOf(items), err, episode)
	}
	// The keyset page compiles the same field, so a reader's grant scopes a listing page.
	page, err := st.QueryPage(ctx, query.New(query.EntityItems).WhereValues("primary_library", query.OpIn, b).Build(), "", 10, false, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].PID != placed || page.HasMore {
		t.Fatalf("page of primary_library in [B] = %+v (err %v), want %s alone", page, err, placed)
	}
}

// TestQueryItemsWithFilesLeavesFilelessOut: the file-operation selection holds the items
// with a file, so a fileless episode neither matches nor takes a Limit slot, where the
// plain read keeps it.
func TestQueryItemsWithFilesLeavesFilelessOut(t *testing.T) {
	t.Parallel()
	st, _, _, copied, placed, episode := placementFixture(t)
	ctx := context.Background()
	sorted := func(items []*model.ItemView) []model.PID {
		got := pidsOf(items)
		slices.Sort(got)
		return got
	}
	want := []model.PID{copied, placed}
	slices.Sort(want)

	items, err := st.QueryItemsWithFiles(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || !slices.Equal(sorted(items), want) {
		t.Fatalf("with files = %v (err %v), want the two placed items", pidsOf(items), err)
	}
	// The episode's title collates first, so two slots on the plain read go to it and one
	// placed item, while the file selection fills both with items that have a file.
	plain, err := st.QueryItems(ctx, query.New(query.EntityItems).Limit(2).Build(), "")
	if err != nil || len(plain) != 2 || plain[0].PID != episode {
		t.Fatalf("plain limit 2 = %v (err %v), want the episode first", pidsOf(plain), err)
	}
	limited, err := st.QueryItemsWithFiles(ctx, query.New(query.EntityItems).Limit(2).Build(), "")
	if err != nil || !slices.Equal(sorted(limited), want) {
		t.Errorf("with files, limit 2 = %v (err %v), want both placed items", pidsOf(limited), err)
	}
}

// TestPrimaryLibraryFacetPartitions: the primaryLibrary dimension counts each item once,
// under its primary file's library, where the library dimension counts a copied item in
// both, and every bucket drills down through primary_library to its own count.
func TestPrimaryLibraryFacetPartitions(t *testing.T) {
	t.Parallel()
	st, lib, lib2, _, _, _ := placementFixture(t)
	ctx := context.Background()
	all := query.New(query.EntityItems).Build()

	res, err := st.Facet(ctx, all, read.GroupPrimaryLibrary, "", 0, "")
	if err != nil {
		t.Fatalf("facet primaryLibrary: %v", err)
	}
	if b, ok := bucketByDisplay(res, "/lib"); !ok || b.Count != 1 || b.EntityPID != lib.PID {
		t.Errorf("/lib bucket = %+v, want count 1 keyed by the library pid", b)
	}
	if b, ok := bucketByDisplay(res, "/lib2"); !ok || b.Count != 1 || b.EntityPID != lib2.PID {
		t.Errorf("/lib2 bucket = %+v, want count 1 (the copy does not place its item here)", b)
	}
	if b, ok := bucketByDisplay(res, read.NoFile); !ok || b.Count != 1 || !b.IsUnknown {
		t.Errorf("no-file bucket = %+v, want count 1 + unknown (the episode)", b)
	}
	total, err := st.CountItems(ctx, all, "")
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, b := range res.Buckets {
		sum += b.Count
		if b.EntityPID == "" {
			continue
		}
		if n := countWhere(t, st, "primary_library", query.OpIs, string(b.EntityPID)); n != b.Count {
			t.Errorf("primaryLibrary bucket %q count %d != filter count %d", b.Display, b.Count, n)
		}
	}
	if sum != total {
		t.Errorf("primaryLibrary buckets sum to %d, want the %d items (a partition)", sum, total)
	}

	anyFile, err := st.Facet(ctx, all, read.GroupLibrary, "", 0, "")
	if err != nil {
		t.Fatalf("facet library: %v", err)
	}
	if b, ok := bucketByDisplay(anyFile, "/lib2"); !ok || b.Count != 2 {
		t.Errorf("library /lib2 bucket = %+v, want count 2 (the copy counts here)", b)
	}

	// Under a filter the dimension keeps the same shape: tracks alone leave no unknown bucket.
	tracks, err := st.Facet(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "track").Build(), read.GroupPrimaryLibrary, "", 0, "")
	if err != nil {
		t.Fatalf("facet primaryLibrary over tracks: %v", err)
	}
	if len(tracks.Buckets) != 2 || tracks.Buckets[0].Count != 1 || tracks.Buckets[1].Count != 1 {
		t.Errorf("primaryLibrary over tracks = %+v, want one item under each library and no unknown bucket", tracks.Buckets)
	}
}
