package waxbin_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestAddRootScansImmediately verifies the runtime add path end to end: the new
// root is registered with a create delta, and the very next scan catalogs files
// under it without a reopen (scan resolves roots from store rows).
func TestAddRootScansImmediately(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(rootB, "late.mp3"), testaudio.BuildMP3("Late Addition", "Adder", "Runtime", 1))

	lib := openManaged(t, ctx, db, rootA)

	added, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeManaged})
	if err != nil {
		t.Fatalf("add root: %v", err)
	}
	if added.PID == "" || added.DisplayRoot != rootB || added.Mode != model.ModeManaged {
		t.Fatalf("added library = %+v, want a managed row at %s", added, rootB)
	}
	// Validation defaults applied like config loading.
	if added.MediaType() != model.MediaMixed || added.Profile != "waxbin-native" {
		t.Fatalf("added library defaults = %s/%s, want mixed/waxbin-native", added.MediaType(), added.Profile)
	}

	libs, err := lib.Libraries(ctx)
	if err != nil || len(libs) != 2 {
		t.Fatalf("libraries = %d (err %v), want 2", len(libs), err)
	}

	// The create delta is the machine-readable signal an embedder (or its
	// watcher supervisor) restarts on.
	changes, err := lib.Changes(ctx, 0)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	found := false
	for _, ch := range changes {
		if ch.EntityType == "library" && ch.EntityPID == added.PID && ch.Op == model.OpCreate {
			found = true
		}
	}
	if !found {
		t.Fatal("no library create delta for the added root")
	}

	// No reopen: the same handle scans the new root immediately.
	res, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Total.ItemsCreated != 1 {
		t.Fatalf("scan created = %d, want the file under the added root", res.Total.ItemsCreated)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).
		Where("title", query.OpIs, "Late Addition").Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("query for the added root's track: err=%v len=%d, want 1", err, len(items))
	}
}

// TestAddRootValidation drives the synthesized-config validation: overlaps with
// registered roots (both nesting directions), the inbox, and the podcast dir all
// reject, an invalid mode rejects, and the internal podcast-mode library row
// does not poison the root set it is validated against.
func TestAddRootValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "music")
	inbox := filepath.Join(base, "inbox")
	podDir := filepath.Join(base, "podcasts")
	writeFile(t, filepath.Join(rootA, ".keep"), nil)
	writeFile(t, filepath.Join(inbox, ".keep"), nil)
	db := filepath.Join(t.TempDir(), "catalog.db")

	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:   db,
		Roots:    []config.Root{{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Inbox:    []string{inbox},
		Podcasts: config.PodcastConfig{Dir: podDir},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })

	// The open made the internal podcast library row. Its "podcast" mode is not
	// user-settable, so mapping it into the validated root set would fail every later
	// AddRoot; the skip is what this half of the test pins.
	if libs, err := lib.Libraries(ctx); err != nil || !hasPodcastLib(libs) {
		t.Fatalf("libraries = %+v (err %v), want the internal podcast row present", libs, err)
	}

	for _, tc := range []struct {
		name string
		spec config.Root
	}{
		{"nested under a root", config.Root{Path: filepath.Join(rootA, "sub"), Mode: model.ModeManaged}},
		{"containing a root", config.Root{Path: base, Mode: model.ModeInPlace}},
		{"the inbox", config.Root{Path: inbox, Mode: model.ModeManaged}},
		{"under the podcast dir", config.Root{Path: filepath.Join(podDir, "sub"), Mode: model.ModeInPlace}},
		{"invalid mode", config.Root{Path: t.TempDir(), Mode: model.Mode("bogus")}},
	} {
		if _, err := lib.AddRoot(ctx, tc.spec); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("AddRoot(%s) = %v, want CodeInvalid", tc.name, err)
		}
	}

	// A clean spec still lands with the podcast-mode row in the store.
	if _, err := lib.AddRoot(ctx, config.Root{Path: t.TempDir(), Mode: model.ModeManaged}); err != nil {
		t.Fatalf("AddRoot with a podcast library registered: %v", err)
	}
}

