package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

func titlesOf(items []*model.ItemView) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

func TestStaticPlaylistOrderAndEdits(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al"}).ItemPID
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "X", album: "Al"}).ItemPID
	c := putTrack(t, st, lib.ID, trackSpec{path: "/lib/c.flac", essence: "ec", content: "cc", title: "C", artist: "X", album: "Al"}).ItemPID

	pl, err := st.CreatePlaylist(ctx, "Mix", "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Add in a deliberate, non-alphabetical order; the playlist preserves it.
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{c, a, b}); err != nil {
		t.Fatalf("add: %v", err)
	}
	items, err := st.PlaylistItems(ctx, pl, "")
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	if got := titlesOf(items); !equalStrings(got, []string{"C", "A", "B"}) {
		t.Errorf("order = %v, want [C A B] (insertion order, not collation)", got)
	}

	// Remove the middle item.
	if err := st.RemovePlaylistItem(ctx, pl, a); err != nil {
		t.Fatalf("remove: %v", err)
	}
	items, _ = st.PlaylistItems(ctx, pl, "")
	if got := titlesOf(items); !equalStrings(got, []string{"C", "B"}) {
		t.Errorf("after remove = %v, want [C B]", got)
	}

	// Replace/reorder.
	if err := st.SetPlaylistItems(ctx, pl, []model.PID{a, b, c}); err != nil {
		t.Fatalf("set: %v", err)
	}
	items, _ = st.PlaylistItems(ctx, pl, "")
	if got := titlesOf(items); !equalStrings(got, []string{"A", "B", "C"}) {
		t.Errorf("after set = %v, want [A B C]", got)
	}

	if p, _ := st.PlaylistByPID(ctx, pl); p.ItemCount != 3 {
		t.Errorf("item count = %d, want 3", p.ItemCount)
	}
}

func TestSmartPlaylistEvaluatedOnRead(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "Old", artist: "X", album: "Al", year: 1990})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "New", artist: "X", album: "Al", year: 2010})

	rule := query.New(query.EntityItems).Where("year", query.OpGte, 2000).Build()
	pl, err := st.CreatePlaylist(ctx, "Recent", "", model.PlaylistSmart, "", &rule)
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}
	items, err := st.PlaylistItems(ctx, pl, "")
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	if got := titlesOf(items); !equalStrings(got, []string{"New"}) {
		t.Errorf("smart membership = %v, want [New] (year>=2000)", got)
	}

	// A track that newly satisfies the rule appears without re-saving the playlist
	// (evaluated on read), proving it is not a frozen snapshot.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3", title: "Newer", artist: "X", album: "Al", year: 2020})
	items, _ = st.PlaylistItems(ctx, pl, "")
	if len(items) != 2 {
		t.Errorf("smart membership after new match = %v, want 2 items", titlesOf(items))
	}

	// Membership edits are rejected on a smart playlist.
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{"whatever"}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("editing a smart playlist err = %v, want CodeInvalid", err)
	}
}

func TestPlaylistRuleRoundTrips(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	_ = lib
	rule := query.New(query.EntityItems).Where("artist", query.OpContains, "Radiohead").OrderBy("year", true).Limit(5).Build()
	pl, err := st.CreatePlaylist(ctx, "RH", "", model.PlaylistSmart, "", &rule)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.PlaylistByPID(ctx, pl)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Rule == nil || got.Rule.Limit != 5 || len(got.Rule.Sorts) != 1 || !got.Rule.Sorts[0].Desc {
		t.Errorf("round-tripped rule = %+v, want limit 5 + desc year sort", got.Rule)
	}
}

func TestCreatePlaylistValidation(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	// Smart requires a rule.
	if _, err := st.CreatePlaylist(ctx, "x", "", model.PlaylistSmart, "", nil); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("smart without rule err = %v, want CodeInvalid", err)
	}
	// Static must not carry a rule.
	r := query.New(query.EntityItems).Build()
	if _, err := st.CreatePlaylist(ctx, "x", "", model.PlaylistStatic, "", &r); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("static with rule err = %v, want CodeInvalid", err)
	}
	// Create validates the rule the way set-rule does: an unknown field, an
	// unqueryable entity, or a bad limit-mode combination is rejected at write
	// time rather than surfacing on every future read.
	for name, bad := range map[string]query.Query{
		"unknown field":   query.New(query.EntityItems).Where("bogus", query.OpIs, "x").Build(),
		"files entity":    query.New(query.EntityFiles).Build(),
		"random no limit": query.New(query.EntityItems).LimitBy(query.LimitRandom).Build(),
	} {
		bad := bad
		if _, err := st.CreatePlaylist(ctx, "x", "", model.PlaylistSmart, "", &bad); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("create with %s err = %v, want CodeInvalid", name, err)
		}
	}
}

