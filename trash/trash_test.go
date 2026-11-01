package trash

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestRestoreIsIdempotent covers the disk side of restore, which must tolerate a
// retry after a re-scan failure: once the file is back at its original path, a
// second Restore is a no-op rather than an error, so a failed restore can be
// re-run. It also checks the occupied and gone cases.
func TestRestoreIsIdempotent(t *testing.T) {
	s := New(nil, nil) // Restore is pure disk; it does not touch the store
	dir := t.TempDir()
	orig := filepath.Join(dir, "lib", "Artist", "Album", "01 - Song.mp3")
	trashed := filepath.Join(dir, "lib", ".waxbin-trash", "abc", "01 - Song.mp3")
	if err := os.MkdirAll(filepath.Dir(trashed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trashed, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := model.TrashEntry{OrigPath: []byte(orig), OrigDisplay: orig, TrashPath: []byte(trashed)}

	// First restore moves the file back.
	if err := s.Restore(entry); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	if !fileExists(orig) || fileExists(trashed) {
		t.Fatal("first restore did not move the file back")
	}

	// Second restore (a retry after, say, a failed re-scan) is a clean no-op.
	if err := s.Restore(entry); err != nil {
		t.Fatalf("retry restore should be a no-op, got: %v", err)
	}
	if !fileExists(orig) {
		t.Fatal("retry restore lost the file")
	}
}

func TestRestoreRefusesOccupiedAndGone(t *testing.T) {
	s := New(nil, nil)
	dir := t.TempDir()

	// Occupied: both the original path and the trash file exist.
	orig := filepath.Join(dir, "orig.mp3")
	trashed := filepath.Join(dir, "t", "orig.mp3")
	mustWrite(t, orig)
	if err := os.MkdirAll(filepath.Dir(trashed), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, trashed)
	if err := s.Restore(model.TrashEntry{OrigPath: []byte(orig), OrigDisplay: orig, TrashPath: []byte(trashed)}); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("occupied original: want CodeConflict, got %v", err)
	}

	// Gone: neither the original nor the trash file exists.
	gone := model.TrashEntry{
		OrigPath: []byte(filepath.Join(dir, "nope.mp3")), OrigDisplay: filepath.Join(dir, "nope.mp3"),
		TrashPath: []byte(filepath.Join(dir, "also-nope.mp3")),
	}
	if err := s.Restore(gone); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("gone file: want CodeNotFound, got %v", err)
	}
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeStore records the catalog writes Execute makes.
type fakeStore struct{}

func (fakeStore) TrashFile(context.Context, model.TrashFileInput) (*model.DetachResult, error) {
	return &model.DetachResult{}, nil
}

func (fakeStore) DetachFile(context.Context, model.PID) (*model.DetachResult, error) {
	return &model.DetachResult{}, nil
}

func (fakeStore) ItemFiles(context.Context, model.PID) ([]model.ItemFileRef, error) { return nil, nil }

func (fakeStore) RipTracks(context.Context, model.PID) ([]model.ItemRef, error) { return nil, nil }

func (fakeStore) PlanTrash(context.Context, model.TrashPending) error { return nil }

func (fakeStore) AbortTrash(context.Context, model.PID) error { return nil }

func writeAt(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTrashLeavesASidecarAnotherEncodingUses: a track's lyrics stay in the folder while
// another encoding of it is left there, in a trash and in a permanent delete.
func TestTrashLeavesASidecarAnotherEncodingUses(t *testing.T) {
	t.Parallel()
	for _, mode := range []model.DeleteMode{model.DeleteTrash, model.DeletePermanent} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			album := filepath.Join(root, "Album")
			for _, name := range []string{"Song.mp3", "Song.flac", "Song.lrc"} {
				writeAt(t, filepath.Join(album, name), name)
			}
			lib := &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged}
			src := filepath.Join(album, "Song.mp3")
			s := New(fakeStore{}, nil)
			plan, err := s.PlanFiles(context.Background(), []*model.Library{lib}, []FileTarget{{ItemPID: "i",
				File: model.ItemFileRef{FilePID: "f", Path: []byte(src), DisplayPath: src}}}, mode)
			if err != nil {
				t.Fatal(err)
			}
			rep, err := s.Execute(context.Background(), plan)
			if err != nil || rep.Errored != 0 {
				t.Fatalf("execute: %+v (err %v)", rep, err)
			}
			if fileExists(src) || !fileExists(filepath.Join(album, "Song.lrc")) || rep.DirsPruned != 0 {
				t.Fatalf("mp3 kept %v, lyrics kept %v, pruned %d; want the mp3 gone and the lyrics left with the flac",
					fileExists(src), fileExists(filepath.Join(album, "Song.lrc")), rep.DirsPruned)
			}
		})
	}
}

