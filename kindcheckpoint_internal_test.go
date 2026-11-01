package waxbin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// watchedJobs records a kind change's checkpoints and lets a test look at the catalog right
// after the nth checkpoint or before the nth file's read (its heartbeat).
type watchedJobs struct {
	model.JobStore
	mu         sync.Mutex
	saved      []KindReport
	beats      int
	checkpoint func(n int)
	beforeRead func(n int)
}

func (s *watchedJobs) SaveJobResult(ctx context.Context, pid model.PID, result json.RawMessage) error {
	if err := s.JobStore.SaveJobResult(ctx, pid, result); err != nil {
		return err
	}
	var rep KindReport
	if err := json.Unmarshal(result, &rep); err != nil {
		return err
	}
	s.mu.Lock()
	s.saved = append(s.saved, rep)
	n := len(s.saved)
	s.mu.Unlock()
	if s.checkpoint != nil {
		s.checkpoint(n)
	}
	return nil
}

func (s *watchedJobs) Heartbeat(ctx context.Context, jobID, ts int64, progress float64, msg string) error {
	if err := s.JobStore.Heartbeat(ctx, jobID, ts, progress, msg); err != nil {
		return err
	}
	s.mu.Lock()
	s.beats++
	n := s.beats
	s.mu.Unlock()
	if s.beforeRead != nil {
		s.beforeRead(n)
	}
	return nil
}

// kindChapters opens a mixed root holding n chapters of one book, each scanned as a track
// of its own, and returns the library, the database path, the root and the tracks in order.
// With media set the root is then declared that, unscanned, as a library moved onto
// audiobooks is before its next scan.
func kindChapters(t *testing.T, n int, media model.MediaType) (*Library, string, string, []model.PID) {
	t.Helper()
	ctx := context.Background()
	root, db := t.TempDir(), filepath.Join(t.TempDir(), "catalog.db")
	if err := os.MkdirAll(filepath.Join(root, "Jane Austen", "Persuasion"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		writeRaw(t, filepath.Join(root, "Jane Austen", "Persuasion", fmt.Sprintf("%02d.mp3", i)), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: fmt.Sprintf("Chapter %02d", i), Artist: "Jane Austen", AlbumArtist: "Jane Austen", Album: "Persuasion",
			Genre: "Speech", Track: i, Audio: testaudio.AudioWithSeed(byte(i * 13))}))
	}
	lib, err := Open(ctx, Options{DBPath: db, Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).OrderBy("title", false).Build(), "")
	if err != nil || len(items) != n {
		t.Fatalf("items = %d (err %v), want %d", len(items), err, n)
	}
	pids := make([]model.PID, n)
	for i, it := range items {
		pids[i] = it.PID
	}
	if media != "" {
		if err := lib.Close(); err != nil {
			t.Fatal(err)
		}
		if lib, err = Open(ctx, Options{DBPath: db, Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Media: media,
			Profile: "waxbin-native"}}}); err != nil {
			t.Fatalf("reopen: %v", err)
		}
		t.Cleanup(func() { _ = lib.Close() })
	}
	return lib, db, root, pids
}

// watchJobs swaps the library's job manager for one whose store the test watches.
func watchJobs(lib *Library) *watchedJobs {
	st := &watchedJobs{JobStore: lib.store}
	lib.jobs = jobs.NewManager(st, "test", lib.log)
	return st
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// snapshotAt copies the catalog at db, as it stands, to a new file: the catalog a crash at
// that moment leaves.
func snapshotAt(t *testing.T, db string) string {
	t.Helper()
	snapshot := filepath.Join(t.TempDir(), "crashed.db")
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(db+suffix, snapshot+suffix); err != nil {
			t.Errorf("snapshot %s: %v", suffix, err)
		}
	}
	return snapshot
}