func TestSetPlaylistRule(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "Old", artist: "X", album: "Al", year: 1990})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "New", artist: "X", album: "Al", year: 2010})

	rule := query.New(query.EntityItems).Where("year", query.OpGte, 2000).Build()
	pl, err := st.CreatePlaylist(ctx, "Recent", "", model.PlaylistSmart, "", &rule)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	items, _ := st.PlaylistItems(ctx, pl, "")
	if got := titlesOf(items); !equalStrings(got, []string{"New"}) {
		t.Fatalf("initial membership = %v, want [New]", got)
	}

	// Replacing the rule flips membership under the same pid and emits exactly
	// one playlist update delta.
	seqBefore, _ := st.LatestChangeSeq(ctx)
	flipped := query.New(query.EntityItems).Where("year", query.OpLt, 2000).Build()
	if err := st.SetPlaylistRule(ctx, pl, flipped); err != nil {
		t.Fatalf("set rule: %v", err)
	}
	changes, _ := st.ChangesSince(ctx, seqBefore)
	if len(changes) != 1 || changes[0].EntityType != "playlist" ||
		changes[0].EntityPID != pl || changes[0].Op != model.OpUpdate {
		t.Errorf("changes after set-rule = %+v, want one playlist update for %s", changes, pl)
	}
	items, _ = st.PlaylistItems(ctx, pl, "")
	if got := titlesOf(items); !equalStrings(got, []string{"Old"}) {
		t.Errorf("membership after set-rule = %v, want [Old] (same pid, new rule)", got)
	}
	// The stored rule round-trips as the new rule.
	got, err := st.PlaylistByPID(ctx, pl)
	if err != nil || got.Rule == nil {
		t.Fatalf("playlist after set-rule = %+v (err %v), want a rule", got, err)
	}
	wantDoc, _ := query.MarshalRule(flipped)
	gotDoc, _ := query.MarshalRule(*got.Rule)
	if string(gotDoc) != string(wantDoc) {
		t.Errorf("stored rule = %s, want %s", gotDoc, wantDoc)
	}

	// Re-writing the byte-identical rule is a silent no-op: no delta.
	seqNoop, _ := st.LatestChangeSeq(ctx)
	if err := st.SetPlaylistRule(ctx, pl, flipped); err != nil {
		t.Fatalf("no-op set rule: %v", err)
	}
	if seqAfter, _ := st.LatestChangeSeq(ctx); seqAfter != seqNoop {
		t.Errorf("no-op set-rule emitted a delta (seq %d -> %d)", seqNoop, seqAfter)
	}

	// Rejections, all without a delta and without touching the stored rule: a
	// static playlist, an unknown pid, and an uncompilable rule.
	static, _ := st.CreatePlaylist(ctx, "S", "", model.PlaylistStatic, "", nil)
	seqRej, _ := st.LatestChangeSeq(ctx)
	if err := st.SetPlaylistRule(ctx, static, flipped); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("set-rule on static err = %v, want CodeInvalid", err)
	}
	if err := st.SetPlaylistRule(ctx, "nope", flipped); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("set-rule on unknown pid err = %v, want CodeNotFound", err)
	}
	bad := query.New(query.EntityItems).Where("bogus", query.OpIs, "x").Build()
	if err := st.SetPlaylistRule(ctx, pl, bad); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("set-rule with unknown field err = %v, want CodeInvalid", err)
	}
	if seqAfter, _ := st.LatestChangeSeq(ctx); seqAfter != seqRej {
		t.Errorf("rejected set-rule emitted a delta (seq %d -> %d)", seqRej, seqAfter)
	}
	items, _ = st.PlaylistItems(ctx, pl, "")
	if got := titlesOf(items); !equalStrings(got, []string{"Old"}) {
		t.Errorf("membership after rejections = %v, want [Old] unchanged", got)
	}
}

func TestRemovePlaylistItemAt(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al"}).ItemPID
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "X", album: "Al"}).ItemPID

	pl, _ := st.CreatePlaylist(ctx, "Dup", "", model.PlaylistStatic, "", nil)
	// A appears twice (indexes 0, 2); B once (index 1).
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{a, b, a}); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Remove the single A at index 0; the other A survives.
	if err := st.RemovePlaylistItemAt(ctx, pl, 0, ""); err != nil {
		t.Fatalf("remove at: %v", err)
	}
	items, _ := st.PlaylistItems(ctx, pl, "")
	if got := titlesOf(items); !equalStrings(got, []string{"B", "A"}) {
		t.Errorf("after removing index 0 = %v, want [B A]", got)
	}
	// Removing past the end errors rather than reporting a no-op.
	if err := st.RemovePlaylistItemAt(ctx, pl, 99, ""); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("removing a missing index err = %v, want CodeNotFound", err)
	}
}