// TestAddRootIdempotentReAdd: re-adding a registered path keeps the pid and
// refreshes the policy, the EnsureLibrary upsert semantics.
func TestAddRootIdempotentReAdd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openManaged(t, ctx, db, rootA)

	first, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeManaged})
	if err != nil {
		t.Fatalf("add root: %v", err)
	}
	again, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeManaged, Media: model.MediaMusic})
	if err != nil {
		t.Fatalf("re-add root: %v", err)
	}
	if again.PID != first.PID {
		t.Fatalf("re-add minted pid %s, want the original %s", again.PID, first.PID)
	}
	if again.MediaType() != model.MediaMusic {
		t.Fatalf("re-add media = %s, want the refreshed music", again.MediaType())
	}

	// A retyped root in a different case names the same directory on Windows, so it
	// has to match the registered library rather than trip the overlap check against
	// the very row it would update.
	if runtime.GOOS == "windows" {
		recased, err := lib.AddRoot(ctx, config.Root{
			Path: strings.ToUpper(rootB), Mode: model.ModeManaged, Media: model.MediaMusic,
		})
		if err != nil {
			t.Fatalf("re-add with a re-cased root: %v", err)
		}
		if recased.PID != first.PID {
			t.Fatalf("re-cased re-add minted pid %s, want the original %s", recased.PID, first.PID)
		}
		if string(recased.Root) != rootB {
			t.Errorf("re-cased re-add moved the stored root to %q, want %q", recased.Root, rootB)
		}
	}
}

// TestAddRootReadOnlyRefuses: a read-only handle refuses before touching the
// store.
func TestAddRootReadOnlyRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	openManaged(t, ctx, db, rootA) // authors the catalog and holds the write lock

	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	defer ro.Close()
	if _, err := ro.AddRoot(ctx, config.Root{Path: t.TempDir(), Mode: model.ModeManaged}); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("read-only AddRoot = %v, want CodeUnsupported", err)
	}
}

// TestRelocateRootValidatesOverlap covers the hardening: a relocation into
// another root rejects with the config vocabulary, a clean relocation still
// works, and an unknown pid stays CodeNotFound.
func TestRelocateRootValidatesOverlap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots: []config.Root{
			{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"},
			{Path: rootB, Mode: model.ModeInPlace, Profile: "waxbin-native"},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })

	var libB *model.Library
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatalf("libraries: %v", err)
	}
	for _, l := range libs {
		if l.DisplayRoot == rootB {
			libB = l
		}
	}
	if libB == nil {
		t.Fatalf("no library row for %s", rootB)
	}

	// Into the other root (nested) and onto it exactly: both overlap.
	if err := lib.RelocateRoot(ctx, libB.PID, filepath.Join(rootA, "sub")); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("relocate into a root = %v, want CodeInvalid", err)
	}
	if err := lib.RelocateRoot(ctx, libB.PID, rootA); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("relocate onto a root = %v, want CodeInvalid", err)
	}

	// A clean destination still relocates.
	rootC := t.TempDir()
	if err := lib.RelocateRoot(ctx, libB.PID, rootC); err != nil {
		t.Fatalf("valid relocate: %v", err)
	}
	libs, _ = lib.Libraries(ctx)
	moved := false
	for _, l := range libs {
		if l.PID == libB.PID && l.DisplayRoot == rootC {
			moved = true
		}
	}
	if !moved {
		t.Fatalf("libraries after relocate = %+v, want %s at %s", libs, libB.PID, rootC)
	}

	if err := lib.RelocateRoot(ctx, model.NewPID(), t.TempDir()); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("relocate unknown pid = %v, want CodeNotFound", err)
	}

	// What a scan could never walk is refused as AddRoot refuses it, before any row moves:
	// a relative path, a file, and a folder that is not there unless the caller allows it.
	file := filepath.Join(t.TempDir(), "file")
	writeFile(t, file, []byte("x"))
	missing := filepath.Join(t.TempDir(), "not", "mounted")
	for _, tc := range []struct{ name, path, reason string }{
		{"relative", filepath.Join("relative", "music"), "absolute"},
		{"a file", file, "not a directory"},
		{"missing", missing, "does not exist"},
	} {
		if err := lib.RelocateRoot(ctx, libB.PID, tc.path); !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("relocate to %s = %v, want CodeInvalid saying %q", tc.name, err, tc.reason)
		}
	}
	if libs, _ := lib.Libraries(ctx); !slices.ContainsFunc(libs, func(l *model.Library) bool { return l.DisplayRoot == rootC }) {
		t.Fatalf("a refused relocation moved the root: %+v", libs)
	}
	if err := lib.RelocateRoot(ctx, libB.PID, missing, waxbin.AllowAbsent()); err != nil {
		t.Fatalf("relocate to a root mounted later, allowed absent: %v", err)
	}
}

