package waxbin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// serveLib starts Serve on an already-open read-write library in the background and
// tears it down at cleanup.
func serveLib(t *testing.T, ctx context.Context, lib *waxbin.Library, sock string) {
	t.Helper()
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- lib.Serve(sctx, sock) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve returned: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serve did not stop")
		}
		_ = lib.Close()
	})
}

// openServedRW opens a read-write managed library advertising a control socket
// (without scanning) and starts Serve. The caller drives the catalog.
func openServedRW(t *testing.T, ctx context.Context, db, root, sock string) *waxbin.Library {
	t.Helper()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:    db,
		Roots:     []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		IPCSocket: sock,
	})
	if err != nil {
		t.Fatalf("open served library: %v", err)
	}
	serveLib(t, ctx, lib, sock)
	return lib
}

// openServed opens a read-write managed library advertising a control socket,
// scans root, and starts Serve in the background. It returns the library and the
// socket path; the server is stopped and the library closed at cleanup.
func openServed(t *testing.T, ctx context.Context, db, root, sock string) *waxbin.Library {
	t.Helper()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:    db,
		Roots:     []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		IPCSocket: sock,
	})
	if err != nil {
		t.Fatalf("open served library: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		_ = lib.Close()
		t.Fatalf("scan: %v", err)
	}
	serveLib(t, ctx, lib, sock)
	return lib
}

// dialWhenReady dials the server socket, retrying until it answers a ping.
func dialWhenReady(t *testing.T, sock string) *proxy.Client {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := proxy.Dial(sock)
		if err == nil {
			if perr := c.Ping(context.Background()); perr == nil {
				t.Cleanup(func() { _ = c.Close() })
				return c
			} else {
				lastErr = perr
			}
			_ = c.Close()
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server not ready on %s: %v", sock, lastErr)
	return nil
}

// TestServeProxiedMutations drives the fast-mutation proxy end to end: with a
// server holding the write lock, a client's edits, ratings, stars, and user
// creation all succeed through the socket instead of failing with CodeConflict.
func TestServeProxiedMutations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := filepath.Join(t.TempDir(), "wax.sock")
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "Original")

	// The lockfile advertises the socket, so a CLI can discover the server.
	if info, err := waxbin.ReadLockOwner(db); err != nil || info.IPCSocket != sock {
		t.Fatalf("lock owner = %+v (err %v), want IPCSocket %s", info, err, sock)
	}

	c := dialWhenReady(t, sock)

	// A field edit succeeds through the proxy while the server holds the lock.
	res, err := c.EditFields(ctx, pid, map[string]string{"artist": "New Artist"}, false, true, false)
	if err != nil {
		t.Fatalf("proxied edit: %v", err)
	}
	if len(res.WriteBackFailures) != 0 {
		t.Fatalf("unexpected write-back failures: %+v", res.WriteBackFailures)
	}
	// The catalog reflects it (read through the server's own library).
	if v, err := lib.Get(ctx, pid); err != nil || v.Artist != "New Artist" {
		t.Fatalf("catalog artist = %q (err %v), want New Artist", v.Artist, err)
	}
	// Provenance (read through the proxy) records a locked user edit.
	prov, err := c.Provenance(ctx, pid)
	if err != nil {
		t.Fatalf("proxied provenance: %v", err)
	}
	if len(prov) != 1 || prov[0].Field != "artist" || prov[0].Source != model.SourceUser || !prov[0].Locked {
		t.Fatalf("provenance = %+v, want one locked user artist row", prov)
	}

	// Play-state mutations round-trip.
	rating := 80
	if err := c.SetRating(ctx, "", pid, &rating, nil); err != nil {
		t.Fatalf("proxied rating: %v", err)
	}
	if err := c.SetStar(ctx, "", pid, true, nil); err != nil {
		t.Fatalf("proxied star: %v", err)
	}
	st, err := c.PlayState(ctx, "", pid)
	if err != nil {
		t.Fatalf("proxied play state: %v", err)
	}
	if !st.HasRating || st.Rating != 80 || !st.Starred {
		t.Fatalf("play state = %+v, want rating 80 + starred", st)
	}

	// The as-of stamp rides the wire (asOfNs): an unstar recorded far in the past,
	// older than the server-now star just applied, is skipped as a stale replay, so
	// the item stays starred. This exercises the client encode, server decode, and
	// store recorded-time guard end to end.
	oldNS := int64(1_000_000_000) // 1970, older than the server-now star above
	if err := c.SetStar(ctx, "", pid, false, &oldNS); err != nil {
		t.Fatalf("proxied stale unstar: %v", err)
	}
	if st, err = c.PlayState(ctx, "", pid); err != nil {
		t.Fatalf("proxied play state after stale unstar: %v", err)
	}
	if !st.Starred {
		t.Fatalf("stale as-of unstar was applied over the wire, want skipped (still starred)")
	}

	// User creation round-trips and is visible in the listing.
	u, err := c.CreateUser(ctx, "Bob")
	if err != nil {
		t.Fatalf("proxied create user: %v", err)
	}
	users, err := c.Users(ctx)
	if err != nil {
		t.Fatalf("proxied users: %v", err)
	}
	if !hasUser(users, u.PID, "Bob") {
		t.Fatalf("users = %+v, want one named Bob", users)
	}
}

