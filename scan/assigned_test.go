package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

func posAt(n int) *int { return &n }

// bookTitled returns the book of a title, failing unless there is exactly one.
func bookTitled(t *testing.T, st *sqlite.Store, title string) *model.ItemView {
	t.Helper()
	var found *model.ItemView
	for _, b := range itemsOfKind(t, st, model.KindBook) {
		if b.Title == title {
			if found != nil {
				t.Fatalf("two books titled %q", title)
			}
			found = b
		}
	}
	if found == nil {
		t.Fatalf("no book titled %q", title)
	}
	return found
}

// partAt returns the position of the book's part on the file at path, or -1.
func partAt(t *testing.T, st *sqlite.Store, book model.PID, path string) int {
	t.Helper()
	files, err := st.ItemFiles(context.Background(), book)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Role != "alternate" && f.DisplayPath == path {
			return f.Position
		}
	}
	return -1
}

// TestAnAssignmentFilesAFileOnEveryRead: a file a host assigned to a book is that book's
// part at the place assigned, whatever its tags name, when the walk reads it, when it is
// read alone, and when a copy of it is promoted into its place and read again; the book
// keeps its title and author.
func TestAnAssignmentFilesAFileOnEveryRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeBookFolder(t, root, 1)
	stray := filepath.Join(root, "Other", "stray.mp3")
	stated := testaudio.MP3Spec{Title: "Stray", Artist: "Someone", Album: "Wrong", Track: 9, Audio: testaudio.AudioWithSeed(30)}
	writeUnder(t, root, "Other/stray.mp3", stated)
	scanAll(t, sc, lib, false)
	tome := bookTitled(t, st, "Tome")
	if _, _, err := sc.ScanFileWith(ctx, lib, stray, FileOptions{Assign: &Assignment{BookPID: tome.PID, Position: posAt(4)}}); err != nil {
		t.Fatal(err)
	}
	check := func(when, path string) {
		t.Helper()
		book := oneBook(t, st, 4)
		if book.PID != tome.PID || book.Title != "Tome" || book.Artist != "Author" {
			t.Fatalf("%s: book = %s %q by %q, want Tome by Author", when, book.PID, book.Title, book.Artist)
		}
		if p := partAt(t, st, tome.PID, path); p != 4 {
			t.Fatalf("%s: part at %d, want 4", when, p)
		}
	}
	check("assigned", stray)
	scanAll(t, sc, lib, true)
	check("a forced walk", stray)
	if _, _, err := sc.ScanFileWith(ctx, lib, stray, FileOptions{}); err != nil {
		t.Fatal(err)
	}
	check("a read alone", stray)

	writeUnder(t, root, "Copy/stray.mp3", stated)
	scanAll(t, sc, lib, false)
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)
	check("the copy promoted", filepath.Join(root, "Copy", "stray.mp3"))
}

// TestTheFolderRuleLeavesAnAssignedFileAlone: a file assigned to a book stays there though
// its folder holds another book its tags name, on a forced walk and when its folder settles
// around it unread; and the book it is assigned to counts as one its folder holds.
func TestTheFolderRuleLeavesAnAssignedFileAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeBookFolder(t, root, 1)
	writeUnder(t, root, "Author/Tome/04.mp3", testaudio.MP3Spec{Title: "Chapter 4", Artist: "Author", Album: "Tome", Track: 4,
		Audio: testaudio.AudioWithSeed(40)})
	writeUnder(t, root, "Writer/Else/01.mp3", testaudio.MP3Spec{Title: "Else One", Artist: "Writer", AlbumArtist: "Writer",
		Album: "Else", Track: 1, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(50)})
	scanAll(t, sc, lib, false)
	tome, elseBook := bookTitled(t, st, "Tome"), bookTitled(t, st, "Else")
	if n := partsOf(t, st, tome.PID); n != 4 {
		t.Fatalf("Tome has %d parts before the assignment, want 4", n)
	}
	four := filepath.Join(root, "Author", "Tome", "04.mp3")
	if _, _, err := sc.ScanFileWith(ctx, lib, four, FileOptions{Assign: &Assignment{BookPID: elseBook.PID, Position: posAt(2)}}); err != nil {
		t.Fatal(err)
	}
	counts := func(when string) {
		t.Helper()
		if a, b := partsOf(t, st, tome.PID), partsOf(t, st, elseBook.PID); a != 3 || b != 2 {
			t.Fatalf("%s: Tome %d parts, Else %d, want 3 and 2", when, a, b)
		}
	}
	counts("assigned")
	scanAll(t, sc, lib, true)
	counts("a forced walk")
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, Genre: "Retagged", TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}},
		Audio: testaudio.AudioWithSeed(1)})
	scanAll(t, sc, lib, false)
	counts("a settle around it")

	loose := filepath.Join(root, "Loose", "01.mp3")
	writeUnder(t, root, "Loose/01.mp3", testaudio.MP3Spec{Title: "Loose One", Track: 1, Audio: testaudio.AudioWithSeed(42)})
	if _, _, err := sc.ScanFileWith(ctx, lib, loose, FileOptions{Assign: &Assignment{BookPID: elseBook.PID, Position: posAt(3)}}); err != nil {
		t.Fatal(err)
	}
	writeUnder(t, root, "Loose/02.mp3", testaudio.MP3Spec{Title: "Loose Two", Track: 2, Audio: testaudio.AudioWithSeed(43)})
	scanAll(t, sc, lib, false)
	if p := partAt(t, st, elseBook.PID, filepath.Join(root, "Loose", "02.mp3")); p < 0 {
		t.Errorf("a numbered part beside an assigned one did not join the book the folder holds")
	}
}

