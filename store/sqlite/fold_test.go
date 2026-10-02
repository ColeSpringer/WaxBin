package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestFoldMovesPlayStateIntoTheBook: three tracks of one folder put again as the parts of
// one book make the first track the book in place, and the other two fold into it: each
// user's plays add up, a star and a rating come across where the book has none and the
// book's own rating stands, a resume position and a bookmark move to where their part
// sits in the book, a part finished is not the book finished, and the queue, playlists,
// sessions and acquisition follow without listing the book twice.
func TestFoldMovesPlayStateIntoTheBook(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	var pids []model.PID
	for i, n := range []string{"01", "02", "03"} {
		res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/" + n + ".mp3", essence: "fe" + n, content: "fc" + n,
			title: "Chapter " + n, artist: "A", album: "T", durationMS: 1000})
		pids = append(pids, res.ItemPID)
		_ = i
	}
	t1, t2, t3 := pids[0], pids[1], pids[2]
	for range 2 {
		if err := st.MarkPlayed(ctx, "", t2, false, nil); err != nil {
			t.Fatalf("played: %v", err)
		}
	}
	if err := st.SetProgress(ctx, "", t2, 300, nil); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if _, err := st.SetStar(ctx, "", t3, true, nil); err != nil {
		t.Fatalf("star: %v", err)
	}
	rating := 80
	if _, err := st.SetRating(ctx, "", t3, &rating, nil); err != nil {
		t.Fatalf("rating: %v", err)
	}
	if err := st.MarkPlayed(ctx, "", t3, true, nil); err != nil {
		t.Fatalf("finished: %v", err)
	}
	if _, err := st.AddBookmark(ctx, "", t3, 200, "mark"); err != nil {
		t.Fatalf("bookmark: %v", err)
	}
	if _, err := st.RecordSession(ctx, "", t3, "test", 1, 2, 1); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := st.PutAcquisition(ctx, t2, model.AcquisitionInput{SourceType: model.SourceManual}); err != nil {
		t.Fatalf("acquisition: %v", err)
	}
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	bobRating := 50
	if _, err := st.SetRating(ctx, bob.PID, t1, &bobRating, nil); err != nil {
		t.Fatalf("bob rating: %v", err)
	}
	if err := st.MarkPlayed(ctx, bob.PID, t2, false, nil); err != nil {
		t.Fatalf("bob played: %v", err)
	}
	if err := st.SetQueue(ctx, "", []model.PID{t1, t2, t3}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	lists := map[string][]model.PID{"P1": {t2, t3}, "P2": {t1, t3}}
	listPIDs := map[string]model.PID{}
	for name, members := range lists {
		pl, err := st.CreatePlaylist(ctx, name, "", model.PlaylistStatic, model.VisibilityPrivate, nil)
		if err != nil {
			t.Fatalf("playlist: %v", err)
		}
		if err := st.AddPlaylistItems(ctx, pl, members); err != nil {
			t.Fatalf("playlist items: %v", err)
		}
		listPIDs[name] = pl
	}
	seq := latestSeq(t, st)

	for i, n := range []string{"01", "02", "03"} {
		putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "fe" + n, content: "fc" + n,
			title: "T", author: "A", position: i + 1, durationMS: 1000})
	}
	book, err := st.ItemByPID(ctx, t1)
	if err != nil || book.Kind != model.KindBook {
		t.Fatalf("first track = %+v (err %v), want it the book", book, err)
	}
	for _, gone := range []model.PID{t2, t3} {
		if _, err := st.ItemByPID(ctx, gone); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Errorf("folded track %s: err %v, want NotFound", gone, err)
		}
		if all, _ := itemChanges(t, st, seq, gone); all != 1 {
			t.Errorf("folded track %s changes = %d, want its one delete", gone, all)
		}
	}
	ps, err := st.PlayStateFor(ctx, "", t1)
	if err != nil {
		t.Fatalf("play state: %v", err)
	}
	if !ps.Starred || ps.PlayCount != 3 || !ps.Played || ps.Finished || !ps.HasRating || ps.Rating != 80 || ps.PositionMS != 1300 {
		t.Errorf("book state = %+v, want starred, 3 plays, played not finished, rated 80, at 1300", ps)
	}
	if ps, err := st.PlayStateFor(ctx, bob.PID, t1); err != nil || !ps.Played || ps.PlayCount != 1 || ps.Rating != 50 {
		t.Errorf("bob's book state = %+v (err %v), want the part's play and the book's own rating", ps, err)
	}
	marks, err := st.Bookmarks(ctx, "", t1)
	if err != nil || len(marks) != 1 || marks[0].PositionMS != 2200 {
		t.Errorf("bookmarks = %+v (err %v), want the part's mark at 2200", marks, err)
	}
	queue, err := st.Queue(ctx, "")
	if err != nil || len(queue) != 1 || queue[0].PID != t1 {
		t.Errorf("queue = %d entries (err %v), want the book once", len(queue), err)
	}
	for name, pl := range listPIDs {
		items, err := st.PlaylistItems(ctx, pl, "")
		if err != nil || len(items) != 1 || items[0].PID != t1 {
			t.Errorf("playlist %s = %d entries (err %v), want the book once", name, len(items), err)
		}
	}
	if n := itemRows(t, st, "play_session", "item_id", t1); n != 1 {
		t.Errorf("sessions = %d, want the part's session on the book", n)
	}
	if a, err := st.AcquisitionByItem(ctx, t1); err != nil || a == nil || a.SourceType != model.SourceManual {
		t.Errorf("acquisition = %+v (err %v), want the folded track's", a, err)
	}
}