// TestAddRootConcurrentOverlapSerialized pins the rootMu guard: two goroutines
// racing to add mutually-nested roots must not both win, or the catalog ends up
// with the overlapping roots Open's validation forbids. Deterministic with the
// mutex (the second to acquire it sees the first's committed row and rejects);
// without it both validate against the pre-race set and both insert.
func TestAddRootConcurrentOverlapSerialized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	rootA := filepath.Join(base, "a")
	writeFile(t, filepath.Join(rootA, ".keep"), nil)
	lib := openManaged(t, ctx, db, rootA)

	// Distinct paths (no UNIQUE(root) collision to mask the race) that nest, so
	// only one can validly join the set.
	specs := []config.Root{
		{Path: filepath.Join(base, "lib"), Mode: model.ModeManaged},
		{Path: filepath.Join(base, "lib", "sub"), Mode: model.ModeManaged},
	}
	writeFile(t, filepath.Join(base, "lib", "sub", ".keep"), nil)
	errs := make([]error, len(specs))
	var wg sync.WaitGroup
	wg.Add(len(specs))
	for i := range specs {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = lib.AddRoot(ctx, specs[i])
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, e := range errs {
		switch {
		case e == nil:
			winners++
		case !waxerr.Is(e, waxerr.CodeInvalid):
			t.Fatalf("concurrent AddRoot failed with %v, want nil or CodeInvalid", e)
		}
	}
	if winners != 1 {
		t.Fatalf("%d of 2 nested concurrent adds succeeded, want exactly 1 (rootMu must serialize validate+write)", winners)
	}
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatalf("libraries: %v", err)
	}
	if len(libs) != 2 {
		t.Fatalf("registered libraries = %d, want 2 (base + one winner, never both racers)", len(libs))
	}
}

// TestAddRootPodcastOverlapWhenDirUnset covers the podcast-row overlap check: a
// process opened with Podcasts.Dir unset, but a podcast-mode row persisting from
// a prior run, must still refuse a root nested in the download tree. The
// configured-dir check is disabled here, so only the row-based overlap can catch
// it, and without it a later scan would ingest episodes as music.
func TestAddRootPodcastOverlapWhenDirUnset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "music")
	podDir := filepath.Join(base, "podcasts")
	writeFile(t, filepath.Join(rootA, ".keep"), nil)
	db := filepath.Join(t.TempDir(), "catalog.db")

	// First run: podcast dir configured, so the internal podcast row is created.
	lib1, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:   db,
		Roots:    []config.Root{{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Podcasts: config.PodcastConfig{Dir: podDir},
	})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	show, err := lib1.Podcasts().AddManual(ctx, "Persisted", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("add manual show: %v", err)
	}
	epRes, err := lib1.Podcasts().AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Ep", GUID: "g1"}, true)
	if err != nil {
		t.Fatalf("add episode: %v", err)
	}
	src := filepath.Join(t.TempDir(), "ep.mp3")
	writeFile(t, src, testaudio.BuildMP3("Ep", "Host", "Show", 1))
	if _, err := lib1.Podcasts().ImportEpisodeFile(ctx, epRes.EpisodePID, src, false); err != nil {
		t.Fatalf("import episode file: %v", err)
	}
	if err := lib1.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	// Second run: podcast dir NOT configured. The podcast-mode row persists, so
	// the configured-dir branch of config.Validate never runs for it.
	lib2, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = lib2.Close() })

	// Each overlaps only the persisted podcast row (not rootA), so only the
	// row-based check can reject them.
	for _, spec := range []config.Root{
		{Path: filepath.Join(podDir, "music"), Mode: model.ModeManaged}, // nested in the tree
		{Path: podDir, Mode: model.ModeInPlace},                         // the tree itself
	} {
		if _, err := lib2.AddRoot(ctx, spec); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("AddRoot(%s) with podcast dir unset = %v, want CodeInvalid (overlaps the persisted podcast row)", spec.Path, err)
		}
	}
	// A root clear of the podcast tree still lands.
	if _, err := lib2.AddRoot(ctx, config.Root{Path: t.TempDir(), Mode: model.ModeManaged}); err != nil {
		t.Fatalf("clean AddRoot with podcast dir unset: %v", err)
	}
}

func hasPodcastLib(libs []*model.Library) bool {
	for _, l := range libs {
		if l.Mode == model.ModePodcast {
			return true
		}
	}
	return false
}