// trashedEntry lays out a trash entry the way Execute leaves one: the file and its
// sidecars in the entry's folder, and the companions of the folders its run emptied
// under the trash's folders store, at their paths below the root.
func trashedEntry(t *testing.T) (root string, entry model.TrashEntry) {
	t.Helper()
	root = t.TempDir()
	orig := filepath.Join(root, "Artist", "Album", "01 Song.mp3")
	trash := filepath.Join(root, model.TrashDirName)
	trashed := filepath.Join(trash, "01ENTRY", "01 Song.mp3")
	writeAt(t, trashed, "audio")
	writeAt(t, filepath.Join(trash, "01ENTRY", "01 Song.lrc"), "lyrics")
	writeAt(t, filepath.Join(trash, "01ENTRY", ".DS_Store"), "junk")
	writeAt(t, filepath.Join(trash, foldersDir, "01OTHER", "Artist", "Album", "Cover.jpg"), "cover")
	writeAt(t, filepath.Join(trash, foldersDir, "01OTHER", "Artist", "folder.jpg"), "artist")
	writeAt(t, filepath.Join(trash, foldersDir, "01OTHER", "Artist", "Other", "cover.jpg"), "other")
	return root, model.TrashEntry{OrigPath: []byte(orig), OrigDisplay: orig, TrashPath: []byte(trashed), TrashDisplay: trashed,
		Size: int64(len("audio"))}
}

// TestRestoreBringsBackWhatCameWithTheFile: a restore puts the file's sidecars beside it
// and the companions of its folder and the folders above it back, whichever run's store
// holds them, leaves another album's, drops junk, and removes what it emptied.
func TestRestoreBringsBackWhatCameWithTheFile(t *testing.T) {
	t.Parallel()
	root, entry := trashedEntry(t)
	if err := New(nil, nil).Restore(entry); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, p := range []string{
		filepath.Join("Artist", "Album", "01 Song.mp3"), filepath.Join("Artist", "Album", "01 Song.lrc"),
		filepath.Join("Artist", "Album", "Cover.jpg"), filepath.Join("Artist", "folder.jpg"),
	} {
		if !fileExists(filepath.Join(root, p)) {
			t.Errorf("%s not restored", p)
		}
	}
	trash := filepath.Join(root, model.TrashDirName)
	if fileExists(filepath.Join(root, "Artist", "Album", ".DS_Store")) || fileExists(filepath.Join(trash, "01ENTRY")) {
		t.Error("the entry's junk was restored, or the emptied entry folder stayed")
	}
	if !fileExists(filepath.Join(trash, foldersDir, "01OTHER", "Artist", "Other", "cover.jpg")) || fileExists(filepath.Join(root, "Artist", "Other")) {
		t.Error("another album's cover was taken out of the store")
	}
}

// TestRestoreLeavesAnExtraWhoseNameIsTaken: a cover put back in the folder since stays,
// and the trashed one waits in the store rather than replacing it.
func TestRestoreLeavesAnExtraWhoseNameIsTaken(t *testing.T) {
	t.Parallel()
	root, entry := trashedEntry(t)
	cover := filepath.Join(root, "Artist", "Album", "Cover.jpg")
	writeAt(t, cover, "new")
	if err := New(nil, nil).Restore(entry); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if b, _ := os.ReadFile(cover); string(b) != "new" {
		t.Errorf("cover = %q, want the one put there since", b)
	}
	if !fileExists(filepath.Join(root, model.TrashDirName, foldersDir, "01OTHER", "Artist", "Album", "Cover.jpg")) {
		t.Error("the trashed cover was lost instead of left in the store")
	}
}

