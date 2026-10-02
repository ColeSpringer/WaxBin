package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
	// The latest listening is part 2's checkpoint, so it is the book's resume position.
	if err := st.SetProgress(ctx, "", t2, 300, nil); err != nil {
		t.Fatalf("progress: %v", err)
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

// TestFoldStarsWhenEitherWasStarred: a book made of tracks is starred when any of them
// was, even when the track that becomes the book was starred and then unstarred; its own
// rating stands over another's.
func TestFoldStarsWhenEitherWasStarred(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	t1 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/01.mp3", essence: "se1", content: "sc1", title: "One", artist: "A", album: "T"}).ItemPID
	t2 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/02.mp3", essence: "se2", content: "sc2", title: "Two", artist: "A", album: "T"}).ItemPID
	for _, starred := range []bool{true, false} {
		if _, err := st.SetStar(ctx, "", t1, starred, nil); err != nil {
			t.Fatalf("star: %v", err)
		}
	}
	if _, err := st.SetStar(ctx, "", t2, true, nil); err != nil {
		t.Fatalf("star: %v", err)
	}
	for pid, r := range map[model.PID]int{t1: 60, t2: 80} {
		if _, err := st.SetRating(ctx, "", pid, &r, nil); err != nil {
			t.Fatalf("rating: %v", err)
		}
	}
	for i, n := range []string{"01", "02"} {
		putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "se" + n[1:], content: "sc" + n[1:],
			title: "T", author: "A", position: i + 1, durationMS: 1000})
	}
	ps, err := st.PlayStateFor(ctx, "", t1)
	if err != nil {
		t.Fatalf("play state: %v", err)
	}
	if !ps.Starred || ps.Rating != 60 {
		t.Errorf("book state = %+v, want starred and its own rating 60", ps)
	}
}

// TestFoldCarriesLockedAndFilledCustomTags: a folded track's custom tags that a scan
// would keep (locked, or filled by enrichment) come across where the book holds no value
// for the key, with their provenance; a key a book owns as a field, and a value the
// track's own file stated, stay behind.
func TestFoldCarriesLockedAndFilledCustomTags(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	t1 := putTrackCustom(t, st, lib.ID, "/lib/A/T/01.mp3", "ce1", "cc1", "One", nil, true).ItemPID
	t2 := putTrackCustom(t, st, lib.ID, "/lib/A/T/02.mp3", "ce2", "cc2", "Two", map[string][]string{"PLAIN": {"x"}}, true).ItemPID
	set := func(pid model.PID, key, value string, attr model.Attribution, lock model.LockChange) {
		t.Helper()
		if _, _, err := st.SetItemTag(ctx, pid, key, []string{value}, attr, lock, false); err != nil {
			t.Fatalf("tag %s: %v", key, err)
		}
	}
	set(t1, "MOOD", "happy", model.Attribution{}, model.LockOn)
	set(t2, "MOOD", "calm", model.Attribution{}, model.LockOn)
	set(t2, "STYLE", "baroque", model.Attribution{}, model.LockOn)
	set(t2, "ASIN", "B000TEST", model.Attribution{}, model.LockOn)
	set(t2, "LANGUAGE", "eng", model.Attribution{Source: model.SourceEnrichment, Provider: "test"}, model.LockOff)
	for i, n := range []string{"01", "02"} {
		putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "ce" + n[1:], content: "cc" + n[1:],
			title: "T", author: "A", position: i + 1, durationMS: 1000, preserveLocks: true})
	}
	for key, want := range map[string][]string{"MOOD": {"happy"}, "STYLE": {"baroque"}, "LANGUAGE": {"eng"}, "ASIN": nil, "PLAIN": nil} {
		if got := tagValues(t, st, t1, key); !slices.Equal(got, want) {
			t.Errorf("book %s = %v, want %v", key, got, want)
		}
	}
	rows, err := st.FieldProvenance(ctx, t1)
	if err != nil {
		t.Fatal(err)
	}
	prov := map[string]model.FieldProvenance{}
	for _, r := range rows {
		prov[r.Field] = r
	}
	if r := prov["tag.STYLE"]; !r.Locked {
		t.Errorf("tag.STYLE = %+v, want the lock carried", r)
	}
	if r := prov["tag.LANGUAGE"]; r.Source != model.SourceEnrichment || r.Provider != "test" {
		t.Errorf("tag.LANGUAGE = %+v, want the enrichment fill carried", r)
	}
	// The primary's next read keeps both, as it keeps the book's own.
	putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/01.mp3", essence: "ce1", content: "cc1",
		title: "T", author: "A", position: 1, durationMS: 1000, preserveLocks: true})
	if got := tagValues(t, st, t1, "STYLE"); !slices.Equal(got, []string{"baroque"}) {
		t.Errorf("STYLE after a re-read = %v, want it kept", got)
	}
	if got := tagValues(t, st, t1, "LANGUAGE"); !slices.Equal(got, []string{"eng"}) {
		t.Errorf("LANGUAGE after a re-read = %v, want it kept", got)
	}
	assertVerifyClean(t, st)
}

