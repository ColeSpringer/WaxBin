package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// filteredLen is the contract's right-hand side, computed the slow way: hydrate the
// playlist and filter in Go. Every count assertion below is checked against it, since
// "CountPlaylistItems equals len(Items matching narrow)" is the whole promise.
func filteredLen(t *testing.T, st *Store, pid, userPID model.PID, narrow query.Node) int {
	t.Helper()
	ctx := context.Background()
	items, err := st.PlaylistItems(ctx, pid, userPID)
	if err != nil {
		t.Fatalf("playlist items: %v", err)
	}
	if narrow == nil {
		return len(items)
	}
	// Resolve the narrow over the whole catalog once, then intersect by pid, which
	// keeps this side independent of the counting code under test.
	matching, err := st.QueryItems(ctx, query.New(query.EntityItems).WhereNode(narrow).Build(), userPID)
	if err != nil {
		t.Fatalf("narrow query: %v", err)
	}
	ok := make(map[model.PID]bool, len(matching))
	for _, m := range matching {
		ok[m.PID] = true
	}
	n := 0
	for _, it := range items {
		if ok[it.PID] {
			n++
		}
	}
	return n
}

// wantCount asserts the count and that it agrees with the hydrate-and-filter answer.
func wantCount(t *testing.T, st *Store, pid, userPID model.PID, narrow query.Node, want int, what string) {
	t.Helper()
	got, err := st.CountPlaylistItems(context.Background(), pid, userPID, narrow)
	if err != nil {
		t.Fatalf("%s: count: %v", what, err)
	}
	if got != want {
		t.Errorf("%s: count = %d, want %d", what, got, want)
	}
	if hydrated := filteredLen(t, st, pid, userPID, narrow); got != hydrated {
		t.Errorf("%s: count = %d but hydrate-and-filter = %d; the contract is that they agree",
			what, got, hydrated)
	}
}

// countFixture catalogs ten tracks alternating Rock and Jazz, titled T0..T9 so
// sort_key order is the numeric order, and returns their pids in that order.
func countFixture(t *testing.T) (*Store, *model.Library, []model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	pids := make([]model.PID, 10)
	for i := range pids {
		genre := "Rock"
		if i%2 == 1 {
			genre = "Jazz"
		}
		n := fmt.Sprintf("%d", i)
		pids[i] = putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/t" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "T" + n, artist: "X", album: "Al", genre: genre,
			durationMS: 60_000,
		}).ItemPID
	}
	return st, lib, pids
}

func rockNarrow() query.Node { return query.Cond{Field: "genre", Op: query.OpIs, Value: "Rock"} }

func TestCountStaticPlaylistItems(t *testing.T) {
	st, _, pids := countFixture(t)
	ctx := context.Background()

	pl, err := st.CreatePlaylist(ctx, "Mix", "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// T0 (Rock) twice, then T1 (Jazz): entries 3, distinct items 2.
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{pids[0], pids[0], pids[1]}); err != nil {
		t.Fatalf("add: %v", err)
	}

	wantCount(t, st, pl, "", nil, 3, "static, no narrow")
	wantCount(t, st, pl, "", rockNarrow(), 2, "static, Rock narrow")

	// The count triangle: this counts entries where the facet counts distinct items.
	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupPlaylist, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	b, ok := bucketByDisplay(res, "Mix")
	if !ok || b.Count != 2 {
		t.Errorf("facet bucket = %+v, want count 2 (distinct items), against the count of 3 entries", b)
	}
	p, err := st.PlaylistByPID(ctx, pl)
	if err != nil {
		t.Fatalf("by pid: %v", err)
	}
	if p.ItemCount != 3 {
		t.Errorf("ItemCount = %d, want 3 entries", p.ItemCount)
	}
}

func TestCountSmartPlaylistUnlimited(t *testing.T) {
	st, _, _ := countFixture(t)
	ctx := context.Background()

	rule := query.New(query.EntityItems).Where("artist", query.OpIs, "X").Build()
	pl, err := st.CreatePlaylist(ctx, "All X", "", model.PlaylistSmart, "", &rule)
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}
	wantCount(t, st, pl, "", nil, 10, "smart unlimited, no narrow")
	wantCount(t, st, pl, "", rockNarrow(), 5, "smart unlimited, Rock narrow")
}

// TestCountSmartPlaylistLimitEvaluationOrder is the load-bearing case. The rule takes
// the first three tracks by title, of which two are Rock, while the catalog holds five
// Rock tracks in total. Pushing the narrow inside the rule would count all five and
// clamp to three; the truth is two.
func TestCountSmartPlaylistLimitEvaluationOrder(t *testing.T) {
	st, _, _ := countFixture(t)
	ctx := context.Background()

	rule := query.New(query.EntityItems).
		Where("artist", query.OpIs, "X").
		OrderBy("title", false).
		Limit(3).Build()
	pl, err := st.CreatePlaylist(ctx, "First three", "", model.PlaylistSmart, "", &rule)
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}
	// T0, T1, T2 selected; T0 and T2 are Rock.
	wantCount(t, st, pl, "", nil, 3, "smart limit 3, no narrow")
	wantCount(t, st, pl, "", rockNarrow(), 2, "smart limit 3, Rock narrow (not the clamp)")
}