// TestRestoreRetryBringsBackWhatWasLeft: a restore that moved the file back and stopped
// before its sidecars finishes on the retry.
func TestRestoreRetryBringsBackWhatWasLeft(t *testing.T) {
	t.Parallel()
	root, entry := trashedEntry(t)
	if err := os.MkdirAll(filepath.Dir(entry.OrigDisplay), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(string(entry.TrashPath), entry.OrigDisplay); err != nil {
		t.Fatal(err)
	}
	if err := New(nil, nil).Restore(entry); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !fileExists(filepath.Join(root, "Artist", "Album", "01 Song.lrc")) || !fileExists(filepath.Join(root, "Artist", "folder.jpg")) {
		t.Error("the retry left the sidecar or a companion in the trash")
	}
}

// TestSweepKeepsWhatActiveEntriesNeed: the sweep drops the stored companions of folders
// no active entry came from and entry folders no active entry names, and keeps the rest.
func TestSweepKeepsWhatActiveEntriesNeed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	trash := filepath.Join(root, model.TrashDirName)
	for _, p := range []string{
		filepath.Join(foldersDir, "01RUN", "Artist", "Album", "Cover.jpg"),
		filepath.Join(foldersDir, "01RUN", "Artist", "artist.jpg"),
		filepath.Join(foldersDir, "01RUN", "Artist", "Gone", "cover.jpg"),
		filepath.Join(foldersDir, "02RUN", "Other", "cover.jpg"),
		filepath.Join("01KEEP", "02 Song.mp3"),
		filepath.Join("01LEFT", "01 Song.lrc"),
	} {
		writeAt(t, filepath.Join(trash, p), "x")
	}
	orig := filepath.Join(root, "Artist", "Album", "02 Song.mp3")
	kept := filepath.Join(trash, "01KEEP", "02 Song.mp3")
	left := filepath.Join(trash, "01LEFT", "01 Song.mp3")
	active := []model.TrashEntry{{OrigPath: []byte(orig), OrigDisplay: orig, TrashPath: []byte(kept), TrashDisplay: kept}}
	restored := []model.TrashEntry{{OrigDisplay: orig, TrashPath: []byte(left), TrashDisplay: left, RestoredAt: 1}}
	New(nil, nil).Sweep(root, active, restored)
	for p, want := range map[string]bool{
		filepath.Join(foldersDir, "01RUN", "Artist", "Album", "Cover.jpg"): true,
		filepath.Join(foldersDir, "01RUN", "Artist", "artist.jpg"):         true,
		filepath.Join(foldersDir, "01RUN", "Artist", "Gone"):               false,
		filepath.Join(foldersDir, "02RUN"):                                 false,
		filepath.Join("01KEEP", "02 Song.mp3"):                             true,
		"01LEFT":                                                           false,
	} {
		if got := fileExists(filepath.Join(trash, p)); got != want {
			t.Errorf("%s kept %v, want %v", p, got, want)
		}
	}
}

// cancelingStore cancels the run as the first file is trashed.
type cancelingStore struct {
	fakeStore
	cancel context.CancelFunc
}

func (c cancelingStore) TrashFile(ctx context.Context, in model.TrashFileInput) (*model.DetachResult, error) {
	c.cancel()
	return c.fakeStore.TrashFile(ctx, in)
}

// TestTrashPrunesWhatACanceledRunEmptied: the folder the first file left empty goes even
// though the run was canceled before the second.
func TestTrashPrunesWhatACanceledRunEmptied(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	one, two := filepath.Join(root, "A", "one.mp3"), filepath.Join(root, "B", "two.mp3")
	writeAt(t, one, "1")
	writeAt(t, two, "2")
	lib := &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(cancelingStore{cancel: cancel}, nil)
	var targets []FileTarget
	for _, p := range []string{one, two} {
		targets = append(targets, FileTarget{ItemPID: "i", File: model.ItemFileRef{FilePID: model.PID(p), Path: []byte(p), DisplayPath: p}})
	}
	plan, err := s.PlanFiles(context.Background(), []*model.Library{lib}, targets, model.DeleteTrash)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.Execute(ctx, plan)
	if err == nil || rep.Trashed != 1 || rep.DirsPruned != 1 {
		t.Fatalf("report = %+v (err %v), want one trashed, its folder pruned and a canceled run", rep, err)
	}
	if fileExists(filepath.Join(root, "A")) || !fileExists(two) {
		t.Fatal("want A removed and B untouched")
	}
}

