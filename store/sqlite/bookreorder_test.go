package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// TestARenumberedPartTakesItsPlacesAlong: a part read again under another number moves
// within its book's reading order, and the places on the book's timeline move with the
// parts holding them, so a resume position and a bookmark stay inside the same file.
func TestARenumberedPartTakesItsPlacesAlong(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	part := func(n, pos int) bookSpec {
		return bookSpec{path: fmt.Sprintf("/lib/b/%d.mp3", n), essence: fmt.Sprintf("re%d", n), content: fmt.Sprintf("rc%d", n),
			title: "Renumbered", author: "Author", position: pos, durationMS: 1000}
	}
	var book model.PID
	for n := 1; n <= 3; n++ {
		book = putBook(t, st, lib.ID, part(n, n)).ItemPID
	}
	// 2500 is half a second into part 3, 2200 a fifth into it, 1500 half into part 2.
	if err := st.SetProgress(ctx, "", book, 2500, nil); err != nil {
		t.Fatal(err)
	}
	for _, pos := range []int64{2200, 1500} {
		if _, err := st.AddBookmark(ctx, "", book, pos, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Part 3 retagged to come first: the order is now 3, 1, 2.
	putBook(t, st, lib.ID, part(3, 0))

	if got := scalarInt(t, st, `SELECT ps.position_ms FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id
		WHERE pi.pid = ?`, string(book)); got != 500 {
		t.Errorf("resume position after the renumber = %d, want 500 (still inside part 3)", got)
	}
	marks, err := st.Bookmarks(ctx, "", book)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, m := range marks {
		got[m.PositionMS] = true
	}
	if len(marks) != 2 || !got[200] || !got[2500] {
		t.Errorf("bookmarks after the renumber = %+v, want 200 (part 3) and 2500 (part 2)", marks)
	}
	assertVerifyClean(t, st)
}

// TestARenumberedPartWithANewLengthPlacesByItsOldOne: a part read again under another
// number with a new length (trimmed, say) moves the places by the timeline they were taken
// on, where it ran its old length, so a place in the next part stays in that part.
func TestARenumberedPartWithANewLengthPlacesByItsOldOne(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	part := func(n, pos int, tag string, ms int64) bookSpec {
		return bookSpec{path: fmt.Sprintf("/lib/b/%d.mp3", n), essence: fmt.Sprintf("le%d%s", n, tag), content: fmt.Sprintf("lc%d%s", n, tag),
			title: "Trimmed", author: "Author", position: pos, durationMS: ms}
	}
	var book model.PID
	for n := 1; n <= 3; n++ {
		book = putBook(t, st, lib.ID, part(n, n, "", 1000)).ItemPID
	}
	// Half a second into part 2.
	if err := st.SetProgress(ctx, "", book, 1500, nil); err != nil {
		t.Fatal(err)
	}

	// Part 1 trimmed to 400 ms and retagged to come last: the order is now 2, 3, 1.
	putBook(t, st, lib.ID, part(1, 4, "-trim", 400))

	if got := scalarInt(t, st, `SELECT ps.position_ms FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id
		WHERE pi.pid = ?`, string(book)); got != 500 {
		t.Errorf("resume position after the renumber = %d, want 500 (still inside part 2)", got)
	}
	assertVerifyClean(t, st)
}

// TestARenumberedPartPlacesByItsNewChapters: the timeline a renumbered part's places land
// on is the one the read leaves, its new chapters included, so a part trimmed with its
// chapters does not run its old chapters' length ahead of the part holding a place.
func TestARenumberedPartPlacesByItsNewChapters(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	part := func(n, pos int, tag string, ms int64) bookSpec {
		return bookSpec{path: fmt.Sprintf("/lib/c/%d.mp3", n), essence: fmt.Sprintf("ce%d%s", n, tag), content: fmt.Sprintf("cc%d%s", n, tag),
			title: "Chaptered", author: "Author", position: pos, durationMS: ms,
			chapters: []model.Chapter{{Title: "Only", FileStartMS: 0, FileEndMS: ms}}}
	}
	var book model.PID
	for n := 1; n <= 3; n++ {
		book = putBook(t, st, lib.ID, part(n, n*10, "", 1000)).ItemPID
	}
	// Half a second into part 3.
	if err := st.SetProgress(ctx, "", book, 2500, nil); err != nil {
		t.Fatal(err)
	}

	// Part 1 trimmed to 400 ms, its chapter with it, and retagged between parts 2 and 3:
	// the order is now 2, 1, 3, and part 3 starts at 1400.
	putBook(t, st, lib.ID, part(1, 25, "-trim", 400))

	if got := scalarInt(t, st, `SELECT ps.position_ms FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id
		WHERE pi.pid = ?`, string(book)); got != 1900 {
		t.Errorf("resume position after the renumber = %d, want 1900 (still half a second into part 3)", got)
	}
	assertVerifyClean(t, st)
}

// TestARipJoiningALargerBookLeavesItUnfinished: a rip's tracks all finished, its file read
// again as one part of a book with another, leaves the book unfinished: the part was heard,
// the book was not.
func TestARipJoiningALargerBookLeavesItUnfinished(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{path: "/lib/t/2.mp3", essence: "te2", content: "tc2", title: "Tome", author: "Author", position: 2, durationMS: 1000})
	in := model.PutScannedVirtualTracksInput{
		LibraryID: lib.ID,
		File: model.File{Path: []byte("/lib/t/1.wav"), DisplayPath: "/lib/t/1.wav", RelPath: []byte("t/1.wav"),
			Kind: model.FileAudio, Size: 3, MTimeNS: 1, ContentHash: "tc1", EssenceHash: "te1", DurationMS: 2000, ScanState: model.ScanIndexed},
	}
	for n, w := range [][2]int64{{0, 44100}, {44100, 0}} {
		title := fmt.Sprintf("Track %d", n+1)
		in.Tracks = append(in.Tracks, model.VirtualTrack{
			Item:        model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: title, SortKey: model.SortKey(title), IdentityKey: identity.VirtualTrackKey("te1", n+1, w[0])},
			Track:       model.Track{Artist: "Author", AlbumArtist: "Author", Album: "Tome", TrackNo: n + 1},
			StartFrames: w[0], EndFrames: w[1],
		})
	}
	if _, err := st.PutScannedVirtualTracks(ctx, in); err != nil {
		t.Fatal(err)
	}
	rows, err := st.read.QueryContext(ctx, "SELECT pid FROM playable_item WHERE kind = 'track'")
	if err != nil {
		t.Fatal(err)
	}
	var tracks []model.PID
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			t.Fatal(err)
		}
		tracks = append(tracks, model.PID(pid))
	}
	rows.Close()
	if len(tracks) != 2 {
		t.Fatalf("rip tracks = %v, want two", tracks)
	}
	for _, pid := range tracks {
		if err := st.MarkPlayed(ctx, "", pid, true, nil); err != nil {
			t.Fatal(err)
		}
	}

	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/t/1.wav", essence: "te1", content: "tc1", title: "Tome", author: "Author", position: 1, durationMS: 2000}).ItemPID
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item WHERE kind = 'track'"); n != 0 {
		t.Fatalf("tracks after the file joined the book = %d, want them folded in", n)
	}
	if got := scalarInt(t, st, `SELECT COALESCE(MAX(ps.finished), 0) FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id
		WHERE pi.pid = ?`, string(book)); got != 0 {
		t.Errorf("the two-part book is finished = %d, want 0 (only the rip's part was heard)", got)
	}
}