// TestFolderStandingSeeksThePath: the folder listing seeks the path index for the folder's
// range rather than walking every file of the library.
func TestFolderStandingSeeksThePath(t *testing.T) {
	st, lib := entityFixture(t)
	plan := explainPlan(t, st, folderStandingQ, lib.ID, []byte("/lib/A/"), []byte("/lib/A0"))
	if strings.Contains(plan, "file_library") || !strings.Contains(plan, "path>? AND path<?") {
		t.Errorf("plan:\n%s\nwant a seek on the path range", plan)
	}
}

// TestFileStandingFindsATrashedItem: a file whose row went with the trash still finds the
// archived item it backed through the trash journal, by its old path or by its audio, so
// a scan that finds it back honours the item's kind lock.
func TestFileStandingFindsATrashedItem(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const path = "/lib/Author/Tome/01.mp3"
	res := putBook(t, st, lib.ID, bookSpec{path: path, essence: "te1", content: "tc1", title: "Tome", author: "Author"})
	if err := st.LockField(ctx, res.ItemPID, model.KindLockField); err != nil {
		t.Fatalf("lock kind: %v", err)
	}
	if _, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: res.FilePID, TrashPath: []byte("/trash/01.mp3"), TrashDisplay: "/trash/01.mp3"}); err != nil {
		t.Fatalf("trash: %v", err)
	}
	for _, c := range []struct{ path, essence string }{{path, ""}, {"/lib/Elsewhere/01.mp3", "te1"}} {
		got, err := st.FileStanding(ctx, lib.ID, []byte(c.path), c.essence)
		if err != nil || got == nil || got.ItemPID != res.ItemPID || got.Kind != model.KindBook || !got.KindLocked {
			t.Errorf("standing of %s = %+v (err %v), want the archived book with its kind lock", c.path, got, err)
		}
	}
	if got, err := st.FileStanding(ctx, lib.ID, []byte("/lib/Other/02.mp3"), "other"); err != nil || got != nil {
		t.Errorf("standing of an unknown file = %+v (err %v), want none", got, err)
	}
}

// TestPartLeavingABookHandsItsPlaceToACopy: a part put into another book leaves its place
// to a copy of it the book holds, so the book keeps its length.
func TestPartLeavingABookHandsItsPlaceToACopy(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	part := func(rel, essence, content string, pos int) bookSpec {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Tome", author: "Author", position: pos, durationMS: 1000}
	}
	putBook(t, st, lib.ID, part("Tome/01.mp3", "ce1", "cc1", 1))
	two := part("Tome/02.mp3", "ce2", "cc2", 2)
	tome := putBook(t, st, lib.ID, two).ItemPID
	spare := part("Zspare/02.mp3", "ce2", "cc2b", 2)
	if out := putBook(t, st, lib.ID, spare); !out.AttachedAsCopy {
		t.Fatalf("spare = %+v, want a copy of part 2", out)
	}
	two.title = "Other Book"
	putBook(t, st, lib.ID, two)
	detail, err := st.BookByPID(ctx, tome)
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if len(detail.Files) != 2 || detail.Files[1].DisplayPath != spare.path {
		t.Errorf("Tome parts = %+v, want the spare in part 2's place", detail.Files)
	}
}
