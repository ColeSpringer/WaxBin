package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// tagFixture scans three tracks with custom tags for the tag-query tests:
//
//	A: MOOD=[happy, sad], MYKEY=foo
//	B: MOOD=[sad]
//	C: no custom tags
func tagFixture(t *testing.T) (st *Store, a, b, c model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	ra := putTrackCustom(t, st, lib.ID, "/lib/a.flac", "ea", "ca", "A",
		map[string][]string{"MOOD": {"happy", "sad"}, "MYKEY": {"foo"}}, true)
	rb := putTrackCustom(t, st, lib.ID, "/lib/b.flac", "eb", "cb", "B",
		map[string][]string{"MOOD": {"sad"}}, true)
	rc := putTrackCustom(t, st, lib.ID, "/lib/c.flac", "ec", "cc", "C", nil, true)
	return st, ra.ItemPID, rb.ItemPID, rc.ItemPID
}

// matchTag returns the set of item pids matching one tag condition.
func matchTag(t *testing.T, st *Store, cond query.Cond) map[model.PID]bool {
	t.Helper()
	items, err := st.QueryItems(context.Background(),
		query.New(query.EntityItems).WhereNode(cond).Build(), "")
	if err != nil {
		t.Fatalf("query %+v: %v", cond, err)
	}
	got := map[model.PID]bool{}
	for _, it := range items {
		got[it.PID] = true
	}
	return got
}

func TestQueryTagEqualityAnyMatch(t *testing.T) {
	st, a, b, c := tagFixture(t)
	// Equality is ANY-match over a multi-valued tag: A carries happy among its values.
	got := matchTag(t, st, query.Cond{Field: "tag.MOOD", Op: query.OpIs, Value: "happy"})
	if !got[a] || got[b] || got[c] {
		t.Errorf("tag.MOOD is happy = %v, want only A (%s)", got, a)
	}
	// A lowercased key resolves to the same canonical tag.
	got = matchTag(t, st, query.Cond{Field: "tag.mood", Op: query.OpIs, Value: "sad"})
	if !got[a] || !got[b] || got[c] {
		t.Errorf("tag.mood is sad = %v, want A and B", got)
	}
	// Value equality is case-sensitive (BINARY): Happy does not match happy.
	if got := matchTag(t, st, query.Cond{Field: "tag.MOOD", Op: query.OpIs, Value: "Happy"}); len(got) != 0 {
		t.Errorf("tag.MOOD is Happy (wrong case) matched %v, want none", got)
	}
}

func TestQueryTagPresenceAndMissing(t *testing.T) {
	st, a, b, c := tagFixture(t)
	if got := matchTag(t, st, query.Cond{Field: "tag.MYKEY", Op: query.OpIsPresent}); !got[a] || got[b] || got[c] {
		t.Errorf("tag.MYKEY isPresent = %v, want only A", got)
	}
	// Only C lacks a MOOD tag entirely.
	if got := matchTag(t, st, query.Cond{Field: "tag.MOOD", Op: query.OpIsMissing}); got[a] || got[b] || !got[c] {
		t.Errorf("tag.MOOD isMissing = %v, want only C", got)
	}
}

func TestQueryTagContainsCaseInsensitive(t *testing.T) {
	st, a, _, _ := tagFixture(t)
	// Substring (LIKE) is ASCII-case-insensitive, unlike equality.
	if got := matchTag(t, st, query.Cond{Field: "tag.MYKEY", Op: query.OpContains, Value: "FO"}); !got[a] || len(got) != 1 {
		t.Errorf("tag.MYKEY contains FO = %v, want only A", got)
	}
}

// TestQueryTagIsNotDenyContract is the deny-list contract: `tag.X isNot V` is
// denied iff the value V is present on key X. An item carrying V is excluded, an item
// with a different value or no tag at all matches.
func TestQueryTagIsNotDenyContract(t *testing.T) {
	st, a, b, c := tagFixture(t)
	got := matchTag(t, st, query.Cond{Field: "tag.MOOD", Op: query.OpIsNot, Value: "happy"})
	if got[a] {
		t.Errorf("A carries MOOD=happy, so it must NOT match tag.MOOD isNot happy")
	}
	if !got[b] {
		t.Errorf("B carries only MOOD=sad, so it must match tag.MOOD isNot happy")
	}
	if !got[c] {
		t.Errorf("C carries no MOOD tag, so it must match tag.MOOD isNot happy (untagged is not denied)")
	}
}

// TestSmartPlaylistTagRule confirms a tag.<KEY> rule survives the smart-playlist
// round-trip: it marshals into the stored rule doc, reloads, and evaluates membership
// on read (only the item carrying the value is a member). No store/CLI code is needed
// beyond the query primitive; rules round-trip verbatim.
func TestSmartPlaylistTagRule(t *testing.T) {
	st, a, _, _ := tagFixture(t)
	ctx := context.Background()
	rule := query.New(query.EntityItems).Where("tag.MOOD", query.OpIs, "happy").Build()
	plPID, err := st.CreatePlaylist(ctx, "Happy", "", model.PlaylistSmart, model.VisibilityPrivate, &rule)
	if err != nil {
		t.Fatalf("create smart playlist: %v", err)
	}
	items, err := st.PlaylistItems(ctx, plPID, "")
	if err != nil {
		t.Fatalf("playlist items: %v", err)
	}
	if len(items) != 1 || items[0].PID != a {
		t.Fatalf("smart membership = %v, want only A (%s)", itemTitles(items), a)
	}
}

func TestQueryTagRejectsReservedAndInvalidKeys(t *testing.T) {
	st, _, _, _ := tagFixture(t)
	ctx := context.Background()
	for _, field := range []string{"tag.TITLE", "tag.A=B", "tag."} {
		_, err := st.QueryItems(ctx, query.New(query.EntityItems).
			Where(field, query.OpIs, "x").Build(), "")
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s should be rejected at compile with CodeInvalid, got %v", field, err)
		}
	}
	// The reserved/invalid rejection covers the presence flag paths too.
	for _, op := range []query.Op{query.OpIsPresent, query.OpIsMissing} {
		_, err := st.QueryItems(ctx, query.New(query.EntityItems).
			WherePresence("tag.TITLE", op).Build(), "")
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("tag.TITLE %s should be CodeInvalid, got %v", op, err)
		}
	}
}