// TestCountSmartPlaylistOffsetEvaluationOrder is the row that looks correct and is
// not: with no limit but an offset, treating the rule as unlimited over-counts by the
// offset.
func TestCountSmartPlaylistOffsetEvaluationOrder(t *testing.T) {
	st, _, _ := countFixture(t)
	ctx := context.Background()

	rule := query.New(query.EntityItems).
		Where("artist", query.OpIs, "X").
		OrderBy("title", false).
		Offset(4).Build()
	pl, err := st.CreatePlaylist(ctx, "All but four", "", model.PlaylistSmart, "", &rule)
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}
	// T4..T9 selected; T4, T6 and T8 are Rock.
	wantCount(t, st, pl, "", nil, 6, "smart offset 4, no narrow (not 10)")
	wantCount(t, st, pl, "", rockNarrow(), 3, "smart offset 4, Rock narrow")
}

func TestCountSmartPlaylistLimitModes(t *testing.T) {
	st, _, _ := countFixture(t)
	ctx := context.Background()
	base := func() *query.Builder {
		return query.New(query.EntityItems).Where("artist", query.OpIs, "X")
	}

	// Count mode under the match set: min(10, 4).
	count := base().OrderBy("title", false).Limit(4).Build()
	countPL, err := st.CreatePlaylist(ctx, "Four", "", model.PlaylistSmart, "", &count)
	if err != nil {
		t.Fatalf("create count-limited: %v", err)
	}
	wantCount(t, st, countPL, "", nil, 4, "count mode")

	// Count mode over the match set clamps to the matches, not the limit.
	over := base().Where("genre", query.OpIs, "Rock").Limit(99).Build()
	overPL, err := st.CreatePlaylist(ctx, "Ninety-nine", "", model.PlaylistSmart, "", &over)
	if err != nil {
		t.Fatalf("create over-limited: %v", err)
	}
	wantCount(t, st, overPL, "", nil, 5, "count mode above the match set")

	// Seeded random: min(n, limit) with a stable draw, so the hydrate-and-filter side
	// of wantCount is comparable.
	rnd := base().Limit(3).LimitBy(query.LimitRandom).Seed(42).Build()
	rndPL, err := st.CreatePlaylist(ctx, "Three random", "", model.PlaylistSmart, "", &rnd)
	if err != nil {
		t.Fatalf("create random: %v", err)
	}
	wantCount(t, st, rndPL, "", nil, 3, "seeded random mode")

	// A minutes budget accumulates row by row: ten one-minute tracks, so a
	// four-minute budget fills with four.
	budget := base().OrderBy("title", false).Limit(4).LimitBy(query.LimitMinutes).Build()
	budgetPL, err := st.CreatePlaylist(ctx, "Four minutes", "", model.PlaylistSmart, "", &budget)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	wantCount(t, st, budgetPL, "", nil, 4, "minutes budget")
	wantCount(t, st, budgetPL, "", rockNarrow(), 2, "minutes budget, Rock narrow")
}

// TestCountPlaylistItemsPerUser pins that the count follows the same per-user
// membership PlaylistItems does, using the starred-rule shape from
// TestSmartPlaylistPerUser.
func TestCountPlaylistItemsPerUser(t *testing.T) {
	st, ids := userStateFixture(t)
	ctx := context.Background()
	bob, _ := st.CreateUser(ctx, "bob")
	if _, err := st.SetStar(ctx, "", ids["Alpha"], true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStar(ctx, bob.PID, ids["Bravo"], true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStar(ctx, bob.PID, ids["Charlie"], true, nil); err != nil {
		t.Fatal(err)
	}

	rule := query.New(query.EntityItems).Where("starred", query.OpIs, 1).Build()
	pl, err := st.CreatePlaylist(ctx, "Faves", "", model.PlaylistSmart, model.VisibilityPrivate, &rule)
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}
	wantCount(t, st, pl, "", nil, 1, "starred rule, default user")
	wantCount(t, st, pl, bob.PID, nil, 2, "starred rule, bob")
}

// TestCountEvaluatedPlaylistChunkBoundary crosses the idBatchSize IN-clause boundary
// on the evaluate-then-count path, where a single condition would otherwise be
// rejected. The items are distinct, so the chunk sums cannot double-count.
func TestCountEvaluatedPlaylistChunkBoundary(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const n = idBatchSize + 7
	for i := 0; i < n; i++ {
		genre := "Rock"
		if i%2 == 1 {
			genre = "Jazz"
		}
		s := fmt.Sprintf("%04d", i)
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/t" + s + ".flac", essence: "e" + s, content: "c" + s,
			title: "T" + s, artist: "X", album: "Al", genre: genre})
	}
	// An offset forces the evaluate-then-count path over the whole remainder.
	rule := query.New(query.EntityItems).
		Where("artist", query.OpIs, "X").
		OrderBy("title", false).
		Offset(1).Build()
	pl, err := st.CreatePlaylist(ctx, "All but one", "", model.PlaylistSmart, "", &rule)
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}
	// T0000 is Rock and is the row the offset skips.
	wantRock := (n+1)/2 - 1
	wantCount(t, st, pl, "", nil, n-1, "chunked, no narrow")
	wantCount(t, st, pl, "", rockNarrow(), wantRock, "chunked, Rock narrow")
}

