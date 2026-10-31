package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// updatesSince counts the update deltas of entityType per pid since seq.
func updatesSince(t *testing.T, st *Store, seq int64, entityType string) map[model.PID]int {
	t.Helper()
	changes, err := st.ChangesSince(context.Background(), seq)
	if err != nil {
		t.Fatal(err)
	}
	out := map[model.PID]int{}
	for _, c := range changes {
		if c.EntityType == entityType && c.Op == model.OpUpdate {
			out[c.EntityPID]++
		}
	}
	return out
}

// bookmarkPIDs maps a book's bookmarks by position to their pids.
func bookmarkPIDs(t *testing.T, st *Store, item model.PID) map[int64]model.PID {
	t.Helper()
	marks, err := st.Bookmarks(context.Background(), "", item)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]model.PID{}
	for _, m := range marks {
		out[m.PositionMS] = m.PID
	}
	return out
}

// TestAChapterEditAnnouncesThePlacesItMoves: a chapter edit that moves resume positions
// and bookmarks says so in the change feed, a play state delta for the book and an
// update for each bookmark it moved, and none for a bookmark it left where it was.
func TestAChapterEditAnnouncesThePlacesItMoves(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{path: "/lib/d/p1.mp3", essence: "dd1", content: "ddc1", title: "Announced", author: "Author",
		position: 1, durationMS: 60000, chapters: []model.Chapter{{Title: "One"}}})
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/d/p2.mp3", essence: "dd2", content: "ddc2", title: "Announced", author: "Author",
		position: 2, durationMS: 100000, chapters: []model.Chapter{{Title: "Two"}}}).ItemPID
	if _, err := st.wdb().ExecContext(ctx, `INSERT INTO chapter(book_item_id, file_id, position, title, start_ms, end_ms, source)
		SELECT pi.id, f.id, 0, 'Stale', 90000, 0, 'user' FROM playable_item pi, file f
		WHERE pi.pid = ? AND f.path = CAST('/lib/d/p1.mp3' AS BLOB)`, string(book)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProgress(ctx, "", book, 100000, nil); err != nil {
		t.Fatal(err)
	}
	for _, pos := range []int64{95000, 10000} {
		if _, err := st.AddBookmark(ctx, "", book, pos, ""); err != nil {
			t.Fatal(err)
		}
	}
	marks := bookmarkPIDs(t, st, book)
	seq, _ := st.LatestChangeSeq(ctx)
	if err := st.SetItemChapters(ctx, book, []model.Chapter{{Title: "New"}}, model.LockUnchanged, false); err != nil {
		t.Fatal(err)
	}
	if got := updatesSince(t, st, seq, "play_state"); got[book] != 1 {
		t.Errorf("play state deltas = %v, want one for the book", got)
	}
	if got := updatesSince(t, st, seq, "bookmark"); got[marks[95000]] != 1 || got[marks[10000]] != 0 {
		t.Errorf("bookmark deltas = %v, want one for the moved mark %s and none for %s", got, marks[95000], marks[10000])
	}
}