// TestServeProxiedSmartPlaylistSetRule drives playlist_set_rule end to end: the
// rule is replaced under the same pid, membership follows on the next read, and a
// bad rule keeps its CodeInvalid class across the wire.
func TestServeProxiedSmartPlaylistSetRule(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := filepath.Join(t.TempDir(), "wax.sock")
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	c := dialWhenReady(t, sock)

	// A rule matching nothing, replaced over the socket by one matching the
	// scanned track.
	empty := query.New(query.EntityItems).Where("title", query.OpContains, "Nothing").Build()
	pid, err := lib.Playlists().CreateSmart(ctx, "Mix", "", "", empty)
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}
	if items, err := lib.Playlists().Items(ctx, pid, ""); err != nil || len(items) != 0 {
		t.Fatalf("initial membership = %d items (err %v), want 0", len(items), err)
	}

	match := query.New(query.EntityItems).Where("title", query.OpContains, "Origin").Build()
	doc, err := query.MarshalRule(match)
	if err != nil {
		t.Fatalf("marshal rule: %v", err)
	}
	if err := c.PlaylistSetRule(ctx, pid, doc); err != nil {
		t.Fatalf("proxied set-rule: %v", err)
	}
	items, err := lib.Playlists().Items(ctx, pid, "")
	if err != nil || len(items) != 1 || items[0].Title != "Original" {
		t.Fatalf("membership after proxied set-rule = %v (err %v), want [Original] under the same pid", items, err)
	}

	// A rule the store rejects keeps its class across the wire.
	bad, err := query.MarshalRule(query.New(query.EntityItems).Where("bogus", query.OpIs, "x").Build())
	if err != nil {
		t.Fatalf("marshal bad rule: %v", err)
	}
	if err := c.PlaylistSetRule(ctx, pid, bad); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("proxied bad rule err = %v, want CodeInvalid", err)
	}
}

// TestServeProxiedTranscript drives put_transcript and fetch_transcript over the
// socket: a supplied body is validated and reduced server-side, a fetch error
// keeps its class across the wire, and the stored transcript reads back through
// the server's own library.
func TestServeProxiedTranscript(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := filepath.Join(t.TempDir(), "wax.sock")

	lib := openServedRW(t, ctx, db, root, sock)
	show, err := lib.Podcasts().AddManual(ctx, "Proxied", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("add manual show: %v", err)
	}
	res, err := lib.Podcasts().AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Ep", GUID: "g1"}, true)
	if err != nil {
		t.Fatalf("add episode: %v", err)
	}
	ep := res.EpisodePID
	c := dialWhenReady(t, sock)

	srt := "1\n00:00:01,000 --> 00:00:04,000\nproxied transcript words\n"
	if err := c.PutTranscript(ctx, ep, "srt", []byte(srt), "https://h/t.srt"); err != nil {
		t.Fatalf("proxied put_transcript: %v", err)
	}
	tr, err := lib.Podcasts().Transcript(ctx, ep)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if tr.Format != "srt" || tr.SourceURL != "https://h/t.srt" {
		t.Fatalf("transcript meta = %+v", tr)
	}
	// The reduction ran server-side: cue timecodes are gone, the words are there.
	if strings.Contains(tr.Body, "-->") || !strings.Contains(tr.Body, "proxied transcript words") {
		t.Fatalf("proxied body not reduced: %q", tr.Body)
	}

	// Validation errors keep their class across the wire.
	if err := c.PutTranscript(ctx, ep, "docx", []byte("x"), ""); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("proxied bad format = %v, want CodeInvalid", err)
	}
	// fetch_transcript on an episode with no declared URL: CodeInvalid, not a
	// transport failure (the fetch would run in the server process).
	if err := c.FetchTranscript(ctx, ep); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("proxied no-url fetch = %v, want CodeInvalid", err)
	}
}