// TestFoldIntoABookAsACopyLandsAtItsPart: a track whose file is a copy of a book's second
// part folds into the book with its position moved to where that part starts.
func TestFoldIntoABookAsACopyLandsAtItsPart(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	p1, p2, cp := file("T/01.mp3", "ac1"), file("T/02.mp3", "ac2"), file("Copies/02.mp3", "ac2b")
	book := putBook(t, st, lib.ID, bookSpec{path: p1, essence: "ae1", content: "ac1", title: "T", author: "A", position: 1, durationMS: 1000}).ItemPID
	putBook(t, st, lib.ID, bookSpec{path: p2, essence: "ae2", content: "ac2", title: "T", author: "A", position: 2, durationMS: 1000})
	track := putTrack(t, st, lib.ID, trackSpec{path: cp, essence: "ae2", content: "ac2b", title: "Two", durationMS: 1000}).ItemPID
	if err := st.SetProgress(ctx, "", track, 300, nil); err != nil {
		t.Fatal(err)
	}
	out := putBook(t, st, lib.ID, bookSpec{path: cp, essence: "ae2", content: "ac2b", title: "T", author: "A", position: 2, durationMS: 1000})
	if !out.AttachedAsCopy || !slices.Equal(out.Folded, []model.PID{track}) {
		t.Fatalf("put = %+v, want a copy of part 2 folding the track", out)
	}
	if ps, err := st.PlayStateFor(ctx, "", book); err != nil || ps.PositionMS != 1300 {
		t.Errorf("book state = %+v (err %v), want the track's place inside part 2, 1300", ps, err)
	}
}

// diskLibrary adds an in-place library over a temp root and returns it with a helper that
// writes a file under the root and returns its path, for tests whose files have to be on
// disk (a copy is told from a moved file by its original still being there).
func diskLibrary(t *testing.T, st *Store) (*model.Library, func(rel, content string) string) {
	t.Helper()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(context.Background(), &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	return lib, func(rel, content string) string {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
}

// TestFoldOffsetsByTheBookTimeline: a part counts for its furthest chapter when that runs
// past its file's known duration, as the book's timeline does.
func TestFoldOffsetsByTheBookTimeline(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/01.mp3", essence: "oe1", content: "oc1", title: "T", author: "A", position: 1,
		chapters: []model.Chapter{{Title: "One", FileStartMS: 0, FileEndMS: 1000}}}).ItemPID
	track := putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/02.mp3", essence: "oe2", content: "oc2", title: "Two", durationMS: 1000}).ItemPID
	if err := st.SetProgress(ctx, "", track, 300, nil); err != nil {
		t.Fatal(err)
	}
	putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/02.mp3", essence: "oe2", content: "oc2", title: "T", author: "A", position: 2, durationMS: 1000})
	if ps, err := st.PlayStateFor(ctx, "", book); err != nil || ps.PositionMS != 1300 {
		t.Errorf("book state = %+v (err %v), want 1300 past part 1's chapter", ps, err)
	}
}