// TestSweepKeepsWhatTheJournalDoesNotKnow: an entry folder with no journal row (a file a
// failed rollback stranded, a catalog restored from an older backup, a second catalog on
// the root) holds the user's only copy, so the sweep leaves it and the companions its run
// stored.
func TestSweepKeepsWhatTheJournalDoesNotKnow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	trash := filepath.Join(root, model.TrashDirName)
	for _, p := range []string{
		filepath.Join("01LOST", "song.flac"),
		filepath.Join(foldersDir, "01LOST", "Artist", "Album", "cover.jpg"),
	} {
		writeAt(t, filepath.Join(trash, p), "x")
	}
	New(nil, nil).Sweep(root, nil, nil)
	for _, p := range []string{filepath.Join("01LOST", "song.flac"), filepath.Join(foldersDir, "01LOST", "Artist", "Album", "cover.jpg")} {
		if !fileExists(filepath.Join(trash, p)) {
			t.Errorf("%s was swept though no row says it was restored", p)
		}
	}
}

// TestPlanSidecarsFollowTheRunOrder: three encodings deleted together leave their shared
// lyrics to the last, which the dry run shows as apply will take them.
func TestPlanSidecarsFollowTheRunOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	// notes.md keeps the folder, so the lyrics go only as the file's own sidecar.
	for _, name := range []string{"Song.flac", "Song.mp3", "Song.ogg", "Song.lrc", "notes.md"} {
		writeAt(t, filepath.Join(album, name), name)
	}
	lib := &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged}
	var targets []FileTarget
	for _, name := range []string{"Song.flac", "Song.mp3", "Song.ogg"} {
		p := filepath.Join(album, name)
		targets = append(targets, FileTarget{ItemPID: "i", File: model.ItemFileRef{FilePID: model.PID(name), Path: []byte(p), DisplayPath: p}})
	}
	s := New(fakeStore{}, nil)
	plan, err := s.PlanFiles(context.Background(), []*model.Library{lib}, targets, model.DeletePermanent)
	if err != nil {
		t.Fatal(err)
	}
	got := plan.Sidecars()
	if len(got) != 3 || len(got[0]) != 0 || len(got[1]) != 0 || len(got[2]) != 1 || got[2][0] != filepath.Join(album, "Song.lrc") {
		t.Fatalf("plan sidecars = %v, want the lyrics under the last file", got)
	}
	if _, err := s.Execute(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(album, "Song.lrc")) {
		t.Error("apply left the lyrics the dry run said it would take")
	}
}

// TestRestoreLeavesExtrasForAnotherFileAtThePath: with the trashed file gone from the
// trash and another file at its path, a restore has nothing of its own to finish, so the
// old sidecars and the stored cover stay in the trash.
func TestRestoreLeavesExtrasForAnotherFileAtThePath(t *testing.T) {
	t.Parallel()
	root, entry := trashedEntry(t)
	if err := os.Remove(string(entry.TrashPath)); err != nil {
		t.Fatal(err)
	}
	writeAt(t, entry.OrigDisplay, "another, longer file")
	if err := New(nil, nil).Restore(entry); err != nil {
		t.Fatalf("restore: %v", err)
	}
	album := filepath.Join(root, "Artist", "Album")
	if fileExists(filepath.Join(album, "01 Song.lrc")) || fileExists(filepath.Join(album, "Cover.jpg")) {
		t.Error("the old sidecars or cover were put beside another file")
	}
}

// ripStore is a catalog of one item whose whole-file primary has a cue rip window on a copy
// as its alternate, counting the reads a plan makes.
type ripStore struct {
	fakeStore
	files                map[model.PID][]model.ItemFileRef
	tracks               map[model.PID][]model.ItemRef
	itemFiles, ripTracks int
}

func (r *ripStore) ItemFiles(_ context.Context, pid model.PID) ([]model.ItemFileRef, error) {
	r.itemFiles++
	return r.files[pid], nil
}

func (r *ripStore) RipTracks(_ context.Context, pid model.PID) ([]model.ItemRef, error) {
	r.ripTracks++
	return r.tracks[pid], nil
}