// TestAddRootRefusesWhatAScanCannotWalk: a relative path, a file and a path that does not
// exist are refused with the reason and register nothing, and a root to be mounted later
// registers when the caller says it may be absent, its scan finding nothing to mark gone.
func TestAddRootRefusesWhatAScanCannotWalk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	file := filepath.Join(t.TempDir(), "song.mp3")
	writeFile(t, file, testaudio.BuildMP3("Song", "Artist", "Album", 1))
	missing := filepath.Join(t.TempDir(), "not", "mounted")
	for _, tc := range []struct{ name, path, reason string }{
		{"relative", filepath.Join("relative", "music"), "absolute"},
		{"a file", file, "not a directory"},
		{"missing", missing, "does not exist"},
	} {
		_, err := lib.AddRoot(ctx, config.Root{Path: tc.path, Mode: model.ModeInPlace})
		if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("AddRoot(%s) = %v, want CodeInvalid saying %q", tc.name, err, tc.reason)
		}
	}
	if libs, err := lib.Libraries(ctx); err != nil || len(libs) != 1 {
		t.Fatalf("libraries = %d (err %v), want only the configured root", len(libs), err)
	}

	added, err := lib.AddRoot(ctx, config.Root{Path: missing, Mode: model.ModeInPlace}, waxbin.AllowAbsent())
	if err != nil || added.DisplayRoot != missing {
		t.Fatalf("AddRoot(missing, AllowAbsent) = %+v, %v; want it registered", added, err)
	}
	if _, err := lib.AddRoot(ctx, config.Root{Path: file, Mode: model.ModeInPlace}, waxbin.AllowAbsent()); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("AddRoot(a file, AllowAbsent) = %v, want CodeInvalid", err)
	}
	res, err := lib.Scan(ctx, waxbin.ScanRequest{LibraryPID: added.PID})
	if err != nil || res.Total.Missing != 0 || res.Total.AudioFiles != 0 {
		t.Fatalf("scan of the absent root = %+v (err %v), want nothing found and nothing missing", res, err)
	}
}

// TestRemoveRootArchivesWhatItHeldAndAReaddRelinksIt: removing a library detaches its
// files, archives the items left with none under their pids with their play state and
// list entries, drops its trash journal, and deletes its row with one library delta,
// leaving every file where it was; an item with a copy in another library keeps that
// copy. Adding the root back and scanning it brings the archived items back under their
// old pids.
func TestRemoveRootArchivesWhatItHeldAndAReaddRelinksIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(rootA, "kept.mp3"), testaudio.BuildMP3WithAudio("Kept", "Artist", "Album", 1, testaudio.AudioWithSeed(41)))
	lib := openManaged(t, ctx, db, rootA)
	scanLib(t, ctx, lib)
	kept := itemPIDByTitle(t, ctx, lib, "Kept")

	writeFile(t, filepath.Join(rootB, "kept copy.mp3"), testaudio.BuildMP3WithAudio("Kept", "Artist", "Album", 1, testaudio.AudioWithSeed(41)))
	writeFile(t, filepath.Join(rootB, "one.mp3"), testaudio.BuildMP3WithAudio("Gone One", "Artist", "Other", 1, testaudio.AudioWithSeed(42)))
	writeFile(t, filepath.Join(rootB, "two.mp3"), testaudio.BuildMP3WithAudio("Gone Two", "Artist", "Other", 2, testaudio.AudioWithSeed(43)))
	writeFile(t, filepath.Join(rootB, "trashed.mp3"), testaudio.BuildMP3WithAudio("Trashed", "Artist", "Other", 3, testaudio.AudioWithSeed(44)))
	b, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeManaged, Profile: "waxbin-native"})
	if err != nil {
		t.Fatalf("add root: %v", err)
	}
	scanLib(t, ctx, lib)
	one, two, trashed := itemPIDByTitle(t, ctx, lib, "Gone One"), itemPIDByTitle(t, ctx, lib, "Gone Two"),
		itemPIDByTitle(t, ctx, lib, "Trashed")
	if files, err := lib.ItemFiles(ctx, kept); err != nil || len(files) != 2 {
		t.Fatalf("kept item files = %+v (err %v), want its own and the copy in the added library", files, err)
	}
	if _, err := lib.Playback().SetStar(ctx, "", one, true, nil); err != nil {
		t.Fatalf("star: %v", err)
	}
	list, err := lib.Playlists().CreateStatic(ctx, "List", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.Playlists().Set(ctx, list, []model.PID{one, two}); err != nil {
		t.Fatal(err)
	}
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{trashed}, model.DeleteTrash)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	before, err := lib.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	seq := before[len(before)-1].Seq

	rep, err := lib.RemoveRoot(ctx, b.PID, waxbin.RemoveRootOptions{})
	if err != nil {
		t.Fatalf("remove root: %v", err)
	}
	if rep.FilesDetached != 3 || rep.ItemsArchived != 2 || rep.TrashRowsDropped != 1 || rep.Root != rootB {
		t.Errorf("report = %+v, want 3 files detached, 2 items archived, 1 trash row dropped at %s", rep, rootB)
	}
	libs, err := lib.Libraries(ctx)
	if err != nil || len(libs) != 1 || libs[0].DisplayRoot != rootA {
		t.Fatalf("libraries = %+v (err %v), want only %s", libs, err, rootA)
	}
	for _, pid := range []model.PID{one, two, trashed} {
		if st := stateOf(t, ctx, lib, pid); st != model.StateArchived {
			t.Errorf("item %s state = %q, want archived", pid, st)
		}
	}
	if ps, err := lib.Playback().State(ctx, "", one); err != nil || !ps.Starred {
		t.Errorf("play state = %+v (err %v), want the star kept", ps, err)
	}
	if items, err := lib.Playlists().Items(ctx, list, ""); err != nil || len(items) != 2 {
		t.Errorf("playlist = %d items (err %v), want both entries kept", len(items), err)
	}
	if st := stateOf(t, ctx, lib, kept); st != model.StatePresent {
		t.Errorf("kept item state = %q, want present", st)
	}
	if files, err := lib.ItemFiles(ctx, kept); err != nil || len(files) != 1 || files[0].DisplayPath != filepath.Join(rootA, "kept.mp3") {
		t.Errorf("kept item files = %+v (err %v), want its own file alone", files, err)
	}
	if entries, err := lib.Trash(ctx, false, 0); err != nil || len(entries) != 0 {
		t.Errorf("trash = %+v (err %v), want the removed library's entry gone", entries, err)
	}
	for _, name := range []string{"kept copy.mp3", "one.mp3", "two.mp3"} {
		if !fileExists(filepath.Join(rootB, name)) {
			t.Errorf("%s was touched on disk", name)
		}
	}
	if held, _ := filepath.Glob(filepath.Join(rootB, model.TrashDirName, "*", "trashed.mp3")); len(held) != 1 {
		t.Errorf("trashed file on disk = %v, want it left in the trash folder", held)
	}
	after, err := lib.Changes(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	deletes := 0
	for _, ch := range after {
		if ch.EntityType == "library" && ch.EntityPID == b.PID {
			if ch.Op != model.OpDelete {
				t.Errorf("library delta %+v, want only the delete", ch)
			}
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("library deletes = %d, want 1", deletes)
	}

	again, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeManaged, Profile: "waxbin-native"})
	if err != nil {
		t.Fatalf("add the root back: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{LibraryPID: again.PID}); err != nil {
		t.Fatalf("scan the root back: %v", err)
	}
	for _, pid := range []model.PID{one, two} {
		if st := stateOf(t, ctx, lib, pid); st != model.StatePresent {
			t.Errorf("item %s after the re-add = %q, want present under its old pid", pid, st)
		}
	}
	if ps, err := lib.Playback().State(ctx, "", one); err != nil || !ps.Starred {
		t.Errorf("play state after the re-add = %+v (err %v), want the star", ps, err)
	}
	if files, err := lib.ItemFiles(ctx, kept); err != nil || len(files) != 2 {
		t.Errorf("kept item files after the re-add = %+v (err %v), want the copy back", files, err)
	}
}