// TestAForcedTrackReadWithdrawsTheAssignment: reading an assigned part as a track by force
// makes it a track and withdraws the assignment, so the next scan reads it by its tags.
func TestAForcedTrackReadWithdrawsTheAssignment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeBookFolder(t, root, 1)
	writeUnder(t, root, "Other/stray.mp3", testaudio.MP3Spec{Title: "Stray", Artist: "Someone", Album: "Wrong", Track: 9,
		Audio: testaudio.AudioWithSeed(30)})
	scanAll(t, sc, lib, false)
	tome := bookTitled(t, st, "Tome")
	stray := filepath.Join(root, "Other", "stray.mp3")
	if _, _, err := sc.ScanFileWith(ctx, lib, stray, FileOptions{Assign: &Assignment{BookPID: tome.PID, Position: posAt(4)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sc.ScanFileAs(ctx, lib, stray, model.KindTrack); err != nil {
		t.Fatal(err)
	}
	if ap, err := st.AssignedPart(ctx, []byte(stray), ""); err != nil || ap != nil {
		t.Fatalf("assignment after a forced track read = %+v (err %v), want none", ap, err)
	}
	scanAll(t, sc, lib, true)
	if tracks := itemsOfKind(t, st, model.KindTrack); len(tracks) != 1 || tracks[0].Title != "Stray" {
		t.Fatalf("tracks = %d, want Stray read by its tags", len(tracks))
	}
	if n := partsOf(t, st, tome.PID); n != 3 {
		t.Errorf("Tome has %d parts, want its own 3", n)
	}
}

// TestAnAssignmentByTitleKeysOneBook: files whose tags name three books, with three ASINs,
// assigned to one book by its title and author are its parts in the order assigned, and the
// book takes none of their identifiers.
func TestAnAssignmentByTitleKeysOneBook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	for i, album := range []string{"First Guess", "Second Guess", "Third Guess"} {
		rel := "Drop/" + string(rune('a'+i)) + ".mp3"
		writeUnder(t, root, rel, testaudio.MP3Spec{Title: album, Artist: "Tagger " + album, Album: album, Track: 1,
			TXXX: []testaudio.TXXXFrame{{Desc: "ASIN", Value: "B00000000" + string(rune('1'+i))}}, Audio: testaudio.AudioWithSeed(byte(60 + i))})
		path := filepath.Join(root, "Drop", string(rune('a'+i))+".mp3")
		if _, _, err := sc.ScanFileWith(ctx, lib, path, FileOptions{Assign: &Assignment{Title: "Dune", Author: "Frank Herbert",
			Total: 3, Position: posAt(3 - i)}}); err != nil {
			t.Fatal(err)
		}
	}
	book := oneBook(t, st, 3)
	if book.Title != "Dune" || book.Artist != "Frank Herbert" {
		t.Fatalf("book = %q by %q, want Dune by Frank Herbert", book.Title, book.Artist)
	}
	d, err := st.BookByPID(ctx, book.PID)
	if err != nil {
		t.Fatal(err)
	}
	if d.ASIN != "" || !d.IdentityHeld {
		t.Errorf("book asin %q, identity held %v; want none and held", d.ASIN, d.IdentityHeld)
	}
	if len(d.Files) != 3 || filepath.Base(d.Files[0].DisplayPath) != "c.mp3" || filepath.Base(d.Files[2].DisplayPath) != "a.mp3" {
		t.Errorf("parts = %+v, want c, b, a", d.Files)
	}
}

// TestThePodcastLibraryReadsNoAssignment: a read in the internal podcast library, whose
// files are episodes, files nothing by the assignment its audio holds.
func TestThePodcastLibraryReadsNoAssignment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeBookFolder(t, root, 1)
	writeUnder(t, root, "Other/stray.mp3", testaudio.MP3Spec{Title: "Stray", Artist: "Someone", Album: "Wrong", Track: 9,
		Audio: testaudio.AudioWithSeed(30)})
	scanAll(t, sc, lib, false)
	tome := bookTitled(t, st, "Tome")
	stray := filepath.Join(root, "Other", "stray.mp3")
	if _, _, err := sc.ScanFileWith(ctx, lib, stray, FileOptions{Assign: &Assignment{BookPID: tome.PID, Position: posAt(4)}}); err != nil {
		t.Fatal(err)
	}
	podcasts := *lib
	podcasts.Mode = model.ModePodcast
	if _, _, err := sc.ScanFileWith(ctx, &podcasts, stray, FileOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := partsOf(t, st, tome.PID); n != 3 {
		t.Errorf("Tome has %d parts after a podcast library read, want its own 3", n)
	}
}
