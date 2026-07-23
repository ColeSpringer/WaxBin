package waxbin_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestAddRootScansImmediately verifies the runtime add path end to end: the new
// root is registered with a create delta, and the very next scan catalogs files
// under it without a reopen (scan resolves roots from store rows).
func TestAddRootScansImmediately(t *testing.T) {
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

	// Materialize the internal podcast library row (lazily created on the first
	// download-path use). Its "podcast" mode is not user-settable, so mapping it
	// into the validated root set would fail every later AddRoot; the skip is
	// what this half of the test pins.
	show, err := lib.Podcasts().AddManual(ctx, "Poison Check", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("add manual show: %v", err)
	}
	epRes, err := lib.Podcasts().AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Ep", GUID: "g1"}, true)
	if err != nil {
		t.Fatalf("add episode: %v", err)
	}
	src := filepath.Join(t.TempDir(), "ep.mp3")
	writeFile(t, src, testaudio.BuildMP3("Ep", "Host", "Show", 1))
	if _, err := lib.Podcasts().ImportEpisodeFile(ctx, epRes.EpisodePID, src, false); err != nil {
		t.Fatalf("import episode file: %v", err)
	}
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
}

// TestAddRootReadOnlyRefuses: a read-only handle refuses before touching the
// store.
func TestAddRootReadOnlyRefuses(t *testing.T) {
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
}

// TestAddRootConcurrentOverlapSerialized pins the rootMu guard: two goroutines
// racing to add mutually-nested roots must not both win, or the catalog ends up
// with the overlapping roots Open's validation forbids. Deterministic with the
// mutex (the second to acquire it sees the first's committed row and rejects);
// without it both validate against the pre-race set and both insert.
func TestAddRootConcurrentOverlapSerialized(t *testing.T) {
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
