package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestSetLibraryFolderFallback: the flag reads back through every library read and
// survives the root being ensured again, a real change emits one library delta and a
// repeat none, and a library whose folders WaxBin lays out itself refuses it: a managed
// one, whose folders render the catalog, and the podcast library.
func TestSetLibraryFolderFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, managed := openStoreAt(t)
	if _, err := st.SetLibraryFolderFallback(ctx, managed.PID, true); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("setting it on a managed library = %v, want CodeInvalid", err)
	}
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte("/inplace"), DisplayRoot: "/inplace",
		Mode: model.ModeInPlace, Profile: "waxbin-native"})
	if err != nil {
		t.Fatal(err)
	}
	seq0, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		got, err := st.SetLibraryFolderFallback(ctx, lib.PID, true)
		if err != nil || got.PID != lib.PID || !got.FolderFallback {
			t.Fatalf("set #%d = %+v (err %v), want the library row with the flag on", i+1, got, err)
		}
	}
	rows, err := st.ChangesSince(ctx, seq0)
	if err != nil || len(rows) != 1 || rows[0].EntityType != "library" || rows[0].Op != model.OpUpdate {
		t.Fatalf("deltas = %+v (err %v), want one library update", rows, err)
	}
	libs, err := st.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range libs {
		if l.FolderFallback != (l.ID == lib.ID) {
			t.Fatalf("libraries = %+v, want the flag read back on the in-place library alone", libs)
		}
	}
	again, err := st.EnsureLibrary(ctx, &model.Library{Root: lib.Root, DisplayRoot: lib.DisplayRoot, Mode: lib.Mode, Profile: lib.Profile})
	if err != nil || !again.FolderFallback {
		t.Fatalf("re-ensured library = %+v (err %v), want the flag kept", again, err)
	}
	if got, err := st.SetLibraryFolderFallback(ctx, lib.PID, false); err != nil || got.FolderFallback {
		t.Fatalf("clearing = %+v (err %v), want the flag off", got, err)
	}

	podID, err := st.EnsurePodcastLibrary(ctx, "/pods")
	if err != nil {
		t.Fatal(err)
	}
	libs, _ = st.Libraries(ctx)
	for _, l := range libs {
		if l.ID == podID {
			if _, err := st.SetLibraryFolderFallback(ctx, l.PID, true); !waxerr.Is(err, waxerr.CodeInvalid) {
				t.Errorf("setting it on the podcast library = %v, want CodeInvalid", err)
			}
		}
	}
	if _, err := st.SetLibraryFolderFallback(ctx, "01NOSUCHLIBRARY000000000000", true); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("an unknown library = %v, want CodeNotFound", err)
	}
}

// TestEnsureLibraryClearsFolderFallbackWhenTheModeLeavesInPlace: the flag belongs to an
// in-place library, so ensuring the same root in another mode clears it. Left alone it
// would read back as on for a library the setter refuses it on, and come back by itself if
// the root were ever flipped to in-place again.
func TestEnsureLibraryClearsFolderFallbackWhenTheModeLeavesInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, _ := openStoreAt(t)
	root := []byte("/flip")
	ensure := func(mode model.Mode) *model.Library {
		t.Helper()
		lib, err := st.EnsureLibrary(ctx, &model.Library{Root: root, DisplayRoot: "/flip", Mode: mode, Profile: "waxbin-native"})
		if err != nil {
			t.Fatalf("ensure as %s: %v", mode, err)
		}
		return lib
	}
	lib := ensure(model.ModeInPlace)
	if _, err := st.SetLibraryFolderFallback(ctx, lib.PID, true); err != nil {
		t.Fatalf("set: %v", err)
	}

	if got := ensure(model.ModeManaged); got.FolderFallback {
		t.Errorf("ensured as managed = %+v, want the folder fallback cleared", got)
	}
	libs, err := st.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range libs {
		if l.ID == lib.ID && l.FolderFallback {
			t.Errorf("libraries = %+v, want the managed library reading the flag off", l)
		}
	}
	if got := ensure(model.ModeInPlace); got.FolderFallback {
		t.Errorf("ensured as in-place again = %+v, want the flag still off", got)
	}
}
