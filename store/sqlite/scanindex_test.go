package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
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
	if got.FilePID != a.FilePID {
		t.Errorf("index file pid = %s, want %s", got.FilePID, a.FilePID)
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

// TestMarkItemMissing pins the state rule the single-item verb owns: present flips
// and emits a delta, a second call is a silent no-op, and an unknown pid is
// CodeNotFound.
func TestMarkItemMissing(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ea", content: "ca", title: "A"})

	before := latestSeq(t, st)
	outcome, err := st.MarkItemMissing(ctx, a.ItemPID)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if outcome != model.OutcomeMarked {
		t.Errorf("outcome = %q, want marked", outcome)
	}
	if s := itemState(t, st, a.ItemPID); s != string(model.StateMissing) {
		t.Errorf("state = %q, want missing", s)
	}
	changes, err := st.ChangesSince(ctx, before)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	marked := false
	for _, c := range changes {
		if c.EntityType == "item" && c.EntityPID == a.ItemPID && c.Op == model.OpUpdate {
			marked = true
		}
	}
	if !marked {
		t.Errorf("no item update delta for the mark: %+v", changes)
	}

	seq := latestSeq(t, st)
	outcome, err = st.MarkItemMissing(ctx, a.ItemPID)
	if err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if outcome != model.OutcomeAlreadyMissing {
		t.Errorf("re-mark outcome = %q, want already-missing", outcome)
	}
	if latestSeq(t, st) != seq {
		t.Error("an already-missing item must not emit a delta")
	}

	if _, err := st.MarkItemMissing(ctx, "nope"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown pid = %v, want CodeNotFound", err)
	}
}

// TestMarkItemMissingRefusesFilelessStates pins the non-obvious half of the contract:
// archived and remote already say there are no local bytes, so each is reported back
// rather than downgraded. Downgrading archived would lose the fact that the listener
// deleted the item and would put it back into every listing that excludes archived.
func TestMarkItemMissingRefusesFilelessStates(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	gone := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.mp3", essence: "ea", content: "ca", title: "A"})
	if err := st.DetachFile(ctx, gone.FilePID); err != nil {
		t.Fatalf("detach: %v", err)
	}
	res, err := st.UpsertFeed(ctx, model.UpsertFeedInput{
		FeedURL:     "http://feed.example/y",
		IdentityKey: "podcast:feed.example/y",
		Feed: model.Feed{Title: "Show", Episodes: []model.FeedEpisode{
			{GUID: "g1", Title: "Unfetched", EnclosureURL: "http://feed.example/1.mp3", EnclosureType: "audio/mpeg"},
		}},
		FetchedAtNS: 1,
	})
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	eps, err := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %d (err %v), want 1", len(eps), err)
	}

	cases := []struct {
		name  string
		pid   model.PID
		want  model.MarkMissingOutcome
		state model.ItemState
	}{
		{"archived", gone.ItemPID, model.OutcomeArchived, model.StateArchived},
		{"remote", eps[0].PID, model.OutcomeRemote, model.StateRemote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq := latestSeq(t, st)
			outcome, err := st.MarkItemMissing(ctx, tc.pid)
			if err != nil {
				t.Fatalf("mark: %v", err)
			}
			if outcome != tc.want {
				t.Errorf("outcome = %q, want %q", outcome, tc.want)
			}
			if s := itemState(t, st, tc.pid); s != string(tc.state) {
				t.Errorf("state = %q, want %q (a refusal must not write)", s, tc.state)
			}
			if latestSeq(t, st) != seq {
				t.Error("a refusal must not emit a delta")
			}
		})
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