// TestServeProxiedAddRoot drives add_root over the socket: the root lands in the
// server's catalog (the process that scans), a proxied run_scan catalogs a file
// under it, and a validation failure keeps its class across the wire.
func TestServeProxiedAddRoot(t *testing.T) {
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := filepath.Join(t.TempDir(), "wax.sock")
	writeFile(t, filepath.Join(rootB, "late.mp3"), testaudio.BuildMP3("Proxied Late", "Adder", "Runtime", 1))

	lib := openServedRW(t, ctx, db, rootA, sock)
	c := dialWhenReady(t, sock)

	added, err := c.AddRoot(ctx, proxy.AddRootParams{Path: rootB, Mode: "managed"})
	if err != nil {
		t.Fatalf("proxied add_root: %v", err)
	}
	if added.PID == "" || added.DisplayRoot != rootB || added.Mode != model.ModeManaged {
		t.Fatalf("proxied add_root row = %+v, want a managed row at %s", added, rootB)
	}
	// The server's own library sees it.
	if libs, err := lib.Libraries(ctx); err != nil || len(libs) != 2 {
		t.Fatalf("server libraries = %d (err %v), want 2", len(libs), err)
	}

	// A proxied scan (run in the server process) catalogs the new root's file.
	jobPID, err := c.RunScan(ctx, proxy.ScanParams{})
	if err != nil {
		t.Fatalf("run_scan after add_root: %v", err)
	}
	waitForJobDone(t, ctx, lib, jobPID)
	items, err := lib.Query(ctx, query.New(query.EntityItems).
		Where("title", query.OpIs, "Proxied Late").Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("track under the proxied-added root: err=%v len=%d, want 1", err, len(items))
	}

	// Validation runs server-side and keeps its class on the wire.
	if _, err := c.AddRoot(ctx, proxy.AddRootParams{Path: filepath.Join(rootA, "sub"), Mode: "managed"}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("proxied overlapping add_root = %v, want CodeInvalid", err)
	}
}

// TestServeProxiedError checks a domain error keeps its code across the proxy: a
// locked field edited without force returns CodeLocked, so the CLI exit code is the
// same as a local edit.
func TestServeProxiedError(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := filepath.Join(t.TempDir(), "wax.sock")
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "Original")
	c := dialWhenReady(t, sock)

	// Lock the field, then a non-force edit of it must be refused with CodeLocked.
	if err := c.Lock(ctx, pid, []string{"artist"}); err != nil {
		t.Fatalf("proxied lock: %v", err)
	}
	_, err := c.EditFields(ctx, pid, map[string]string{"artist": "Nope"}, false, false, false)
	if !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("edit of a locked field = %v, want CodeLocked", err)
	}
}