// playlistTracks puts one track per title and returns their pids in order.
func playlistTracks(t *testing.T, st *Store, libID int64, titles ...string) []model.PID {
	t.Helper()
	out := make([]model.PID, len(titles))
	for i, title := range titles {
		out[i] = putTrack(t, st, libID, trackSpec{path: "/lib/" + title + ".flac", essence: "e" + title,
			content: "c" + title, title: title, artist: "X", album: "Al"}).ItemPID
	}
	return out
}

// staticPlaylist creates a static playlist holding members in order.
func staticPlaylist(t *testing.T, st *Store, name string, members ...model.PID) model.PID {
	t.Helper()
	ctx := context.Background()
	pl, err := st.CreatePlaylist(ctx, name, "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := st.AddPlaylistItems(ctx, pl, members); err != nil {
		t.Fatalf("add to %s: %v", name, err)
	}
	return pl
}

// playlistPositions reads a playlist's stored positions in listing order.
func playlistPositions(t *testing.T, st *Store, pl model.PID) []int {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(), `SELECT pli.position FROM playlist_item pli
		JOIN playlist p ON p.id = pli.playlist_id WHERE p.pid = ? ORDER BY pli.position`, string(pl))
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan position: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// assertListing checks a playlist's titles in order and that its positions are its
// listing indexes.
func assertListing(t *testing.T, st *Store, pl model.PID, want ...string) {
	t.Helper()
	items, err := st.PlaylistItems(context.Background(), pl, "")
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	if got := titlesOf(items); !equalStrings(got, want) {
		t.Errorf("listing = %v, want %v", got, want)
	}
	for i, p := range playlistPositions(t, st, pl) {
		if p != i {
			t.Errorf("positions = %v, want 0..%d", playlistPositions(t, st, pl), len(want)-1)
			break
		}
	}
}

// playlistChanges counts the playlist deltas after seq, per playlist pid.
func playlistChanges(t *testing.T, st *Store, seq int64) map[model.PID]int {
	t.Helper()
	changes, err := st.ChangesSince(context.Background(), seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	out := map[model.PID]int{}
	for _, c := range changes {
		if c.EntityType == "playlist" && c.Op == model.OpUpdate {
			out[c.EntityPID]++
		}
	}
	return out
}

func playlistUpdatedAt(t *testing.T, st *Store, pl model.PID) int64 {
	t.Helper()
	p, err := st.PlaylistByPID(context.Background(), pl)
	if err != nil {
		t.Fatalf("playlist: %v", err)
	}
	return p.UpdatedAt
}

// TestRemoveAtAddressesTheListingIndex is the COMPAT-01 sequence: each removal names
// an index of the listing as it stands, never a position an earlier removal left behind.
func TestRemoveAtAddressesTheListingIndex(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pl := staticPlaylist(t, st, "Five", playlistTracks(t, st, lib.ID, "S0", "S1", "S2", "S3", "S4")...)
	for _, step := range []struct {
		index int
		want  []string
	}{
		{0, []string{"S1", "S2", "S3", "S4"}},
		{1, []string{"S1", "S3", "S4"}},
		{0, []string{"S3", "S4"}},
		{0, []string{"S4"}},
	} {
		if err := st.RemovePlaylistItemAt(ctx, pl, step.index, ""); err != nil {
			t.Fatalf("remove at %d: %v", step.index, err)
		}
		assertListing(t, st, pl, step.want...)
	}
}

// TestPlaylistRemovalsKeepPositionsDense: removing every occurrence of an item and
// removing the last of a duplicate both leave positions equal to listing indexes.
func TestPlaylistRemovalsKeepPositionsDense(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	p := playlistTracks(t, st, lib.ID, "A", "B", "C")
	a, b, c := p[0], p[1], p[2]

	dup := staticPlaylist(t, st, "Dup", a, b, a, c)
	if err := st.RemovePlaylistItem(ctx, dup, a); err != nil {
		t.Fatalf("remove A: %v", err)
	}
	assertListing(t, st, dup, "B", "C")

	last := staticPlaylist(t, st, "Last", a, b, a)
	if err := st.RemovePlaylistItemAt(ctx, last, 2, ""); err != nil {
		t.Fatalf("remove index 2: %v", err)
	}
	assertListing(t, st, last, "A", "B")
}

// TestRemoveAtRefusesANegativeIndex: a negative index is a caller error, never the first
// entry, and nothing moves.
func TestRemoveAtRefusesANegativeIndex(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pl := staticPlaylist(t, st, "P", playlistTracks(t, st, lib.ID, "A", "B")...)
	seq := latestSeq(t, st)
	if err := st.RemovePlaylistItemAt(ctx, pl, -1, ""); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("remove at -1: err = %v, want CodeInvalid", err)
	}
	if err := st.RemovePlaylistItemsAt(ctx, pl, []int{0, -1}, nil); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("remove at [0 -1]: err = %v, want CodeInvalid", err)
	}
	assertListing(t, st, pl, "A", "B")
	if n := playlistChanges(t, st, seq)[pl]; n != 0 {
		t.Errorf("refused removals emitted %d deltas, want none", n)
	}
}

