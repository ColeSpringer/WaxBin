package sqlite_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// openEmptyStore opens a catalog with no library registered, so a test can control
// exactly which roots exist.
func openEmptyStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{
		Path: filepath.Join(t.TempDir(), "catalog.db"), Owner: "test",
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// A Windows root retyped in a different case names the same directory, so it has to
// find the library already registered over that tree rather than adding a second one.
// The stored spelling is what every file.path was built from, so it must not move.
func TestEnsureLibraryFoldsRootCaseOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("path case folding is a Windows filesystem property")
	}
	ctx := context.Background()
	st := openEmptyStore(t)

	first, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(`C:\Music`), DisplayRoot: `C:\Music`,
		Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("first EnsureLibrary: %v", err)
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}

	second, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(`c:\music`), DisplayRoot: `c:\music`,
		Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("re-cased EnsureLibrary: %v", err)
	}
	if second.PID != first.PID {
		t.Fatalf("re-cased root got pid %q, want the first library's %q", second.PID, first.PID)
	}
	if string(second.Root) != `C:\Music` || second.DisplayRoot != `C:\Music` {
		t.Errorf("root/display_root = %q/%q, want the first-registered spelling",
			second.Root, second.DisplayRoot)
	}
	libs, err := st.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 1 {
		t.Fatalf("catalog holds %d libraries, want 1", len(libs))
	}

	// The re-open must be silent. The exact-match branch guards on DisplayRoot, which
	// differs by definition on a fold match, so without its own guard this branch would
	// re-spell the row and append a delta on every single open.
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("re-cased re-open emitted %d change deltas, want none: %+v", len(changes), changes)
	}
}

// The fold is gated on pathx.FoldsCase, a build-tag constant, not on what the
// filesystem under the test actually does. Off Windows it is false, so two spellings
// stay two libraries even on a stock Mac, whose APFS volume does fold. That is
// deliberate: the store's rule has to match pathx.SamePath's, and pathx cannot fold on
// darwin without folding for every POSIX caller. The audit's library_conflict check is
// what reports the collision there.
func TestEnsureLibraryKeepsRootBytesExactOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pins the non-folding branch of pathx.FoldsCase")
	}
	ctx := context.Background()
	st := openEmptyStore(t)

	first, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/Music"), DisplayRoot: "/Music", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("first EnsureLibrary: %v", err)
	}
	second, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/music"), DisplayRoot: "/music", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("second EnsureLibrary: %v", err)
	}
	if second.PID == first.PID {
		t.Fatalf("/music matched /Music; raw path bytes are identity off Windows")
	}
	libs, err := st.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 2 {
		t.Fatalf("catalog holds %d libraries, want 2", len(libs))
	}
}

// A fold match still refreshes the policy fields, and says so in the change log.
func TestEnsureLibraryFoldRefreshesPolicyOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("path case folding is a Windows filesystem property")
	}
	ctx := context.Background()
	st := openEmptyStore(t)

	first, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(`C:\Music`), DisplayRoot: `C:\Music`,
		Mode: model.ModeManaged, Media: model.MediaMusic, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("first EnsureLibrary: %v", err)
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(`c:\music`), DisplayRoot: `c:\music`,
		Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("re-cased EnsureLibrary: %v", err)
	}
	if second.PID != first.PID || second.MediaType() != model.MediaAudiobook {
		t.Fatalf("re-cased row = pid %q media %q, want pid %q media audiobook",
			second.PID, second.MediaType(), first.PID)
	}
	if string(second.Root) != `C:\Music` {
		t.Errorf("root moved to %q on a policy refresh", second.Root)
	}
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].EntityType != "library" || changes[0].Op != model.OpUpdate {
		t.Errorf("policy refresh emitted %+v, want one library update", changes)
	}
}

// LibraryByRoot folds too, since it shares the lookup.
func TestLibraryByRootFoldsCaseOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("path case folding is a Windows filesystem property")
	}
	ctx := context.Background()
	st := openEmptyStore(t)
	first, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(`C:\Music`), DisplayRoot: `C:\Music`, Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.LibraryByRoot(ctx, []byte(`c:\MUSIC`))
	if err != nil {
		t.Fatalf("LibraryByRoot on a re-cased root: %v", err)
	}
	if got.PID != first.PID {
		t.Errorf("LibraryByRoot returned %q, want %q", got.PID, first.PID)
	}
}