func openCrashed(t *testing.T, snapshot, root string, media model.MediaType) *Library {
	t.Helper()
	lib, err := Open(context.Background(), Options{DBPath: snapshot,
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Media: media, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open the crashed catalog: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	return lib
}

// lastJob reads the newest job and its report.
func lastJob(t *testing.T, lib *Library) (*model.Job, KindReport) {
	t.Helper()
	list, err := lib.Jobs(context.Background(), 1)
	if err != nil || len(list) == 0 {
		t.Fatalf("jobs = %+v (err %v)", list, err)
	}
	var rep KindReport
	if list[0].Result != "" {
		if err := json.Unmarshal([]byte(list[0].Result), &rep); err != nil {
			t.Fatalf("result %q: %v", list[0].Result, err)
		}
	}
	return list[0], rep
}

// TestAKindChangeCheckpointsOnItsCadence: a kind change records its targets when it starts
// and then, every so many files at a target's end, how many targets it has finished, which
// its row holds while the change goes on.
func TestAKindChangeCheckpointsOnItsCadence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, _, pids := kindChapters(t, 6, "")
	lib.kindCheckpointEvery, lib.kindCheckpointAfter = 2, time.Hour
	st := watchJobs(lib)
	st.checkpoint = func(n int) {
		if n != 2 {
			return
		}
		list, err := lib.store.ListJobs(ctx, 1)
		if err != nil || len(list) != 1 || list[0].State != model.JobRunning {
			t.Errorf("jobs = %+v (err %v), want the running set-kind job", list, err)
			return
		}
		var row KindReport
		var params kindParams
		if err := json.Unmarshal([]byte(list[0].Result), &row); err != nil || row.Reads != 4 {
			t.Errorf("row mid-change = %q (err %v), want four targets read", list[0].Result, err)
		}
		if err := json.Unmarshal([]byte(list[0].Params), &params); err != nil || !slices.Equal(params.Targets, pids) || params.Kind != model.KindBook {
			t.Errorf("params = %q (err %v), want the targets and the kind", list[0].Params, err)
		}
	}
	rep, err := lib.SetItemKind(ctx, pids, model.KindBook, KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	var reads []int
	for _, c := range st.saved {
		reads = append(reads, c.Reads)
		if len(c.Absorbed)+len(c.Created)+len(c.Converted)+len(c.Targets) != 0 {
			t.Errorf("checkpoint = %+v, want only how far the change got", c)
		}
	}
	if !slices.Equal(reads, []int{2, 4, 6}) {
		t.Errorf("checkpoint reads = %v, want 2, 4 and 6", reads)
	}
	if rep.Reads != 6 || !slices.Equal(rep.Targets, pids) || !slices.Equal(rep.Converted, pids[:1]) || len(rep.Absorbed) != 5 {
		t.Errorf("report = %+v, want every target read, chapter 1 converted and the rest absorbed", rep)
	}
}

// TestAKindChangeWritesNoCheckpointPerFile: at the default cadence a short change writes its
// progress once at its end, and otherwise only as each second passes, never once a file.
func TestAKindChangeWritesNoCheckpointPerFile(t *testing.T) {
	t.Parallel()
	lib, _, _, pids := kindChapters(t, 6, "")
	st := watchJobs(lib)
	began := time.Now()
	if _, err := lib.SetItemKind(context.Background(), pids, model.KindBook, KindOptions{}); err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if n, most := len(st.saved), 1+int(time.Since(began)/time.Second); n == 0 || n > most || n >= len(pids) {
		t.Errorf("checkpoints = %d for %d files, want one at the end and at most one a second", n, len(pids))
	}
}

// crashBeforeRead runs a kind change of the six chapters to a book, in a root now declared
// audiobook so no read pins a kind its rule would not give, with a checkpoint every two
// files, and copies the catalog as it stands just before the nth file's read: the catalog a
// crash at that moment leaves.
func crashBeforeRead(t *testing.T, n int) (snapshot, root string, pids []model.PID) {
	t.Helper()
	lib, db, root, pids := kindChapters(t, 6, model.MediaAudiobook)
	lib.kindCheckpointEvery, lib.kindCheckpointAfter = 2, time.Hour
	st := watchJobs(lib)
	st.beforeRead = func(k int) {
		if k == n {
			snapshot = snapshotAt(t, db)
		}
	}
	if _, err := lib.SetItemKind(context.Background(), pids, model.KindBook, KindOptions{}); err != nil {
		t.Fatalf("set kind: %v", err)
	}
	return snapshot, root, pids
}

// TestAReclaimedKindChangeReportsWhatItDid: a kind change cut off by a crash between two
// checkpoints is reclaimed as crashed with its report completed from what its writes
// recorded: every target it folded away and where it went, whatever the crash's moment, and
// the targets it converted, pinned.
func TestAReclaimedKindChangeReportsWhatItDid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	snapshot, root, pids := crashBeforeRead(t, 6)
	lib := openCrashed(t, snapshot, root, model.MediaAudiobook)
	job, rep := lastJob(t, lib)
	if job.Kind != "set-kind" || job.State != model.JobCrashed {
		t.Fatalf("job = %+v, want the set-kind job reclaimed as crashed", job)
	}
	want := map[model.PID]model.PID{pids[1]: pids[0], pids[2]: pids[0], pids[3]: pids[0], pids[4]: pids[0]}
	if rep.Reads != 4 || !slices.Equal(rep.Targets, pids) || !slices.Equal(rep.Converted, pids[:1]) || !maps.Equal(rep.Absorbed, want) {
		t.Errorf("reclaimed report = %+v, want four read at the last checkpoint, chapter 1 converted, 2 to 5 absorbed", rep)
	}
	if v, err := lib.Get(ctx, pids[5]); err != nil || v.Kind != model.KindTrack {
		t.Errorf("chapter 6 = %+v (err %v), want it still a track", v, err)
	}
	rows, err := lib.Provenance(ctx, pids[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rows, func(r model.FieldProvenance) bool { return r.Field == model.KindLockField && r.Locked }) {
		t.Errorf("provenance = %+v, want the book the crashed change converted pinned", rows)
	}
}

// TestARetryAfterACrashCompletesTheChange: the same kind change asked again after a crash
// completes it: a target the crashed change absorbed answers with where it went, a target it
// had not read is read now, and the book it converted is only pinned (the crashed change's
// report names its conversion).
func TestARetryAfterACrashCompletesTheChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	snapshot, root, pids := crashBeforeRead(t, 6)
	lib := openCrashed(t, snapshot, root, model.MediaAudiobook)
	rep, err := lib.SetItemKind(ctx, pids, model.KindBook, KindOptions{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, p := range pids[1:] {
		if rep.Absorbed[p] != pids[0] {
			t.Errorf("retry absorbed = %v, want %s mapped to the book %s", rep.Absorbed, p, pids[0])
		}
	}
	if len(rep.Converted) != 0 || !slices.Equal(rep.Targets, []model.PID{pids[0], pids[5]}) || rep.Reads != 2 {
		t.Errorf("retry report = %+v, want the book and the unread chapter its targets, nothing converted", rep)
	}
	if files, err := lib.ItemFiles(ctx, pids[0]); err != nil || len(files) != 6 {
		t.Errorf("book files = %d (err %v), want all six chapters", len(files), err)
	}
}

// TestASplitCutOffMidBookNamesEveryPart: a book split into tracks, a crash landing after
// three of its parts were read, is reclaimed with each track already made named with the
// book it came from, and the change asked again names the tracks it makes, so the two
// reports name every track the split made.
func TestASplitCutOffMidBookNamesEveryPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, db, root, _ := kindChapters(t, 6, model.MediaAudiobook)
	if _, err := lib.Scan(ctx, ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Kind != model.KindBook {
		t.Fatalf("items = %+v (err %v), want one book", items, err)
	}
	book := items[0].PID
	var snapshot string
	st := watchJobs(lib)
	st.beforeRead = func(k int) {
		if k == 4 {
			snapshot = snapshotAt(t, db)
		}
	}
	if _, err := lib.SetItemKind(ctx, []model.PID{book}, model.KindTrack, KindOptions{}); err != nil {
		t.Fatalf("set kind: %v", err)
	}
	crashed, err := Open(ctx, Options{DBPath: snapshot, Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = crashed.Close() })
	_, rep := lastJob(t, crashed)
	if len(rep.Created) != 3 || rep.Reads != 0 {
		t.Errorf("reclaimed report = %+v, want the three tracks made before the crash, no target finished", rep)
	}
	for track, from := range rep.Created {
		if from != book {
			t.Errorf("created %s from %s, want the book %s", track, from, book)
		}
	}
	retry, err := crashed.SetItemKind(ctx, []model.PID{book}, model.KindTrack, KindOptions{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(retry.Created) != 2 || !slices.Equal(retry.Converted, []model.PID{book}) {
		t.Errorf("retry report = %+v, want the two tracks made now and the book converted", retry)
	}
	all := maps.Clone(rep.Created)
	maps.Copy(all, retry.Created)
	if len(all) != 5 {
		t.Errorf("tracks named = %v, want all five the split made", all)
	}
}

// TestARetryAfterACancelCompletesTheChange: a kind change canceled partway (a server
// stopping) is asked again and completes, the targets the canceled run absorbed answering
// with where they went.
func TestARetryAfterACancelCompletesTheChange(t *testing.T) {
	t.Parallel()
	lib, _, _, pids := kindChapters(t, 6, model.MediaAudiobook)
	st := watchJobs(lib)
	ctx, cancel := context.WithCancel(context.Background())
	st.beforeRead = func(k int) {
		if k == 4 {
			cancel()
		}
	}
	if _, err := lib.SetItemKind(ctx, pids, model.KindBook, KindOptions{}); err == nil {
		t.Fatal("set kind succeeded, want the cancel")
	}
	if job, _ := lastJob(t, lib); job.State != model.JobCanceled {
		t.Fatalf("job = %+v, want canceled", job)
	}
	st.beforeRead = nil
	rep, err := lib.SetItemKind(context.Background(), pids, model.KindBook, KindOptions{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, p := range pids[1:] {
		if rep.Absorbed[p] != pids[0] {
			t.Errorf("retry absorbed = %v, want %s in the book", rep.Absorbed, p)
		}
	}
}

// TestARetryFollowsOnlyWhatTheCutOffChangeDid: a pid a canceled kind change named but never
// read, which a scan folded into another item since, is refused on a retry rather than
// resolved to that item, which nobody named.
func TestARetryFollowsOnlyWhatTheCutOffChangeDid(t *testing.T) {
	t.Parallel()
	lib, _, root, pids := kindChapters(t, 3, "")
	st := watchJobs(lib)
	ctx, cancel := context.WithCancel(context.Background())
	st.beforeRead = func(int) { cancel() }
	if _, err := lib.SetItemKind(ctx, []model.PID{pids[0], pids[2]}, model.KindBook, KindOptions{}); err == nil {
		t.Fatal("set kind succeeded, want the cancel")
	}
	st.beforeRead = nil
	dir := filepath.Join(root, "Jane Austen", "Persuasion")
	if err := copyFile(filepath.Join(dir, "02.mp3"), filepath.Join(dir, "03.mp3")); err != nil {
		t.Fatal(err)
	}
	res, err := lib.Scan(context.Background(), ScanRequest{})
	if err != nil || res.Total.Absorbed[pids[2]] != pids[1] {
		t.Fatalf("scan = %+v (err %v), want chapter 3 folded into chapter 2", res.Total, err)
	}
	if _, err := lib.SetItemKind(context.Background(), []model.PID{pids[2]}, model.KindBook, KindOptions{}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("retry = %v, want chapter 3 not found", err)
	}
	if v, err := lib.Get(context.Background(), pids[1]); err != nil || v.Kind != model.KindTrack {
		t.Errorf("chapter 2 = %+v (err %v), want it left a track", v, err)
	}
}

// snapshotAfterBook copies the catalog right after the first book put lands, the state a
// crash at that moment leaves.
type snapshotAfterBook struct {
	model.Catalog
	once sync.Once
	snap func()
}

func (c *snapshotAfterBook) PutScannedBook(ctx context.Context, in model.PutScannedBookInput) (*model.ScanItemResult, error) {
	res, err := c.Catalog.PutScannedBook(ctx, in)
	c.once.Do(c.snap)
	return res, err
}

// TestAKindChangePinsInTheReadsOwnWrite: the item a kind change's read lands a file on is
// pinned by the write that lands it, so a crash right after that write leaves it converted
// and pinned, its rule agreeing with the kind or not.
func TestAKindChangePinsInTheReadsOwnWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, db, root, pids := kindChapters(t, 2, model.MediaAudiobook)
	var snapshot string
	lib.scanner = scan.New(&snapshotAfterBook{Catalog: lib.store, snap: func() { snapshot = snapshotAt(t, db) }}, meta.NewReader(), lib.log)
	if _, err := lib.SetItemKind(ctx, pids, model.KindBook, KindOptions{}); err != nil {
		t.Fatalf("set kind: %v", err)
	}
	crashed := openCrashed(t, snapshot, root, model.MediaAudiobook)
	targets, err := crashed.store.KindTargets(ctx, pids[:1])
	if err != nil || len(targets) != 1 || targets[0].Kind != model.KindBook || !targets[0].KindLocked {
		t.Errorf("after the first read = %+v (err %v), want chapter 1 a book and pinned", targets, err)
	}
}

// TestAKindChangeReportsWhereARereadTookATarget: a target whose copy, re-encoded on disk
// into another track's audio, is read again when the target's main file goes to the book
// ends in that track, and the report names the track, not the book its main file went to.
func TestAKindChangeReportsWhereARereadTookATarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, root, pids := kindChapters(t, 2, "")
	dir := filepath.Join(root, "Jane Austen", "Persuasion")
	if err := copyFile(filepath.Join(dir, "02.mp3"), filepath.Join(dir, "02 copy.mp3")); err != nil {
		t.Fatal(err)
	}
	other := testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Other", Artist: "Jane Austen", AlbumArtist: "Jane Austen",
		Album: "Elsewhere", Genre: "Speech", Track: 1, Audio: testaudio.AudioWithSeed(99)})
	writeRaw(t, filepath.Join(root, "Other.mp3"), other)
	if res, err := lib.Scan(ctx, ScanRequest{}); err != nil || res.Total.Copies != 1 {
		t.Fatalf("scan = %+v (err %v), want the copy attached to chapter 2", res, err)
	}
	writeRaw(t, filepath.Join(dir, "02 copy.mp3"), other)
	rep, err := lib.SetItemKind(ctx, pids, model.KindBook, KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	files, err := lib.ItemFiles(ctx, rep.Absorbed[pids[1]])
	if err != nil || len(files) == 0 || filepath.Base(files[0].DisplayPath) != "Other.mp3" {
		t.Errorf("chapter 2 absorbed into %s holding %+v (err %v), want the Other track", rep.Absorbed[pids[1]], files, err)
	}
}

// TestAFinishedKindChangeLeavesNoLineage: a change that finishes drops what its writes
// recorded, which only a retry or a crash's report would read.
func TestAFinishedKindChangeLeavesNoLineage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, _, pids := kindChapters(t, 3, model.MediaAudiobook)
	if rep, err := lib.SetItemKind(ctx, pids, model.KindBook, KindOptions{}); err != nil || len(rep.Absorbed) != 2 {
		t.Fatalf("set kind = %+v (err %v), want chapters 2 and 3 absorbed", rep, err)
	}
	job, _ := lastJob(t, lib)
	if got, err := lib.store.AbsorbedInto(ctx, job.PID, nil); err != nil || len(got) != 0 {
		t.Errorf("lineage after the change = %v (err %v), want none", got, err)
	}
}
