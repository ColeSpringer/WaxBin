package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestLoadScopedFileIndex verifies the preloaded index carries each present file's
// pids, size, and mtime, scoped by path prefix.
func TestLoadScopedFileIndex(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/one.mp3", essence: "ea", content: "ca", title: "One"})
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/two.mp3", essence: "eb", content: "cb", title: "Two"})

	// Whole-library scope: both files present.
	idx, err := st.LoadScopedFileIndex(ctx, lib.ID, nil)
	if err != nil {
		t.Fatalf("load index: %v", err)
	}
	if len(idx) != 2 {
		t.Fatalf("whole-library index size = %d, want 2", len(idx))
	}
	got, ok := idx["/lib/a/one.mp3"]
	if !ok {
		t.Fatal("index missing /lib/a/one.mp3")
	}
	if got.FilePID != a.FilePID || got.ItemPID != a.ItemPID {
		t.Errorf("index pids = file %s / item %s, want file %s / item %s", got.FilePID, got.ItemPID, a.FilePID, a.ItemPID)
	}
	if got.Size != int64(len("ca")) || got.MTimeNS != 1 {
		t.Errorf("index size/mtime = %d/%d, want %d/1", got.Size, got.MTimeNS, len("ca"))
	}

	// Path-prefix scope: only files under /lib/b/.
	scoped, err := st.LoadScopedFileIndex(ctx, lib.ID, []byte("/lib/b/"))
	if err != nil {
		t.Fatalf("load scoped index: %v", err)
	}
	if len(scoped) != 1 {
		t.Fatalf("scoped index size = %d, want 1", len(scoped))
	}
	if _, ok := scoped["/lib/b/two.mp3"]; !ok {
		t.Errorf("scoped index missing /lib/b/two.mp3, got %v", keysOf(scoped))
	}
	_ = b
}

// TestMarkFilesMissing marks a single-file item missing but keeps a multi-file book
// present when only one of its parts vanished.
func TestMarkFilesMissing(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ea", content: "ca", title: "A"})
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.mp3", essence: "eb", content: "cb", title: "B"})

	// Mark A's file missing; A must go to state 'missing', B stays present.
	n, err := st.MarkFilesMissing(ctx, []model.PID{a.FilePID})
	if err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked %d items, want 1", n)
	}
	if s := itemState(t, st, a.ItemPID); s != string(model.StateMissing) {
		t.Errorf("A state = %q, want missing", s)
	}
	if s := itemState(t, st, b.ItemPID); s != string(model.StatePresent) {
		t.Errorf("B state = %q, want present", s)
	}

	// Idempotent: re-marking A yields no newly-marked items.
	n, err = st.MarkFilesMissing(ctx, []model.PID{a.FilePID})
	if err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if n != 0 {
		t.Errorf("re-mark marked %d, want 0 (idempotent)", n)
	}
}

// TestMarkFilesMissingMultiFileBook confirms a book with one vanished part but a
// still-present part is NOT marked missing.
func TestMarkFilesMissingMultiFileBook(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	p1 := putBookPart(t, st, lib.ID, "/lib/book/p1.m4b", "bk1", "e1", 0)
	putBookPart(t, st, lib.ID, "/lib/book/p2.m4b", "bk1", "e2", 1)

	n, err := st.MarkFilesMissing(ctx, []model.PID{p1.FilePID})
	if err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	if n != 0 {
		t.Fatalf("marked %d, want 0 (book keeps a present part)", n)
	}
	if s := itemState(t, st, p1.ItemPID); s != string(model.StatePresent) {
		t.Errorf("book state = %q, want present", s)
	}

	// Both parts gone -> the book is marked missing.
	p2Files, _ := st.ItemFiles(ctx, p1.ItemPID)
	var pids []model.PID
	for _, f := range p2Files {
		pids = append(pids, f.FilePID)
	}
	n, err = st.MarkFilesMissing(ctx, pids)
	if err != nil {
		t.Fatalf("mark all: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked %d, want 1 (all parts gone)", n)
	}
	if s := itemState(t, st, p1.ItemPID); s != string(model.StateMissing) {
		t.Errorf("book state = %q, want missing", s)
	}
}