// TestRemoveRootRefuses: an unknown pid is not found, a read-only handle cannot remove,
// and a root the catalog was opened with is refused without force, saying it comes
// from the configuration. Forced, it goes, and a reopen of the same handle does not
// register it again.
func TestRemoveRootRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, Roots: []config.Root{
		{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"},
		{Path: rootB, Mode: model.ModeInPlace, Profile: "waxbin-native"},
	}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	libs, err := lib.Libraries(ctx)
	if err != nil || len(libs) != 2 {
		t.Fatalf("libraries = %+v (err %v)", libs, err)
	}
	var b model.PID
	for _, l := range libs {
		if l.DisplayRoot == rootB {
			b = l.PID
		}
	}

	if _, err := lib.RemoveRoot(ctx, model.NewPID(), waxbin.RemoveRootOptions{}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("remove an unknown library = %v, want CodeNotFound", err)
	}
	_, err = lib.RemoveRoot(ctx, b, waxbin.RemoveRootOptions{})
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("remove a configured root = %v, want CodeInvalid naming the configuration", err)
	}
	if libs, _ := lib.Libraries(ctx); len(libs) != 2 {
		t.Fatalf("a refused removal removed something: %+v", libs)
	}

	if _, err := lib.RemoveRoot(ctx, b, waxbin.RemoveRootOptions{Force: true}); err != nil {
		t.Fatalf("forced removal of a configured root: %v", err)
	}
	if err := lib.BeginMaintenance(ctx); err != nil {
		t.Fatalf("begin maintenance: %v", err)
	}
	if err := lib.EndMaintenance(ctx); err != nil {
		t.Fatalf("end maintenance: %v", err)
	}
	if libs, err := lib.Libraries(ctx); err != nil || len(libs) != 1 || libs[0].DisplayRoot != rootA {
		t.Fatalf("libraries after a reopen = %+v (err %v), want the removed root kept out", libs, err)
	}

	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()
	if _, err := ro.RemoveRoot(ctx, libs[0].PID, waxbin.RemoveRootOptions{Force: true}); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("remove on a read-only handle = %v, want CodeUnsupported", err)
	}
}

