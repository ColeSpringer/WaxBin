package scan

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
)

// titled returns the catalog's item with the title, failing when there is not exactly one.
func titled(t *testing.T, st *sqlite.Store, title string) *model.ItemView {
	t.Helper()
	items, err := st.QueryItems(context.Background(), query.New(query.EntityItems).Where("title", query.OpIs, title).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items titled %q = %d (err %v), want 1", title, len(items), err)
	}
	return items[0]
}

func TestPartShaped(t *testing.T) {
	for _, c := range []struct {
		title        string
		track, total int
		want         bool
	}{
		{"01", 0, 0, true}, {"7", 0, 0, true}, {"01 Intro", 0, 0, true}, {"07 - Arrival", 0, 0, true},
		{"1 - Intro", 0, 0, true}, {"1. Intro", 0, 0, true}, {"Part 2", 0, 0, true}, {"Chapter 12 - X", 0, 0, true},
		{"Chapter One", 0, 0, true}, {"CD1", 0, 0, true}, {"Prologue", 0, 0, true}, {"Epilogue", 0, 0, true},
		{"Introduction", 0, 0, true},
		{"12 Rules for Life", 0, 0, false}, {"11-22-63", 0, 0, false}, {"1Q84", 0, 0, false},
		{"48 Laws of Power", 0, 0, false}, {"Book 2 - The Two Towers", 0, 0, false}, {"1984", 0, 0, false},
		{"Part IV", 0, 0, true}, {"Track 03", 0, 0, true}, {"Chapterhouse Dune", 0, 0, false},
		{"Introduction to Algorithms", 0, 0, false}, {"Epilogue: Aftermath", 0, 0, true},
		{"Dune", 0, 0, false}, {"Dune", 1, 0, false}, {"Dune", 1, 1, false}, {"Dune", 2, 0, true}, {"Dune", 1, 12, true},
		{"", 1, 0, false},
	} {
		tags := &model.Tags{Title: c.title, TrackNo: c.track, TrackTotal: c.total}
		if got := PartShaped(tags, "/lib/Author/x.mp3"); got != c.want {
			t.Errorf("PartShaped(%q, track %d/%d) = %v, want %v", c.title, c.track, c.total, got, c.want)
		}
	}
}