// TestRemovePlaylistItemsAtUsesOneSnapshot: every index names the listing as it stood
// before any entry went, the removal is atomic, and it emits one delta.
func TestRemovePlaylistItemsAtUsesOneSnapshot(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pl := staticPlaylist(t, st, "Five", playlistTracks(t, st, lib.ID, "S0", "S1", "S2", "S3", "S4")...)

	seq := latestSeq(t, st)
	if err := st.RemovePlaylistItemsAt(ctx, pl, []int{3, 1}, nil); err != nil {
		t.Fatalf("remove [3 1]: %v", err)
	}
	assertListing(t, st, pl, "S0", "S2", "S4")
	if n := playlistChanges(t, st, seq)[pl]; n != 1 {
		t.Errorf("batch removal emitted %d deltas, want 1", n)
	}

	// An index past the end refuses the whole batch.
	if err := st.RemovePlaylistItemsAt(ctx, pl, []int{0, 3}, nil); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("remove [0 3] of three: err = %v, want CodeNotFound", err)
	}
	assertListing(t, st, pl, "S0", "S2", "S4")

	// An index named twice is one entry.
	if err := st.RemovePlaylistItemsAt(ctx, pl, []int{1, 1}, nil); err != nil {
		t.Fatalf("remove [1 1]: %v", err)
	}
	assertListing(t, st, pl, "S0", "S4")

	// Nothing named is nothing done.
	seq = latestSeq(t, st)
	if err := st.RemovePlaylistItemsAt(ctx, pl, nil, nil); err != nil {
		t.Fatalf("remove none: %v", err)
	}
	if n := playlistChanges(t, st, seq)[pl]; n != 0 {
		t.Errorf("an empty batch emitted %d deltas, want none", n)
	}
}

// TestRemoveAtRefusesAnEntryHoldingAnotherItem: the guarded forms remove an entry only
// while it still holds the item the caller saw there.
func TestRemoveAtRefusesAnEntryHoldingAnotherItem(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	p := playlistTracks(t, st, lib.ID, "A", "B", "C")
	a, b, c := p[0], p[1], p[2]
	pl := staticPlaylist(t, st, "P", a, b, c)

	if err := st.RemovePlaylistItemAt(ctx, pl, 1, a); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("remove index 1 expecting A: err = %v, want CodeConflict", err)
	}
	if err := st.RemovePlaylistItemsAt(ctx, pl, []int{0, 2}, []model.PID{a, b}); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("remove [0 2] expecting [A B]: err = %v, want CodeConflict", err)
	}
	if err := st.RemovePlaylistItemsAt(ctx, pl, []int{0, 2}, []model.PID{a}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("remove [0 2] with one expectation: err = %v, want CodeInvalid", err)
	}
	// An entry the caller saw that is no longer there is a stale listing too.
	if err := st.RemovePlaylistItemAt(ctx, pl, 3, c); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("remove index 3 expecting C: err = %v, want CodeConflict", err)
	}
	if err := st.RemovePlaylistItemsAt(ctx, pl, []int{0, 7}, []model.PID{a, c}); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("remove [0 7] expecting [A C]: err = %v, want CodeConflict", err)
	}
	if err := st.RemovePlaylistItemAt(ctx, pl, 3, ""); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("remove index 3 unguarded: err = %v, want CodeNotFound", err)
	}
	assertListing(t, st, pl, "A", "B", "C")

	if err := st.RemovePlaylistItemAt(ctx, pl, 1, b); err != nil {
		t.Fatalf("remove index 1 expecting B: %v", err)
	}
	assertListing(t, st, pl, "A", "C")
	if err := st.RemovePlaylistItemsAt(ctx, pl, []int{1, 0}, []model.PID{c, a}); err != nil {
		t.Fatalf("remove [1 0] expecting [C A]: %v", err)
	}
	assertListing(t, st, pl)
}