// TestRelocateRootKeepsAReopenFromRegisteringTheOldPath: a configured root relocated by
// this handle is ensured at its new path when the handle reopens after a maintenance
// hand-off, not registered again as an empty library at the old one.
func TestRelocateRootKeepsAReopenFromRegisteringTheOldPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB, rootC := t.TempDir(), t.TempDir(), t.TempDir()
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"), Roots: []config.Root{
		{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"},
		{Path: rootB, Mode: model.ModeInPlace, Profile: "waxbin-native"},
	}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range libs {
		if l.DisplayRoot == rootB {
			if err := lib.RelocateRoot(ctx, l.PID, rootC); err != nil {
				t.Fatalf("relocate: %v", err)
			}
		}
	}
	if err := lib.BeginMaintenance(ctx); err != nil {
		t.Fatalf("begin maintenance: %v", err)
	}
	if err := lib.EndMaintenance(ctx); err != nil {
		t.Fatalf("end maintenance: %v", err)
	}
	libs, err = lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, l := range libs {
		roots = append(roots, l.DisplayRoot)
	}
	if len(roots) != 2 || slices.Contains(roots, rootB) || !slices.Contains(roots, rootC) {
		t.Fatalf("roots after a reopen = %v, want %s and %s", roots, rootA, rootC)
	}
}

// TestRelocateRootKeepsTheFastPath: a file and its folder's cover moved with their root
// are unchanged to the next scan, since the cover's observation moved with them, where it
// used to read the file in full as though the cover had gone.
func TestRelocateRootKeepsTheFastPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	oldRoot, newRoot := filepath.Join(base, "old"), filepath.Join(base, "new")
	writeFile(t, filepath.Join(oldRoot, "song.mp3"), testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(73)))
	var cover bytes.Buffer
	if err := png.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(oldRoot, "cover.png"), cover.Bytes())
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	added, err := lib.AddRoot(ctx, config.Root{Path: oldRoot, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatalf("add root: %v", err)
	}
	scanLib(t, ctx, lib)
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	if err := lib.RelocateRoot(ctx, added.PID, newRoot); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	res, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Total.Unchanged != 1 || res.Total.Reread != 0 || res.Total.SidecarsUpdated != 0 {
		t.Errorf("scan after the relocation = %+v, want the file unchanged and not read again", res.Total)
	}
}

// TestRelocateRootCarriesTheTrashJournal: a library moved with its trash folder still
// restores what it trashed, into the new root, since the journal's paths moved with it.
func TestRelocateRootCarriesTheTrashJournal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	oldRoot, newRoot := filepath.Join(base, "old"), filepath.Join(base, "new")
	writeFile(t, filepath.Join(oldRoot, "Artist", "keep.mp3"), testaudio.BuildMP3WithAudio("Keep", "Artist", "Album", 1, testaudio.AudioWithSeed(71)))
	writeFile(t, filepath.Join(oldRoot, "Artist", "gone.mp3"), testaudio.BuildMP3WithAudio("Gone", "Artist", "Album", 2, testaudio.AudioWithSeed(72)))
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	added, err := lib.AddRoot(ctx, config.Root{Path: oldRoot, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatalf("add root: %v", err)
	}
	scanLib(t, ctx, lib)
	gone := itemPIDByTitle(t, ctx, lib, "Gone")
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{gone}, model.DeleteTrash)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatalf("apply delete: %v", err)
	}

	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	if err := lib.RelocateRoot(ctx, added.PID, newRoot); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %+v (err %v), want the one entry", entries, err)
	}
	e := entries[0]
	want := filepath.Join(newRoot, "Artist", "gone.mp3")
	if string(e.OrigPath) != want || e.OrigDisplay != want || !strings.HasPrefix(e.TrashDisplay, newRoot+string(filepath.Separator)) ||
		!strings.HasPrefix(string(e.TrashPath), newRoot+string(filepath.Separator)) {
		t.Fatalf("entry = %+v, want its paths under %s", e, newRoot)
	}
	if err := lib.RestoreTrash(ctx, e.PID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !fileExists(want) {
		t.Errorf("%s was not restored", want)
	}
	if st := stateOf(t, ctx, lib, gone); st != model.StatePresent {
		t.Errorf("restored item state = %q, want present", st)
	}
}

