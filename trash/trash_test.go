package trash

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
			plan, err := s.PlanFiles([]*model.Library{lib}, []FileTarget{{ItemPID: "i",
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
	plan, err := s.PlanFiles([]*model.Library{lib}, targets, model.DeleteTrash)
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
	plan, err := s.PlanFiles([]*model.Library{lib}, targets, model.DeletePermanent)
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