// TestCountPlaylistItemsMatrix walks the branch matrix and checks each row against the
// hydrate-and-filter answer, which is the honest way to hold the stated contract:
// CountItems equals len(Items filtered by narrow).
func TestCountPlaylistItemsMatrix(t *testing.T) {
	st, _, pids := countFixture(t)
	ctx := context.Background()

	static, err := st.CreatePlaylist(ctx, "Static", "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create static: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, static, []model.PID{pids[0], pids[0], pids[1], pids[2]}); err != nil {
		t.Fatalf("add: %v", err)
	}
	mk := func(name string, q query.Query) model.PID {
		t.Helper()
		pid, err := st.CreatePlaylist(ctx, name, "", model.PlaylistSmart, "", &q)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return pid
	}
	x := func() *query.Builder { return query.New(query.EntityItems).Where("artist", query.OpIs, "X") }

	for _, tc := range []struct {
		name   string
		pid    model.PID
		narrow query.Node
	}{
		{"static/no-narrow", static, nil},
		{"static/narrow", static, rockNarrow()},
		{"smart/unlimited/no-narrow", mk("U1", x().Build()), nil},
		{"smart/unlimited/narrow", mk("U2", x().Build()), rockNarrow()},
		{"smart/count/no-narrow", mk("C1", x().OrderBy("title", false).Limit(3).Build()), nil},
		{"smart/count/narrow", mk("C2", x().OrderBy("title", false).Limit(3).Build()), rockNarrow()},
		{"smart/random-seeded/no-narrow", mk("R1", x().Limit(3).LimitBy(query.LimitRandom).Seed(7).Build()), nil},
		{"smart/random-seeded/narrow", mk("R2", x().Limit(3).LimitBy(query.LimitRandom).Seed(7).Build()), rockNarrow()},
		{"smart/minutes/no-narrow", mk("M1", x().OrderBy("title", false).Limit(3).LimitBy(query.LimitMinutes).Build()), nil},
		{"smart/minutes/narrow", mk("M2", x().OrderBy("title", false).Limit(3).LimitBy(query.LimitMinutes).Build()), rockNarrow()},
		{"smart/offset/no-narrow", mk("O1", x().OrderBy("title", false).Offset(3).Build()), nil},
		{"smart/offset/narrow", mk("O2", x().OrderBy("title", false).Offset(3).Build()), rockNarrow()},
		{"smart/limit+offset/no-narrow", mk("LO1", x().OrderBy("title", false).Limit(4).Offset(2).Build()), nil},
		{"smart/limit+offset/narrow", mk("LO2", x().OrderBy("title", false).Limit(4).Offset(2).Build()), rockNarrow()},
	} {
		got, err := st.CountPlaylistItems(ctx, tc.pid, "", tc.narrow)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if want := filteredLen(t, st, tc.pid, "", tc.narrow); got != want {
			t.Errorf("%s: count = %d, want %d (len of the filtered membership)", tc.name, got, want)
		}
	}
}

// TestCountStaticPlaylistDrivesFromPlaylist pins the static shape's plan: it seeks the
// playlist by pid and never scans playable_item, which is what keeps a per-playlist
// count O(members) rather than O(catalog).
func TestCountStaticPlaylistDrivesFromPlaylist(t *testing.T) {
	st, _, pids := countFixture(t)
	ctx := context.Background()
	pl, err := st.CreatePlaylist(ctx, "Mix", "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, pl, pids[:3]); err != nil {
		t.Fatalf("add: %v", err)
	}
	stmt := itemCountSelect +
		" JOIN playlist_item pcli ON pcli.item_id = pi.id" +
		" JOIN playlist pcl ON pcl.id = pcli.playlist_id" +
		" WHERE pcl.pid = ?"
	plan := explainPlan(t, st, stmt, string(pl))
	if !strings.Contains(plan, "pcl") || !strings.Contains(plan, "sqlite_autoindex_playlist_1") {
		t.Errorf("the static count no longer seeks playlist(pid):\n%s", plan)
	}
	if strings.Contains(plan, "SCAN pi") {
		t.Errorf("the static count scans playable_item, so it is O(catalog) not O(members):\n%s", plan)
	}
}
