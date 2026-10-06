package organize

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// holdLib opens a catalog with one managed library at a fresh root.
func holdLib(t *testing.T) (*Organizer, *sqlite.Store, *model.Library) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged, Profile: DefaultProfileName})
	if err != nil {
		t.Fatalf("ensure library: %v", err)
	}
	return New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), st, lib
}

// holdTrack writes a file at rel below the library root and catalogs it as track no of
// album Al by A, titled title, its audio named by essence (a second file of one essence
// is a copy of the first's item).
func holdTrack(t *testing.T, st *sqlite.Store, lib *model.Library, rel, essence, title string, no int) *model.ItemView {
	t.Helper()
	return holdTrackWith(t, st, lib, rel, essence, title, model.Track{Artist: "A", AlbumArtist: "A", Album: "Al", TrackNo: no})
}

// holdTrackWith is holdTrack with the track's tags given whole.
func holdTrackWith(t *testing.T, st *sqlite.Store, lib *model.Library, rel, essence, title string, tr model.Track) *model.ItemView {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(lib.RootPath(), rel)
	mustMkdir(t, filepath.Dir(path))
	mustWrite(t, path)
	res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
		LibraryID: lib.ID,
		File: model.File{Path: []byte(path), DisplayPath: path, RelPath: []byte(rel),
			Kind: model.FileAudio, Size: 1, MTimeNS: 1, DurationMS: 1000,
			ContentHash: "c-" + rel, EssenceHash: essence, ScanState: model.ScanIndexed},
		Item: model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence},
		Track: tr,
	})
	if err != nil {
		t.Fatalf("catalog %s: %v", rel, err)
	}
	it, err := st.ItemByPID(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return it
}

// holdBookPart writes a file at rel and catalogs it as part position of the book Dune by
// Frank Herbert, returning the book's pid.
func holdBookPart(t *testing.T, st *sqlite.Store, lib *model.Library, rel, essence string, position int) model.PID {
	t.Helper()
	path := filepath.Join(lib.RootPath(), rel)
	mustMkdir(t, filepath.Dir(path))
	mustWrite(t, path)
	res, err := st.PutScannedBook(context.Background(), model.PutScannedBookInput{
		LibraryID: lib.ID,
		File: model.File{Path: []byte(path), DisplayPath: path, RelPath: []byte(rel),
			Kind: model.FileAudio, Size: 1, MTimeNS: 1, DurationMS: 1000,
			ContentHash: "c-" + rel, EssenceHash: essence, ScanState: model.ScanIndexed},
		Item: model.PlayableItem{Kind: model.KindBook, State: model.StatePresent, Title: "Dune",
			SortKey: model.SortKey("Dune"), IdentityKey: "book:dune"},
		Book:     model.Book{Author: "Frank Herbert", Authors: []string{"Frank Herbert"}},
		Position: position,
	})
	if err != nil {
		t.Fatalf("catalog %s: %v", rel, err)
	}
	return res.ItemPID
}