// TestATrimmedPartMovesThePlacesAfterIt: a part read again at a new length under the same
// number moves the parts after it on the book's timeline, and their places with them, so
// a place in a later part stays in the same audio.
func TestATrimmedPartMovesThePlacesAfterIt(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	part := func(n int, tag string, ms int64) bookSpec {
		return bookSpec{path: fmt.Sprintf("/lib/m/%d.mp3", n), essence: fmt.Sprintf("me%d%s", n, tag), content: fmt.Sprintf("mc%d%s", n, tag),
			title: "Trimmed In Place", author: "Author", position: n, durationMS: ms}
	}
	var book model.PID
	for n := 1; n <= 3; n++ {
		book = putBook(t, st, lib.ID, part(n, "", 1000)).ItemPID
	}
	// Half a second into part 3, and a bookmark half a second into part 1.
	if err := st.SetProgress(ctx, "", book, 2500, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBookmark(ctx, "", book, 500, ""); err != nil {
		t.Fatal(err)
	}

	// Part 1 trimmed to 400 ms: part 3 now starts at 1400.
	putBook(t, st, lib.ID, part(1, "-trim", 400))

	if got := scalarInt(t, st, `SELECT ps.position_ms FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id
		WHERE pi.pid = ?`, string(book)); got != 1900 {
		t.Errorf("resume position after the trim = %d, want 1900 (still half a second into part 3)", got)
	}
	marks, err := st.Bookmarks(ctx, "", book)
	if err != nil || len(marks) != 1 || marks[0].PositionMS != 399 {
		t.Errorf("bookmarks after the trim = %+v (err %v), want the one in part 1 at its new last moment, 399", marks, err)
	}
	assertVerifyClean(t, st)
}

// TestAShortenedLastPartKeepsItsPlacesInside: the last part read again shorter moves no
// part's start, and a place past its new end comes back to its last moment.
func TestAShortenedLastPartKeepsItsPlacesInside(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	part := func(n int, tag string, ms int64) bookSpec {
		return bookSpec{path: fmt.Sprintf("/lib/s/%d.mp3", n), essence: fmt.Sprintf("se%d%s", n, tag), content: fmt.Sprintf("sc%d%s", n, tag),
			title: "Shortened", author: "Author", position: n, durationMS: ms}
	}
	var book model.PID
	for n := 1; n <= 2; n++ {
		book = putBook(t, st, lib.ID, part(n, "", 4000)).ItemPID
	}
	if err := st.SetProgress(ctx, "", book, 7900, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBookmark(ctx, "", book, 7900, ""); err != nil {
		t.Fatal(err)
	}

	putBook(t, st, lib.ID, part(2, "-cut", 3000))

	if got := scalarInt(t, st, `SELECT ps.position_ms FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id
		WHERE pi.pid = ?`, string(book)); got != 6999 {
		t.Errorf("resume position after the cut = %d, want 6999 (the last part's last moment)", got)
	}
	marks, err := st.Bookmarks(ctx, "", book)
	if err != nil || len(marks) != 1 || marks[0].PositionMS != 6999 {
		t.Errorf("bookmarks after the cut = %+v (err %v), want the one at 6999", marks, err)
	}
	assertVerifyClean(t, st)
}

// TestANewPartJoiningABookIsAnArrival: a part file new to the catalog joining a book that
// holds parts already arrives as a part from another item does: the book a listener
// finished did not hold it, so it is unfinished, and the places after it move on by its
// length, keeping their audio.
func TestANewPartJoiningABookIsAnArrival(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	part := func(dir string, n int) bookSpec {
		return bookSpec{path: fmt.Sprintf("/lib/%s/%d.mp3", dir, n), essence: fmt.Sprintf("%se%d", dir, n), content: fmt.Sprintf("%sc%d", dir, n),
			title: "Grown " + dir, author: "Author", position: n, durationMS: 1000}
	}
	finished := putBook(t, st, lib.ID, part("f", 1)).ItemPID
	if err := st.MarkPlayed(ctx, "", finished, true, nil); err != nil {
		t.Fatal(err)
	}
	putBook(t, st, lib.ID, part("f", 2))
	if got := scalarInt(t, st, `SELECT ps.finished FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id
		WHERE pi.pid = ?`, string(finished)); got != 0 {
		t.Errorf("finished after a new part joined = %d, want 0", got)
	}

	putBook(t, st, lib.ID, part("g", 1))
	placed := putBook(t, st, lib.ID, part("g", 3)).ItemPID
	// Half a second into part 3.
	if err := st.SetProgress(ctx, "", placed, 1500, nil); err != nil {
		t.Fatal(err)
	}
	putBook(t, st, lib.ID, part("g", 2))
	if got := scalarInt(t, st, `SELECT ps.position_ms FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id
		WHERE pi.pid = ?`, string(placed)); got != 2500 {
		t.Errorf("resume position after part 2 joined = %d, want 2500 (still half a second into part 3)", got)
	}
	assertVerifyClean(t, st)
}

// TestAPlaceMovedOntoATrackDatesItsRow: a resume position moved into a rip track that has
// play state of its own carries its time with it, so the row reads as changed when the
// newer place landed; the track's finished mark stays, as a newer place leaves it under
// SetProgress.
func TestAPlaceMovedOntoATrackDatesItsRow(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	whole := putTrack(t, st, lib.ID, trackSpec{path: "/lib/w/1.flac", essence: "we1", content: "wc1", title: "Whole", artist: "A", album: "W"}).ItemPID
	track := putTrack(t, st, lib.ID, trackSpec{path: "/lib/w/2.flac", essence: "we2", content: "wc2", title: "Track", artist: "A", album: "W"}).ItemPID
	if err := st.MarkPlayed(ctx, "", track, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProgress(ctx, "", whole, 5000, nil); err != nil {
		t.Fatal(err)
	}
	id := func(pid model.PID) int64 {
		return int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	}
	if _, err := st.write.ExecContext(ctx, "UPDATE play_state SET updated_at = 100, last_progress_at = 100 WHERE item_id = ?", id(track)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.ExecContext(ctx, "UPDATE play_state SET updated_at = 200, last_progress_at = 200 WHERE item_id = ?", id(whole)); err != nil {
		t.Fatal(err)
	}
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		return movePlacesIntoRipTx(ctx, tx, id(whole), []ripWindow{{item: id(track), start: 0, end: -1}})
	}); err != nil {
		t.Fatal(err)
	}
	var pos, updated, finished int64
	if err := st.read.QueryRowContext(ctx, "SELECT position_ms, updated_at, finished FROM play_state WHERE item_id = ?", id(track)).
		Scan(&pos, &updated, &finished); err != nil {
		t.Fatal(err)
	}
	if pos != 5000 || updated != 200 || finished != 1 {
		t.Errorf("the track's row after the move = position %d, updated %d, finished %d; want 5000, 200, 1", pos, updated, finished)
	}
}

// TestAPartWhoseChaptersShrankMovesThePlacesAfterIt: a part of unknown duration runs as
// far as its chapters, so new chapters under the same number and duration move the parts
// after it, and their places with them.
func TestAPartWhoseChaptersShrankMovesThePlacesAfterIt(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	part := func(n int, tag string, end int64) bookSpec {
		return bookSpec{path: fmt.Sprintf("/lib/k/%d.mp3", n), essence: fmt.Sprintf("ke%d%s", n, tag), content: fmt.Sprintf("kc%d%s", n, tag),
			title: "Chapter Run", author: "Author", position: n,
			chapters: []model.Chapter{{Title: "Only", FileStartMS: 0, FileEndMS: end}}}
	}
	var book model.PID
	for n := 1; n <= 3; n++ {
		book = putBook(t, st, lib.ID, part(n, "", 1000)).ItemPID
	}
	if err := st.SetProgress(ctx, "", book, 2500, nil); err != nil {
		t.Fatal(err)
	}
	// Part 1's chapter now ends at 400: part 3 starts at 1400.
	putBook(t, st, lib.ID, part(1, "", 400))
	if got := scalarInt(t, st, `SELECT ps.position_ms FROM play_state ps JOIN playable_item pi ON pi.id = ps.item_id
		WHERE pi.pid = ?`, string(book)); got != 1900 {
		t.Errorf("resume position after part 1's chapters shrank = %d, want 1900 (still half a second into part 3)", got)
	}
}