// TestPartJoiningABookMovesLaterPositions: a part another item brings into a book before
// where a listener is moves that listener's place, and a bookmark, on by its length, so
// both still point at the same audio.
func TestPartJoiningABookMovesLaterPositions(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	track := func(n string) model.PID {
		return putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/" + n + ".mp3", essence: "je" + n, content: "jc" + n,
			title: "Chapter " + n, artist: "A", album: "T", durationMS: 1000}).ItemPID
	}
	part := func(n string, pos int) {
		putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "je" + n, content: "jc" + n,
			title: "T", author: "A", position: pos, durationMS: 1000})
	}
	t3, t1 := track("03"), track("01")
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetProgress(ctx, "", t3, 400, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBookmark(ctx, "", t3, 600, "mark"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProgress(ctx, bob.PID, t1, 200, nil); err != nil {
		t.Fatal(err)
	}
	part("03", 3)
	part("01", 1)
	track("02")
	part("02", 2)
	if ps, err := st.PlayStateFor(ctx, "", t3); err != nil || ps.PositionMS != 2400 {
		t.Errorf("book state = %+v (err %v), want 2400, inside part 3 after two parts joined before it", ps, err)
	}
	if marks, err := st.Bookmarks(ctx, "", t3); err != nil || len(marks) != 1 || marks[0].PositionMS != 2600 {
		t.Errorf("bookmarks = %+v (err %v), want the mark at 2600", marks, err)
	}
	if ps, err := st.PlayStateFor(ctx, bob.PID, t3); err != nil || ps.PositionMS != 200 {
		t.Errorf("bob's book state = %+v (err %v), want part 1's place, 200", ps, err)
	}
}

// TestPartLeavingABookTakesItsPositions: a part leaving a book for a track of its own
// takes the places inside it along, as offsets into the part, and moves later places back
// by its length; the book's last part turns it into a track in place.
func TestPartLeavingABookTakesItsPositions(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	var book model.PID
	for i, n := range []string{"01", "02", "03"} {
		res := putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "le" + n, content: "lc" + n,
			title: "T", author: "A", position: i + 1, durationMS: 1000})
		book = res.ItemPID
	}
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	carol, err := st.CreateUser(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	for user, pos := range map[model.PID]int64{"": 1500, bob.PID: 2500, carol.PID: 500} {
		if err := st.SetProgress(ctx, user, book, pos, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, pos := range []int64{1200, 2100} {
		if _, err := st.AddBookmark(ctx, "", book, pos, "mark"); err != nil {
			t.Fatal(err)
		}
	}
	split := func(n string) model.PID {
		return putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/" + n + ".mp3", essence: "le" + n, content: "lc" + n,
			title: "Chapter " + n, durationMS: 1000}).ItemPID
	}
	position := func(user, pid model.PID) (int64, bool) {
		t.Helper()
		ps, err := st.PlayStateFor(ctx, user, pid)
		if err != nil {
			t.Fatal(err)
		}
		return ps.PositionMS, ps.LastProgressAt != 0
	}
	marks := func(pid model.PID) []int64 {
		t.Helper()
		bs, err := st.Bookmarks(ctx, "", pid)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, b := range bs {
			out = append(out, b.PositionMS)
		}
		return out
	}

	t2 := split("02")
	if pos, ok := position("", t2); !ok || pos != 500 {
		t.Errorf("part 2's track = %d (set %v), want 500", pos, ok)
	}
	if _, ok := position("", book); ok {
		t.Error("the book kept the place part 2 took")
	}
	if pos, _ := position(bob.PID, book); pos != 1500 {
		t.Errorf("bob's book place = %d, want 1500, moved back past part 2", pos)
	}
	if got := marks(t2); !slices.Equal(got, []int64{200}) {
		t.Errorf("part 2's marks = %v, want [200]", got)
	}
	if got := marks(book); !slices.Equal(got, []int64{1100}) {
		t.Errorf("book marks = %v, want [1100]", got)
	}

	t3 := split("03")
	if pos, ok := position(bob.PID, t3); !ok || pos != 500 {
		t.Errorf("bob's part 3 track = %d (set %v), want 500", pos, ok)
	}
	if got := marks(t3); !slices.Equal(got, []int64{100}) {
		t.Errorf("part 3's marks = %v, want [100]", got)
	}

	if one := split("01"); one != book {
		t.Fatalf("part 1 = %s, want the book's item %s in place", one, book)
	}
	if pos, _ := position(carol.PID, book); pos != 500 {
		t.Errorf("carol's place = %d, want 500 in part 1", pos)
	}
	assertVerifyClean(t, st)
}