// TestEveryMembershipChangeTouchesThePlaylist: each way of changing a static playlist's
// entries moves updated_at and emits one playlist delta.
func TestEveryMembershipChangeTouchesThePlaylist(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	p := playlistTracks(t, st, lib.ID, "A", "B", "C")
	a, b, c := p[0], p[1], p[2]
	pl := staticPlaylist(t, st, "P", a, b)
	for name, change := range map[string]func() error{
		"add":         func() error { return st.AddPlaylistItems(ctx, pl, []model.PID{c, a}) },
		"set":         func() error { return st.SetPlaylistItems(ctx, pl, []model.PID{a, b, c, a}) },
		"remove":      func() error { return st.RemovePlaylistItem(ctx, pl, a) },
		"remove at":   func() error { return st.RemovePlaylistItemAt(ctx, pl, 0, "") },
		"remove many": func() error { return st.RemovePlaylistItemsAt(ctx, pl, []int{0}, nil) },
	} {
		if err := st.SetPlaylistItems(ctx, pl, []model.PID{a, b, c, a}); err != nil {
			t.Fatalf("reset: %v", err)
		}
		before, seq := playlistUpdatedAt(t, st, pl), latestSeq(t, st)
		if err := change(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if after := playlistUpdatedAt(t, st, pl); after <= before {
			t.Errorf("%s: updated_at %d -> %d, want it moved", name, before, after)
		}
		if n := playlistChanges(t, st, seq)[pl]; n != 1 {
			t.Errorf("%s: %d playlist deltas, want 1", name, n)
		}
	}
}

// TestFoldSettlesThePlaylistsItTouches: tracks folding into a book they became leave the
// playlists and the queue that held them listing the book, with positions kept dense,
// updated_at moved, and a delta for each playlist and the queue.
func TestFoldSettlesThePlaylistsItTouches(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	var parts []model.PID
	for _, n := range []string{"01", "02", "03"} {
		parts = append(parts, putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/" + n + ".mp3", essence: "fe" + n,
			content: "fc" + n, title: "Chapter " + n, artist: "A", album: "T", durationMS: 1000}).ItemPID)
	}
	x := putTrack(t, st, lib.ID, trackSpec{path: "/lib/x.flac", essence: "ex", content: "cx", title: "X", artist: "X", album: "Al"}).ItemPID
	both := staticPlaylist(t, st, "Both", parts[0], parts[1], x, parts[2])
	moved := staticPlaylist(t, st, "Moved", x, parts[2])
	if err := st.SetQueue(ctx, "", []model.PID{parts[1], x}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	stamps := map[model.PID]int64{both: playlistUpdatedAt(t, st, both), moved: playlistUpdatedAt(t, st, moved)}
	seq := latestSeq(t, st)

	for i, n := range []string{"01", "02", "03"} {
		putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "fe" + n, content: "fc" + n,
			title: "T", author: "A", position: i + 1, durationMS: 1000})
	}
	assertListing(t, st, both, "T", "X")
	assertListing(t, st, moved, "X", "T")
	deltas := playlistChanges(t, st, seq)
	for pl, before := range stamps {
		if deltas[pl] == 0 {
			t.Errorf("playlist %s: no delta after the fold", pl)
		}
		if after := playlistUpdatedAt(t, st, pl); after <= before {
			t.Errorf("playlist %s: updated_at %d -> %d, want it moved", pl, before, after)
		}
	}
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	queued := 0
	for _, c := range changes {
		if c.EntityType == "play_queue" {
			queued++
		}
	}
	if queued == 0 {
		t.Error("the queue changed with no play_queue delta")
	}
}

// TestPlaylistPositionDriftIsReportedAndCompacted: positions a catalog wrote before they
// were kept dense are reported, though indexes resolve by rank so the catalog stays
// consistent, and the repair renumbers them in listing order without a delta, since the
// listing does not change.
func TestPlaylistPositionDriftIsReportedAndCompacted(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	p := playlistTracks(t, st, lib.ID, "A", "B", "C")
	sparse := staticPlaylist(t, st, "Sparse", p...)
	dense := staticPlaylist(t, st, "Dense", p...)
	if _, err := st.write.ExecContext(ctx, `UPDATE playlist_item SET position = CASE position
		WHEN 0 THEN -3 WHEN 1 THEN 4 ELSE 10 END
		WHERE playlist_id = (SELECT id FROM playlist WHERE pid = ?)`, string(sparse)); err != nil {
		t.Fatalf("spread the positions: %v", err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.PlaylistPositionDrift != 1 || !rep.Consistent() {
		t.Fatalf("report = %+v, want one playlist drifting and the catalog consistent", rep)
	}
	seq := latestSeq(t, st)
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	assertListing(t, st, sparse, "A", "B", "C")
	assertListing(t, st, dense, "A", "B", "C")
	if rep, err := st.VerifyDerived(ctx); err != nil || rep.PlaylistPositionDrift != 0 {
		t.Errorf("after the repair: %+v (err %v), want no drift", rep, err)
	}
	if n := len(playlistChanges(t, st, seq)); n != 0 {
		t.Errorf("the repair emitted deltas for %d playlists, want none", n)
	}
}

func TestRemovePlaylistItemNonMemberIsNoOp(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al"}).ItemPID
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "X", album: "Al"}).ItemPID

	pl, _ := st.CreatePlaylist(ctx, "P", "", model.PlaylistStatic, "", nil)
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{a}); err != nil {
		t.Fatalf("add: %v", err)
	}
	seqBefore, _ := st.LatestChangeSeq(ctx)
	// Removing an item that is not in the playlist must not report success or churn
	// the change feed.
	if err := st.RemovePlaylistItem(ctx, pl, b); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("removing a non-member err = %v, want CodeNotFound", err)
	}
	seqAfter, _ := st.LatestChangeSeq(ctx)
	if seqAfter != seqBefore {
		t.Errorf("a no-op remove emitted a spurious change delta (seq %d -> %d)", seqBefore, seqAfter)
	}
}