// TestUpdateItemSidecars applies a .lrc over unchanged audio, emitting an item delta
// on change and staying silent on a no-op.
func TestUpdateItemSidecars(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ea", content: "ca", title: "A"})

	before := latestSeq(t, st)
	ly := &model.Lyrics{Source: "lrc", Synced: []model.SyncedLine{{TimeMS: 0, Text: "hi"}}}
	changed, err := st.UpdateItemSidecars(ctx, model.SidecarUpdate{ItemPID: a.ItemPID, FilePID: a.FilePID, Lyrics: ly})
	if err != nil {
		t.Fatalf("update sidecars: %v", err)
	}
	if !changed {
		t.Fatal("first lyrics write reported unchanged")
	}
	if latestSeq(t, st) <= before {
		t.Error("changed sidecar update emitted no change_log delta")
	}
	got, err := st.LyricsByItem(ctx, a.ItemPID)
	if err != nil || len(got.Synced) != 1 || got.Synced[0].Text != "hi" {
		t.Fatalf("lyrics not persisted: %v %+v", err, got)
	}

	// No-op: same lyrics again reports unchanged and emits no delta.
	seq := latestSeq(t, st)
	changed, err = st.UpdateItemSidecars(ctx, model.SidecarUpdate{ItemPID: a.ItemPID, FilePID: a.FilePID, Lyrics: ly})
	if err != nil {
		t.Fatalf("re-update: %v", err)
	}
	if changed {
		t.Error("identical lyrics reported changed")
	}
	if latestSeq(t, st) != seq {
		t.Error("no-op sidecar update emitted a delta")
	}
}

// TestUpdateItemSidecarsPersistsObservations confirms file_aux_state rows are
// written so a subsequent index load carries them.
func TestUpdateItemSidecarsPersistsObservations(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ea", content: "ca", title: "A"})

	obs := []model.AuxObservation{{Kind: model.AuxLyrics, Path: []byte("/lib/a.lrc"), Size: 10, MTimeNS: 42, Hash: "h"}}
	if _, err := st.UpdateItemSidecars(ctx, model.SidecarUpdate{ItemPID: a.ItemPID, FilePID: a.FilePID, Observations: obs}); err != nil {
		t.Fatalf("update sidecars: %v", err)
	}
	idx, err := st.LoadScopedFileIndex(ctx, lib.ID, nil)
	if err != nil {
		t.Fatalf("load index: %v", err)
	}
	entry := idx["/lib/a.mp3"]
	if len(entry.Aux) != 1 || entry.Aux[0].Kind != model.AuxLyrics || entry.Aux[0].MTimeNS != 42 {
		t.Fatalf("aux observation not carried in index: %+v", entry.Aux)
	}
}

// TestUpdateFileStateIfUnchanged updates on a size/mtime match and skips on a
// mismatch (optimistic concurrency).
func TestUpdateFileStateIfUnchanged(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ea", content: "ca", title: "A"})
	origSize := int64(len("ca"))

	// Match: stored size/mtime are (len, 1); update succeeds.
	updated, err := st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID: a.FilePID, ExpectedSize: origSize, ExpectedMTimeNS: 1,
		NewSize: 99, NewMTimeNS: 2, NewContentHash: "newhash",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !updated {
		t.Fatal("expected update on matching size/mtime")
	}
	f, err := st.FileByPID(ctx, a.FilePID)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if f.Size != 99 || f.MTimeNS != 2 || f.ContentHash != "newhash" {
		t.Errorf("file not updated: size=%d mtime=%d hash=%s", f.Size, f.MTimeNS, f.ContentHash)
	}
	if f.EssenceHash != "ea" {
		t.Errorf("essence changed by tag-write update: %s", f.EssenceHash)
	}

	// Mismatch: stale expected size/mtime; update is skipped.
	updated, err = st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID: a.FilePID, ExpectedSize: origSize, ExpectedMTimeNS: 1,
		NewSize: 5, NewMTimeNS: 5, NewContentHash: "z",
	})
	if err != nil {
		t.Fatalf("update stale: %v", err)
	}
	if updated {
		t.Fatal("expected skip on stale size/mtime")
	}
}