// TestSweepLeavesARipItMatchedThroughACopy: a sweep leaves out an item one of whose files
// is a cue rip it matched only part of, an alternate copy of the rip included, rather than
// failing on it.
func TestSweepLeavesARipItMatchedThroughACopy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lib := &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged}
	whole, rip := filepath.Join(root, "x.mp3"), filepath.Join(root, "copy", "album.wav")
	st := &ripStore{
		files: map[model.PID][]model.ItemFileRef{"x": {
			{FilePID: "fx", Path: []byte(whole), DisplayPath: whole, Role: "primary"},
			{FilePID: "frip", Path: []byte(rip), DisplayPath: rip, Role: "alternate", Virtual: true},
		}},
		tracks: map[model.PID][]model.ItemRef{"frip": {{PID: "x", Title: "X"}, {PID: "y", Title: "Y"}}},
	}
	plan, err := New(st, nil).PlanSweep(context.Background(), []*model.Library{lib}, []*model.ItemView{{PID: "x", Title: "X"}}, model.DeleteTrash)
	if err != nil || len(plan.Actions) != 0 || plan.SkippedRipTracks != 1 {
		t.Fatalf("sweep = %+v (err %v), want x left out and counted", plan, err)
	}
}

// TestSweepReadsEachRipOnce: a sweep over every track of a rip reads each item's files and
// the rip's tracks once, however many of its tracks it matched.
func TestSweepReadsEachRipOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lib := &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged}
	rip := filepath.Join(root, "album.wav")
	st := &ripStore{files: map[model.PID][]model.ItemFileRef{}, tracks: map[model.PID][]model.ItemRef{}}
	var items []*model.ItemView
	for _, pid := range []model.PID{"a", "b", "c"} {
		st.files[pid] = []model.ItemFileRef{{FilePID: "frip", Path: []byte(rip), DisplayPath: rip, Role: "primary", Virtual: true}}
		st.tracks["frip"] = append(st.tracks["frip"], model.ItemRef{PID: pid})
		items = append(items, &model.ItemView{PID: pid, Virtual: true})
	}
	plan, err := New(st, nil).PlanSweep(context.Background(), []*model.Library{lib}, items, model.DeleteTrash)
	if err != nil || len(plan.Actions) != 1 || len(plan.Actions[0].Tracks) != 3 {
		t.Fatalf("sweep = %+v (err %v), want the rip's file once with its three tracks", plan, err)
	}
	if st.itemFiles != 3 || st.ripTracks != 1 {
		t.Errorf("reads = %d item files, %d rip tracks; want 3 and 1", st.itemFiles, st.ripTracks)
	}
}

// TestExecuteNamesEverySkip: a run's report names each action it skipped with its code and
// reason, the plan's skips (no library holds the file, a read-only library) and the run's
// own alike, beside the count.
func TestExecuteNamesEverySkip(t *testing.T) {
	t.Parallel()
	root, roRoot, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()
	libs := []*model.Library{
		{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged},
		{Root: []byte(roRoot), DisplayRoot: roRoot, Mode: model.ModeManaged, ReadOnly: true},
	}
	var targets []FileTarget
	for _, p := range []string{filepath.Join(root, "a.mp3"), filepath.Join(roRoot, "b.mp3"), filepath.Join(elsewhere, "c.mp3")} {
		writeAt(t, p, "x")
		targets = append(targets, FileTarget{ItemPID: "i", File: model.ItemFileRef{FilePID: model.PID(filepath.Base(p)), Path: []byte(p), DisplayPath: p}})
	}
	s := New(fakeStore{}, nil)
	plan, err := s.PlanFiles(context.Background(), libs, targets, model.DeletePermanent)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.Execute(context.Background(), plan)
	if err != nil || rep.Deleted != 1 || rep.Skipped != 2 {
		t.Fatalf("Execute = %+v (err %v), want one delete and two skips", rep, err)
	}
	want := []Skip{
		{FilePID: "b.mp3", Src: filepath.Join(roRoot, "b.mp3"), Code: SkipReadOnly, Reason: model.ReasonLibraryReadOnly},
		{FilePID: "c.mp3", Src: filepath.Join(elsewhere, "c.mp3"), Code: SkipNoRoot, Reason: "file is not under a known library root"},
	}
	if len(rep.Skips) != len(want) {
		t.Fatalf("skips = %+v, want %+v", rep.Skips, want)
	}
	for i := range want {
		if rep.Skips[i] != want[i] {
			t.Errorf("skip %d = %+v, want %+v", i, rep.Skips[i], want[i])
		}
	}
}

// stubStore answers the catalog writes a run makes with the errors and hooks a test sets,
// refusing a canceled context the way the store does, and records what it was asked.
type stubStore struct {
	fakeStore
	planErr, trashErr, detachErr error
	onPlan, onTrash              func()
	planned                      []model.TrashPending
	aborted                      []model.PID
	committed                    []model.TrashFileInput
	detached                     int
}

