package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// userStateFixture scans three tracks and returns the store plus their item pids
// keyed by title, ready for per-user play-state assertions.
func userStateFixture(t *testing.T) (*Store, map[string]model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	ids := map[string]model.PID{}
	for _, title := range []string{"Alpha", "Bravo", "Charlie"} {
		res := putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + title + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "X", album: "Al",
		})
		ids[title] = res.ItemPID
	}
	return st, ids
}

// userQueryTitles runs q for userPID and returns the matched item titles, sorted so
// assertions do not depend on row order.
func userQueryTitles(t *testing.T, st *Store, q query.Query, userPID model.PID) []string {
	t.Helper()
	items, err := st.QueryItems(context.Background(), q, userPID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	titles := make([]string, 0, len(items))
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	sort.Strings(titles)
	return titles
}

func joinTitles(ss []string) string { return strings.Join(ss, ",") }

// TestQueryUserStateFields exercises a filter over each new per-user field for the
// default user, confirming the play_state join surfaces the right rows.
func TestQueryUserStateFields(t *testing.T) {
	st, ids := userStateFixture(t)
	ctx := context.Background()

	// Alpha: rated 80 + starred + played twice. Bravo: rated 20. Charlie: untouched.
	r80, r20 := 80, 20
	if err := st.SetRating(ctx, "", ids["Alpha"], &r80); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStar(ctx, "", ids["Alpha"], true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.MarkPlayed(ctx, "", ids["Alpha"], true); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetRating(ctx, "", ids["Bravo"], &r20); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		q    query.Query
		want []string
	}{
		{"rating gte 50", query.New(query.EntityItems).Where("rating", query.OpGte, 50).Build(), []string{"Alpha"}},
		{"rating present", query.New(query.EntityItems).WherePresence("rating", query.OpIsPresent).Build(), []string{"Alpha", "Bravo"}},
		{"rating missing", query.New(query.EntityItems).WherePresence("rating", query.OpIsMissing).Build(), []string{"Charlie"}},
		{"starred is 1", query.New(query.EntityItems).Where("starred", query.OpIs, 1).Build(), []string{"Alpha"}},
		{"starred is 0", query.New(query.EntityItems).Where("starred", query.OpIs, 0).Build(), []string{"Bravo", "Charlie"}},
		{"play_count gt 0", query.New(query.EntityItems).Where("play_count", query.OpGt, 0).Build(), []string{"Alpha"}},
		{"play_count is 2", query.New(query.EntityItems).Where("play_count", query.OpIs, 2).Build(), []string{"Alpha"}},
		{"played is 1", query.New(query.EntityItems).Where("played", query.OpIs, 1).Build(), []string{"Alpha"}},
		{"finished is 1", query.New(query.EntityItems).Where("finished", query.OpIs, 1).Build(), []string{"Alpha"}},
		{"last_played present", query.New(query.EntityItems).WherePresence("last_played", query.OpIsPresent).Build(), []string{"Alpha"}},
	}
	for _, c := range cases {
		got := userQueryTitles(t, st, c.q, "")
		if joinTitles(got) != joinTitles(c.want) {
			t.Errorf("%s = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestQueryUserStateLeftJoinVisibility is the load-bearing LEFT-join assertion: an
// item with no play_state row must stay visible for the "0-default" predicates
// (played is 0, play_count is 0, rating isMissing) and only drop out once a row
// exists. A silent INNER join would hide every never-played item.
func TestQueryUserStateLeftJoinVisibility(t *testing.T) {
	st, ids := userStateFixture(t)
	ctx := context.Background()

	// Only Alpha gets a play_state row; Bravo and Charlie have none at all.
	if err := st.MarkPlayed(ctx, "", ids["Alpha"], false); err != nil {
		t.Fatal(err)
	}

	// Unplayed items (no row) appear for played is 0.
	if got := userQueryTitles(t, st, query.New(query.EntityItems).Where("played", query.OpIs, 0).Build(), ""); joinTitles(got) != "Bravo,Charlie" {
		t.Errorf("played is 0 = %v, want [Bravo Charlie] (row-less items must remain visible)", got)
	}
	// play_count is 0 likewise coalesces a missing row to 0.
	if got := userQueryTitles(t, st, query.New(query.EntityItems).Where("play_count", query.OpIs, 0).Build(), ""); joinTitles(got) != "Bravo,Charlie" {
		t.Errorf("play_count is 0 = %v, want [Bravo Charlie]", got)
	}
	// rating isMissing must include the row-less items AND Alpha (whose row has a
	// NULL rating), i.e. all three.
	if got := userQueryTitles(t, st, query.New(query.EntityItems).WherePresence("rating", query.OpIsMissing).Build(), ""); joinTitles(got) != "Alpha,Bravo,Charlie" {
		t.Errorf("rating isMissing = %v, want all three", got)
	}
	// Only Alpha has a play, so play_count gt 0 returns just it.
	if got := userQueryTitles(t, st, query.New(query.EntityItems).Where("play_count", query.OpGt, 0).Build(), ""); joinTitles(got) != "Alpha" {
		t.Errorf("play_count gt 0 = %v, want [Alpha]", got)
	}
}

// TestQueryUserStateCrossUserIsolation is the explicit cross-user-leak assertion:
// one user's play_state must never surface for another user, and an item played by
// user A must read as unplayed for user B. This is exactly the bug a WHERE-clause
// user_id predicate (instead of an ON-clause one) would introduce.
func TestQueryUserStateCrossUserIsolation(t *testing.T) {
	st, ids := userStateFixture(t)
	ctx := context.Background()

	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Alpha is played + rated for the DEFAULT user only.
	r90 := 90
	if err := st.MarkPlayed(ctx, "", ids["Alpha"], false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRating(ctx, "", ids["Alpha"], &r90); err != nil {
		t.Fatal(err)
	}

	playedQ := query.New(query.EntityItems).Where("play_count", query.OpGt, 0).Build()
	ratedQ := query.New(query.EntityItems).Where("rating", query.OpGte, 50).Build()

	// The default user sees Alpha as played and rated.
	if got := userQueryTitles(t, st, playedQ, ""); joinTitles(got) != "Alpha" {
		t.Errorf("default user play_count gt 0 = %v, want [Alpha]", got)
	}
	if got := userQueryTitles(t, st, ratedQ, ""); joinTitles(got) != "Alpha" {
		t.Errorf("default user rating gte 50 = %v, want [Alpha]", got)
	}

	// Bob sees NONE of the default user's state: Alpha must not leak.
	if got := userQueryTitles(t, st, playedQ, bob.PID); len(got) != 0 {
		t.Errorf("bob play_count gt 0 = %v, want [] (no cross-user leak)", got)
	}
	if got := userQueryTitles(t, st, ratedQ, bob.PID); len(got) != 0 {
		t.Errorf("bob rating gte 50 = %v, want []", got)
	}
	// And Alpha reads as unplayed for Bob: it appears under played is 0.
	if got := userQueryTitles(t, st, query.New(query.EntityItems).Where("played", query.OpIs, 0).Build(), bob.PID); joinTitles(got) != "Alpha,Bravo,Charlie" {
		t.Errorf("bob played is 0 = %v, want all three (Alpha is unplayed for bob)", got)
	}
}

// TestCountItemsUserState confirms Count honors the per-user filter and, because the
// join is on play_state's primary key, never multiplies the count.
func TestCountItemsUserState(t *testing.T) {
	st, ids := userStateFixture(t)
	ctx := context.Background()
	if err := st.MarkPlayed(ctx, "", ids["Alpha"], false); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPlayed(ctx, "", ids["Bravo"], false); err != nil {
		t.Fatal(err)
	}
	n, err := st.CountItems(ctx, query.New(query.EntityItems).Where("play_count", query.OpGt, 0).Build(), "")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("count play_count gt 0 = %d, want 2", n)
	}
	// A different user counts zero (isolation), not the whole catalog.
	bob, _ := st.CreateUser(ctx, "bob")
	if n, err = st.CountItems(ctx, query.New(query.EntityItems).Where("play_count", query.OpGt, 0).Build(), bob.PID); err != nil {
		t.Fatalf("count bob: %v", err)
	}
	if n != 0 {
		t.Errorf("bob count play_count gt 0 = %d, want 0", n)
	}
}

// TestQueryPageUserState confirms keyset pagination still filters by user state
// while keeping its canonical sort_key ordering.
func TestQueryPageUserState(t *testing.T) {
	st, ids := userStateFixture(t)
	ctx := context.Background()
	for _, title := range []string{"Alpha", "Charlie"} {
		if err := st.MarkPlayed(ctx, "", ids[title], false); err != nil {
			t.Fatal(err)
		}
	}
	page, err := st.QueryPage(ctx, query.New(query.EntityItems).Where("play_count", query.OpGt, 0).Build(), "", 50, false, "")
	if err != nil {
		t.Fatalf("query page: %v", err)
	}
	var titles []string
	for _, it := range page.Items {
		titles = append(titles, it.Title)
	}
	// Canonical sort_key order (Alpha before Charlie), only the played items.
	if joinTitles(titles) != "Alpha,Charlie" {
		t.Errorf("paged play_count gt 0 = %v, want [Alpha Charlie]", titles)
	}
}

// TestSmartPlaylistPerUser confirms one smart-playlist rule yields per-user
// membership: the user is bound at read time, never stored in the rule.
func TestSmartPlaylistPerUser(t *testing.T) {
	st, ids := userStateFixture(t)
	ctx := context.Background()
	bob, _ := st.CreateUser(ctx, "bob")

	// Alpha starred for the default user; Bravo starred for bob.
	if err := st.SetStar(ctx, "", ids["Alpha"], true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStar(ctx, bob.PID, ids["Bravo"], true); err != nil {
		t.Fatal(err)
	}

	rule := query.New(query.EntityItems).Where("starred", query.OpIs, 1).Build()
	plPID, err := st.CreatePlaylist(ctx, "Faves", "", model.PlaylistSmart, model.VisibilityPrivate, &rule)
	if err != nil {
		t.Fatalf("create smart playlist: %v", err)
	}

	def, err := st.PlaylistItems(ctx, plPID, "")
	if err != nil {
		t.Fatalf("items default: %v", err)
	}
	if len(def) != 1 || def[0].Title != "Alpha" {
		t.Errorf("default user smart membership = %v, want [Alpha]", itemTitles(def))
	}
	bobItems, err := st.PlaylistItems(ctx, plPID, bob.PID)
	if err != nil {
		t.Fatalf("items bob: %v", err)
	}
	if len(bobItems) != 1 || bobItems[0].Title != "Bravo" {
		t.Errorf("bob smart membership = %v, want [Bravo]", itemTitles(bobItems))
	}
}

func itemTitles(items []*model.ItemView) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

// TestUserStateJoinIndexSeek confirms the per-user join resolves play_state by an
// index seek (its primary key / item index) rather than a full table scan, so no
// extra index is needed as the plan claims. It inspects the actual generated join.
func TestUserStateJoinIndexSeek(t *testing.T) {
	st, _ := userStateFixture(t)
	ctx := context.Background()
	uid, err := userIDByPID(ctx, st.read, "", "test")
	if err != nil {
		t.Fatalf("resolve default user: %v", err)
	}

	// The exact join + a per-user predicate the store emits.
	stmt := "EXPLAIN QUERY PLAN SELECT COUNT(*)" + itemJoins + userStateJoinClause +
		" WHERE COALESCE(ps.play_count, 0) > 0"
	rows, err := st.read.QueryContext(ctx, stmt, uid)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var plan strings.Builder
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range cells {
			dest[i] = &cells[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		for _, c := range cells {
			if c.Valid {
				plan.WriteString(c.String)
				plan.WriteByte(' ')
			}
		}
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}

	text := plan.String()
	if !strings.Contains(text, "play_state") {
		t.Fatalf("query plan does not reference play_state:\n%s", text)
	}
	// A full scan of play_state is the failure this guards against. The join has to be
	// a per-item index SEARCH, never a "SCAN play_state".
	if strings.Contains(text, "SCAN play_state") {
		t.Errorf("play_state is full-scanned (no index seek):\n%s", text)
	}
	if !strings.Contains(text, "SEARCH") {
		t.Errorf("query plan has no index SEARCH step:\n%s", text)
	}
}

// TestQueryUnknownUserValidated confirms a non-empty userPID that names no user
// errors even when the query references no user-state field, so a typo is caught
// rather than silently returning default-scoped results. The default user ("")
// stays valid and skips the lookup.
func TestQueryUnknownUserValidated(t *testing.T) {
	st, _ := userStateFixture(t)
	ctx := context.Background()

	// A user-agnostic query (no user-state field) with an unknown user still errors.
	noUserField := query.New(query.EntityItems).Where("title", query.OpIs, "Alpha").Build()
	if _, err := st.QueryItems(ctx, noUserField, "nope"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("QueryItems unknown user: want CodeNotFound, got %v", err)
	}
	if _, err := st.CountItems(ctx, noUserField, "nope"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("CountItems unknown user: want CodeNotFound, got %v", err)
	}
	if _, err := st.QueryPage(ctx, noUserField, "", 10, false, "nope"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("QueryPage unknown user: want CodeNotFound, got %v", err)
	}
	if _, err := st.Facet(ctx, noUserField, read.GroupGenre, "nope"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("Facet unknown user: want CodeNotFound, got %v", err)
	}

	// The default user is always valid (no lookup, no error).
	if _, err := st.QueryItems(ctx, noUserField, ""); err != nil {
		t.Errorf("default user query: %v", err)
	}
}

// TestFacetUserState confirms a facet honors a per-user filter (only the current
// user's played items are grouped).
func TestFacetUserState(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/l/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al", genre: "Rock"})
	putTrack(t, st, lib.ID, trackSpec{path: "/l/b.flac", essence: "eb", content: "cb", title: "B", artist: "Y", album: "Bl", genre: "Jazz"})
	if err := st.MarkPlayed(ctx, "", a.ItemPID, false); err != nil {
		t.Fatal(err)
	}

	// Facet the played items by genre: only Rock (A) has a play.
	q := query.New(query.EntityItems).Where("play_count", query.OpGt, 0).Build()
	res, err := st.Facet(ctx, q, read.GroupGenre, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	total := 0
	for _, b := range res.Buckets {
		total += b.Count
	}
	if total != 1 {
		t.Errorf("facet over played items counted %d, want 1 (only A is played)", total)
	}
}