// TestNumberedTitlesAreBooksOfTheirOwn: a book whose title opens with a number is not a
// numbered part, so it stays apart from the other book in its author's folder.
func TestNumberedTitlesAreBooksOfTheirOwn(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	narrator := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	writeUnder(t, root, "Jordan Peterson/12 Rules for Life.mp3", testaudio.MP3Spec{Title: "12 Rules for Life", Artist: "Jordan Peterson",
		TXXX: narrator, Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Jordan Peterson/Maps of Meaning.mp3", testaudio.MP3Spec{Title: "Maps of Meaning", Artist: "Jordan Peterson",
		Album: "Maps of Meaning", TXXX: narrator, Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	if books := itemsOfKind(t, st, model.KindBook); len(books) != 2 {
		t.Errorf("books = %d, want 12 Rules for Life and Maps of Meaning apart", len(books))
	}
}

// TestSingleTrackNumbersDoNotMakeParts: books with no album each tagged track 1 are books
// of their own, not parts of one book named for their author's folder.
func TestSingleTrackNumbersDoNotMakeParts(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	writeUnder(t, root, "Frank Herbert/Dune.mp3", testaudio.MP3Spec{Title: "Dune", Artist: "Frank Herbert", Track: 1,
		Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Frank Herbert/Children of Dune.mp3", testaudio.MP3Spec{Title: "Children of Dune", Artist: "Frank Herbert",
		Track: 1, TrackTotal: 1, Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	if books := itemsOfKind(t, st, model.KindBook); len(books) != 2 {
		t.Errorf("books = %d, want Dune and Children of Dune apart", len(books))
	}
}

// TestWordNamedPartsAreOneBook: untagged parts named for sections of a book are one book
// named for their folder, in reading order.
func TestWordNamedPartsAreOneBook(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	for i, name := range []string{"Epilogue", "Chapter One", "Prologue"} {
		writeUnder(t, root, "Author/Tome/"+name+".mp3", testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	scanAll(t, sc, lib, false)
	book := oneBook(t, st, 3)
	if book.Title != "Tome" {
		t.Errorf("book = %q, want Tome", book.Title)
	}
	detail, err := st.BookByPID(context.Background(), book.PID)
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	var order []string
	for _, p := range detail.Files {
		order = append(order, filepath.Base(p.DisplayPath))
	}
	if len(order) != 3 || order[0] != "Prologue.mp3" || order[1] != "Chapter One.mp3" || order[2] != "Epilogue.mp3" {
		t.Errorf("reading order = %v, want Prologue, Chapter One, Epilogue", order)
	}
}

// TestJoinedPartsKeepTheirNumberedOrder: untagged parts the folder rule takes in are read
// in the order their names number, around the tagged part.
func TestJoinedPartsKeepTheirNumberedOrder(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Author/Tome/02.mp3", testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(2)})
	writeUnder(t, root, "Author/Tome/03.mp3", testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(3)})
	scanAll(t, sc, lib, false)
	book := oneBook(t, st, 3)
	detail, err := st.BookByPID(context.Background(), book.PID)
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	var order []string
	for _, p := range detail.Files {
		order = append(order, filepath.Base(p.DisplayPath))
	}
	if len(order) != 3 || order[0] != "01.mp3" || order[1] != "02.mp3" || order[2] != "03.mp3" {
		t.Errorf("reading order = %v, want 01, 02, 03", order)
	}
}

// TestAudiobookRootFoldsItsTracks: a root re-declared audiobook makes one book of an
// album's tracks, and the tracks that do not become it fold into it: their stars, plays
// and playlist entries move to the book.
func TestAudiobookRootFoldsItsTracks(t *testing.T) {
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	for i := 1; i <= 3; i++ {
		writeUnder(t, root, "Author/Tome/0"+string(rune('0'+i))+".mp3", testaudio.MP3Spec{Title: "Chapter " + string(rune('0'+i)),
			Artist: "Author", AlbumArtist: "Author", Album: "Tome", Track: i, Audio: testaudio.AudioWithSeed(byte(i))})
	}
	scanAll(t, sc, lib, false)
	two, three := titled(t, st, "Chapter 2").PID, titled(t, st, "Chapter 3").PID
	if _, err := st.SetStar(ctx, "", two, true, nil); err != nil {
		t.Fatalf("star: %v", err)
	}
	if err := st.MarkPlayed(ctx, "", three, false, nil); err != nil {
		t.Fatalf("played: %v", err)
	}
	pl, err := st.CreatePlaylist(ctx, "List", "", model.PlaylistStatic, model.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("playlist: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{three}); err != nil {
		t.Fatalf("playlist items: %v", err)
	}
	lib, err = st.EnsureLibrary(ctx, &model.Library{Root: lib.Root, DisplayRoot: lib.DisplayRoot, Mode: lib.Mode,
		Media: model.MediaAudiobook, Profile: lib.Profile})
	if err != nil {
		t.Fatalf("declare audiobook: %v", err)
	}
	scanAll(t, sc, lib, false)
	book := oneBook(t, st, 3)
	state, err := st.PlayStateFor(ctx, "", book.PID)
	if err != nil {
		t.Fatalf("play state: %v", err)
	}
	if !state.Starred || state.PlayCount != 1 {
		t.Errorf("book state = starred %v, plays %d, want the folded track's star and play", state.Starred, state.PlayCount)
	}
	items, err := st.PlaylistItems(ctx, pl, "")
	if err != nil || len(items) != 1 || items[0].PID != book.PID {
		t.Errorf("playlist = %d items (err %v), want the book in the folded track's place", len(items), err)
	}
}

// TestForcedTrackTheFolderRuleWouldTakeIsLocked: a file forced to a track where the
// folder rule would give it to a sibling's book carries a kind lock, so a forced scan
// keeps it a track.
func TestForcedTrackTheFolderRuleWouldTakeIsLocked(t *testing.T) {
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(1)})
	scanAll(t, sc, lib, false)
	two := filepath.Join(root, "Author", "Tome", "02.mp3")
	writeUnder(t, root, "Author/Tome/02.mp3", testaudio.MP3Spec{Title: "Bonus", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 2, Audio: testaudio.AudioWithSeed(2)})
	_, out, err := sc.ScanFileAs(ctx, lib, two, model.KindTrack)
	if err != nil {
		t.Fatalf("scan as track: %v", err)
	}
	scanAll(t, sc, lib, true)
	if v, err := st.ItemByPID(ctx, out.ItemPID); err != nil || v.Kind != model.KindTrack {
		t.Errorf("forced track = %+v (err %v), want it kept a track", v, err)
	}
}

// TestRetitledBookTakesItsNewKey: a book with no album that is retitled keys under its new
// title, so a book later found under the old title is a book of its own.
func TestRetitledBookTakesItsNewKey(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	writeUnder(t, root, "A/Dune.mp3", testaudio.MP3Spec{Title: "Dune", Artist: "Frank Herbert", Audio: testaudio.AudioWithSeed(1)})
	scanAll(t, sc, lib, false)
	writeUnder(t, root, "A/Dune.mp3", testaudio.MP3Spec{Title: "Children of Dune", Artist: "Frank Herbert", Audio: testaudio.AudioWithSeed(1)})
	scanAll(t, sc, lib, false)
	writeUnder(t, root, "B/Dune.mp3", testaudio.MP3Spec{Title: "Dune", Artist: "Frank Herbert", Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	if books := itemsOfKind(t, st, model.KindBook); len(books) != 2 {
		t.Errorf("books = %d, want Children of Dune and Dune apart", len(books))
	}
}

// TestIdentifierBookKeepsAPartAfterATitleEdit: a part the folder rule gave a book keyed
// by its ASIN stays in it when read again after a catalog-only retitle of the book.
func TestIdentifierBookKeepsAPartAfterATitleEdit(t *testing.T) {
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}, {Desc: "ASIN", Value: "B00TOME"}},
		Audio: testaudio.AudioWithSeed(1)})
	for i := 2; i <= 3; i++ {
		writeUnder(t, root, "Author/Tome/0"+string(rune('0'+i))+".mp3", testaudio.MP3Spec{Title: "Chapter " + string(rune('0'+i)),
			Artist: "Author", AlbumArtist: "Author", Album: "Tome", Track: i, Audio: testaudio.AudioWithSeed(byte(i))})
	}
	scanAll(t, sc, lib, false)
	book := oneBook(t, st, 3)
	if err := st.EditItemField(ctx, book.PID, "title", "Tome Revised", model.Attribution{}, model.LockOn, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	writeUnder(t, root, "Author/Tome/02.mp3", testaudio.MP3Spec{Title: "Chapter Two", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 2, Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	oneBook(t, st, 3)
}

// TestSpeechInterludesStayOnTheirAlbum: a Speech genre (ID3's 101) names no audiobook in a
// mixed root, so the interludes on an album stay its tracks.
func TestSpeechInterludesStayOnTheirAlbum(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	for i, genre := range []string{"Hip-Hop", "(101)", "Hip-Hop", "Speech", "Hip-Hop"} {
		writeUnder(t, root, "Rapper/Record/0"+string(rune('1'+i))+".mp3", testaudio.MP3Spec{Title: "Cut " + string(rune('1'+i)),
			Artist: "Rapper", AlbumArtist: "Rapper", Album: "Record", Track: i + 1, Genre: genre, Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	scanAll(t, sc, lib, false)
	if tracks := itemsOfKind(t, st, model.KindTrack); len(tracks) != 5 {
		t.Errorf("tracks = %d, want the album's five cuts", len(tracks))
	}
	if books := itemsOfKind(t, st, model.KindBook); len(books) != 0 {
		t.Errorf("books = %d, want none", len(books))
	}
}

// TestFolderRuleIgnoresWalkOrderBetweenBooks: an untagged numbered part in a folder that
// holds two books joins neither, whichever book the walk reaches first.
func TestFolderRuleIgnoresWalkOrderBetweenBooks(t *testing.T) {
	for _, name := range []string{"a5.mp3", "c5.mp3"} {
		st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
		writeUnder(t, root, "Shelf/a1.mp3", testaudio.MP3Spec{Title: "One", Artist: "Author", Album: "A", Track: 1, Audio: testaudio.AudioWithSeed(1)})
		writeUnder(t, root, "Shelf/"+name, testaudio.MP3Spec{Title: "Part", Artist: "Author", Track: 5, Audio: testaudio.AudioWithSeed(5)})
		writeUnder(t, root, "Shelf/b1.mp3", testaudio.MP3Spec{Title: "One", Artist: "Author", Album: "B", Track: 1, Audio: testaudio.AudioWithSeed(2)})
		scanAll(t, sc, lib, false)
		if books := itemsOfKind(t, st, model.KindBook); len(books) != 3 {
			t.Errorf("%s: books = %d, want A, B and the part on its own", name, len(books))
		}
	}
}

// TestCopyFillsTheGapItsPartLeaves: when a part is retagged into another book, the copy
// of it left behind takes its place in the book it came from.
func TestCopyFillsTheGapItsPartLeaves(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	writeUnder(t, root, "Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", Album: "Tome", Track: 1,
		Audio: testaudio.AudioWithSeed(1)})
	part := testaudio.MP3Spec{Title: "Chapter 2", Artist: "Author", Album: "Tome", Track: 2, Audio: testaudio.AudioWithSeed(2)}
	writeUnder(t, root, "Tome/02.mp3", part)
	writeUnder(t, root, "Zspare/02.mp3", part)
	scanAll(t, sc, lib, false)
	tome := titled(t, st, "Tome")
	if n := partsOf(t, st, tome.PID); n != 2 {
		t.Fatalf("Tome parts = %d, want 2 with the spare a copy", n)
	}
	part.Album = "Other Book"
	writeUnder(t, root, "Tome/02.mp3", part)
	scanAll(t, sc, lib, false)
	if n := partsOf(t, st, tome.PID); n != 2 {
		t.Errorf("Tome parts = %d after its part 2 left, want the copy in its place", n)
	}
}

// TestLesserPartKeepsTheBookKey: a part with no ALBUM and no author left alone in its book
// when the tagged part is trashed keeps the book's key, so the tagged part put back joins
// the same book rather than starting another.
func TestLesserPartKeepsTheBookKey(t *testing.T) {
	ctx := context.Background()
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	first := testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", Album: "Tome", Track: 1, Audio: testaudio.AudioWithSeed(1)}
	writeUnder(t, root, "Author/Tome/01.mp3", first)
	writeUnder(t, root, "Author/Tome/02.mp3", testaudio.MP3Spec{Track: 2, Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	book := oneBook(t, st, 2)
	file, err := st.FileByPath(ctx, []byte(filepath.Join(root, "Author", "Tome", "01.mp3")))
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	res, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: file.PID, TrashPath: []byte("/trash/01.mp3"), TrashDisplay: "/trash/01.mp3"})
	if err != nil || len(res.Promoted) != 1 {
		t.Fatalf("trash = %+v (err %v), want part 2 promoted", res, err)
	}
	sc.RereadPromoted(ctx, res.Promoted)
	writeUnder(t, root, "Author/Tome/01.mp3", first)
	scanAll(t, sc, lib, false)
	if after := oneBook(t, st, 2); after.PID != book.PID {
		t.Errorf("book = %s, want %s with its tagged part back", after.PID, book.PID)
	}
}

// TestFolderRuleCountsTheCatalogsBooks: a numbered part with no ALBUM arriving in a folder
// of two books joins neither when the walk reads only one of them again.
func TestFolderRuleCountsTheCatalogsBooks(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	writeUnder(t, root, "Shelf/a1.mp3", testaudio.MP3Spec{Title: "One", Artist: "Author", Album: "A", Track: 1, Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Shelf/b1.mp3", testaudio.MP3Spec{Title: "One", Artist: "Author", Album: "B", Track: 1, Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	writeUnder(t, root, "Shelf/a1.mp3", testaudio.MP3Spec{Title: "One Again", Artist: "Author", Album: "A", Track: 1, Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "Shelf/a5.mp3", testaudio.MP3Spec{Title: "Part", Artist: "Author", Track: 5, Audio: testaudio.AudioWithSeed(5)})
	scanAll(t, sc, lib, false)
	if books := itemsOfKind(t, st, model.KindBook); len(books) != 3 {
		t.Errorf("books = %d, want A, B and the part on its own", len(books))
	}
}

// TestRetitleOntoAnotherBooksKeyKeepsItsBook: a book with no ALBUM retitled to the title
// another book of its author holds keeps its own item, since that key is taken.
func TestRetitleOntoAnotherBooksKeyKeepsItsBook(t *testing.T) {
	st, lib, sc, root := kindFixture(t, model.MediaAudiobook)
	writeUnder(t, root, "A/Dune.mp3", testaudio.MP3Spec{Title: "Dune", Artist: "Frank Herbert", Audio: testaudio.AudioWithSeed(1)})
	writeUnder(t, root, "B/Children.mp3", testaudio.MP3Spec{Title: "Children of Dune", Artist: "Frank Herbert", Audio: testaudio.AudioWithSeed(2)})
	scanAll(t, sc, lib, false)
	dune := titled(t, st, "Dune")
	writeUnder(t, root, "A/Dune.mp3", testaudio.MP3Spec{Title: "Children of Dune", Artist: "Frank Herbert", Audio: testaudio.AudioWithSeed(1)})
	if res := scanAll(t, sc, lib, false); res.Errored != 0 {
		t.Errorf("errored = %d, want none", res.Errored)
	}
	if books := itemsOfKind(t, st, model.KindBook); len(books) != 2 {
		t.Errorf("books = %d, want both kept", len(books))
	}
	if _, err := st.ItemByPID(context.Background(), dune.PID); err != nil {
		t.Errorf("retitled book: %v, want its item kept", err)
	}
}