func (s *stubStore) PlanTrash(ctx context.Context, in model.TrashPending) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.onPlan != nil {
		s.onPlan()
	}
	if s.planErr != nil {
		return s.planErr
	}
	s.planned = append(s.planned, in)
	return nil
}

func (s *stubStore) AbortTrash(ctx context.Context, pid model.PID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.aborted = append(s.aborted, pid)
	return nil
}

func (s *stubStore) TrashFile(ctx context.Context, in model.TrashFileInput) (*model.DetachResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.onTrash != nil {
		s.onTrash()
	}
	if s.trashErr != nil {
		return nil, s.trashErr
	}
	s.committed = append(s.committed, in)
	return &model.DetachResult{TrashPID: in.PID}, nil
}

func (s *stubStore) DetachFile(ctx context.Context, _ model.PID) (*model.DetachResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.detached++
	if s.detachErr != nil {
		return nil, s.detachErr
	}
	return &model.DetachResult{}, nil
}

// runOne plans and runs the deletion of the file at src under mode against st.
func runOne(t *testing.T, ctx context.Context, st Store, root, src string, mode model.DeleteMode) (*Plan, *Report) {
	t.Helper()
	lib := &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged}
	s := New(st, nil)
	plan, err := s.PlanFiles(context.Background(), []*model.Library{lib},
		[]FileTarget{{ItemPID: "i", File: model.ItemFileRef{FilePID: "f", Path: []byte(src), DisplayPath: src}}}, mode)
	if err != nil {
		t.Fatal(err)
	}
	plan.JobPID = "JOB"
	rep, err := s.Execute(ctx, plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return plan, rep
}

// TestAFailedDetachSaysTheFileIsGone: a permanent delete whose catalog write fails after
// the file left the disk says so, and counts the bytes it reclaimed.
func TestAFailedDetachSaysTheFileIsGone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "Album", "song.mp3")
	writeAt(t, src, "audio")
	_, rep := runOne(t, context.Background(), &stubStore{detachErr: waxerr.New(waxerr.CodeIO, "test", "catalog write failed")}, root, src, model.DeletePermanent)
	if rep.Errored != 1 || len(rep.Failures) != 1 || rep.Failures[0].Disk != DiskRemoved || rep.ReclaimedBytes != int64(len("audio")) {
		t.Fatalf("report = %+v, want one failure saying the file left the disk and its bytes reclaimed", rep)
	}
	if fileExists(src) {
		t.Error("the file is still on disk")
	}
}

// TestAFailedRemoveSaysTheFileStayed: a permanent delete that could not remove the file
// says it is where it was and writes nothing to the catalog.
func TestAFailedRemoveSaysTheFileStayed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A folder holding a file is something os.Remove refuses on every platform.
	src := filepath.Join(root, "Album", "song.mp3")
	writeAt(t, filepath.Join(src, "inside"), "x")
	st := &stubStore{}
	_, rep := runOne(t, context.Background(), st, root, src, model.DeletePermanent)
	if rep.Errored != 1 || len(rep.Failures) != 1 || rep.Failures[0].Disk != DiskInPlace || rep.ReclaimedBytes != 0 || st.detached != 0 {
		t.Fatalf("report = %+v (detached %d), want one failure leaving the file in place and no catalog write", rep, st.detached)
	}
}

// TestAFailedRollbackSaysTheFileIsStranded: a trash move whose catalog write fails and
// whose move back fails too (something took the original path meanwhile) names where the
// file now is.
func TestAFailedRollbackSaysTheFileIsStranded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "Album", "song.mp3")
	writeAt(t, src, "audio")
	st := &stubStore{trashErr: waxerr.New(waxerr.CodeIO, "test", "catalog write failed")}
	st.onTrash = func() { writeAt(t, src, "another") }
	plan, rep := runOne(t, context.Background(), st, root, src, model.DeleteTrash)
	trashed := plan.Actions[0].TrashDst
	if rep.Errored != 1 || len(rep.Failures) != 1 || rep.Failures[0].Disk != DiskStranded || rep.Failures[0].TrashPath != trashed {
		t.Fatalf("report = %+v, want one failure naming the file stranded at %s", rep, trashed)
	}
	if b, _ := os.ReadFile(trashed); string(b) != "audio" {
		t.Errorf("trash copy = %q, want the file there", b)
	}
}