// TestCopyFillingAPartKeepsThePositions: while a copy takes a leaving part's place the
// book's timeline is unchanged, so no place moves; the copy leaving too is what moves them.
func TestCopyFillingAPartKeepsThePositions(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	p1, p2, c2 := file("T/01.mp3", "fc1"), file("T/02.mp3", "fc2"), file("Spare/02.mp3", "fc2b")
	book := putBook(t, st, lib.ID, bookSpec{path: p1, essence: "fe1", content: "fc1", title: "T", author: "A", position: 1, durationMS: 1000}).ItemPID
	putBook(t, st, lib.ID, bookSpec{path: p2, essence: "fe2", content: "fc2", title: "T", author: "A", position: 2, durationMS: 1000})
	putBook(t, st, lib.ID, bookSpec{path: c2, essence: "fe2", content: "fc2b", title: "T", author: "A", position: 2, durationMS: 1000})
	if err := st.SetProgress(ctx, "", book, 1500, nil); err != nil {
		t.Fatal(err)
	}
	t2 := putTrack(t, st, lib.ID, trackSpec{path: p2, essence: "fe2", content: "fc2", title: "Two", durationMS: 1000}).ItemPID
	if ps, _ := st.PlayStateFor(ctx, "", book); ps.PositionMS != 1500 {
		t.Errorf("book place = %d while the copy fills part 2, want 1500", ps.PositionMS)
	}
	putTrack(t, st, lib.ID, trackSpec{path: c2, essence: "fe2", content: "fc2b", title: "Two", durationMS: 1000})
	if ps, _ := st.PlayStateFor(ctx, "", t2); ps.PositionMS != 500 {
		t.Errorf("part 2's track place = %d, want 500 once the copy left too", ps.PositionMS)
	}
}

// TestFoldFollowsTheLatestListening: folded parts leave the book's resume position where
// the listener was last: a checkpoint later than the book's own wins over it, and a part
// finished last puts the listener at that part's last moment.
func TestFoldFollowsTheLatestListening(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	var pids []model.PID
	for _, n := range []string{"01", "02", "03"} {
		pids = append(pids, putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/" + n + ".mp3", essence: "ll" + n, content: "lc" + n,
			title: "Chapter " + n, artist: "A", album: "T", durationMS: 1000}).ItemPID)
	}
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetProgress(ctx, "", pids[0], 100, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProgress(ctx, "", pids[1], 400, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProgress(ctx, bob.PID, pids[1], 900, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPlayed(ctx, bob.PID, pids[1], true, nil); err != nil {
		t.Fatal(err)
	}
	for i, n := range []string{"01", "02", "03"} {
		putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "ll" + n, content: "lc" + n,
			title: "T", author: "A", position: i + 1, durationMS: 1000})
	}
	for user, want := range map[model.PID]int64{"": 1400, bob.PID: 1999} {
		if ps, err := st.PlayStateFor(ctx, user, pids[0]); err != nil || ps.PositionMS != want || ps.Finished {
			t.Errorf("user %q book state = %+v (err %v), want at %d and not finished", user, ps, err, want)
		}
	}
}

// TestFoldStarKeepsTheLaterChangeStamp: a star a folded part brings across keeps the later
// of the two change stamps, so a replayed change older than the book's own unstar cannot
// undo it.
func TestFoldStarKeepsTheLaterChangeStamp(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	t1 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/01.mp3", essence: "ks1", content: "kc1", title: "One", artist: "A", album: "T"}).ItemPID
	t2 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/02.mp3", essence: "ks2", content: "kc2", title: "Two", artist: "A", album: "T"}).ItemPID
	if _, err := st.SetStar(ctx, "", t2, true, nil); err != nil {
		t.Fatal(err)
	}
	for _, starred := range []bool{true, false} {
		if _, err := st.SetStar(ctx, "", t1, starred, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := st.PlayStateFor(ctx, "", t1)
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range []string{"01", "02"} {
		putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "ks" + n[1:], content: "kc" + n[1:],
			title: "T", author: "A", position: i + 1, durationMS: 1000})
	}
	ps, err := st.PlayStateFor(ctx, "", t1)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Starred || ps.StarredChangedAt != before.StarredChangedAt {
		t.Errorf("book star = %v changed at %d, want starred with the unstar's later stamp %d", ps.Starred, ps.StarredChangedAt, before.StarredChangedAt)
	}
}

// TestPartJoiningAFinishedBookUnfinishesIt: a book a finished track became is finished only
// for a listener who finished every part that joined it too.
func TestPartJoiningAFinishedBookUnfinishesIt(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	t1 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/01.mp3", essence: "fu1", content: "fc1", title: "One", artist: "A", album: "T", durationMS: 1000}).ItemPID
	t2 := putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/02.mp3", essence: "fu2", content: "fc2", title: "Two", artist: "A", album: "T", durationMS: 1000}).ItemPID
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		user model.PID
		item model.PID
	}{{"", t1}, {bob.PID, t1}, {bob.PID, t2}} {
		if err := st.MarkPlayed(ctx, c.user, c.item, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, n := range []string{"01", "02"} {
		putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "fu" + n[1:], content: "fc" + n[1:],
			title: "T", author: "A", position: i + 1, durationMS: 1000})
	}
	for user, want := range map[model.PID]bool{"": false, bob.PID: true} {
		if ps, err := st.PlayStateFor(ctx, user, t1); err != nil || ps.Finished != want {
			t.Errorf("user %q book finished = %v (err %v), want %v", user, ps.Finished, err, want)
		}
	}
}

