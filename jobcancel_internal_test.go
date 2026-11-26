package waxbin

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxbin/analyze"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// cancelNewestJob cancels the newest job the library lists by its pid, as a host would.
func cancelNewestJob(t *testing.T, lib *Library) {
	t.Helper()
	list, err := lib.Jobs(context.Background(), 1)
	if err != nil || len(list) == 0 {
		t.Errorf("jobs = %+v (err %v), want the running one", list, err)
		return
	}
	if err := lib.CancelJob(context.Background(), list[0].PID); err != nil {
		t.Errorf("cancel job %s: %v", list[0].PID, err)
	}
}

// cancelOnPut cancels the newest job just before the scan's nth track or book put, so that
// put meets the cancel.
type cancelOnPut struct {
	model.Catalog
	t    *testing.T
	lib  *Library
	n    int32
	puts *atomic.Int32
}

func (c cancelOnPut) PutScannedTrack(ctx context.Context, in model.PutScannedTrackInput) (*model.ScanItemResult, error) {
	if c.puts.Add(1) == c.n {
		cancelNewestJob(c.t, c.lib)
	}
	return c.Catalog.PutScannedTrack(ctx, in)
}

func (c cancelOnPut) PutScannedBook(ctx context.Context, in model.PutScannedBookInput) (*model.ScanItemResult, error) {
	if c.puts.Add(1) == c.n {
		cancelNewestJob(c.t, c.lib)
	}
	return c.Catalog.PutScannedBook(ctx, in)
}

// cancelScansAt swaps the library's scanner for one that cancels the newest job just
// before the nth track put, counted across scans.
func cancelScansAt(t *testing.T, lib *Library, n int32) {
	lib.scanner = scan.New(cancelOnPut{Catalog: lib.store, t: t, lib: lib, n: n, puts: new(atomic.Int32)}, meta.NewReader(), lib.log)
}

// cancelOnBeat cancels the newest job at its nth heartbeat, or once its row is written
// when n is 0.
type cancelOnBeat struct {
	model.JobStore
	t     *testing.T
	lib   *Library
	n     int32
	beats atomic.Int32
}

func (s *cancelOnBeat) CreateJob(ctx context.Context, j *model.Job) error {
	err := s.JobStore.CreateJob(ctx, j)
	if err == nil && s.n == 0 {
		cancelNewestJob(s.t, s.lib)
	}
	return err
}

func (s *cancelOnBeat) Heartbeat(ctx context.Context, jobID, ts int64, progress float64, msg string) error {
	err := s.JobStore.Heartbeat(ctx, jobID, ts, progress, msg)
	if s.n > 0 && s.beats.Add(1) == s.n {
		cancelNewestJob(s.t, s.lib)
	}
	return err
}

// cancelJobsAt swaps the library's job manager for one that cancels each job at its nth
// heartbeat (cancelOnBeat).
func cancelJobsAt(t *testing.T, lib *Library, n int32) {
	lib.jobs = jobs.NewManager(&cancelOnBeat{JobStore: lib.store, t: t, lib: lib, n: n}, "test", lib.log)
}