// TestMaintenanceHandoffReopen exercises the A6b maintenance-mode cycle: the server
// yields the lock, a foreground process opens the catalog directly and writes, then
// the server reopens and sees the write.
func TestMaintenanceHandoffReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := filepath.Join(t.TempDir(), "wax.sock")
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	c := dialWhenReady(t, sock)

	dvBefore, err := lib.DataVersion(ctx)
	if err != nil {
		t.Fatalf("data version: %v", err)
	}

	// Hand off: the server closes and releases the lock.
	if err := c.MaintenanceBegin(ctx); err != nil {
		t.Fatalf("maintenance begin: %v", err)
	}

	// A direct read-write open now succeeds, proving the lock was released.
	lib2, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("direct open during maintenance should succeed: %v", err)
	}
	if _, err := lib2.CreateUser(ctx, "ViaMaintenance"); err != nil {
		_ = lib2.Close()
		t.Fatalf("foreground mutation: %v", err)
	}
	if err := lib2.Close(); err != nil { // release the lock
		t.Fatalf("close foreground lib: %v", err)
	}

	// End maintenance: the server reopens and reacquires the lock.
	if err := c.MaintenanceEnd(ctx); err != nil {
		t.Fatalf("maintenance end: %v", err)
	}

	// The reopened server sees the foreground write.
	users, err := lib.Users(ctx)
	if err != nil {
		t.Fatalf("users after reopen: %v", err)
	}
	if !hasUsernamed(users, "ViaMaintenance") {
		t.Fatalf("users after reopen = %+v, want ViaMaintenance", users)
	}
	// DataVersion advanced across the hand-off.
	if dvAfter, err := lib.DataVersion(ctx); err != nil || dvAfter == dvBefore {
		t.Fatalf("data version after reopen = %d (err %v), want != %d", dvAfter, err, dvBefore)
	}
	// The server can mutate again through the proxy.
	if _, err := c.CreateUser(ctx, "AfterReopen"); err != nil {
		t.Fatalf("proxied mutation after reopen: %v", err)
	}
	if users, _ := lib.Users(ctx); !hasUsernamed(users, "AfterReopen") {
		t.Fatalf("users = %+v, want AfterReopen", users)
	}
}

// TestServerRunJobKeepsServerUp covers the core of the A6a design. A long job
// (scan/analyze/enrich/organize) submitted to a running server runs in the server's
// process, so the server never closes and stays available. The maintenance hand-off
// would pause it instead. A CLI-embedding host such as WaxDeck must keep serving
// while a submitted scan runs.
func TestServerRunJobKeepsServerUp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := filepath.Join(t.TempDir(), "wax.sock")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3WithAudio("A", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3WithAudio("B", "Artist", "Album", 2, testaudio.AudioWithSeed(2)))
	writeFile(t, filepath.Join(root, "c.mp3"), testaudio.BuildMP3WithAudio("C", "Artist", "Album", 3, testaudio.AudioWithSeed(3)))

	lib := openServedRW(t, ctx, db, root, sock) // no initial scan; the server-run job does it
	c := dialWhenReady(t, sock)

	// Submit the scan as a server-run job; it returns a job PID immediately.
	jobPID, err := c.RunScan(ctx, proxy.ScanParams{})
	if err != nil {
		t.Fatalf("run_scan: %v", err)
	}
	if jobPID == "" {
		t.Fatal("run_scan returned an empty job pid")
	}

	// The server was NOT paused: a proxied fast mutation still succeeds (it would be
	// a CodeConflict "in maintenance" on the hand-off path), and the server's own
	// library handle is still open (it would error "store is closed" if maintenance
	// had closed it). This is the property the correction restores.
	if _, err := c.CreateUser(ctx, "DuringJob"); err != nil {
		t.Fatalf("proxied mutation while a server-run job runs: %v", err)
	}
	if _, err := lib.Users(ctx); err != nil {
		t.Fatalf("server library was closed during a server-run job: %v", err)
	}

	// Tail the job to completion through the (still-open) catalog.
	job := waitForJobDone(t, ctx, lib, jobPID)

	// The job actually ran the scan and recorded its result summary for the tailer.
	var res scan.Result
	if err := json.Unmarshal([]byte(job.Result), &res); err != nil {
		t.Fatalf("decode job result %q: %v", job.Result, err)
	}
	if res.ItemsCreated != 3 {
		t.Fatalf("scan result created = %d, want 3 (result=%q)", res.ItemsCreated, job.Result)
	}
	// The catalog reflects both the job's work and the concurrent proxied mutation.
	if n, err := lib.Count(ctx, query.New(query.EntityItems).Build(), ""); err != nil || n != 3 {
		t.Fatalf("catalog item count = %d (err %v), want 3", n, err)
	}
	if users, _ := lib.Users(ctx); !hasUsernamed(users, "DuringJob") {
		t.Fatalf("users = %+v, want DuringJob (the mutation served during the job)", users)
	}
}

