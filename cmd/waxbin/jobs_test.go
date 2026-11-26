package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testcatalog"
	"github.com/colespringer/waxbin/internal/testsock"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestJobsNamesWhatAJobTargeted: a scan of one library shows that library as its
// target in both outputs, and a scan of every library shows none.
func TestJobsNamesWhatAJobTargeted(t *testing.T) {
	t.Parallel()
	db, root := testcatalog.Seed(t, filepath.Join(t.TempDir(), "catalog.db")), t.TempDir()
	if _, err := runCLIJSON(t, db, root, "scan"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	out, err := runCLIJSON(t, db, root, "library", "list")
	if err != nil {
		t.Fatalf("library list: %v", err)
	}
	var libs struct {
		Data []struct {
			PID string `json:"pid"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &libs); err != nil || len(libs.Data) != 1 {
		t.Fatalf("library list printed %q (err %v), want one library", out, err)
	}
	pid := libs.Data[0].PID
	if _, err := runCLIJSON(t, db, root, "scan", "--library", pid); err != nil {
		t.Fatalf("scoped scan: %v", err)
	}

	out, err = runCLIJSON(t, db, root, "jobs")
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	var jobs struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &jobs); err != nil || len(jobs.Data) != 2 {
		t.Fatalf("jobs printed %q (err %v), want two", out, err)
	}
	if j := jobs.Data[0]; j["targetType"] != "library" || j["targetPid"] != pid {
		t.Errorf("scoped scan job = %v, want library:%s", j, pid)
	}
	if j := jobs.Data[1]; j["targetType"] != nil || j["targetPid"] != nil {
		t.Errorf("whole scan job = %v, want no target keys", j)
	}

	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", db, "--root", root + ":managed:waxbin-native", "jobs"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs text: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "TARGET") || !strings.Contains(lines[1], "library:"+pid) ||
		strings.Contains(lines[2], "library:") {
		t.Errorf("jobs text =\n%s\nwant a TARGET column naming library:%s on the scoped scan alone", stdout.String(), pid)
	}
}

// TestJobErrorExitsWithTheJobsClass: a tailed job that failed answers with the class and
// reason its row kept, so the CLI exits as the run itself would have; a row with none
// (a crashed job) reads as internal.
func TestJobErrorExitsWithTheJobsClass(t *testing.T) {
	t.Parallel()
	err := jobError(&model.Job{Kind: "set-kind", State: model.JobFailed, Error: "item is locked",
		ErrorCode: waxerr.CodeLocked, ErrorReason: "kind_locked"})
	if !waxerr.Is(err, waxerr.CodeLocked) || waxerr.ReasonOf(err) != "kind_locked" || !strings.Contains(err.Error(), "item is locked") {
		t.Errorf("failed job error = %v (reason %q), want locked with its reason", err, waxerr.ReasonOf(err))
	}
	if got, want := exitCodeFor(err), exitCodeFor(waxerr.New(waxerr.CodeLocked, "x", "y")); got != want {
		t.Errorf("exit = %d, want %d, the locked class's", got, want)
	}
	if err := jobError(&model.Job{Kind: "scan", State: model.JobCrashed, Error: "reclaimed: owner not live"}); !waxerr.Is(err, waxerr.CodeInternal) {
		t.Errorf("crashed job error = %v, want internal", err)
	}
	if err := jobError(&model.Job{Kind: "scan", State: model.JobCanceled}); !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Errorf("canceled job error = %v, want canceled", err)
	}
}

// TestJobsJSONCarriesTheErrorClass: the job listing's JSON names a failed job's class and
// reason beside its message.
func TestJobsJSONCarriesTheErrorClass(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(jobViews([]*model.Job{{PID: "J", Kind: "set-kind", State: model.JobFailed, Error: "locked",
		ErrorCode: waxerr.CodeLocked, ErrorReason: "kind_locked"}}))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"errorCode":"locked"`) || !strings.Contains(s, `"errorReason":"kind_locked"`) {
		t.Errorf("jobs json = %s, want the error class and reason", s)
	}
}

// heldCatalog opens a catalog over root holding a track by each of answered+1 artists,
// whose enrichment provider answers the first answered requests and holds the next until
// its job is canceled, served on sock when it is set, and returns it with the channel the
// provider signals on when it starts holding.
func heldCatalog(t *testing.T, db, root, sock string, answered int32) (*waxbin.Library, <-chan struct{}) {
	t.Helper()
	ctx := context.Background()
	for i := range answered + 1 {
		writeTestFile(t, filepath.Join(root, fmt.Sprintf("%d.mp3", i)),
			testaudio.BuildMP3WithAudio("One", fmt.Sprintf("Artist %d", i), "Held", 1, testaudio.AudioWithSeed(byte(i))))
	}
	asked := make(chan struct{}, 1)
	var calls atomic.Int32
	held := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapArtistArt,
		EnrichFunc: func(ctx context.Context, _ enrich.Request) (*enrich.Candidate, error) {
			if calls.Add(1) <= answered {
				return nil, nil
			}
			select {
			case asked <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	opts := waxbin.Options{DBPath: db, Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		EnrichmentProviders: []enrich.Provider{held}, IPCSocket: sock}
	var lib *waxbin.Library
	if sock != "" {
		lib = serveCatalog(t, opts)
	} else {
		var err error
		if lib, err = openSeeded(t, ctx, opts); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lib.Close() })
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	return lib, asked
}

// heldEnrichment starts an enrichment on a heldCatalog and returns it once the provider
// holds it.
func heldEnrichment(t *testing.T, db, root, sock string) (*waxbin.Library, model.PID) {
	t.Helper()
	lib, asked := heldCatalog(t, db, root, sock, 0)
	phases := []model.EnrichPhase{model.EnrichPhaseArtistArt}
	job, err := lib.StartEnrich(context.Background(), waxbin.EnrichOptions{Phases: phases, ForcePhases: phases})
	if err != nil {
		t.Fatal(err)
	}
	<-asked
	return lib, job
}

// TestTailJobSaysHowToCancel: interrupting a command that follows a server's job says the
// job runs on and how to stop it.
func TestTailJobSaysHowToCancel(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	lib, _ := heldCatalog(t, db, root, testsock.Path(t), 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := newRootCmd(&globals{})
	stderr := &cancelOnProgress{cancel: cancel}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{"--db", db, "--root", root + ":managed:waxbin-native", "enrich"})
	if err := cmd.ExecuteContext(ctx); !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Fatalf("interrupted enrich = %v, want canceled", err)
	}
	list, err := lib.Jobs(context.Background(), 1)
	if err != nil || len(list) == 0 || list[0].State != model.JobRunning {
		t.Fatalf("jobs = %+v (err %v), want the enrichment still running", list, err)
	}
	if want := "waxbin jobs cancel " + string(list[0].PID); !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to say %q", stderr.String(), want)
	}
}

// cancelOnProgress is a stderr that cancels the command once it prints a progress line,
// which a tailer does only while it follows a job.
type cancelOnProgress struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	cancel func()
}