// waitJobEnd polls a job until it is no longer running.
func waitJobEnd(t *testing.T, lib *Library, pid model.PID) *model.Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		job, err := lib.Job(context.Background(), pid)
		if err != nil {
			t.Fatalf("job %s: %v", pid, err)
		}
		if job.State != model.JobRunning {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still running: %+v", pid, job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// canceledOnRequest fails the test unless job ended canceled by a request, and decodes
// the result it recorded into v.
func canceledOnRequest(t *testing.T, job *model.Job, v any) {
	t.Helper()
	if job.State != model.JobCanceled || job.ErrorReason != jobs.ReasonCancelRequested || job.Error != "canceled on request" {
		t.Fatalf("job = %+v, want canceled on request", job)
	}
	if err := json.Unmarshal([]byte(job.Result), v); err != nil {
		t.Fatalf("job result %q: %v, want what the job did before the cancel", job.Result, err)
	}
}

// threeTracks opens a library over a root holding three tracks, unscanned.
func threeTracks(t *testing.T, mode model.Mode) (*Library, string) {
	t.Helper()
	root := t.TempDir()
	for i, title := range []string{"a", "b", "c"} {
		writeRaw(t, filepath.Join(root, title+".mp3"),
			testaudio.BuildMP3WithAudio(title, "Band", "Record", i+1, testaudio.AudioWithSeed(byte(40+i))))
	}
	roots := []config.Root{{Path: root, Mode: mode}}
	if mode == model.ModeManaged {
		roots[0].Profile = "waxbin-native"
	}
	lib, err := OpenSeeded(t, context.Background(), Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"), Roots: roots})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	return lib, root
}

// TestCancelJobEndsABackgroundScan: a scan canceled by pid ends canceled on request with
// what it cataloged before the cancel, and the file the cancel cut short is no error.
func TestCancelJobEndsABackgroundScan(t *testing.T) {
	t.Parallel()
	lib, _ := threeTracks(t, model.ModeInPlace)
	cancelScansAt(t, lib, 2)
	pid, err := lib.StartScan(context.Background(), ScanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var res scan.Result
	canceledOnRequest(t, waitJobEnd(t, lib, pid), &res)
	if res.ItemsCreated != 1 || res.Errored != 0 {
		t.Errorf("scan result = %+v, want one item cataloged and nothing errored", res)
	}
}

// TestASynchronousScanCanceledByPIDSaysSo: a scan run in the caller's call and canceled by
// its pid returns the request's class and reason, which the caller's own cancel lacks.
func TestASynchronousScanCanceledByPIDSaysSo(t *testing.T) {
	t.Parallel()
	lib, _ := threeTracks(t, model.ModeInPlace)
	cancelScansAt(t, lib, 2)
	res, err := lib.Scan(context.Background(), ScanRequest{})
	if !waxerr.Is(err, waxerr.CodeCanceled) || waxerr.ReasonOf(err) != jobs.ReasonCancelRequested {
		t.Fatalf("scan = %v (%s %q), want canceled on request", err, waxerr.CodeOf(err), waxerr.ReasonOf(err))
	}
	if res.Total.ItemsCreated != 1 || res.Total.Errored != 0 {
		t.Errorf("scan result = %+v, want one item cataloged and nothing errored", res.Total)
	}
}

// TestAWatchCarriesOnAfterItsScanIsCanceled: a watch tick's scan canceled by pid stops
// that scan alone; the next tick scans again and catalogs the rest.
func TestAWatchCarriesOnAfterItsScanIsCanceled(t *testing.T) {
	t.Parallel()
	lib, _ := threeTracks(t, model.ModeInPlace)
	cancelScansAt(t, lib, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lib.Watch(ctx, WatchOptions{Interval: 20 * time.Millisecond}) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		items, err := lib.Query(context.Background(), query.New(query.EntityItems).Build(), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("items = %d after the watch ran on, want 3", len(items))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	list, err := lib.Jobs(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	first := list[len(list)-1]
	if first.Kind != "scan" || first.ErrorReason != jobs.ReasonCancelRequested {
		t.Errorf("first watch scan = %+v, want canceled on request", first)
	}
}

// analyzedPair opens a managed library holding two decodable tracks of one album,
// scanned, and returns it with the first track's pid.
func analyzedPair(t *testing.T) (*Library, model.PID) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	const rate = 22050
	writeRaw(t, filepath.Join(root, "a.wav"), testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 5, testaudio.MusicalPartials, 1)))
	writeRaw(t, filepath.Join(root, "b.wav"), testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 5, testaudio.AltPartials, 2)))
	lib, err := OpenSeeded(t, ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).OrderBy("title", false).Build(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %d (err %v), want 2", len(items), err)
	}
	if _, err := lib.EditManyFields(ctx, []model.PID{items[0].PID, items[1].PID}, map[string]string{"album": "Shared", "album_artist": "Band"},
		EditOptions{}); err != nil {
		t.Fatal(err)
	}
	return lib, items[0].PID
}

// cancelOnAnalysis cancels the newest job just before the pass stores its nth file.
type cancelOnAnalysis struct {
	analyze.Store
	t    *testing.T
	lib  *Library
	n    int32
	puts atomic.Int32
}

func (c *cancelOnAnalysis) PutAnalysis(ctx context.Context, in model.AnalysisInput) error {
	if c.puts.Add(1) == c.n {
		cancelNewestJob(c.t, c.lib)
	}
	return c.Store.PutAnalysis(ctx, in)
}

// TestCancelJobEndsABackgroundAnalyze: an analyze pass canceled by pid records what it
// measured, and the album gain of what it measured is brought up to date.
func TestCancelJobEndsABackgroundAnalyze(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, first := analyzedPair(t)
	lib.analyzer = analyze.New(&cancelOnAnalysis{Store: lib.store, t: t, lib: lib, n: 2}, lib.decoder, lib.fpcalc, lib.log)
	pid, err := lib.StartAnalyze(ctx, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var res analyze.Result
	canceledOnRequest(t, waitJobEnd(t, lib, pid), &res)
	if res.Analyzed != 1 || res.LoudnessMeasured != 1 {
		t.Errorf("analyze result = %+v, want the one file measured", res)
	}
	ld, err := lib.Loudness(ctx, first)
	if err != nil || !ld.HasAlbum {
		t.Errorf("loudness = %+v (err %v), want the album gain refreshed for the measured track", ld, err)
	}
}

// TestCancelJobEndsABackgroundEnrich: an enrichment canceled by pid records what it did.
func TestCancelJobEndsABackgroundEnrich(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeRaw(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3WithAudio("One", "First Artist", "One", 1, testaudio.AudioWithSeed(1)))
	writeRaw(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3WithAudio("Two", "Second Artist", "Two", 1, testaudio.AudioWithSeed(2)))
	var lib *Library
	var asked atomic.Int32
	art := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(ctx context.Context, _ enrich.Request) (*enrich.Candidate, error) {
			if asked.Add(1) == 2 {
				cancelNewestJob(t, lib)
				return nil, ctx.Err()
			}
			return nil, nil
		}}
	lib, err := OpenSeeded(t, ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}, EnrichmentProviders: []enrich.Provider{art}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	phases := []model.EnrichPhase{model.EnrichPhaseArtistArt}
	pid, err := lib.StartEnrich(ctx, EnrichOptions{Phases: phases, ForcePhases: phases})
	if err != nil {
		t.Fatal(err)
	}
	var res enrich.Result
	canceledOnRequest(t, waitJobEnd(t, lib, pid), &res)
}

// TestCancelJobEndsABackgroundOrganize: an organize canceled by pid after its first move
// records that move, which stays made.
func TestCancelJobEndsABackgroundOrganize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _ := threeTracks(t, model.ModeManaged)
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	cancelJobsAt(t, lib, 1)
	pid, err := lib.RunOrganize(ctx, query.New(query.EntityItems).Build(), OrganizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var res organize.RunResult
	canceledOnRequest(t, waitJobEnd(t, lib, pid), &res)
	if res.Report.Moved != 1 {
		t.Errorf("organize report = %+v, want the one move made before the cancel", res.Report)
	}
}

// TestCancelJobEndsABackgroundKindChange: a kind change canceled by pid records the reads it
// made.
func TestCancelJobEndsABackgroundKindChange(t *testing.T) {
	t.Parallel()
	lib, _, _, pids := kindChapters(t, 3, model.MediaAudiobook)
	cancelJobsAt(t, lib, 2)
	pid, err := lib.StartSetItemKind(context.Background(), pids, model.KindBook, KindOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var rep KindReport
	canceledOnRequest(t, waitJobEnd(t, lib, pid), &rep)
	if rep.Reads == 0 || rep.Reads == len(pids) {
		t.Errorf("kind report = %+v, want some of the %d reads made", rep, len(pids))
	}
}

// TestACanceledRemoveRootFinishesWhenAskedAgain: a root removal canceled by pid ends
// canceled, leaves the library standing, and a second removal finishes it.
func TestACanceledRemoveRootFinishesWhenAskedAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := twoRootLibrary(t)
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gone := libs[1].PID
	plain := lib.jobs
	cancelJobsAt(t, lib, 0)
	pid, err := lib.StartRemoveRoot(ctx, gone, RemoveRootOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if job := waitJobEnd(t, lib, pid); job.State != model.JobCanceled || job.ErrorReason != jobs.ReasonCancelRequested {
		t.Fatalf("removal = %+v, want canceled on request", job)
	}
	if !hasLibrary(t, lib, gone) {
		t.Fatal("a canceled removal took the library out")
	}
	lib.jobs = plain
	if _, err := lib.RemoveRoot(ctx, gone, RemoveRootOptions{Force: true}); err != nil {
		t.Fatalf("removal asked again: %v", err)
	}
	if hasLibrary(t, lib, gone) {
		t.Error("the second removal left the library standing")
	}
}

func hasLibrary(t *testing.T, lib *Library, pid model.PID) bool {
	t.Helper()
	libs, err := lib.Libraries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range libs {
		if l.PID == pid {
			return true
		}
	}
	return false
}

// TestCancelJobEndsABackgroundUnfold: an unfold canceled by pid while it reads files again
// records the folds it forgot.
func TestCancelJobEndsABackgroundUnfold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, db := t.TempDir(), filepath.Join(t.TempDir(), "catalog.db")
	writeRaw(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3WithAudio("One", "The Beatles", "One", 1, testaudio.AudioWithSeed(1)))
	writeRaw(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3WithAudio("Two", "Beatles", "Two", 1, testaudio.AudioWithSeed(2)))
	lib, err := OpenSeeded(t, ctx, Options{DBPath: db, Roots: []config.Root{{Path: root, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Merge(ctx, model.MergeArtist, artistNamed(t, db, "The Beatles"), artistNamed(t, db, "Beatles")); err != nil {
		t.Fatal(err)
	}
	lib.roots = fsx.RootChecker{Stat: func(string) error { cancelNewestJob(t, lib); return nil }}
	pid, err := lib.StartUnfold(ctx, model.MergeArtist, []string{"beatles"}, UnfoldOptions{ReResolve: true})
	if err != nil {
		t.Fatal(err)
	}
	var rep UnfoldReport
	canceledOnRequest(t, waitJobEnd(t, lib, pid), &rep)
	if rep.Folds != 1 || len(rep.Skipped) != 0 {
		t.Errorf("unfold report = %+v, want the fold forgotten and nothing skipped", rep)
	}
}

// artistNamed reads an artist's pid by name straight from the catalog file.
func artistNamed(t *testing.T, db, name string) model.PID {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var pid string
	if err := raw.QueryRow("SELECT pid FROM artist WHERE name = ?", name).Scan(&pid); err != nil {
		t.Fatalf("artist %q: %v", name, err)
	}
	return model.PID(pid)
}

// TestCancelJobRefusals: an unknown job is not found, a finished one needs nothing, a
// running row with no job here is a conflict, and a read-only library cancels nothing.
func TestCancelJobRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := OpenSeeded(t, ctx, Options{DBPath: db, Roots: []config.Root{{Path: t.TempDir(), Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if err := lib.CancelJob(ctx, model.NewPID()); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown job = %v, want not found", err)
	}
	res, err := lib.Scan(ctx, ScanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.CancelJob(ctx, res.JobPID); err != nil {
		t.Errorf("finished job = %v, want nil", err)
	}
	elsewhere := &model.Job{Kind: "scan", Scope: "fs-mutate", State: model.JobRunning, Owner: "another", StartedAt: 1, HeartbeatAt: 1}
	if err := lib.store.CreateJob(ctx, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := lib.CancelJob(ctx, elsewhere.PID); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("a running row no job here runs = %v, want a conflict", err)
	}
	ro, err := Open(ctx, Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.CancelJob(ctx, res.JobPID); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("read-only cancel = %v, want unsupported", err)
	}
}

// blockedPut holds each track put until its scan's context ends, telling entered.
type blockedPut struct {
	model.Catalog
	entered chan struct{}
}

func (b blockedPut) PutScannedTrack(ctx context.Context, _ model.PutScannedTrackInput) (*model.ScanItemResult, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestCloseStopsABackgroundScan: Close cancels a background job rather than waiting for it
// to end on its own, and the job is recorded as interrupted before the catalog closes.
func TestCloseStopsABackgroundScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _ := threeTracks(t, model.ModeInPlace)
	entered := make(chan struct{}, 1)
	lib.scanner = scan.New(blockedPut{Catalog: lib.store, entered: entered}, meta.NewReader(), lib.log)
	pid, err := lib.StartScan(ctx, ScanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- lib.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close waited on the background scan")
	}
	ro, err := Open(ctx, Options{DBPath: lib.opts.DBPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if job, err := ro.Job(ctx, pid); err != nil || job.State != model.JobCanceled || job.ErrorReason != "" {
		t.Errorf("scan after Close = %+v (err %v), want interrupted", job, err)
	}
}

// TestACanceledImportLeavesNoPlacedFileUncataloged: an import canceled as its first file is
// cataloged still catalogs that file, which is already in the library, and stops before
// the next.
func TestACanceledImportLeavesNoPlacedFileUncataloged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	lib, err := OpenSeeded(t, ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	for i, title := range []string{"One", "Two"} {
		writeRaw(t, filepath.Join(staging, strings.ToLower(title)+".mp3"),
			testaudio.BuildMP3WithAudio(title, "Band", "Record", i+1, testaudio.AudioWithSeed(byte(60+i))))
	}
	plan, err := lib.PlanImport(ctx, ImportRequest{Source: staging})
	if err != nil {
		t.Fatal(err)
	}
	cataloger := scan.New(cancelOnPut{Catalog: lib.store, t: t, lib: lib, n: 1, puts: new(atomic.Int32)}, meta.NewReader(), lib.log)
	lib.importer = inbox.New(lib.store, meta.NewReader(), cataloger, lib.log)
	rep, err := lib.ApplyImport(ctx, plan)
	if waxerr.ReasonOf(err) != jobs.ReasonCancelRequested {
		t.Fatalf("import = %+v (err %v), want canceled on request", rep, err)
	}
	if rep.Imported != 1 || rep.Errored != 0 {
		t.Errorf("import report = %+v, want the first file imported and nothing errored", rep)
	}
	var placed int
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".mp3") {
			placed++
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || placed != 1 || len(items) != placed {
		t.Errorf("%d files placed, %d items cataloged (err %v), want the one placed file cataloged", placed, len(items), err)
	}
	if _, err := os.Stat(filepath.Join(staging, "two.mp3")); err != nil {
		t.Errorf("the second file left the staging folder: %v", err)
	}
}

// TestACanceledAssignedImportFilesWhatLandedByItsAssignment: an import of assigned parts
// canceled as its first part is cataloged files that part into the assigned book, never one
// its tags name, and stops before the next.
func TestACanceledAssignedImportFilesWhatLandedByItsAssignment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, err := OpenSeeded(t, ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: t.TempDir(), Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	upload := t.TempDir()
	var files []AcquiredFile
	for i, title := range []string{"One", "Two"} {
		p := filepath.Join(upload, strings.ToLower(title)+".mp3")
		writeRaw(t, p, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: title, Audio: testaudio.AudioWithSeed(byte(80 + i))}))
		files = append(files, AcquiredFile{Path: p, Part: i + 1})
	}
	res, err := lib.ImportAcquiredUnit(ctx, files, model.KindBook, AcquiredMeta{Book: &AcquiredBook{Title: "Dune", Author: "Frank Herbert"}})
	if err != nil {
		t.Fatal(err)
	}
	cataloger := scan.New(cancelOnPut{Catalog: lib.store, t: t, lib: lib, n: 1, puts: new(atomic.Int32)}, meta.NewReader(), lib.log)
	lib.importer = inbox.New(lib.store, meta.NewReader(), cataloger, lib.log)
	if _, err := lib.ApplyImport(ctx, res.Plan); waxerr.ReasonOf(err) != jobs.ReasonCancelRequested {
		t.Fatalf("import = %v, want canceled on request", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Kind != model.KindBook || items[0].Title != "Dune" {
		t.Fatalf("items = %+v (err %v), want the one landed part in Dune", items, err)
	}
}

// TestACanceledWithdrawalLeavesTheAssignment: a withdrawal the cancel cuts off before its
// read lands leaves the assignment and the part where they were, for the withdrawal asked
// again to finish.
func TestACanceledWithdrawalLeavesTheAssignment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, _, pids := kindChapters(t, 3, model.MediaAudiobook)
	files, err := lib.ItemFiles(ctx, pids[0])
	if err != nil {
		t.Fatal(err)
	}
	book := pids[0]
	stray := files[0]
	if got, err := lib.store.ItemByPID(ctx, book); err != nil || got.Kind != model.KindBook {
		// kindChapters scans the chapters as tracks; make them the book they name first.
		if _, err := lib.SetItemKind(ctx, pids, model.KindBook, KindOptions{}); err != nil {
			t.Fatal(err)
		}
		if files, err = lib.ItemFiles(ctx, book); err != nil {
			t.Fatal(err)
		}
		stray = files[2]
	}
	if _, err := lib.AssignBookParts(ctx, []BookPartChange{{FilePID: stray.FilePID, BookPID: book, Part: 7}}); err != nil {
		t.Fatal(err)
	}
	cancelScansAt(t, lib, 1)
	if _, err := lib.AssignBookParts(ctx, []BookPartChange{{FilePID: stray.FilePID}}); waxerr.ReasonOf(err) != jobs.ReasonCancelRequested {
		t.Fatalf("withdrawal = %v, want canceled on request", err)
	}
	if ap, err := lib.store.AssignedPart(ctx, stray.Path, ""); err != nil || ap == nil || ap.Position == nil || *ap.Position != 7 {
		t.Fatalf("assignment after the cut-off withdrawal = %+v (err %v), want it standing at 7", ap, err)
	}
}
