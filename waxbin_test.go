package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestEndToEndSingleFile verifies the core flow from scan to store to query to
// organize and read back.
func TestEndToEndSingleFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 3))

	lib := openManaged(t, ctx, db, root)

	// SCAN
	scanRes, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanRes.Total.AudioFiles != 1 || scanRes.Total.ItemsCreated != 1 {
		t.Fatalf("scan tally = %+v, want 1 audio / 1 created", scanRes.Total)
	}
	if scanRes.JobPID == "" {
		t.Fatal("scan did not run under a job")
	}

	// QUERY (by substring, through the shared query engine)
	items, err := lib.Query(ctx, query.New(query.EntityItems).
		Where("title", query.OpContains, "Midnight").Build())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("query returned %d items, want 1", len(items))
	}
	got := items[0]
	if got.Title != "Midnight Drive" || got.Artist != "The Foobars" || got.Album != "Night Moves" {
		t.Fatalf("read-back tags wrong: %+v", got)
	}
	if got.TrackNo != 3 {
		t.Fatalf("track no = %d, want 3", got.TrackNo)
	}
	pid := got.PID

	// ORGANIZE (dry run, then apply)
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan organize: %v", err)
	}
	if plan.Pending() != 1 {
		t.Fatalf("plan pending = %d, want 1", plan.Pending())
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil {
		t.Fatalf("apply organize: %v", err)
	}
	if rep.Moved != 1 || rep.Errored != 0 {
		t.Fatalf("organize report = %+v, want moved 1 errored 0", rep)
	}

	// READ BACK: the catalog reflects the new templated location, and the file
	// is actually there (and gone from its origin).
	wantPath := filepath.Join(root, "The Foobars", "Night Moves", "03 - Midnight Drive.mp3")
	after, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("get after organize: %v", err)
	}
	if after.DisplayPath != wantPath {
		t.Fatalf("path after organize = %q, want %q", after.DisplayPath, wantPath)
	}
	if !fileExists(wantPath) {
		t.Fatalf("organized file missing on disk: %s", wantPath)
	}
	if fileExists(src) {
		t.Fatalf("source file still present after move: %s", src)
	}

	// JOBS: both the scan and organize jobs are recorded and done.
	jobs, err := lib.Jobs(ctx, 10)
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if !hasDoneJob(jobs, "scan") || !hasDoneJob(jobs, "organize") {
		t.Fatalf("expected done scan+organize jobs, got %+v", jobs)
	}

	// CHANGE LOG: mutations were recorded for delta consumers.
	changes, err := lib.Changes(ctx, 0)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("expected change_log rows")
	}
}

// TestWriteOwnershipConflict verifies a second writer is refused via the flock.
func TestWriteOwnershipConflict(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	first := openManaged(t, ctx, db, root)
	_ = first

	_, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged}},
	})
	if err == nil {
		t.Fatal("second writer should be refused while the first holds the lock")
	}
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("want CodeConflict, got %v (code %s)", err, waxerr.CodeOf(err))
	}
}

// TestReadOnlyRefusesMutations verifies a read-only library cannot mutate but
// can read, and that read-only opens do not contend for the write lock.
func TestReadOnlyRefusesMutations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("A", "Artist", "Album", 1))

	// Seed via a read-write session, then close it.
	rw := openManaged(t, ctx, db, root)
	if _, err := rw.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close rw: %v", err)
	}

	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()

	items, err := ro.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil {
		t.Fatalf("read-only query: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("read-only query returned %d items, want 1", len(items))
	}

	if _, err := ro.Scan(ctx, waxbin.ScanRequest{}); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("read-only scan: want CodeUnsupported, got %v (code %s)", err, waxerr.CodeOf(err))
	}
}