// TestMiddleInsertionKeepsAFinishedPartsPlace: a part finished leaves the listener at its
// last moment, inside it, so a part joining right after it is what they hear next rather
// than a part they skip.
func TestMiddleInsertionKeepsAFinishedPartsPlace(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	track := func(n string) model.PID {
		return putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/" + n + ".mp3", essence: "mi" + n, content: "mc" + n,
			title: "Chapter " + n, artist: "A", album: "T", durationMS: 1000}).ItemPID
	}
	part := func(n string, pos int) {
		putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "mi" + n, content: "mc" + n,
			title: "T", author: "A", position: pos, durationMS: 1000})
	}
	t3, t1 := track("03"), track("01")
	track("02")
	if err := st.MarkPlayed(ctx, "", t1, true, nil); err != nil {
		t.Fatal(err)
	}
	part("03", 3)
	part("01", 1)
	part("02", 2)
	if ps, err := st.PlayStateFor(ctx, "", t3); err != nil || ps.PositionMS != 999 {
		t.Errorf("book state = %+v (err %v), want part 1's last moment, 999, with part 2 next", ps, err)
	}
}

// TestFoldLeavesLockedTagsWhenLocksAreIgnored: a scan that ignores locks carries no folded
// track's locked tag onto the book, while a fill the track holds still comes across.
func TestFoldLeavesLockedTagsWhenLocksAreIgnored(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrackCustom(t, st, lib.ID, "/lib/A/T/01.mp3", "il1", "ic1", "One", nil, false)
	t2 := putTrackCustom(t, st, lib.ID, "/lib/A/T/02.mp3", "il2", "ic2", "Two", nil, false).ItemPID
	if _, _, err := st.SetItemTag(ctx, t2, "STYLE", []string{"baroque"}, model.Attribution{}, model.LockOn, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetItemTag(ctx, t2, "LANGUAGE", []string{"eng"}, model.Attribution{Source: model.SourceEnrichment, Provider: "test"}, model.LockOff, false); err != nil {
		t.Fatal(err)
	}
	var book model.PID
	for i, n := range []string{"01", "02"} {
		book = putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "il" + n[1:], content: "ic" + n[1:],
			title: "T", author: "A", position: i + 1, durationMS: 1000}).ItemPID
	}
	if got := tagValues(t, st, book, "STYLE"); got != nil {
		t.Errorf("STYLE = %v, want the locked tag left behind", got)
	}
	if got := tagValues(t, st, book, "LANGUAGE"); !slices.Equal(got, []string{"eng"}) {
		t.Errorf("LANGUAGE = %v, want the fill carried", got)
	}
}