// TestPodcastLibraryExistsAfterOpen: a read-write open with a podcast dir creates the
// podcast library before any download, and a later open with the dir spelled another
// way finds the same library rather than making a second.
func TestPodcastLibraryExistsAfterOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pods := filepath.Join(t.TempDir(), "pods")
	db := filepath.Join(t.TempDir(), "catalog.db")
	library := func(dir string) *model.Library {
		t.Helper()
		lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, Podcasts: config.PodcastConfig{Dir: dir}})
		if err != nil {
			t.Fatalf("open with %s: %v", dir, err)
		}
		defer func() { _ = lib.Close() }()
		pl, err := lib.PodcastLibrary(ctx)
		if err != nil {
			t.Fatalf("podcast library with %s: %v", dir, err)
		}
		libs, err := lib.Libraries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, l := range libs {
			if l.Mode == model.ModePodcast {
				n++
			}
		}
		if n != 1 || pl.Mode != model.ModePodcast || pl.DisplayRoot != pods {
			t.Fatalf("with %s: podcast library %+v among %d podcast rows, want one at %s", dir, pl, n, pods)
		}
		return pl
	}
	first := library(pods)
	sep := string(filepath.Separator)
	if again := library(pods + sep + "." + sep); again.PID != first.PID {
		t.Fatalf("a second open's podcast library is %s, want %s", again.PID, first.PID)
	}
}

// TestReopenEnsuresThePodcastLibrary: a restore taken through the hand-off can bring in a
// catalog with no podcast library, and the reopen creates one, as an open does.
func TestReopenEnsuresThePodcastLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bare, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "bare.db")})
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := bare.Backup(ctx, backup, port.BackupOptions{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := bare.Close(); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, Podcasts: config.PodcastConfig{Dir: t.TempDir()}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if err := lib.BeginMaintenance(ctx); err != nil {
		t.Fatalf("begin maintenance: %v", err)
	}
	if err := port.Restore(ctx, backup, db, true); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := lib.EndMaintenance(ctx); err != nil {
		t.Fatalf("end maintenance: %v", err)
	}
	if pl, err := lib.PodcastLibrary(ctx); err != nil || pl.Mode != model.ModePodcast {
		t.Fatalf("podcast library after the restore = %+v (err %v), want one", pl, err)
	}
}

// TestOpenRefusesARootInsideThePodcastDir: an embedder's open checks the podcast dir and
// the inbox against the roots as the CLI's does, before the catalog opens.
func TestOpenRefusesARootInsideThePodcastDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	pods := filepath.Join(base, "podcasts")
	root := func(path string) []config.Root {
		return []config.Root{{Path: path, Mode: model.ModeManaged, Profile: "waxbin-native"}}
	}
	for name, opts := range map[string]waxbin.Options{
		"a root inside the podcast dir": {Roots: root(filepath.Join(pods, "music")), Podcasts: config.PodcastConfig{Dir: pods}},
		"the podcast dir inside a root": {Roots: root(base), Podcasts: config.PodcastConfig{Dir: pods}},
		"an inbox inside a root":        {Roots: root(filepath.Join(base, "music")), Inbox: []string{filepath.Join(base, "music", "inbox")}},
		"the podcast dir in the inbox":  {Inbox: []string{base}, Podcasts: config.PodcastConfig{Dir: pods}},
	} {
		opts.DBPath = filepath.Join(t.TempDir(), "catalog.db")
		lib, err := waxbin.Open(ctx, opts)
		if err == nil {
			_ = lib.Close()
		}
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("open with %s = %v, want CodeInvalid", name, err)
		}
	}
}

// TestPodcastLibraryAnswersReadOnly: a read-only open finds the podcast library a
// read-write one made, by its dir, and answers CodeNotFound for a dir with none or for
// no dir at all.
func TestPodcastLibraryAnswersReadOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pods := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	rw, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, Podcasts: config.PodcastConfig{Dir: pods}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	made, err := rw.PodcastLibrary(ctx)
	if err != nil {
		t.Fatalf("podcast library: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"its dir": pods, "another dir": t.TempDir(), "no dir": ""} {
		ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true, Podcasts: config.PodcastConfig{Dir: dir}})
		if err != nil {
			t.Fatalf("read-only open with %s: %v", name, err)
		}
		pl, err := ro.PodcastLibrary(ctx)
		_ = ro.Close()
		switch {
		case dir == pods && (err != nil || pl.PID != made.PID):
			t.Errorf("read-only with %s = %+v (err %v), want %s", name, pl, err, made.PID)
		case dir != pods && !waxerr.Is(err, waxerr.CodeNotFound):
			t.Errorf("read-only with %s = %+v (err %v), want CodeNotFound", name, pl, err)
		}
	}
}