// TestUpdateItemSidecarsPreservesUnsynced: a fast-path .lrc update (synced only)
// preserves the stored embedded unsynchronized block instead of clobbering it.
func TestUpdateItemSidecarsPreservesUnsynced(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ea", content: "ca", title: "A"})

	// Seed lyrics as the full path would: .lrc synced lines merged with an embedded
	// unsynchronized block.
	if _, err := st.UpdateItemSidecars(ctx, model.SidecarUpdate{
		ItemPID: a.ItemPID, FilePID: a.FilePID,
		Lyrics: &model.Lyrics{Source: "lrc", Synced: []model.SyncedLine{{TimeMS: 0, Text: "one"}}, Unsynced: "embedded block"},
	}); err != nil {
		t.Fatalf("seed lyrics: %v", err)
	}

	// A fast-path .lrc update carries only synced lines (no unsynced).
	if _, err := st.UpdateItemSidecars(ctx, model.SidecarUpdate{
		ItemPID: a.ItemPID, FilePID: a.FilePID,
		Lyrics: &model.Lyrics{Source: "lrc", Synced: []model.SyncedLine{{TimeMS: 0, Text: "one"}, {TimeMS: 1000, Text: "two"}}},
	}); err != nil {
		t.Fatalf("update lyrics: %v", err)
	}

	ly, err := st.LyricsByItem(ctx, a.ItemPID)
	if err != nil {
		t.Fatalf("read lyrics: %v", err)
	}
	if len(ly.Synced) != 2 {
		t.Errorf("synced lines = %d, want 2 (the updated .lrc)", len(ly.Synced))
	}
	if ly.Unsynced != "embedded block" {
		t.Errorf("unsynced = %q, want the preserved embedded block", ly.Unsynced)
	}
}

func TestChapterSourceRank(t *testing.T) {
	// podcast_url outranks embedded (the episode contract); embedded outranks cue;
	// synthetic is lowest of the named sources.
	if !(chapterSourceRank("podcast_url") < chapterSourceRank("embedded")) {
		t.Error("podcast_url must outrank embedded")
	}
	if !(chapterSourceRank("embedded") < chapterSourceRank("cue")) {
		t.Error("embedded must outrank cue")
	}
	if !(chapterSourceRank("cue") < chapterSourceRank("synthetic")) {
		t.Error("cue must outrank synthetic")
	}
}

func keysOf(m map[string]model.ScopedFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itemState(t *testing.T, st *Store, pid model.PID) string {
	t.Helper()
	var s string
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT state FROM playable_item WHERE pid = ?", string(pid)).Scan(&s); err != nil {
		t.Fatalf("item state: %v", err)
	}
	return s
}

func latestSeq(t *testing.T, st *Store) int64 {
	t.Helper()
	seq, err := st.LatestChangeSeq(context.Background())
	if err != nil {
		t.Fatalf("latest seq: %v", err)
	}
	return seq
}

// putBookPart persists one part of a multi-file book keyed by bookKey.
func putBookPart(t *testing.T, st *Store, libID int64, path, bookKey, essence string, position int) *model.ScanItemResult {
	t.Helper()
	in := model.PutScannedBookInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(path),
			Kind: model.FileAudio, Size: int64(len(essence)), MTimeNS: 1,
			ContentHash: essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: "Book",
			SortKey: model.SortKey("Book"), IdentityKey: "book:" + bookKey,
		},
		Book:     model.Book{Author: "Auth", Authors: []string{"Auth"}},
		Position: position,
		Chapters: []model.Chapter{{Position: 0, Title: "Ch"}},
	}
	res, err := st.PutScannedBook(context.Background(), in)
	if err != nil {
		t.Fatalf("put book part %s: %v", path, err)
	}
	return res
}