// TestAPartArrivingAnnouncesThePlacesItMoves: a part joining a book ahead of a place moves
// it on, and says so.
func TestAPartArrivingAnnouncesThePlacesItMoves(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	part := func(n int) bookSpec {
		return bookSpec{path: fmt.Sprintf("/lib/ar/%d.mp3", n), essence: fmt.Sprintf("are%d", n), content: fmt.Sprintf("arc%d", n),
			title: "Arrivals", author: "Author", position: n, durationMS: 1000}
	}
	putBook(t, st, lib.ID, part(1))
	book := putBook(t, st, lib.ID, part(3)).ItemPID
	if err := st.SetProgress(ctx, "", book, 1500, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBookmark(ctx, "", book, 1600, ""); err != nil {
		t.Fatal(err)
	}
	mark := bookmarkPIDs(t, st, book)[1600]
	seq, _ := st.LatestChangeSeq(ctx)
	putBook(t, st, lib.ID, part(2))
	if got := updatesSince(t, st, seq, "play_state"); got[book] != 1 {
		t.Errorf("play state deltas = %v, want one for the book", got)
	}
	if got := updatesSince(t, st, seq, "bookmark"); got[mark] != 1 {
		t.Errorf("bookmark deltas = %v, want one for %s", got, mark)
	}
}

// TestAPartLeavingAnnouncesThePlacesItMoves: a part leaving a book for a track of its own
// takes the places inside it along and moves later ones back, and says so for the book
// and for the track.
func TestAPartLeavingAnnouncesThePlacesItMoves(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	var book model.PID
	for i, n := range []string{"01", "02", "03"} {
		book = putBook(t, st, lib.ID, bookSpec{path: "/lib/lv/" + n + ".mp3", essence: "lve" + n, content: "lvc" + n,
			title: "Leaving", author: "A", position: i + 1, durationMS: 1000}).ItemPID
	}
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetProgress(ctx, "", book, 1500, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProgress(ctx, bob.PID, book, 2500, nil); err != nil {
		t.Fatal(err)
	}
	for _, pos := range []int64{1200, 2100} {
		if _, err := st.AddBookmark(ctx, "", book, pos, ""); err != nil {
			t.Fatal(err)
		}
	}
	marks := bookmarkPIDs(t, st, book)
	seq, _ := st.LatestChangeSeq(ctx)
	track := putTrack(t, st, lib.ID, trackSpec{path: "/lib/lv/02.mp3", essence: "lve02", content: "lvc02",
		title: "Chapter 02", durationMS: 1000}).ItemPID
	// The book is announced for the place that left it and again for the place moved back.
	if got := updatesSince(t, st, seq, "play_state"); got[book] == 0 || got[track] != 1 {
		t.Errorf("play state deltas = %v, want the book %s and the track %s announced", got, book, track)
	}
	if got := updatesSince(t, st, seq, "bookmark"); got[marks[1200]] != 1 || got[marks[2100]] != 1 {
		t.Errorf("bookmark deltas = %v, want one for each moved mark", got)
	}
}

// TestAFoldAnnouncesThePlacesItBrings: a track whose file becomes a book's part folds
// into the book, bringing its resume position and bookmark, and says so for the book.
func TestAFoldAnnouncesThePlacesItBrings(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	track := putTrack(t, st, lib.ID, trackSpec{path: "/lib/fo/02.mp3", essence: "foe2", content: "foc2",
		title: "Chapter 02", artist: "A", album: "T", durationMS: 1000}).ItemPID
	if err := st.SetProgress(ctx, "", track, 400, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBookmark(ctx, "", track, 600, ""); err != nil {
		t.Fatal(err)
	}
	mark := bookmarkPIDs(t, st, track)[600]
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/fo/01.mp3", essence: "foe1", content: "foc1",
		title: "T", author: "A", position: 1, durationMS: 1000}).ItemPID
	seq, _ := st.LatestChangeSeq(ctx)
	putBook(t, st, lib.ID, bookSpec{path: "/lib/fo/02.mp3", essence: "foe2", content: "foc2",
		title: "T", author: "A", position: 2, durationMS: 1000})
	if got := updatesSince(t, st, seq, "play_state"); got[book] != 1 {
		t.Errorf("play state deltas = %v, want one for the book %s", got, book)
	}
	if got := updatesSince(t, st, seq, "bookmark"); got[mark] != 1 {
		t.Errorf("bookmark deltas = %v, want one for %s", got, mark)
	}
}

// TestAnEntityMergeAnnouncesTheStarsItMoves: merging a starred artist into another moves
// the star onto the survivor, and says so under the survivor's per-user delta.
func TestAnEntityMergeAnnouncesTheStarsItMoves(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/m/1.flac", essence: "me1", content: "mc1", title: "One", artist: "Beatles", album: "A1"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/m/2.flac", essence: "me2", content: "mc2", title: "Two", artist: "The Beatles", album: "A2"})
	survivor, loser := entityPID(t, st, "artist", "The Beatles"), entityPID(t, st, "artist", "Beatles")
	if _, err := st.SetEntityStar(ctx, "", model.MergeArtist, loser, true, nil); err != nil {
		t.Fatal(err)
	}
	seq, _ := st.LatestChangeSeq(ctx)
	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatal(err)
	}
	if got := updatesSince(t, st, seq, "entity_play_state"); got[survivor] != 1 {
		t.Errorf("entity play state deltas = %v, want one for the survivor %s", got, survivor)
	}
}