func TestItemsByPlaylistPathMatching(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Stored paths are OS-native and absolute, as a real scan writes them. The
	// playlist entries below stay forward-slash, which is what M3U8 carries on
	// every platform and what the matcher normalizes; a bare "/lib" is not even
	// absolute on Windows without a volume.
	vol := ""
	if wd, err := os.Getwd(); err == nil {
		vol = filepath.VolumeName(wd)
	}
	nat := func(p string) string { return filepath.FromSlash(vol + p) }
	putTrack(t, st, lib.ID, trackSpec{path: nat("/lib/al/1.flac"), essence: "e1", content: "c1", title: "One", artist: "X", album: "Al"})
	one := func(p string) string {
		t.Helper()
		items, err := st.ItemsByPlaylistPath(ctx, p)
		if err != nil {
			t.Fatalf("items by %s: %v", p, err)
		}
		if len(items) != 1 {
			return ""
		}
		return items[0].Title
	}

	// Absolute path: exact (indexed) match.
	if got := one(vol + "/lib/al/1.flac"); got != "One" {
		t.Errorf("absolute match = %q, want One", got)
	}
	// Relative path: suffix match.
	if got := one("al/1.flac"); got != "One" {
		t.Errorf("relative suffix match = %q, want One", got)
	}
	// A suffix that anchors at a separator does not match a partial path component.
	if items, err := st.ItemsByPlaylistPath(ctx, "l/1.flac"); err != nil || len(items) != 0 {
		t.Errorf("partial-component suffix = %d items (err %v), want none", len(items), err)
	}
	// Dotted relative entries are cleaned before matching.
	for _, dotted := range []string{"./al/1.flac", "al/./1.flac", "x/../al/1.flac"} {
		if got := one(dotted); got != "One" {
			t.Errorf("dotted path %q = %q, want One", dotted, got)
		}
	}

	// A basename in two folders names both items, for the caller to choose between.
	putTrack(t, st, lib.ID, trackSpec{path: nat("/lib/x/dup.flac"), essence: "ex", content: "cx", title: "Dx", artist: "X", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: nat("/lib/y/dup.flac"), essence: "ey", content: "cy", title: "Dy", artist: "X", album: "Al"})
	if items, err := st.ItemsByPlaylistPath(ctx, "dup.flac"); err != nil || len(items) != 2 {
		t.Errorf("a basename in two folders = %d items (err %v), want both", len(items), err)
	}
}

// TestLikePatternAtTheCapRuns: a pattern of exactly SQLite's 50,000-byte limit runs, and
// a rule one byte over it is refused when saved rather than failing every read.
func TestLikePatternAtTheCapRuns(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "Al"})
	for _, q := range []query.Query{
		query.New(query.EntityItems).Where("title", query.OpContains, strings.Repeat("a", 49998)).Build(),
		query.New(query.EntityItems).Where("title", query.OpStartsWith, strings.Repeat("%", 24999)+"a").Build(),
	} {
		if items, err := st.QueryItems(ctx, q, ""); err != nil || len(items) != 0 {
			t.Errorf("pattern at the cap = %d items (err %v), want none and no error", len(items), err)
		}
	}
	rule := query.New(query.EntityItems).Where("title", query.OpContains, strings.Repeat("a", 49999)).Build()
	if _, err := st.CreatePlaylist(ctx, "Long", "", model.PlaylistSmart, "", &rule); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("saving a rule one byte over the cap: err = %v, want CodeInvalid", err)
	}
}

