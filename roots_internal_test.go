package waxbin

import (
	"context"
	"testing"
	"time"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestRemoveRootRefusesThePodcastLibraryAndWaitsForTheFsLease: the podcast library
// follows its config and is never removed this way, and a removal while a scan or
// another filesystem mutator holds the lease is a conflict that leaves the library in
// place.
func TestRemoveRootRefusesThePodcastLibraryAndWaitsForTheFsLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, _, _ := ownershipFixture(t)
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var music, pod model.PID
	for _, l := range libs {
		if l.Mode == model.ModePodcast {
			pod = l.PID
		} else {
			music = l.PID
		}
	}
	if music == "" || pod == "" {
		t.Fatalf("libraries = %+v, want a music and a podcast library", libs)
	}
	if _, err := lib.RemoveRoot(ctx, pod, RemoveRootOptions{Force: true}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("remove the podcast library = %v, want CodeInvalid", err)
	}

	holdScope(t, lib, fsMutateScope)
	if _, err := lib.RemoveRoot(ctx, music, RemoveRootOptions{Force: true}); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("remove during a scan = %v, want CodeConflict", err)
	}
	if after, err := lib.Libraries(ctx); err != nil || len(after) != 2 {
		t.Errorf("libraries = %+v (err %v), want both kept", after, err)
	}
}

// TestRelocateRootWaitsForTheFsLease: a relocation while an organize or another
// filesystem mutator holds the lease is a conflict that leaves the root where it was,
// since the running pass reads and writes paths under it.
func TestRelocateRootWaitsForTheFsLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, _, _ := ownershipFixture(t)
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var music *model.Library
	for _, l := range libs {
		if l.Mode != model.ModePodcast {
			music = l
		}
	}
	holdScope(t, lib, fsMutateScope)
	if err := lib.RelocateRoot(ctx, music.PID, t.TempDir()); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("relocate during an organize = %v, want CodeConflict", err)
	}
	after, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range after {
		if l.PID == music.PID && l.DisplayRoot != music.DisplayRoot {
			t.Errorf("root moved to %s during a refused relocation", l.DisplayRoot)
		}
	}
}

// TestAddRootWaitsOutARemovalOfItsPath: while a library is being removed, re-adding its
// path is a conflict rather than a row the removal is about to delete, and any other root
// is added at once, since the removal holds no lock that would make it wait.
func TestAddRootWaitsOutARemovalOfItsPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, _, _ := ownershipFixture(t)
	gone := t.TempDir()
	added, err := lib.AddRoot(ctx, config.Root{Path: gone, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.beginRemoval(ctx, "test", added.PID, false); err != nil {
		t.Fatalf("begin removal: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := lib.AddRoot(ctx, config.Root{Path: t.TempDir(), Mode: model.ModeInPlace})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("another root during a removal = %v, want it added", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adding another root waited on the removal")
	}
	if _, err := lib.AddRoot(ctx, config.Root{Path: gone, Mode: model.ModeInPlace}); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("re-adding the path being removed = %v, want CodeConflict", err)
	}
	lib.endRemoval(added.PID, nil)
	if _, err := lib.AddRoot(ctx, config.Root{Path: gone, Mode: model.ModeInPlace}); err != nil {
		t.Errorf("re-adding the path once the removal ended = %v", err)
	}
}