// TestFoldIntoATrackKeepsTheLaterChanges: two tracks that turn out to hold one recording
// keep the later star change and the later rating of the two, rather than a star from
// before the survivor's own unstar.
func TestFoldIntoATrackKeepsTheLaterChanges(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	const mbid = "4e2b1b2a-0000-4000-8000-0000000000bb"
	flacPath, mp3Path := file("A/one.flac", "f"), file("B/one.mp3", "m")
	enc := func(path, essence, codec, recording string) model.PutScannedTrackInput {
		in := trackSpecInput(lib.ID, trackSpec{path: path, essence: essence, content: essence + "-bytes", title: "One",
			artist: "A", album: "T", mbRecording: recording, durationMS: 1000})
		in.File.Codec, in.File.SampleRate = codec, 44100
		return in
	}
	survivor := putTrackInput(t, st, enc(flacPath, "lc-flac", "flac", mbid)).ItemPID
	loser := putTrackInput(t, st, enc(mp3Path, "lc-mp3", "mp3", "")).ItemPID
	if _, err := st.SetStar(ctx, "", loser, true, nil); err != nil {
		t.Fatal(err)
	}
	for _, starred := range []bool{true, false} {
		if _, err := st.SetStar(ctx, "", survivor, starred, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		item   model.PID
		rating int
	}{{survivor, 60}, {loser, 80}} {
		if _, err := st.SetRating(ctx, "", c.item, &c.rating, nil); err != nil {
			t.Fatal(err)
		}
	}
	if out := putTrackInput(t, st, enc(mp3Path, "lc-mp3", "mp3", mbid)); !slices.Equal(out.Folded, []model.PID{loser}) {
		t.Fatalf("mp3 under the recording id = %+v, want it folding its track into the FLAC's", out)
	}
	ps, err := st.PlayStateFor(ctx, "", survivor)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Starred || ps.Rating != 80 {
		t.Errorf("track state = %+v, want unstarred (the later change) and rated 80 (the later rating)", ps)
	}
}

// TestPlaceAtAPartsStartMovesWithIt: a resume position or bookmark exactly where a part
// starts is the start of that part, so a part joining right before it moves it on with the
// part, the way a player resuming there would start that part.
func TestPlaceAtAPartsStartMovesWithIt(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, n := range []string{"01", "02", "03"} {
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/" + n + ".mp3", essence: "ps" + n, content: "pc" + n,
			title: "Chapter " + n, artist: "A", album: "T", durationMS: 1000})
	}
	part := func(n string, pos int) model.PID {
		return putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "ps" + n, content: "pc" + n,
			title: "T", author: "A", position: pos, durationMS: 1000}).ItemPID
	}
	book := part("01", 1)
	part("03", 3)
	if err := st.SetProgress(ctx, "", book, 1000, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBookmark(ctx, "", book, 1000, "chapter 3"); err != nil {
		t.Fatal(err)
	}
	part("02", 2)
	if ps, err := st.PlayStateFor(ctx, "", book); err != nil || ps.PositionMS != 2000 {
		t.Errorf("book state = %+v (err %v), want part 3's start, now 2000", ps, err)
	}
	if marks, err := st.Bookmarks(ctx, "", book); err != nil || len(marks) != 1 || marks[0].PositionMS != 2000 {
		t.Errorf("bookmarks = %+v (err %v), want the mark at part 3's start, 2000", marks, err)
	}
}

// TestFoldedPlacesLandInsideTheirPart: a track's place at or past its end folds to its
// part's last moment, so it belongs to that part rather than to the next one or to no part.
func TestFoldedPlacesLandInsideTheirPart(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	var two model.PID
	for _, n := range []string{"01", "02"} {
		two = putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/" + n + ".mp3", essence: "fi" + n, content: "fc" + n,
			title: "Chapter " + n, artist: "A", album: "T", durationMS: 1000}).ItemPID
	}
	if err := st.SetProgress(ctx, "", two, 1000, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBookmark(ctx, "", two, 1500, "past the end"); err != nil {
		t.Fatal(err)
	}
	var book model.PID
	for i, n := range []string{"01", "02"} {
		res := putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".mp3", essence: "fi" + n, content: "fc" + n,
			title: "T", author: "A", position: i + 1, durationMS: 1000})
		if i == 0 {
			book = res.ItemPID
		}
	}
	if ps, err := st.PlayStateFor(ctx, "", book); err != nil || ps.PositionMS != 1999 {
		t.Errorf("book state = %+v (err %v), want part 2's last moment, 1999", ps, err)
	}
	if marks, err := st.Bookmarks(ctx, "", book); err != nil || len(marks) != 1 || marks[0].PositionMS != 1999 {
		t.Errorf("bookmarks = %+v (err %v), want the mark at part 2's last moment, 1999", marks, err)
	}
}

// TestPartAppendedAfterTheEndComesNext: a listener at the end of a book has heard all of
// it, so a part joining at the end is what they hear next; their place stays put.
func TestPartAppendedAfterTheEndComesNext(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, n := range []string{"01", "02"} {
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/T/" + n + ".mp3", essence: "ae" + n, content: "ac" + n,
			title: "Chapter " + n, artist: "A", album: "T", durationMS: 1000})
	}
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/01.mp3", essence: "ae01", content: "ac01",
		title: "T", author: "A", position: 1, durationMS: 1000}).ItemPID
	if err := st.SetProgress(ctx, "", book, 1000, nil); err != nil {
		t.Fatal(err)
	}
	putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/02.mp3", essence: "ae02", content: "ac02",
		title: "T", author: "A", position: 2, durationMS: 1000})
	if ps, err := st.PlayStateFor(ctx, "", book); err != nil || ps.PositionMS != 1000 {
		t.Errorf("book state = %+v (err %v), want 1000, the start of the part appended", ps, err)
	}
}