// TestOrganizeLeavesInPlaceLibraryFiles verifies organize only moves files in
// the managed library and never touches an in-place library's files.
func TestOrganizeLeavesInPlaceLibraryFiles(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	inplaceRoot := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	managedFile := filepath.Join(managedRoot, "m.mp3")
	inplaceFile := filepath.Join(inplaceRoot, "i.mp3")
	// Distinct audio payloads so the two files have distinct essence (and thus
	// are two separate items, not one deduped item).
	audioM := testaudio.DefaultAudio()
	audioI := testaudio.DefaultAudio()
	audioI[10] = 0x55
	writeFile(t, managedFile, testaudio.BuildMP3WithAudio("Managed Song", "M Artist", "M Album", 1, audioM))
	writeFile(t, inplaceFile, testaudio.BuildMP3WithAudio("InPlace Song", "I Artist", "I Album", 2, audioI))

	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots: []config.Root{
			{Path: managedRoot, Mode: model.ModeManaged, Profile: "waxbin-native"},
			{Path: inplaceRoot, Mode: model.ModeInPlace},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })

	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Pending() != 1 {
		t.Fatalf("plan should move only the managed file, got %d pending", plan.Pending())
	}
	if _, err := lib.ApplyOrganize(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if !fileExists(inplaceFile) {
		t.Fatal("in-place library file was moved; ModeInPlace was violated")
	}
	if fileExists(managedFile) {
		t.Fatal("managed file was not moved")
	}
}

// TestScanRelativeSubPath verifies a relative --sub-path is resolved under the
// library root rather than rejected.
func TestScanRelativeSubPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "sub1", "a.mp3"), testaudio.BuildMP3("A", "Ar", "Al", 1))
	writeFile(t, filepath.Join(root, "sub2", "b.mp3"), testaudio.BuildMP3("B", "Br", "Bl", 2))

	lib := openManaged(t, ctx, db, root)

	res, err := lib.Scan(ctx, waxbin.ScanRequest{SubPath: "sub1"})
	if err != nil {
		t.Fatalf("scan sub-path: %v", err)
	}
	if res.Total.AudioFiles != 1 {
		t.Fatalf("expected 1 file under sub1, got %d", res.Total.AudioFiles)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(items) != 1 || items[0].Title != "A" {
		t.Fatalf("expected only sub1's track, got %+v", items)
	}
}

// TestOpenRejectsOverlappingRoots verifies an embedder gets the same
// non-overlapping-roots guarantee as the CLI (config.Validate runs in Open).
func TestOpenRejectsOverlappingRoots(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	_, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots: []config.Root{
			{Path: filepath.Join(base, "music"), Mode: model.ModeManaged},
			{Path: filepath.Join(base, "music", "sub"), Mode: model.ModeInPlace}, // nested
		},
	})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for overlapping roots, got %v (code %s)", err, waxerr.CodeOf(err))
	}
}

// TestReadOnlyConcurrentWithWriter verifies a read-only consumer can open and
// query while another session holds the write lock (what the read-only CLI
// commands rely on).
func TestReadOnlyConcurrentWithWriter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("A", "Ar", "Al", 1))

	rw := openManaged(t, ctx, db, root) // holds the write lock for the test
	if _, err := rw.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open while write-locked should succeed: %v", err)
	}
	defer ro.Close()

	items, err := ro.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil || len(items) != 1 {
		t.Fatalf("concurrent read-only query: err=%v len=%d", err, len(items))
	}
}

// TestOrganizeMoveFailureRollsBack verifies a colliding destination fails the
// action (reported, not fatal) and leaves the source in place.
func TestOrganizeMoveFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 3))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Occupy the destination so the move collides.
	dst := filepath.Join(root, "The Foobars", "Night Moves", "03 - Midnight Drive.mp3")
	writeFile(t, dst, []byte("occupied"))

	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Moved != 0 || rep.Errored != 1 {
		t.Fatalf("expected 0 moved / 1 errored on collision, got %+v", rep)
	}
	if !fileExists(src) {
		t.Fatal("source should remain after a failed move")
	}
}

func openManaged(t *testing.T, ctx context.Context, db, root string) *waxbin.Library {
	t.Helper()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	return lib
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasDoneJob(jobs []*model.Job, kind string) bool {
	for _, j := range jobs {
		if j.Kind == kind && j.State == model.JobDone {
			return true
		}
	}
	return false
}
