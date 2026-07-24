package waxbin_test

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// TestUserStateQueryThroughFacade exercises the public per-user query surface
// WaxDeck consumes: filtering on user-state fields scopes to the passed user, one
// user's state never leaks to another, and a smart playlist yields per-user
// membership from a single stored rule.
func TestUserStateQueryThroughFacade(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	// Three distinct items (distinct audio payloads so essence does not collapse them).
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "A", Artist: "Alpha", Album: "One", Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "B", Artist: "Beta", Album: "Two", Audio: testaudio.AudioWithSeed(2)}))
	writeFile(t, filepath.Join(root, "c.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "C", Artist: "Gamma", Album: "Three", Audio: testaudio.AudioWithSeed(3)}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	byTitle := map[string]model.PID{}
	all, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	for _, it := range all {
		byTitle[it.Title] = it.PID
	}
	if len(byTitle) != 3 {
		t.Fatalf("want 3 items, got %d", len(byTitle))
	}

	bob, err := lib.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Default user stars A and rates it 90; bob stars B.
	pb := lib.Playback()
	if err := pb.SetStar(ctx, "", byTitle["A"], true, nil); err != nil {
		t.Fatal(err)
	}
	r90 := 90
	if err := pb.SetRating(ctx, "", byTitle["A"], &r90, nil); err != nil {
		t.Fatal(err)
	}
	if err := pb.SetStar(ctx, bob.PID, byTitle["B"], true, nil); err != nil {
		t.Fatal(err)
	}

	starredQ := query.New(query.EntityItems).Where("starred", query.OpIs, 1).Build()

	// The default user's starred set is {A}; bob's is {B}. No leak either way.
	if got := facadeTitles(t, lib, starredQ, ""); strings.Join(got, ",") != "A" {
		t.Errorf("default starred = %v, want [A]", got)
	}
	if got := facadeTitles(t, lib, starredQ, bob.PID); strings.Join(got, ",") != "B" {
		t.Errorf("bob starred = %v, want [B]", got)
	}

	// rating gte 50 is the default user's A, but empty for bob (A is unrated for bob).
	ratedQ := query.New(query.EntityItems).Where("rating", query.OpGte, 50).Build()
	if got := facadeTitles(t, lib, ratedQ, ""); strings.Join(got, ",") != "A" {
		t.Errorf("default rating gte 50 = %v, want [A]", got)
	}
	if got := facadeTitles(t, lib, ratedQ, bob.PID); len(got) != 0 {
		t.Errorf("bob rating gte 50 = %v, want [] (no leak)", got)
	}

	// Count agrees with the per-user filter.
	if n, err := lib.Count(ctx, starredQ, ""); err != nil || n != 1 {
		t.Errorf("default count starred = %d (err %v), want 1", n, err)
	}
	if n, err := lib.Count(ctx, starredQ, bob.PID); err != nil || n != 1 {
		t.Errorf("bob count starred = %d (err %v), want 1", n, err)
	}

	// One smart-playlist rule, evaluated per user, yields each user's own faves.
	plPID, err := lib.Playlists().CreateSmart(ctx, "Faves", "", model.VisibilityShared, starredQ)
	if err != nil {
		t.Fatalf("create smart playlist: %v", err)
	}
	defItems, err := lib.Playlists().Items(ctx, plPID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(defItems) != 1 || defItems[0].Title != "A" {
		t.Errorf("default smart membership = %v, want [A]", facadeItemTitles(defItems))
	}
	bobItems, err := lib.Playlists().Items(ctx, plPID, bob.PID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobItems) != 1 || bobItems[0].Title != "B" {
		t.Errorf("bob smart membership = %v, want [B]", facadeItemTitles(bobItems))
	}
}

// TestPlayStatesForItemsFacade drives the bulk play-state read end to end: the
// facade map agrees with the per-pair State reads, a buffered (unflushed)
// position is overlaid in-process, and untouched items are absent.
func TestPlayStatesForItemsFacade(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "A", Artist: "Alpha", Album: "One", Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "B", Artist: "Beta", Album: "Two", Audio: testaudio.AudioWithSeed(2)}))
	writeFile(t, filepath.Join(root, "c.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "C", Artist: "Gamma", Album: "Three", Audio: testaudio.AudioWithSeed(3)}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	byTitle := map[string]model.PID{}
	all, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	for _, it := range all {
		byTitle[it.Title] = it.PID
	}

	bob, err := lib.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	def, err := lib.DefaultUser(ctx)
	if err != nil {
		t.Fatalf("default user: %v", err)
	}

	pb := lib.Playback()
	if err := pb.SetStar(ctx, def.PID, byTitle["A"], true, nil); err != nil {
		t.Fatal(err)
	}
	if err := pb.SetStar(ctx, bob.PID, byTitle["A"], true, nil); err != nil {
		t.Fatal(err)
	}
	r := 90
	if err := pb.SetRating(ctx, bob.PID, byTitle["B"], &r, nil); err != nil {
		t.Fatal(err)
	}
	// A buffered tick that has NOT been flushed: the bulk read must still see it.
	pb.Progress(bob.PID, byTitle["B"], 123000)

	states, err := lib.PlayStatesForItems(ctx, []model.PID{byTitle["A"], byTitle["B"], byTitle["C"]})
	if err != nil {
		t.Fatalf("PlayStatesForItems: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("states map has %d items, want 2 (untouched C absent): %v", len(states), states)
	}
	if a := states[byTitle["A"]]; len(a) != 2 {
		t.Fatalf("item A states = %+v, want both users", a)
	}
	b := states[byTitle["B"]]
	if len(b) != 1 || b[0].UserPID != bob.PID {
		t.Fatalf("item B states = %+v, want bob only", b)
	}
	if b[0].PositionMS != 123000 {
		t.Errorf("item B position = %d, want the buffered 123000 overlaid", b[0].PositionMS)
	}

	// Each bulk entry agrees with the per-pair State read (the overlay applies to
	// both paths in-process).
	for _, itemPID := range []model.PID{byTitle["A"], byTitle["B"]} {
		for _, s := range states[itemPID] {
			single, err := pb.State(ctx, s.UserPID, itemPID)
			if err != nil {
				t.Fatalf("State(%s,%s): %v", s.UserPID, itemPID, err)
			}
			if single.PositionMS != s.PositionMS || single.Starred != s.Starred ||
				single.HasRating != s.HasRating || single.Rating != s.Rating ||
				single.StarredChangedAt != s.StarredChangedAt || single.RatingChangedAt != s.RatingChangedAt {
				t.Errorf("bulk state %+v disagrees with State %+v", s, single)
			}
		}
	}
}

func facadeTitles(t *testing.T, lib *waxbin.Library, q query.Query, userPID model.PID) []string {
	t.Helper()
	items, err := lib.Query(context.Background(), q, userPID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return facadeItemTitles(items)
}

func facadeItemTitles(items []*model.ItemView) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	sort.Strings(out)
	return out
}