// waitForJobDone polls a job until it reaches a terminal state and fails the test
// unless it finished successfully.
func waitForJobDone(t *testing.T, ctx context.Context, lib *waxbin.Library, jobPID model.PID) *model.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := lib.Job(ctx, jobPID)
		if err != nil {
			t.Fatalf("job read: %v", err)
		}
		switch job.State {
		case model.JobDone:
			return job
		case model.JobFailed, model.JobCrashed, model.JobCanceled:
			t.Fatalf("job ended %s: %s", job.State, job.Error)
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", jobPID)
	return nil
}

// TestMaintenanceRefusedWhileJobRuns verifies a maintenance hand-off is refused
// while a server-run job is in flight, rather than closing the store out from under
// the running scan (which would abort it partway). See BeginMaintenance's guard.
func TestMaintenanceRefusedWhileJobRuns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	// Enough distinct files that the scan is unmistakably still running when we check
	// (StartScan returns the moment the job row exists, before the work processes any
	// file, so the scan has all of these still to do).
	for i := 0; i < 24; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("t%02d.mp3", i)),
			testaudio.BuildMP3WithAudio(fmt.Sprintf("S%d", i), "Artist", "Album", i+1, testaudio.AudioWithSeed(byte(i+1))))
	}
	lib := openManaged(t, ctx, db, root)

	jobPID, err := lib.StartScan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("start scan: %v", err)
	}
	if jobPID == "" {
		t.Fatal("empty job pid")
	}

	// A hand-off must be refused with CodeConflict while the job runs.
	if err := lib.BeginMaintenance(ctx); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("BeginMaintenance while a job runs = %v, want CodeConflict", err)
	}

	// The scan was not disturbed: it completes normally.
	job := waitForJobDone(t, ctx, lib, jobPID)
	var res scan.Result
	if err := json.Unmarshal([]byte(job.Result), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.ItemsCreated != 24 {
		t.Fatalf("scan created = %d, want 24 (the refused hand-off must not abort it)", res.ItemsCreated)
	}
}

// TestSubscriberSurvivesMaintenance verifies an in-process change subscription
// survives a maintenance hand-off: BeginMaintenance suspends the store (keeping
// subscribers) rather than closing it, so after EndMaintenance the embedder's
// channel still delivers deltas for subsequent writes.
func TestSubscriberSurvivesMaintenance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := filepath.Join(t.TempDir(), "wax.sock")
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "Original")

	ch, cancel := lib.Subscribe()
	defer cancel()

	c := dialWhenReady(t, sock)
	if err := c.MaintenanceBegin(ctx); err != nil {
		t.Fatalf("maintenance begin: %v", err)
	}
	lib2, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("foreground open: %v", err)
	}
	if _, err := lib2.CreateUser(ctx, "fg"); err != nil {
		_ = lib2.Close()
		t.Fatalf("foreground mutation: %v", err)
	}
	_ = lib2.Close()
	if err := c.MaintenanceEnd(ctx); err != nil {
		t.Fatalf("maintenance end: %v", err)
	}

	// A write through the reopened server library must still publish to the
	// subscription (the channel was not closed by the hand-off).
	if err := lib.EditField(ctx, pid, "genre", "PostMaint", waxbin.EditOptions{Lock: true}); err != nil {
		t.Fatalf("post-maintenance edit: %v", err)
	}
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("subscription channel was closed by the maintenance hand-off")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no change delivered after maintenance; the subscription was lost")
	}
}

func hasUser(users []*model.User, pid model.PID, name string) bool {
	for _, u := range users {
		if u.PID == pid && u.Name == name {
			return true
		}
	}
	return false
}

func hasUsernamed(users []*model.User, name string) bool {
	for _, u := range users {
		if u.Name == name {
			return true
		}
	}
	return false
}