func planFor(t *testing.T, o *Organizer, lib *model.Library, p Profile, items ...*model.ItemView) *Plan {
	t.Helper()
	plan, err := o.Plan(context.Background(), lib, p, items, PlanOptions{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan
}

func actionOf(t *testing.T, plan *Plan, item model.PID) Action {
	t.Helper()
	for _, a := range plan.Actions {
		if a.ItemPID == item {
			return a
		}
	}
	t.Fatalf("no action for %s in %+v", item, plan.Actions)
	return Action{}
}

// TestPlanHoldsADestinationAFileTakes: a file the catalog does not know already stands at
// the destination, so the move is held as occupied, naming the file, and nothing would
// move.
func TestPlanHoldsADestinationAFileTakes(t *testing.T) {
	t.Parallel()
	o, st, lib := holdLib(t)
	it := holdTrack(t, st, lib, filepath.Join("in", "song.mp3"), "e-1", "Song", 1)
	dst := filepath.Join(lib.RootPath(), "A", "Al", "01 - Song.mp3")
	mustMkdir(t, filepath.Dir(dst))
	mustWrite(t, dst)

	plan := planFor(t, o, lib, nativeProfile, it)
	if a := actionOf(t, plan, it.PID); !a.Skip || a.Code != HoldOccupied || !strings.Contains(a.Reason, dst) {
		t.Fatalf("action = %+v, want held occupied naming %s", a, dst)
	}
	if plan.Pending() != 0 || plan.Held() != 1 {
		t.Errorf("pending %d, held %d; want 0 and 1", plan.Pending(), plan.Held())
	}
}

// TestPlanHoldsADestinationAnItemInPlaceKeeps: an item already at its destination keeps
// it, so another item rendering to that path collides with it even when the other is
// planned first.
func TestPlanHoldsADestinationAnItemInPlaceKeeps(t *testing.T) {
	t.Parallel()
	o, st, lib := holdLib(t)
	there := holdTrack(t, st, lib, filepath.Join("A", "Al", "01 - Song.mp3"), "e-1", "Song", 1)
	other := holdTrack(t, st, lib, filepath.Join("in", "song.mp3"), "e-2", "Song", 1)

	plan := planFor(t, o, lib, nativeProfile, other, there)
	if a := actionOf(t, plan, there.PID); !a.Skip || a.Code != HoldInPlace {
		t.Errorf("item in place = %+v, want in-place", a)
	}
	if a := actionOf(t, plan, other.PID); !a.Skip || a.Code != HoldCollision || !strings.Contains(a.Reason, there.DisplayPath) {
		t.Errorf("other item = %+v, want held as a collision with %s", a, there.DisplayPath)
	}
	if plan.Pending() != 0 || plan.Held() != 1 {
		t.Errorf("pending %d, held %d; want 0 and 1 (the item in place is not held)", plan.Pending(), plan.Held())
	}
}

// TestPlanHoldsADestinationTheCatalogHolds: a destination holding this item's own other
// copy, a cataloged file that backs no item, or the path of another item's file gone from
// disk (which the catalog's one row per path would refuse at commit) is held as occupied,
// saying which.
func TestPlanHoldsADestinationTheCatalogHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dstRel := filepath.Join("A", "Al", "01 - Song.mp3")
	for _, tc := range []struct {
		name, says string
		occupy     func(t *testing.T, st *sqlite.Store, lib *model.Library)
	}{
		{"a copy of this item", "this item", func(t *testing.T, st *sqlite.Store, lib *model.Library) {
			holdTrack(t, st, lib, dstRel, "e-1", "Song", 1)
		}},
		{"a file of no item", "no item", func(t *testing.T, st *sqlite.Store, lib *model.Library) {
			path := filepath.Join(lib.RootPath(), dstRel)
			mustMkdir(t, filepath.Dir(path))
			mustWrite(t, path)
			if _, err := st.PutScannedVirtualTracks(ctx, model.PutScannedVirtualTracksInput{LibraryID: lib.ID,
				File: model.File{Path: []byte(path), DisplayPath: path, RelPath: []byte(dstRel),
					Kind: model.FileAudio, Size: 1, MTimeNS: 1, DurationMS: 1000,
					ContentHash: "c-rip", EssenceHash: "e-rip", ScanState: model.ScanIndexed}}); err != nil {
				t.Fatalf("catalog an ownerless file: %v", err)
			}
		}},
		{"another item's file gone from disk", "not on disk", func(t *testing.T, st *sqlite.Store, lib *model.Library) {
			holdTrack(t, st, lib, dstRel, "e-9", "Other", 7)
			if err := os.Remove(filepath.Join(lib.RootPath(), dstRel)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, st, lib := holdLib(t)
			it := holdTrack(t, st, lib, filepath.Join("in", "song.mp3"), "e-1", "Song", 1)
			tc.occupy(t, st, lib)
			if it, _ = st.ItemByPID(ctx, it.PID); it.DisplayPath != filepath.Join(lib.RootPath(), "in", "song.mp3") {
				t.Fatalf("the item's primary moved to %s", it.DisplayPath)
			}
			a := actionOf(t, planFor(t, o, lib, nativeProfile, it), it.PID)
			dst := filepath.Join(lib.RootPath(), dstRel)
			if !a.Skip || a.Code != HoldOccupied || !strings.Contains(a.Reason, dst) || !strings.Contains(a.Reason, tc.says) {
				t.Fatalf("action = %+v, want held occupied naming %s and saying %q", a, dst, tc.says)
			}
		})
	}
}

// TestPlanHoldsADestinationACaseSiblingShadows: a file whose name differs from the
// destination's only by case stands beside it, which a filesystem that ignores case takes
// for the destination itself, so the move is held as occupied-case wherever it runs.
func TestPlanHoldsADestinationACaseSiblingShadows(t *testing.T) {
	t.Parallel()
	o, st, lib := holdLib(t)
	it := holdTrack(t, st, lib, filepath.Join("in", "song.mp3"), "e-1", "Song", 1)
	sib := filepath.Join(lib.RootPath(), "A", "Al", "01 - SONG.mp3")
	mustMkdir(t, filepath.Dir(sib))
	mustWrite(t, sib)

	if a := actionOf(t, planFor(t, o, lib, nativeProfile, it), it.PID); !a.Skip || a.Code != HoldOccupiedCase || !strings.Contains(a.Reason, sib) {
		t.Fatalf("action = %+v, want held occupied-case naming %s", a, sib)
	}
}

// TestPlanLeavesAMoveOntoItsOwnEntryFree: a destination that is the source's own entry
// under another spelling holds nothing in the way, so neither a case-only rename nor the
// same file reached through a linked folder is held.
func TestPlanLeavesAMoveOntoItsOwnEntryFree(t *testing.T) {
	t.Parallel()
	t.Run("a case-only rename", func(t *testing.T) {
		t.Parallel()
		o, st, lib := holdLib(t)
		it := holdTrack(t, st, lib, filepath.Join("A", "Al", "01 - song.mp3"), "e-1", "Song", 1)
		plan := planFor(t, o, lib, nativeProfile, it)
		if a := actionOf(t, plan, it.PID); a.Skip || plan.Pending() != 1 {
			t.Fatalf("action = %+v, want the rename to its own name pending", a)
		}
	})
	t.Run("a linked folder", func(t *testing.T) {
		t.Parallel()
		o, st, lib := holdLib(t)
		real := filepath.Join(lib.RootPath(), "Al")
		mustMkdir(t, real)
		if err := os.Symlink(real, filepath.Join(lib.RootPath(), "link")); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		it := holdTrack(t, st, lib, filepath.Join("link", "Song.mp3"), "e-1", "Song", 1)
		flat := Profile{Name: "flat", Music: "{album}/{title}.{ext}", Audiobook: nativeProfile.Audiobook, Podcast: nativeProfile.Podcast}
		plan := planFor(t, o, lib, flat, it)
		if a := actionOf(t, plan, it.PID); a.Skip || a.Dst != filepath.Join(real, "Song.mp3") {
			t.Fatalf("action = %+v, want the move onto the file's own entry pending", a)
		}
	})
}

// TestPlanFreesADestinationAMoveVacates: a destination a move in the plan vacates is free,
// as when two tracks trade names, and stays taken once that move is held.
func TestPlanFreesADestinationAMoveVacates(t *testing.T) {
	t.Parallel()
	t.Run("a swap", func(t *testing.T) {
		t.Parallel()
		o, st, lib := holdLib(t)
		one := holdTrack(t, st, lib, filepath.Join("A", "Al", "02 - Two.mp3"), "e-1", "One", 1)
		two := holdTrack(t, st, lib, filepath.Join("A", "Al", "01 - One.mp3"), "e-2", "Two", 2)
		if plan := planFor(t, o, lib, nativeProfile, one, two); plan.Pending() != 2 {
			t.Fatalf("plan = %+v, want both moves pending", plan.Actions)
		}
	})
	t.Run("a chain whose last move is held", func(t *testing.T) {
		t.Parallel()
		o, st, lib := holdLib(t)
		one := holdTrack(t, st, lib, filepath.Join("A", "Al", "02 - Two.mp3"), "e-1", "One", 1)
		three := holdTrack(t, st, lib, filepath.Join("A", "Al", "01 - One.mp3"), "e-3", "Three", 3)
		mustWrite(t, filepath.Join(lib.RootPath(), "A", "Al", "03 - Three.mp3"))
		plan := planFor(t, o, lib, nativeProfile, one, three)
		if a := actionOf(t, plan, three.PID); a.Code != HoldOccupied {
			t.Errorf("last move = %+v, want held occupied", a)
		}
		if a := actionOf(t, plan, one.PID); a.Code != HoldCollision || !strings.Contains(a.Reason, three.DisplayPath) {
			t.Errorf("first move = %+v, want held as a collision with %s, which stays", a, three.DisplayPath)
		}
	})
}

// TestPlanHoldsEveryPartOfABookOnePartOfWhichIsHeld: a book moves whole or not at all, so
// a part whose destination is taken holds the others too.
func TestPlanHoldsEveryPartOfABookOnePartOfWhichIsHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, st, lib := holdLib(t)
	book := holdBookPart(t, st, lib, filepath.Join("in", "dune1.mp3"), "e-1", 1)
	holdBookPart(t, st, lib, filepath.Join("in", "dune2.mp3"), "e-2", 2)
	taken := filepath.Join(lib.RootPath(), "Frank Herbert", "Dune", "Dune - 02.mp3")
	mustMkdir(t, filepath.Dir(taken))
	mustWrite(t, taken)
	view, err := st.ItemByPID(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	plan := planFor(t, o, lib, nativeProfile, view)
	if len(plan.Actions) != 2 {
		t.Fatalf("plan = %+v, want one action per part", plan.Actions)
	}
	for _, a := range plan.Actions {
		want := HoldBookHeld
		if a.Dst == taken {
			want = HoldOccupied
		}
		if !a.Skip || a.Code != want {
			t.Errorf("part %s = %+v, want held %s", a.Src, a, want)
		}
	}
}

// TestExecuteCountsHoldsApartFromWhatIsInPlace: an applied plan reports its held actions
// as held and those already in place as skipped.
func TestExecuteCountsHoldsApartFromWhatIsInPlace(t *testing.T) {
	t.Parallel()
	plan := &Plan{Actions: []Action{
		{Src: "/lib/a.mp3", Dst: "/lib/a.mp3", Skip: true, Code: HoldInPlace},
		{Src: "/lib/b.mp3", Dst: "/lib/c.mp3", Skip: true, Code: HoldOccupied},
		{Src: "/lib/d.mp3", Dst: "/lib/e.mp3", Skip: true, Code: HoldReadOnly},
	}}
	rep, err := New(nil, nil, nil).Execute(context.Background(), plan, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.Held != 2 || rep.Moved != 0 || rep.Errored != 0 {
		t.Fatalf("report = %+v, want one skipped and two held", rep)
	}
	if len(rep.Holds) != 2 || rep.Holds[0].Src != "/lib/b.mp3" || rep.Holds[0].Code != HoldOccupied || rep.Holds[1].Code != HoldReadOnly {
		t.Errorf("held moves = %+v, want b.mp3 occupied and d.mp3 read-only", rep.Holds)
	}
}

// TestPlanHoldsAFileInPlaceThatIsMissing: a file already at its destination but gone from
// disk is missing, not in place.
func TestPlanHoldsAFileInPlaceThatIsMissing(t *testing.T) {
	t.Parallel()
	o, st, lib := holdLib(t)
	it := holdTrack(t, st, lib, filepath.Join("A", "Al", "01 - Song.mp3"), "e-1", "Song", 1)
	if err := os.Remove(it.DisplayPath); err != nil {
		t.Fatal(err)
	}
	if a := actionOf(t, planFor(t, o, lib, nativeProfile, it), it.PID); a.Code != HoldMissing {
		t.Errorf("gone file in place = %+v, want held missing", a)
	}
}

// TestPlanGivesADestinationBackWhenItsClaimantIsHeld: a move that lost its destination to
// a book part claims it once that part is held with its book, since nothing else will
// move there.
func TestPlanGivesADestinationBackWhenItsClaimantIsHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, st, lib := holdLib(t)
	book := holdBookPart(t, st, lib, filepath.Join("in", "dune1.mp3"), "e-1", 1)
	holdBookPart(t, st, lib, filepath.Join("in", "dune2.mp3"), "e-2", 2)
	x := holdTrackWith(t, st, lib, filepath.Join("in", "x.mp3"), "e-3", "Dune - 02",
		model.Track{Artist: "Frank Herbert", AlbumArtist: "Frank Herbert", Album: "Dune"})
	taken := filepath.Join(lib.RootPath(), "Frank Herbert", "Dune", "Dune - 01.mp3")
	mustMkdir(t, filepath.Dir(taken))
	mustWrite(t, taken)
	view, err := st.ItemByPID(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	flat := Profile{Name: "flat", Music: "{albumartist}/{album}/{title}.{ext}", Audiobook: nativeProfile.Audiobook, Podcast: nativeProfile.Podcast}
	p, err := o.Plan(ctx, lib, flat, []*model.ItemView{view, x}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a := actionOf(t, p, x.PID); a.Skip || a.Dst != filepath.Join(lib.RootPath(), "Frank Herbert", "Dune", "Dune - 02.mp3") {
		t.Errorf("track = %+v, want it moved to the place the held book gave up", a)
	}
}

// TestReopenLetsTheDiskDecideAgain: a plan's holds that depend on the disk or catalog are
// cleared for a later look, a move already in place stays so, and an untagged item, a cue
// rip or a book part with no destination (a part of its book outside the library) stays
// held.
func TestReopenLetsTheDiskDecideAgain(t *testing.T) {
	t.Parallel()
	plan := &Plan{Actions: []Action{
		{Src: "/lib/a.mp3", Dst: "/lib/b.mp3", Skip: true, Code: HoldOccupied, Reason: "r"},
		{Src: "/lib/c.mp3", Dst: "/lib/c.mp3", Skip: true, Code: HoldMissing, Reason: "r"},
		{Src: "/lib/d.mp3", Dst: "/lib/e.mp3", Skip: true, Code: HoldUntagged, Reason: "r"},
		{Src: "/lib/f.wav", Skip: true, Code: HoldVirtual, Reason: "r"},
		{Src: "/lib/g.mp3", Dst: "/lib/h.mp3", Skip: true, Code: HoldBookHeld, Reason: "r"},
		{Src: "/lib/i.mp3", Skip: true, Code: HoldBookHeld, Reason: "a part of its book is outside this library"},
	}}
	plan.Reopen()
	want := []HoldCode{"", HoldInPlace, HoldUntagged, HoldVirtual, "", HoldBookHeld}
	for i, a := range plan.Actions {
		if a.Code != want[i] || a.Skip != (want[i] != "") {
			t.Errorf("action %s = %+v, want code %q", a.Src, a, want[i])
		}
	}
}

// TestExecuteFailsAMoveRacedToItsDestination: a file landing on a destination after the
// plan's last look at it fails that move, leaving the source and its catalog row where
// they were.
func TestExecuteFailsAMoveRacedToItsDestination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, st, lib := holdLib(t)
	it := holdTrack(t, st, lib, filepath.Join("in", "song.mp3"), "e-1", "Song", 1)
	plan := planFor(t, o, lib, nativeProfile, it)
	if plan.Pending() != 1 {
		t.Fatalf("plan = %+v, want the move pending", plan.Actions)
	}
	mustMkdir(t, filepath.Dir(plan.Actions[0].Dst))
	mustWrite(t, plan.Actions[0].Dst)

	rep, err := o.Execute(ctx, plan, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Moved != 0 || rep.Errored != 1 {
		t.Fatalf("report = %+v, want the move errored", rep)
	}
	if _, err := os.Stat(it.DisplayPath); err != nil {
		t.Errorf("the source left: %v", err)
	}
	if _, err := st.FileByPath(ctx, it.Path); err != nil {
		t.Errorf("the catalog lost the source: %v", err)
	}
}

// TestPlanHoldsAnUntaggedItem: an item with no artist, album artist or album would be
// filed under the unknown buckets with every other such item, so it is held, unless its
// layout files it by what it does carry.
func TestPlanHoldsAnUntaggedItem(t *testing.T) {
	t.Parallel()
	o, st, lib := holdLib(t)
	solo := holdTrackWith(t, st, lib, filepath.Join("in", "solo.flac"), "e-1", "Solo", model.Track{TrackNo: 3})
	if a := actionOf(t, planFor(t, o, lib, nativeProfile, solo), solo.PID); !a.Skip || a.Code != HoldUntagged {
		t.Errorf("untagged item = %+v, want held untagged", a)
	}
	byTitle := Profile{Name: "by-title", Music: "{title}.{ext}", Audiobook: nativeProfile.Audiobook, Podcast: nativeProfile.Podcast}
	if a := actionOf(t, planFor(t, o, lib, byTitle, solo), solo.PID); a.Skip || a.RelDst != "Solo.flac" {
		t.Errorf("untagged item by title = %+v, want moved to Solo.flac", a)
	}
	album := holdTrackWith(t, st, lib, filepath.Join("in", "two.flac"), "e-2", "Two", model.Track{Album: "Al", TrackNo: 2})
	if a := actionOf(t, planFor(t, o, lib, nativeProfile, album), album.PID); a.Skip {
		t.Errorf("an item with an album = %+v, want it moved", a)
	}
}

// TestPlanFilesAnItemUnderItsEntitysSpelling: a path takes the catalog's spelling of the
// item's artist, album artist and author where the item's own text is that name in
// another spelling, and its album's title; a joint credit, whose entity is only its first
// name, keeps its own text.
func TestPlanFilesAnItemUnderItsEntitysSpelling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, st, lib := holdLib(t)
	names := func(it *model.ItemView, artist, album string) PlanOptions {
		return PlanOptions{Names: map[model.PID]string{it.ArtistPID: artist, it.AlbumArtistPID: artist, it.AlbumPID: album}}
	}
	plan := func(it *model.ItemView, opts PlanOptions) Action {
		t.Helper()
		p, err := o.Plan(ctx, lib, nativeProfile, []*model.ItemView{it}, opts)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		return actionOf(t, p, it.PID)
	}

	obe := holdTrackWith(t, st, lib, filepath.Join("in", "two.mp3"), "e-1", "Two",
		model.Track{Artist: "Amir Obè", AlbumArtist: "Amir Obè", Album: "NIGHT!", TrackNo: 2})
	if a := plan(obe, names(obe, "Amir Obe", "Night")); a.RelDst != filepath.Join("Amir Obe", "Night", "02 - Two.mp3") {
		t.Errorf("respelled track = %s, want it under the catalog's spellings", a.RelDst)
	}
	byArtist := Profile{Name: "by-artist", Music: "{artist}/{title}.{ext}", Audiobook: nativeProfile.Audiobook, Podcast: nativeProfile.Podcast}
	p, err := o.Plan(ctx, lib, byArtist, []*model.ItemView{obe}, names(obe, "Amir Obe", "Night"))
	if err != nil {
		t.Fatal(err)
	}
	if a := actionOf(t, p, obe.PID); a.RelDst != filepath.Join("Amir Obe", "Two.mp3") {
		t.Errorf("a layout by {artist} = %s, want the catalog's spelling there too", a.RelDst)
	}
	joint := holdTrackWith(t, st, lib, filepath.Join("in", "duet.mp3"), "e-2", "Duet",
		model.Track{Artist: "Amir Obe & Friend", AlbumArtist: "Amir Obe & Friend", Album: "Duets", TrackNo: 1})
	if joint.AlbumArtistPID == "" {
		t.Fatal("the joint credit resolved no album artist")
	}
	if a := plan(joint, names(joint, "Amir Obe", "Duets")); a.RelDst != filepath.Join("Amir Obe & Friend", "Duets", "01 - Duet.mp3") {
		t.Errorf("joint credit = %s, want its own text kept", a.RelDst)
	}

	book := holdBookPart(t, st, lib, filepath.Join("in", "dune.mp3"), "e-3", 0)
	view, err := st.ItemByPID(ctx, book)
	if err != nil || view.ArtistPID == "" {
		t.Fatalf("book = %+v (err %v), want its author resolved", view, err)
	}
	opts := PlanOptions{Names: map[model.PID]string{view.ArtistPID: "FRANK HERBERT"}}
	if a := plan(view, opts); a.RelDst != filepath.Join("FRANK HERBERT", "Dune", "Dune.mp3") {
		t.Errorf("book = %s, want it under the author's catalog spelling", a.RelDst)
	}
}

// TestPlanHoldsAnItemWhoseFileIsMissing: a file gone from disk cannot move, so its item is
// held as missing, and a book missing one part holds the rest.
func TestPlanHoldsAnItemWhoseFileIsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, st, lib := holdLib(t)
	gone := holdTrack(t, st, lib, filepath.Join("in", "gone.mp3"), "e-1", "Gone", 1)
	if err := os.Remove(gone.DisplayPath); err != nil {
		t.Fatal(err)
	}
	if a := actionOf(t, planFor(t, o, lib, nativeProfile, gone), gone.PID); !a.Skip || a.Code != HoldMissing {
		t.Errorf("gone file = %+v, want held missing", a)
	}

	book := holdBookPart(t, st, lib, filepath.Join("in", "dune1.mp3"), "e-2", 1)
	holdBookPart(t, st, lib, filepath.Join("in", "dune2.mp3"), "e-3", 2)
	if err := os.Remove(filepath.Join(lib.RootPath(), "in", "dune2.mp3")); err != nil {
		t.Fatal(err)
	}
	view, err := st.ItemByPID(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range planFor(t, o, lib, nativeProfile, view).Actions {
		want := HoldBookHeld
		if filepath.Base(a.Src) == "dune2.mp3" {
			want = HoldMissing
		}
		if !a.Skip || a.Code != want {
			t.Errorf("part %s = %+v, want held %s", a.Src, a, want)
		}
	}
}

// TestPlanHoldsABookWithAPartOutsideTheLibrary: a book one part of which lies outside the
// library cannot move whole, so its parts here are held, naming the part, rather than left
// out of the plan unseen.
func TestPlanHoldsABookWithAPartOutsideTheLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, st, lib := holdLib(t)
	otherRoot := t.TempDir()
	other, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(otherRoot), DisplayRoot: otherRoot, Mode: model.ModeManaged, Profile: DefaultProfileName})
	if err != nil {
		t.Fatal(err)
	}
	book := holdBookPart(t, st, lib, filepath.Join("in", "dune1.mp3"), "e-1", 1)
	holdBookPart(t, st, other, filepath.Join("in", "dune2.mp3"), "e-2", 2)
	view, err := st.ItemByPID(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(otherRoot, "in", "dune2.mp3")
	plan := planFor(t, o, lib, nativeProfile, view)
	if len(plan.Actions) != 1 {
		t.Fatalf("plan = %+v, want the part in this library, held", plan.Actions)
	}
	if a := plan.Actions[0]; !a.Skip || a.Code != HoldBookHeld || !strings.Contains(a.Reason, outside) {
		t.Errorf("part = %+v, want held book-held naming %s", a, outside)
	}
}

// holdBookOf writes a file at rel and catalogs it as part position of a book titled Dune by
// Frank Herbert under key, returning the book's pid.
func holdBookOf(t *testing.T, st *sqlite.Store, lib *model.Library, key, rel, essence string, position int) model.PID {
	t.Helper()
	path := filepath.Join(lib.RootPath(), rel)
	mustMkdir(t, filepath.Dir(path))
	mustWrite(t, path)
	res, err := st.PutScannedBook(context.Background(), model.PutScannedBookInput{
		LibraryID: lib.ID,
		File: model.File{Path: []byte(path), DisplayPath: path, RelPath: []byte(rel),
			Kind: model.FileAudio, Size: 1, MTimeNS: 1, DurationMS: 1000,
			ContentHash: "c-" + rel, EssenceHash: essence, ScanState: model.ScanIndexed},
		Item: model.PlayableItem{Kind: model.KindBook, State: model.StatePresent, Title: "Dune",
			SortKey: model.SortKey("Dune"), IdentityKey: key},
		Book:     model.Book{Author: "Frank Herbert", Authors: []string{"Frank Herbert"}},
		Position: position,
	})
	if err != nil {
		t.Fatalf("catalog %s: %v", rel, err)
	}
	return res.ItemPID
}

// TestPlanNeverSplitsABookWhenAHoldIsLifted: a book part that lost its destination to a
// part of another book, held with its own book later, does not move while the part its
// first hold held stays put; a book moves whole or not at all.
func TestPlanNeverSplitsABookWhenAHoldIsLifted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, st, lib := holdLib(t)
	other := holdBookOf(t, st, lib, "book:dune-c", filepath.Join("c", "two.mp3"), "e-c2", 2)
	holdBookOf(t, st, lib, "book:dune-c", filepath.Join("c", "three.mp3"), "e-c3", 3)
	book := holdBookOf(t, st, lib, "book:dune-b", filepath.Join("b", "one.mp3"), "e-b1", 1)
	holdBookOf(t, st, lib, "book:dune-b", filepath.Join("b", "two.mp3"), "e-b2", 2)
	taken := filepath.Join(lib.RootPath(), "Frank Herbert", "Dune - 03.mp3")
	mustMkdir(t, filepath.Dir(taken))
	mustWrite(t, taken)
	flat := Profile{Name: "flat", Music: nativeProfile.Music, Audiobook: "{author}/{title}.{ext}", Podcast: nativeProfile.Podcast}
	var views []*model.ItemView
	for _, pid := range []model.PID{other, book} {
		v, err := st.ItemByPID(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, v)
	}
	plan, err := o.Plan(ctx, lib, flat, views, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	moving := map[bool]int{}
	for _, a := range plan.Actions {
		if a.ItemPID == book {
			moving[!a.Skip]++
		}
	}
	if moving[true] != 0 && moving[false] != 0 {
		t.Fatalf("book parts = %+v, want them all moving or all held", plan.Actions)
	}
}

// countingCatalog counts the catalog reads a plan's hold pass makes by path.
type countingCatalog struct {
	model.Catalog
	byPath, byPaths int
}

func (c *countingCatalog) FileByPath(ctx context.Context, path []byte) (*model.File, error) {
	c.byPath++
	return c.Catalog.FileByPath(ctx, path)
}

func (c *countingCatalog) FilePIDsByPath(ctx context.Context, paths [][]byte) (map[string]model.PID, error) {
	c.byPaths++
	return c.Catalog.FilePIDsByPath(ctx, paths)
}

// TestHoldReadsTheCatalogOnceForItsDestinations: the hold pass reads what the catalog
// holds at the plan's destinations in one go, not once per move.
func TestHoldReadsTheCatalogOnceForItsDestinations(t *testing.T) {
	t.Parallel()
	_, st, lib := holdLib(t)
	var items []*model.ItemView
	for i, title := range []string{"One", "Two", "Three"} {
		items = append(items, holdTrack(t, st, lib, filepath.Join("in", title+".mp3"), "e-"+title, title, i+1))
	}
	cat := &countingCatalog{Catalog: st}
	plan, err := New(cat, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).Plan(context.Background(), lib, nativeProfile, items, PlanOptions{})
	if err != nil || plan.Pending() != 3 {
		t.Fatalf("plan = %+v (err %v), want three moves", plan, err)
	}
	if cat.byPath != 0 || cat.byPaths != 1 {
		t.Errorf("catalog reads by path = %d single, %d batched; want none single and one batched", cat.byPath, cat.byPaths)
	}
}