// TestReopenWithAPodcastDirConflictStillHandsBack: a restore whose catalog holds the
// configured podcast dir as a music root still reopens, hears its hook and allows the
// next hand-off, the podcast library left unmade as a download would report.
func TestReopenWithAPodcastDirConflictStillHandsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pods := t.TempDir()
	music, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "music.db"),
		Roots: []config.Root{{Path: pods, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open the music catalog: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := music.Backup(ctx, backup, port.BackupOptions{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := music.Close(); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "catalog.db")
	var reopens atomic.Int32
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, Podcasts: config.PodcastConfig{Dir: pods},
		OnReopen: func(context.Context, waxbin.ReopenEvent) { reopens.Add(1) }})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if err := lib.BeginMaintenance(ctx); err != nil {
		t.Fatalf("begin maintenance: %v", err)
	}
	if err := port.Restore(ctx, backup, db, true); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := lib.EndMaintenance(ctx); err != nil {
		t.Fatalf("end maintenance over a podcast dir conflict: %v", err)
	}
	if reopens.Load() != 1 {
		t.Fatalf("the reopen hook fired %d times, want once", reopens.Load())
	}
	if _, err := lib.PodcastLibrary(ctx); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("podcast library = %v, want CodeNotFound while its dir is a music root", err)
	}
	if err := lib.BeginMaintenance(ctx); err != nil {
		t.Fatalf("the next hand-off: %v", err)
	}
	if err := lib.EndMaintenance(ctx); err != nil {
		t.Fatalf("end the next hand-off: %v", err)
	}
}

// TestAnOverlapWithAStoredRootOpensAndRefusesAtUse: a root added at runtime, which no
// configuration names, that a podcast dir or an inbox nests with leaves the catalog
// opening, so the command that removes the root still runs, and refuses the downloads
// and the inbox import that would file into it; a dir apart from every root works.
func TestAnOverlapWithAStoredRootOpensAndRefusesAtUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	music := filepath.Join(base, "music")
	writeFile(t, filepath.Join(music, ".keep"), nil)
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	root, err := lib.AddRoot(ctx, config.Root{Path: music, Mode: model.ModeManaged})
	if err != nil {
		t.Fatalf("add root: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	inbox := filepath.Join(music, "inbox")
	writeFile(t, filepath.Join(inbox, ".keep"), nil)
	lib, err = waxbin.Open(ctx, waxbin.Options{DBPath: db, Inbox: []string{inbox},
		Podcasts: config.PodcastConfig{Dir: filepath.Join(music, "pods")}})
	if err != nil {
		t.Fatalf("open over the overlaps: %v", err)
	}
	if _, err := lib.PodcastLibrary(ctx); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("podcast library = %v, want none made inside the root", err)
	}
	show, err := lib.Podcasts().AddManual(ctx, "Local", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("add manual: %v", err)
	}
	ep, err := lib.Podcasts().AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Ep", GUID: "g"}, true)
	if err != nil {
		t.Fatalf("add episode: %v", err)
	}
	src := filepath.Join(t.TempDir(), "ep.mp3")
	writeFile(t, src, testaudio.BuildMP3("Ep", "Host", "Show", 1))
	if _, err := lib.Podcasts().ImportEpisodeFile(ctx, ep.EpisodePID, src, true); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("an episode import into the root = %v, want CodeConflict", err)
	}
	if _, err := os.Stat(filepath.Join(music, "pods")); !os.IsNotExist(err) {
		t.Errorf("the refused import made %s (err %v)", filepath.Join(music, "pods"), err)
	}
	if _, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inbox}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("an import of the inbox inside the root = %v, want CodeInvalid", err)
	}
	if _, err := lib.RemoveRoot(ctx, root.PID, waxbin.RemoveRootOptions{}); err != nil {
		t.Fatalf("remove the root the overlaps are with: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	lib, err = waxbin.Open(ctx, waxbin.Options{DBPath: db, Podcasts: config.PodcastConfig{Dir: filepath.Join(base, "pods")}})
	if err != nil {
		t.Fatalf("open with a podcast dir apart from the root: %v", err)
	}
	defer lib.Close()
	if _, err := lib.PodcastLibrary(ctx); err != nil {
		t.Fatalf("podcast library apart from the root: %v", err)
	}
}

// TestABlankPodcastDirOpensAsNone: a podcast dir of spaces is no podcast dir.
func TestABlankPodcastDirOpensAsNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Podcasts: config.PodcastConfig{Dir: "  "}})
	if err != nil {
		t.Fatalf("open with a blank podcast dir: %v", err)
	}
	defer lib.Close()
	if _, err := lib.PodcastLibrary(ctx); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("podcast library = %v, want none configured", err)
	}
}