func (w *cancelOnProgress) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if bytes.HasPrefix(p, []byte("\r[")) {
		w.cancel()
	}
	return w.buf.Write(p)
}

func (w *cancelOnProgress) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestJobsCancelThroughAServer: with a server running the job, jobs cancel stops it, waits
// for it to end and says how it ended, in text and as a job view under --json.
func TestJobsCancelThroughAServer(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	_, job := heldEnrichment(t, db, root, testsock.Path(t))
	out, err := runCLIJSON(t, db, root, "jobs", "cancel", string(job))
	if err != nil {
		t.Fatalf("jobs cancel: %v", err)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("jobs cancel printed %q: %v", out, err)
	}
	if env.Data["pid"] != string(job) || env.Data["state"] != "canceled" || env.Data["errorReason"] != string(jobs.ReasonCancelRequested) {
		t.Fatalf("jobs cancel = %v, want the job canceled on request", env.Data)
	}
	text, err := runCLIText(t, db, root, "jobs", "cancel", string(job))
	if err != nil || !strings.Contains(text, "had already ended: canceled") {
		t.Errorf("jobs cancel of the ended job = %q (err %v), want it said to have ended already", text, err)
	}
}

// TestJobsCancelWithNoServer: with no server, a job that has ended says so, and a running one
// is a conflict, since it runs in another process.
func TestJobsCancelWithNoServer(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	lib, job := heldEnrichment(t, db, root, "")
	_, err := runCLIText(t, db, root, "jobs", "cancel", string(job))
	if !waxerr.Is(err, waxerr.CodeConflict) || !strings.Contains(err.Error(), "another process") {
		t.Fatalf("jobs cancel of another process's job = %v, want a conflict saying where it runs", err)
	}
	if err := lib.CancelJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	for {
		row, err := lib.Job(context.Background(), job)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != model.JobRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	text, err := runCLIText(t, db, root, "jobs", "cancel", string(job))
	if err != nil || !strings.Contains(text, "already ended") {
		t.Errorf("jobs cancel of an ended job = %q (err %v), want it said to have ended", text, err)
	}
	if _, err := runCLIText(t, db, root, "jobs", "cancel", string(model.NewPID())); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("jobs cancel of an unknown pid = %v, want not found", err)
	}
}

// TestCancelOutcomeSaysWhatTheCancelDid: jobs cancel names a job its cancel ended as canceled,
// one that ended another way after the cancel was sent as having ended before it took effect,
// and one it found ended as ended already.
func TestCancelOutcomeSaysWhatTheCancelDid(t *testing.T) {
	t.Parallel()
	canceled := &model.Job{PID: "J", Kind: "scan", State: model.JobCanceled, ErrorReason: jobs.ReasonCancelRequested, Progress: 0.4}
	done := &model.Job{PID: "J", Kind: "scan", State: model.JobDone, Progress: 1}
	for _, c := range []struct {
		job  *model.Job
		sent bool
		want string
	}{
		{canceled, true, "Job J (scan) canceled at 40%"},
		{done, true, "Job J (scan) ended before the cancel took effect: done"},
		{done, false, "Job J (scan) had already ended: done"},
		{canceled, false, "Job J (scan) had already ended: canceled"},
	} {
		if got := cancelOutcome(c.job, c.sent); got != c.want {
			t.Errorf("cancelOutcome(%s, sent %v) = %q, want %q", c.job.State, c.sent, got, c.want)
		}
	}
}