// TestAFailedRollbackKeepsThePendingRow: the pending row of a stranded file stays, since it
// is the file's only record until the next open finishes the move.
func TestAFailedRollbackKeepsThePendingRow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "Album", "song.mp3")
	writeAt(t, src, "audio")
	st := &stubStore{trashErr: waxerr.New(waxerr.CodeIO, "test", "catalog write failed")}
	st.onTrash = func() { writeAt(t, src, "another") }
	runOne(t, context.Background(), st, root, src, model.DeleteTrash)
	if len(st.planned) != 1 || len(st.aborted) != 0 {
		t.Fatalf("planned %v, aborted %v; want the pending row planned and kept", st.planned, st.aborted)
	}
}

// TestAFailedMoveLeavesNoPendingRowAndNoEntryFolder: a move that never happened drops its
// pending row and the entry folder made for it.
func TestAFailedMoveLeavesNoPendingRowAndNoEntryFolder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "Album", "song.mp3") // never written: the move fails
	writeAt(t, filepath.Join(root, "Album", "other.mp3"), "x")
	st := &stubStore{}
	plan, rep := runOne(t, context.Background(), st, root, src, model.DeleteTrash)
	if rep.Errored != 1 || rep.Failures[0].Disk != DiskInPlace {
		t.Fatalf("report = %+v, want one failure leaving the file in place", rep)
	}
	if len(st.planned) != 1 || len(st.aborted) != 1 || st.aborted[0] != st.planned[0].PID || len(st.committed) != 0 {
		t.Fatalf("planned %v, aborted %v, committed %v; want the planned row aborted", st.planned, st.aborted, st.committed)
	}
	if fileExists(filepath.Dir(plan.Actions[0].TrashDst)) {
		t.Error("the entry folder made for the move is still there")
	}
}

// TestACancelAfterTheMoveStillWritesTheRow: a cancel that arrives once the move is under
// way still lets the catalog write land, under the pid the pending row was planned with.
func TestACancelAfterTheMoveStillWritesTheRow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "Album", "song.mp3")
	writeAt(t, src, "audio")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &stubStore{onPlan: cancel}
	plan, rep := runOne(t, ctx, st, root, src, model.DeleteTrash)
	if rep.Trashed != 1 || rep.Errored != 0 || len(st.committed) != 1 {
		t.Fatalf("report = %+v, committed %v; want the file trashed and its row written", rep, st.committed)
	}
	if len(st.planned) != 1 || st.committed[0].PID != st.planned[0].PID || st.planned[0].JobPID != "JOB" ||
		string(st.planned[0].TrashPath) != plan.Actions[0].TrashDst {
		t.Fatalf("planned %+v, committed %+v; want one pending row, written under its own pid", st.planned, st.committed)
	}
}

// TestASidecarLeftBehindIsAFailure: a sidecar the run could not take with its file is a
// failure naming it, left in place, beside the file's own success.
func TestASidecarLeftBehindIsAFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	src, lrc := filepath.Join(album, "Song.mp3"), filepath.Join(album, "Song.lrc")
	writeAt(t, src, "audio")
	writeAt(t, lrc, "lyrics")
	lib := &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged}
	s := New(&stubStore{}, nil)
	plan, err := s.PlanFiles(context.Background(), []*model.Library{lib},
		[]FileTarget{{ItemPID: "i", File: model.ItemFileRef{FilePID: "f", Path: []byte(src), DisplayPath: src}}}, model.DeleteTrash)
	if err != nil {
		t.Fatal(err)
	}
	// A file already where the lyrics would go in the entry keeps them out.
	writeAt(t, filepath.Join(filepath.Dir(plan.Actions[0].TrashDst), "Song.lrc"), "taken")
	rep, err := s.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Trashed != 1 || len(rep.Failures) != 1 || rep.Failures[0].Src != lrc || rep.Failures[0].Disk != DiskInPlace || rep.Failures[0].FilePID != "f" {
		t.Fatalf("report = %+v, want the file trashed and its lyrics named as left in place", rep)
	}
	if !fileExists(lrc) {
		t.Error("the lyrics are gone from the folder")
	}
}

