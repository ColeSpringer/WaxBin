package sqlite

import (
	"context"
	"testing"

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
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al"}).ItemPID
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "X", album: "Al"}).ItemPID

	pl, _ := st.CreatePlaylist(ctx, "Dup", "", model.PlaylistStatic, "", nil)
	// A appears twice (positions 0, 2); B once (position 1).
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{a, b, a}); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Remove the single A at position 0; the other A (position 2) survives.
	if err := st.RemovePlaylistItemAt(ctx, pl, 0); err != nil {
		t.Fatalf("remove at: %v", err)
	}
	items, _ := st.PlaylistItems(ctx, pl, "")
	if got := titlesOf(items); !equalStrings(got, []string{"B", "A"}) {
		t.Errorf("after removing position 0 = %v, want [B A]", got)
	}
	// Removing an empty position errors rather than reporting a no-op.
	if err := st.RemovePlaylistItemAt(ctx, pl, 99); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("removing a missing position err = %v, want CodeNotFound", err)
	}
}

func TestRemovePlaylistItemNonMemberIsNoOp(t *testing.T) {
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

func TestItemByPlaylistPathMatching(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/al/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "Al"})

	// Absolute path: exact (indexed) match.
	if it, err := st.ItemByPlaylistPath(ctx, "/lib/al/1.flac"); err != nil || it.Title != "One" {
		t.Errorf("absolute match = %v (err %v), want One", it, err)
	}
	// Relative path: unique suffix match.
	if it, err := st.ItemByPlaylistPath(ctx, "al/1.flac"); err != nil || it.Title != "One" {
		t.Errorf("relative suffix match = %v (err %v), want One", it, err)
	}
	// A suffix that anchors at a separator does not match a partial path component.
	if _, err := st.ItemByPlaylistPath(ctx, "l/1.flac"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("partial-component suffix should not match, err = %v", err)
	}
	// Dotted relative entries are cleaned before matching.
	for _, dotted := range []string{"./al/1.flac", "al/./1.flac", "x/../al/1.flac"} {
		if it, err := st.ItemByPlaylistPath(ctx, dotted); err != nil || it.Title != "One" {
			t.Errorf("dotted path %q = %v (err %v), want One", dotted, it, err)
		}
	}

	// Ambiguous basename across two folders is not guessed.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/x/dup.flac", essence: "ex", content: "cx", title: "Dx", artist: "X", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/y/dup.flac", essence: "ey", content: "cy", title: "Dy", artist: "X", album: "Al"})
	if _, err := st.ItemByPlaylistPath(ctx, "dup.flac"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("ambiguous basename should be CodeNotFound, got %v", err)
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