// TestItemsByPlaylistPathPastTheLikeCap: a relative entry too long for a LIKE pattern
// names no file the catalog can hold, so it names no item rather than failing as I/O.
// SQLite checks the length per row, so the catalog holds a file.
func TestItemsByPlaylistPathPastTheLikeCap(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "Al"})
	if items, err := st.ItemsByPlaylistPath(context.Background(), strings.Repeat("a/", 30000)+"x.flac"); err != nil || len(items) != 0 {
		t.Errorf("60,000-byte relative entry: %d items (err %v), want none and no error", len(items), err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCompactPlaylistRenumbersAnyPositions: whatever distinct positions a playlist
// holds, compaction keeps the order and writes 0..n-1, including shapes where moving an
// entry straight to its index would collide with one not yet moved.
func TestCompactPlaylistRenumbersAnyPositions(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	p := playlistTracks(t, st, lib.ID, "A", "B", "C")
	for _, shape := range [][]int{{0, 1, 2}, {1, 2, 3}, {2, 3, 9}, {-2, -1, 0}, {-3, 4, 10}, {-9, -8, -7}, {5, 6, 7}} {
		pl := staticPlaylist(t, st, "P", p...)
		if _, err := st.write.ExecContext(ctx, `UPDATE playlist_item SET position = position + 1000
			WHERE playlist_id = (SELECT id FROM playlist WHERE pid = ?)`, string(pl)); err != nil {
			t.Fatalf("lift: %v", err)
		}
		for i, pos := range shape {
			if _, err := st.write.ExecContext(ctx, `UPDATE playlist_item SET position = ?
				WHERE playlist_id = (SELECT id FROM playlist WHERE pid = ?) AND position = ?`, pos, string(pl), 1000+i); err != nil {
				t.Fatalf("shape %v: %v", shape, err)
			}
		}
		var moved bool
		if err := st.writeTx(ctx, func(tx *sql.Tx) error {
			var id int64
			if err := tx.QueryRowContext(ctx, "SELECT id FROM playlist WHERE pid = ?", string(pl)).Scan(&id); err != nil {
				return err
			}
			var err error
			moved, err = compactPlaylistTx(ctx, tx, id)
			return err
		}); err != nil {
			t.Fatalf("compact %v: %v", shape, err)
		}
		if want := shape[0] != 0 || shape[2] != 2; moved != want {
			t.Errorf("compact %v: moved = %v, want %v", shape, moved, want)
		}
		assertListing(t, st, pl, "A", "B", "C")
	}
}

// TestSetPlaylistOwnerMovesThePlaylist: a playlist moves to another user, who now sees
// it while its old owner does not, with updated_at moved and one delta; an unknown
// playlist or user is CodeNotFound and the owner it already has changes nothing.
func TestSetPlaylistOwnerMovesThePlaylist(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	pl := staticPlaylist(t, st, "Mine", playlistTracks(t, st, lib.ID, "A")...)
	before, seq := playlistUpdatedAt(t, st, pl), latestSeq(t, st)

	if err := st.SetPlaylistOwner(ctx, pl, bob.PID); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	p, err := st.PlaylistByPID(ctx, pl)
	if err != nil || p.OwnerPID != bob.PID || p.OwnerName != "bob" || p.UpdatedAt <= before {
		t.Fatalf("playlist = %+v (err %v), want bob's with updated_at moved", p, err)
	}
	if n := playlistChanges(t, st, seq)[pl]; n != 1 {
		t.Errorf("deltas = %d, want 1", n)
	}
	visible := func(user model.PID) bool {
		pls, err := st.ListPlaylists(ctx, user)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, p := range pls {
			if p.PID == pl {
				return true
			}
		}
		return false
	}
	if visible("") || !visible(bob.PID) {
		t.Errorf("visible to the old owner %v, to bob %v; want only bob", visible(""), visible(bob.PID))
	}

	seq = latestSeq(t, st)
	if err := st.SetPlaylistOwner(ctx, "nope", bob.PID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown playlist: err = %v, want CodeNotFound", err)
	}
	if err := st.SetPlaylistOwner(ctx, pl, "nobody"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown user: err = %v, want CodeNotFound", err)
	}
	if err := st.SetPlaylistOwner(ctx, pl, bob.PID); err != nil {
		t.Errorf("the owner it has: %v", err)
	}
	if n := len(playlistChanges(t, st, seq)); n != 0 {
		t.Errorf("refusals and a no-op emitted deltas for %d playlists", n)
	}
}

// TestTransferPlaylistsMovesEveryPlaylistOfAUser: every playlist one user owns moves to
// another and the count comes back; a smart rule over per-user state still evaluates for
// whoever reads it, not for its owner.
func TestTransferPlaylistsMovesEveryPlaylistOfAUser(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	p := playlistTracks(t, st, lib.ID, "A", "B")
	static := staticPlaylist(t, st, "Static", p...)
	rule := query.New(query.EntityItems).Where("starred", query.OpIs, 1).Build()
	smart, err := st.CreatePlaylist(ctx, "Starred", "", model.PlaylistSmart, model.VisibilityShared, &rule)
	if err != nil {
		t.Fatalf("smart: %v", err)
	}
	bobs, err := st.CreatePlaylist(ctx, "Bob's", bob.PID, model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("bob's: %v", err)
	}
	if _, err := st.SetStar(ctx, "", p[0], true, nil); err != nil {
		t.Fatalf("star: %v", err)
	}
	starred := func(reader model.PID) []string {
		items, err := st.PlaylistItems(ctx, smart, reader)
		if err != nil {
			t.Fatalf("smart items: %v", err)
		}
		return titlesOf(items)
	}
	seq := latestSeq(t, st)

	n, err := st.TransferPlaylists(ctx, "", bob.PID)
	if err != nil || n != 2 {
		t.Fatalf("transfer = %d (err %v), want 2", n, err)
	}
	for _, pl := range []model.PID{static, smart, bobs} {
		if got, err := st.PlaylistByPID(ctx, pl); err != nil || got.OwnerPID != bob.PID {
			t.Errorf("playlist %s = %+v (err %v), want bob's", pl, got, err)
		}
	}
	deltas := playlistChanges(t, st, seq)
	if deltas[static] != 1 || deltas[smart] != 1 || deltas[bobs] != 0 {
		t.Errorf("deltas = %v, want one each for the two that moved", deltas)
	}
	if got := starred(""); !equalStrings(got, []string{"A"}) {
		t.Errorf("the old owner reads %v, want [A] (the reader's stars)", got)
	}
	if got := starred(bob.PID); len(got) != 0 {
		t.Errorf("bob reads %v, want nothing (bob starred nothing)", got)
	}

	if n, err := st.TransferPlaylists(ctx, bob.PID, bob.PID); err != nil || n != 0 {
		t.Errorf("transfer to the same user = %d (err %v), want 0", n, err)
	}
	if _, err := st.TransferPlaylists(ctx, "nobody", bob.PID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown source: err = %v, want CodeNotFound", err)
	}
	if _, err := st.TransferPlaylists(ctx, bob.PID, "nobody"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown target: err = %v, want CodeNotFound", err)
	}
}

// TestCreatePlaylistWithItemsIsAtomic: a playlist and its entries are written together
// with one create delta, and an entry naming no item leaves no playlist behind.
func TestCreatePlaylistWithItemsIsAtomic(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	p := playlistTracks(t, st, lib.ID, "A", "B")
	seq := latestSeq(t, st)
	pl, err := st.CreatePlaylistWithItems(ctx, "Imported", "", "", []model.PID{p[0], p[1], p[0]})
	if err != nil {
		t.Fatalf("create with items: %v", err)
	}
	assertListing(t, st, pl, "A", "B", "A")
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) != 1 || changes[0].EntityType != "playlist" || changes[0].Op != model.OpCreate {
		t.Errorf("changes = %+v, want one playlist create", changes)
	}

	count := func() int {
		return scalarInt(t, st, "SELECT COUNT(*) FROM playlist")
	}
	before, seq := count(), latestSeq(t, st)
	if _, err := st.CreatePlaylistWithItems(ctx, "Broken", "", "", []model.PID{p[0], "nope"}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("an entry naming no item: err = %v, want CodeNotFound", err)
	}
	if after := count(); after != before {
		t.Errorf("playlists %d -> %d, want the failed import to leave none", before, after)
	}
	if latestSeq(t, st) != seq {
		t.Error("the failed import emitted a delta")
	}
}

// TestDeleteItemCascadeSettlesByItself: a delete handed no batch settles the playlists
// and queues it changed on its own, so no delete path depends on a later step to do it.
func TestDeleteItemCascadeSettlesByItself(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	p := playlistTracks(t, st, lib.ID, "A", "B")
	pl := staticPlaylist(t, st, "P", p[0], p[1], p[0])
	if err := st.SetQueue(ctx, "", []model.PID{p[0]}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	seq := latestSeq(t, st)
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		var id int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE pid = ?", string(p[0])).Scan(&id); err != nil {
			return err
		}
		_, err := deleteItemCascade(ctx, tx, id, nil)
		return err
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertListing(t, st, pl, "B")
	if n := playlistChanges(t, st, seq)[pl]; n != 1 {
		t.Errorf("playlist deltas = %d, want 1", n)
	}
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	queued := 0
	for _, c := range changes {
		if c.EntityType == "play_queue" {
			queued++
		}
	}
	if queued != 1 {
		t.Errorf("queue deltas = %d, want 1", queued)
	}
}

// TestWriteTxRollsBackAPanic: a write whose function panics is rolled back before the
// panic goes on, so the single write connection is free for the next write instead of
// held for good (a server that recovers a request's panic would otherwise hang every
// later write).
func TestWriteTxRollsBackAPanic(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not reach the caller")
			}
		}()
		_ = st.writeTx(ctx, func(*sql.Tx) error { panic("boom") })
	}()
	next, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := st.writeTx(next, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("a write after a panicked one: %v", err)
	}
}

// TestAddPlaylistItemsClosesOldGaps: appending to a playlist whose positions a catalog
// wrote before they were kept dense renumbers it first, so the write leaves positions
// equal to listing indexes.
func TestAddPlaylistItemsClosesOldGaps(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	p := playlistTracks(t, st, lib.ID, "A", "B", "C", "D")
	pl := staticPlaylist(t, st, "P", p[0], p[1], p[2])
	for _, q := range []string{"position + 1000", "(position - 1000) * 3 + 1"} {
		if _, err := st.write.ExecContext(ctx, `UPDATE playlist_item SET position = `+q+`
			WHERE playlist_id = (SELECT id FROM playlist WHERE pid = ?)`, string(pl)); err != nil {
			t.Fatalf("spread the positions: %v", err)
		}
	}
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{p[3]}); err != nil {
		t.Fatalf("add: %v", err)
	}
	assertListing(t, st, pl, "A", "B", "C", "D")
}