// TestPlanFoldersFollowTheRun: the folders a plan says its run will remove are the ones the
// run removes: two deletes sharing a folder empty it, its parent goes in turn with the
// companions each held, and an in-place library's folder stays for the cover it keeps.
func TestPlanFoldersFollowTheRun(t *testing.T) {
	t.Parallel()
	for _, mode := range []model.DeleteMode{model.DeleteTrash, model.DeletePermanent} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			managed, inPlace := t.TempDir(), t.TempDir()
			artist := filepath.Join(managed, "Artist")
			album := filepath.Join(artist, "Album")
			other := filepath.Join(inPlace, "Other")
			for _, p := range []string{
				filepath.Join(album, "01.mp3"), filepath.Join(album, "01.lrc"), filepath.Join(album, "02.mp3"), filepath.Join(album, "cover.jpg"),
				filepath.Join(album, ".DS_Store"), filepath.Join(artist, "folder.jpg"),
				filepath.Join(other, "03.mp3"), filepath.Join(other, "cover.jpg"),
			} {
				writeAt(t, p, filepath.Base(p))
			}
			libs := []*model.Library{
				{Root: []byte(managed), DisplayRoot: managed, Mode: model.ModeManaged},
				{Root: []byte(inPlace), DisplayRoot: inPlace, Mode: model.ModeInPlace},
			}
			var targets []FileTarget
			for _, p := range []string{filepath.Join(album, "01.mp3"), filepath.Join(album, "02.mp3"), filepath.Join(other, "03.mp3")} {
				targets = append(targets, FileTarget{ItemPID: "i", File: model.ItemFileRef{FilePID: model.PID(p), Path: []byte(p), DisplayPath: p}})
			}
			s := New(&stubStore{}, nil)
			plan, err := s.PlanFiles(context.Background(), libs, targets, mode)
			if err != nil {
				t.Fatal(err)
			}
			folders := plan.Folders(plan.Sidecars())
			want := []PrunedFolder{
				{Dir: album, Junk: []string{filepath.Join(album, ".DS_Store")}, Companions: []string{filepath.Join(album, "cover.jpg")}},
				{Dir: artist, Companions: []string{filepath.Join(artist, "folder.jpg")}},
			}
			if !reflect.DeepEqual(folders, want) {
				t.Fatalf("plan folders = %+v, want %+v", folders, want)
			}
			rep, err := s.Execute(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rep.Pruned, folders) || rep.DirsPruned != 2 {
				t.Fatalf("run pruned %+v (%d), want what the plan said", rep.Pruned, rep.DirsPruned)
			}
			if fileExists(artist) || !fileExists(filepath.Join(other, "cover.jpg")) {
				t.Errorf("artist kept %v, in-place cover kept %v; want the artist gone and the cover kept", fileExists(artist), fileExists(filepath.Join(other, "cover.jpg")))
			}
		})
	}
}

// TestAMoveThatLeavesACopyKeepsItsPendingRow: a move across filesystems whose source would
// not go, and whose copy would not go either, leaves both files. The pending row stays, so
// the next open settles the pair rather than the copy sitting in the trash unrecorded, and
// the failure says the file is in place and where the copy is.
func TestAMoveThatLeavesACopyKeepsItsPendingRow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "Album", "song.mp3")
	writeAt(t, src, "audio")
	st := &stubStore{}
	s := New(st, nil)
	s.move = func(from, to string) error {
		writeAt(t, to, "audio")
		return fmt.Errorf("remove %s: held open (%w)", from, fsx.ErrCopyLeft)
	}
	lib := &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged}
	plan, err := s.PlanFiles(context.Background(), []*model.Library{lib},
		[]FileTarget{{ItemPID: "i", File: model.ItemFileRef{FilePID: "f", Path: []byte(src), DisplayPath: src}}}, model.DeleteTrash)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	trashed := plan.Actions[0].TrashDst
	if len(rep.Failures) != 1 || rep.Failures[0].Disk != DiskInPlace || rep.Failures[0].TrashPath != trashed {
		t.Fatalf("failures = %+v, want the file in place and the copy named at %s", rep.Failures, trashed)
	}
	if len(st.planned) != 1 || len(st.aborted) != 0 || len(st.committed) != 0 {
		t.Errorf("planned %d, aborted %v, committed %d; want the pending row kept", len(st.planned), st.aborted, len(st.committed))
	}
	if !fileExists(src) || !fileExists(trashed) {
		t.Errorf("original kept %v, copy kept %v; want both, for the next open to settle", fileExists(src), fileExists(trashed))
	}
}
