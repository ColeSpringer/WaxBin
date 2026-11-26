package waxbin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// partsLibrary opens a library over a root of the given media holding Tome by Author, three
// numbered parts in Author/Tome, scanned, with the options each extra sets.
func partsLibrary(t *testing.T, media model.MediaType, extra ...func(*waxbin.Options)) (*waxbin.Library, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	for i := 1; i <= 3; i++ {
		spec := testaudio.MP3Spec{Title: "Chapter " + string(rune('0'+i)), Artist: "Author", AlbumArtist: "Author", Album: "Tome",
			Track: i, Audio: testaudio.AudioWithSeed(byte(i))}
		if i == 1 {
			spec.TXXX = []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
		}
		writeFile(t, filepath.Join(root, "Author", "Tome", "0"+string(rune('0'+i))+".mp3"), testaudio.BuildMP3FromSpec(spec))
	}
	opts := waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace, Media: media}}}
	for _, f := range extra {
		f(&opts)
	}
	lib, err := waxbin.OpenSeeded(t, ctx, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	return lib, root
}

// bookNamed returns the one book of a title.
func bookNamed(t *testing.T, lib *waxbin.Library, title string) *model.BookDetail {
	t.Helper()
	ctx := context.Background()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Where("title", query.OpIs, title).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("books titled %q = %d (err %v), want one", title, len(items), err)
	}
	d, err := lib.Book(ctx, items[0].PID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// fileAt returns the pid of the file the catalog holds at path.
func fileAt(t *testing.T, lib *waxbin.Library, path string) model.PID {
	t.Helper()
	items, err := lib.Query(context.Background(), query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		files, err := lib.ItemFiles(context.Background(), it.PID)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if f.DisplayPath == path {
				return f.FilePID
			}
		}
	}
	t.Fatalf("no file at %s", path)
	return ""
}

// placeOf returns the position of a book's part on the file, -1 when it is none of its parts.
func placeOf(d *model.BookDetail, file model.PID) int {
	for _, p := range d.Files {
		if p.FilePID == file {
			return p.Position
		}
	}
	return -1
}

// TestAssignBookPartsFilesAndRenumbersAPart: a file whose tags make it a track of another
// album is filed into a book at the place named, the track it was folding into the book,
// and is then renumbered onto a second disc; the book keeps its title and author.
func TestAssignBookPartsFilesAndRenumbersAPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root := partsLibrary(t, model.MediaMixed)
	stray := filepath.Join(root, "Other", "stray.mp3")
	writeFile(t, stray, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Stray", Artist: "Someone", Album: "Wrong", Track: 9,
		Audio: testaudio.AudioWithSeed(30)}))
	scanLib(t, ctx, lib)
	tome, track, file := bookNamed(t, lib, "Tome"), itemPIDByTitle(t, ctx, lib, "Stray"), fileAt(t, lib, stray)
	rep, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: file, BookPID: tome.Item.PID, Part: 4}})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if rep.Absorbed[track] != tome.Item.PID || rep.Files[file] != tome.Item.PID {
		t.Errorf("report = %+v, want the track folded into Tome and the file there", rep)
	}
	d := bookNamed(t, lib, "Tome")
	if placeOf(d, file) != 4 || len(d.Files) != 4 || d.Item.Artist != "Author" {
		t.Fatalf("Tome = %d parts by %q, stray at %d; want four by Author, stray at 4", len(d.Files), d.Item.Artist, placeOf(d, file))
	}
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: file, BookPID: tome.Item.PID, Disc: 2, Part: 1}}); err != nil {
		t.Fatalf("renumber: %v", err)
	}
	if p := placeOf(bookNamed(t, lib, "Tome"), file); p != model.PartPosition(2, 1) {
		t.Errorf("renumbered part at %d, want disc 2 place 1", p)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	if p := placeOf(bookNamed(t, lib, "Tome"), file); p != model.PartPosition(2, 1) {
		t.Errorf("after a forced scan the part is at %d, want disc 2 place 1", p)
	}
}

// TestAssignBookPartsWithdraws: withdrawing an assignment files the part as WaxBin would on
// its own, and a forced scan files it the same: an untagged numbered part in its book's
// folder goes back to its own number there, a part whose tags name another album goes back
// to them, and a file with no assignment is left as it is.
func TestAssignBookPartsWithdraws(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root := partsLibrary(t, model.MediaMixed)
	loose := filepath.Join(root, "Author", "Tome", "04.mp3")
	writeFile(t, loose, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Chapter 4", Track: 4, Audio: testaudio.AudioWithSeed(40)}))
	stray := filepath.Join(root, "Other", "stray.mp3")
	writeFile(t, stray, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Stray", Artist: "Someone", Album: "Wrong", Track: 9,
		Audio: testaudio.AudioWithSeed(30)}))
	scanLib(t, ctx, lib)
	tome := bookNamed(t, lib, "Tome").Item.PID
	four, away, first := fileAt(t, lib, loose), fileAt(t, lib, stray), fileAt(t, lib, filepath.Join(root, "Author", "Tome", "01.mp3"))
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: four, BookPID: tome, Part: 9},
		{FilePID: away, BookPID: tome, Part: 5}}); err != nil {
		t.Fatal(err)
	}
	if d, _ := lib.Book(ctx, tome); placeOf(d, four) != 9 || placeOf(d, away) != 5 {
		t.Fatalf("assigned parts at %d and %d, want 9 and 5", placeOf(d, four), placeOf(d, away))
	}
	rep, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: four}, {FilePID: away}, {FilePID: first}})
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	check := func(when string) {
		t.Helper()
		d, err := lib.Book(ctx, tome)
		if err != nil {
			t.Fatal(err)
		}
		if placeOf(d, four) != 4 || placeOf(d, away) >= 0 || placeOf(d, first) != 1 || len(d.Files) != 4 {
			t.Errorf("%s: Tome = %+v, want 01 at 1, the untagged part back at 4 and the stray gone", when, d.Files)
		}
		for _, p := range d.Files {
			if p.Assigned {
				t.Errorf("%s: part %s still assigned", when, p.DisplayPath)
			}
		}
		if v, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "Stray")); err != nil || v.Kind != model.KindTrack || v.Album != "Wrong" {
			t.Errorf("%s: stray = %+v (err %v), want the track its tags name", when, v, err)
		}
	}
	check("withdrawn")
	if rep.Files[first] != tome {
		t.Errorf("report = %+v, want the unassigned part left in Tome", rep)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	check("after a forced scan")
}

// TestAssignBookPartsGathersOneBookFromOrphans: three one-part books an import left are
// gathered into the first, in order, the others folding into it with their stars.
func TestAssignBookPartsGathersOneBookFromOrphans(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	var paths []string
	for i, album := range []string{"Dune Part One", "Dune Disc 2", "dune_3"} {
		p := filepath.Join(root, "Drop", album, "track.mp3")
		writeFile(t, p, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: album, Artist: "Frank Herbert", Album: album,
			TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Scott Brick"}}, Audio: testaudio.AudioWithSeed(byte(70 + i))}))
		paths = append(paths, p)
	}
	lib, err := waxbin.OpenSeeded(t, ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	first, second := bookNamed(t, lib, "Dune Part One"), bookNamed(t, lib, "Dune Disc 2")
	if _, err := lib.Playback().SetStar(ctx, "", second.Item.PID, true, nil); err != nil {
		t.Fatal(err)
	}
	var changes []waxbin.BookPartChange
	for i, p := range paths {
		changes = append(changes, waxbin.BookPartChange{FilePID: fileAt(t, lib, p), BookPID: first.Item.PID, Part: i + 1})
	}
	rep, err := lib.AssignBookParts(ctx, changes)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Absorbed[second.Item.PID] != first.Item.PID || len(rep.Absorbed) != 2 {
		t.Errorf("absorbed = %v, want the other two books in the first", rep.Absorbed)
	}
	d := bookNamed(t, lib, "Dune Part One")
	if len(d.Files) != 3 || placeOf(d, fileAt(t, lib, paths[2])) != 3 {
		t.Fatalf("book = %+v, want three parts in order", d.Files)
	}
	if st, err := lib.Playback().State(ctx, "", first.Item.PID); err != nil || !st.Starred {
		t.Errorf("play state = %+v (err %v), want the star the second book carried", st, err)
	}
}

// TestAssignBookPartsRefusals: a target that is no book, an episode's file, a cue rip's file,
// a file not on disk, a library that takes no books, a track whose kind is locked, a book
// whose parts sit in another library, and two changes giving one place are each refused
// before anything changes.
func TestAssignBookPartsRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	podDir := t.TempDir()
	lib, root := partsLibrary(t, model.MediaMixed, func(o *waxbin.Options) { o.Podcasts = config.PodcastConfig{Dir: podDir} })
	epFile := filepath.Join(t.TempDir(), "episode.mp3")
	writeFile(t, epFile, testaudio.BuildMP3WithAudio("Episode", "Host", "Show", 1, testaudio.AudioWithSeed(83)))
	ep, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: epFile}, model.KindEpisode, waxbin.AcquiredMeta{ShowTitle: "Show"})
	if err != nil {
		t.Fatal(err)
	}
	music := t.TempDir()
	song := filepath.Join(music, "song.mp3")
	writeFile(t, song, testaudio.BuildMP3WithAudio("Song", "Band", "Record", 1, testaudio.AudioWithSeed(80)))
	if _, err := lib.AddRoot(ctx, config.Root{Path: music, Mode: model.ModeInPlace, Media: model.MediaMusic}); err != nil {
		t.Fatal(err)
	}
	const rate = 22050
	writeFile(t, filepath.Join(root, "Rip", "album.wav"), testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 20, testaudio.AltPartials, 7)))
	writeFile(t, filepath.Join(root, "Rip", "album.cue"), []byte("PERFORMER \"Rip\"\nTITLE \"Rip\"\nFILE \"album.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:10:00\n"))
	locked := filepath.Join(root, "Locked", "track.mp3")
	writeFile(t, locked, testaudio.BuildMP3WithAudio("Pinned", "Band", "Record", 2, testaudio.AudioWithSeed(81)))
	gone := filepath.Join(root, "Gone", "track.mp3")
	writeFile(t, gone, testaudio.BuildMP3WithAudio("Gone", "Band", "Record", 3, testaudio.AudioWithSeed(82)))
	second := t.TempDir()
	elsewhere := filepath.Join(second, "Else", "01.mp3")
	writeFile(t, elsewhere, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Else", Artist: "W", Album: "Else",
		TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "R"}}, Audio: testaudio.AudioWithSeed(84)}))
	if _, err := lib.AddRoot(ctx, config.Root{Path: second, Mode: model.ModeInPlace}); err != nil {
		t.Fatal(err)
	}
	scanLib(t, ctx, lib)
	pinned := itemPIDByTitle(t, ctx, lib, "Pinned")
	if err := lib.Lock(ctx, pinned, model.KindLockField); err != nil {
		t.Fatal(err)
	}
	goneFile := fileAt(t, lib, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	tome := bookNamed(t, lib, "Tome").Item.PID
	part := fileAt(t, lib, filepath.Join(root, "Author", "Tome", "01.mp3"))
	for _, c := range []struct {
		name    string
		changes []waxbin.BookPartChange
		code    waxerr.Code
		reason  waxerr.Reason
	}{
		{"no book", []waxbin.BookPartChange{{FilePID: part, BookPID: pinned}}, waxerr.CodeInvalid, waxbin.ReasonBookPartNotBook},
		{"an episode", []waxbin.BookPartChange{{FilePID: ep.FilePID, BookPID: tome}}, waxerr.CodeInvalid, waxbin.ReasonBookPartEpisode},
		{"a rip", []waxbin.BookPartChange{{FilePID: fileAt(t, lib, filepath.Join(root, "Rip", "album.wav")), BookPID: tome}},
			waxerr.CodeInvalid, waxbin.ReasonBookPartShared},
		{"not on disk", []waxbin.BookPartChange{{FilePID: goneFile, BookPID: tome}}, waxerr.CodeConflict, waxbin.ReasonBookPartNotOnDisk},
		{"music", []waxbin.BookPartChange{{FilePID: fileAt(t, lib, song), BookPID: tome}}, waxerr.CodeInvalid, waxbin.ReasonBookPartNoBooks},
		{"locked", []waxbin.BookPartChange{{FilePID: fileAt(t, lib, locked), BookPID: tome}}, waxerr.CodeLocked, waxbin.ReasonBookPartLocked},
		{"another library's book", []waxbin.BookPartChange{{FilePID: fileAt(t, lib, elsewhere), BookPID: tome}},
			waxerr.CodeInvalid, waxbin.ReasonBookPartOtherLibrary},
		{"one place", []waxbin.BookPartChange{{FilePID: part, BookPID: tome, Part: 2},
			{FilePID: fileAt(t, lib, filepath.Join(root, "Author", "Tome", "02.mp3")), BookPID: tome, Part: 2}},
			waxerr.CodeInvalid, waxbin.ReasonBookPartPlaceTaken},
		{"unknown file", []waxbin.BookPartChange{{FilePID: model.NewPID(), BookPID: tome}}, waxerr.CodeNotFound, ""},
		{"bad number", []waxbin.BookPartChange{{FilePID: part, BookPID: tome, Part: model.DiscStride}}, waxerr.CodeInvalid, ""},
	} {
		_, err := lib.AssignBookParts(ctx, c.changes)
		if !waxerr.Is(err, c.code) || waxerr.ReasonOf(err) != c.reason {
			t.Errorf("%s: err = %v (%s %q), want %s %q", c.name, err, waxerr.CodeOf(err), waxerr.ReasonOf(err), c.code, c.reason)
		}
	}
	if jobs, err := lib.Jobs(ctx, 50); err != nil || strings.Contains(jobKinds(jobs), "book-parts") {
		t.Errorf("jobs = %s (err %v), want no book-parts job started for a refusal", jobKinds(jobs), err)
	}
}

func jobKinds(list []*model.Job) string {
	var kinds []string
	for _, j := range list {
		kinds = append(kinds, j.Kind)
	}
	return strings.Join(kinds, ",")
}

// TestAKindChangeToTrackDropsAssignments: changing an assigned book to tracks withdraws its
// assignments, so a forced scan reads its files by their tags.
func TestAKindChangeToTrackDropsAssignments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root := partsLibrary(t, model.MediaMixed)
	stray := filepath.Join(root, "Other", "stray.mp3")
	writeFile(t, stray, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Stray", Artist: "Someone", Album: "Wrong", Track: 9,
		Audio: testaudio.AudioWithSeed(30)}))
	scanLib(t, ctx, lib)
	tome, file := bookNamed(t, lib, "Tome"), fileAt(t, lib, stray)
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: file, BookPID: tome.Item.PID, Part: 4}}); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.SetItemKind(ctx, []model.PID{tome.Item.PID}, model.KindTrack, waxbin.KindOptions{}); err != nil {
		t.Fatalf("kind change: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, IgnoreLocks: true}); err != nil {
		t.Fatal(err)
	}
	if v, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "Stray")); err != nil || v.Kind != model.KindTrack || v.Album != "Wrong" {
		t.Errorf("stray = %+v (err %v), want the track of Wrong its tags name", v, err)
	}
}

// TestStartAssignBookPartsRecordsItsReport: the background form runs the same change and
// records the report as its result.
func TestStartAssignBookPartsRecordsItsReport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root := partsLibrary(t, model.MediaMixed)
	stray := filepath.Join(root, "Other", "stray.mp3")
	writeFile(t, stray, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Stray", Artist: "Someone", Album: "Wrong", Track: 9,
		Audio: testaudio.AudioWithSeed(30)}))
	scanLib(t, ctx, lib)
	tome, file := bookNamed(t, lib, "Tome"), fileAt(t, lib, stray)
	job, err := lib.StartAssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: file, BookPID: tome.Item.PID, Part: 4}})
	if err != nil {
		t.Fatal(err)
	}
	var rep waxbin.BookPartsReport
	if err := json.Unmarshal([]byte(waitForJobDone(t, ctx, lib, job).Result), &rep); err != nil || rep.Files[file] != tome.Item.PID {
		t.Errorf("job result %+v (err %v), want the file in Tome", rep, err)
	}
}

// TestAWithdrawalNeedsNoBookLibrary: an assignment is withdrawn from a file whose library has
// since been declared music, which takes no new assignment.
func TestAWithdrawalNeedsNoBookLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root := partsLibrary(t, model.MediaMixed)
	stray := filepath.Join(root, "Other", "stray.mp3")
	writeFile(t, stray, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Stray", Artist: "Someone", Album: "Wrong", Track: 9,
		Audio: testaudio.AudioWithSeed(30)}))
	scanLib(t, ctx, lib)
	tome, file := bookNamed(t, lib, "Tome"), fileAt(t, lib, stray)
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: file, BookPID: tome.Item.PID, Part: 4}}); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.AddRoot(ctx, config.Root{Path: root, Mode: model.ModeInPlace, Media: model.MediaMusic}); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: file, BookPID: tome.Item.PID, Part: 5}}); waxerr.ReasonOf(err) != waxbin.ReasonBookPartNoBooks {
		t.Fatalf("a new assignment in a music library = %v, want refused", err)
	}
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: file}}); err != nil {
		t.Fatalf("withdrawal in a music library: %v", err)
	}
	if d, err := lib.Book(ctx, tome.Item.PID); err != nil || placeOf(d, file) >= 0 {
		t.Errorf("Tome after the withdrawal = %+v (err %v), want the file gone from it", d, err)
	}
}

// TestAssigningACopyMovesItsAudio: a copy of a part named into another book takes its audio
// there whole, the part it copies following at once and its one-part book folding in.
func TestAssigningACopyMovesItsAudio(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root := partsLibrary(t, model.MediaMixed)
	lone := testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Lone", Artist: "Writer", Album: "Lone",
		TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(50)})
	writeFile(t, filepath.Join(root, "Lone", "one.mp3"), lone)
	scanLib(t, ctx, lib)
	writeFile(t, filepath.Join(root, "Copy", "one.mp3"), lone)
	scanLib(t, ctx, lib)
	tome, loneBook := bookNamed(t, lib, "Tome").Item.PID, bookNamed(t, lib, "Lone").Item.PID
	part, cp := fileAt(t, lib, filepath.Join(root, "Lone", "one.mp3")), fileAt(t, lib, filepath.Join(root, "Copy", "one.mp3"))
	rep, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: cp, BookPID: tome, Part: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files[cp] != tome || rep.Files[part] != tome || rep.Absorbed[loneBook] != tome {
		t.Errorf("report = %+v, want both files in Tome and Lone folded into it", rep)
	}
	files, err := lib.ItemFiles(ctx, tome)
	if err != nil || len(files) != 5 {
		t.Fatalf("Tome files = %+v (err %v), want its three parts and the audio's two files", files, err)
	}
}

// TestAnAssignmentLeavesAnEpisodesDownload: a book part whose audio is also an episode's
// download is filed and withdrawn without the episode's file being read as a book or a track.
func TestAnAssignmentLeavesAnEpisodesDownload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root := partsLibrary(t, model.MediaMixed, func(o *waxbin.Options) { o.Podcasts = config.PodcastConfig{Dir: t.TempDir()} })
	chapter := testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Chapter", Artist: "LibriVox", Album: "Feed Book",
		TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Volunteer"}}, Audio: testaudio.AudioWithSeed(77)})
	download := filepath.Join(t.TempDir(), "download.mp3")
	writeFile(t, download, chapter)
	ep, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: download}, model.KindEpisode, waxbin.AcquiredMeta{ShowTitle: "Feed"})
	if err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(root, "Feed Book", "chapter.mp3")
	writeFile(t, part, chapter)
	scanLib(t, ctx, lib)
	tome, file := bookNamed(t, lib, "Tome").Item.PID, fileAt(t, lib, part)
	episodeKeepsItsFile := func(when string) {
		t.Helper()
		files, err := lib.ItemFiles(ctx, ep.EpisodePID)
		if err != nil || len(files) != 1 || files[0].FilePID != ep.FilePID {
			t.Fatalf("%s: episode files = %+v (err %v), want its download", when, files, err)
		}
		if v, err := lib.Get(ctx, ep.EpisodePID); err != nil || v.Kind != model.KindEpisode {
			t.Fatalf("%s: episode = %+v (err %v)", when, v, err)
		}
	}
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: file, BookPID: tome, Part: 4}}); err != nil {
		t.Fatal(err)
	}
	episodeKeepsItsFile("assigned")
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, ReResolve: true}); err != nil {
		t.Fatal(err)
	}
	episodeKeepsItsFile("a rescan")
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: file}}); err != nil {
		t.Fatal(err)
	}
	episodeKeepsItsFile("withdrawn")
}

// TestAPartTakesNoPlaceAnotherPartHolds: a part is refused a place another part of the book
// holds, unless that part moves off it in the same change, as a swap does.
func TestAPartTakesNoPlaceAnotherPartHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root := partsLibrary(t, model.MediaMixed)
	stray := filepath.Join(root, "Other", "stray.mp3")
	writeFile(t, stray, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Stray", Artist: "Someone", Album: "Wrong", Track: 9,
		Audio: testaudio.AudioWithSeed(30)}))
	scanLib(t, ctx, lib)
	tome := bookNamed(t, lib, "Tome").Item.PID
	two, three := fileAt(t, lib, filepath.Join(root, "Author", "Tome", "02.mp3")), fileAt(t, lib, filepath.Join(root, "Author", "Tome", "03.mp3"))
	_, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: fileAt(t, lib, stray), BookPID: tome, Part: 2}})
	if !waxerr.Is(err, waxerr.CodeInvalid) || waxerr.ReasonOf(err) != waxbin.ReasonBookPartPlaceTaken {
		t.Fatalf("a part given part 2's place = %v, want refused", err)
	}
	// A withdrawal leaves a part where its own tags put it, which may be the same place.
	_, err = lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: two}, {FilePID: fileAt(t, lib, stray), BookPID: tome, Part: 2}})
	if waxerr.ReasonOf(err) != waxbin.ReasonBookPartPlaceTaken {
		t.Fatalf("a part given the place of a part withdrawn beside it = %v, want refused", err)
	}
	// Another file of part 2's audio holds its place as that part, so naming it there is no clash.
	body, err := os.ReadFile(filepath.Join(root, "Author", "Tome", "02.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(root, "Copy", "02.mp3")
	writeFile(t, cp, body)
	scanLib(t, ctx, lib)
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: fileAt(t, lib, cp), BookPID: tome, Part: 2}}); err != nil {
		t.Fatalf("a copy of part 2 named to its place: %v", err)
	}
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: two, BookPID: tome, Part: 3},
		{FilePID: three, BookPID: tome, Part: 2}}); err != nil {
		t.Fatalf("a swap: %v", err)
	}
	if d := bookNamed(t, lib, "Tome"); placeOf(d, two) != 3 || placeOf(d, three) != 2 {
		t.Errorf("after the swap 02 is at %d and 03 at %d, want 3 and 2", placeOf(d, two), placeOf(d, three))
	}
}

// TestACopyThatCannotBeReadHoldsUpNothing: a copy of a part's audio that cannot be read
// again is left for its next scan, and the change goes on to file the rest.
func TestACopyThatCannotBeReadHoldsUpNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root := partsLibrary(t, model.MediaMixed)
	lone := testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Lone", Artist: "Writer", Album: "Lone",
		TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(50)})
	writeFile(t, filepath.Join(root, "Lone", "one.mp3"), lone)
	scanLib(t, ctx, lib)
	cp := filepath.Join(root, "Copy", "one.mp3")
	writeFile(t, cp, lone)
	stray := filepath.Join(root, "Other", "stray.mp3")
	writeFile(t, stray, testaudio.BuildMP3WithAudio("Stray", "Someone", "Wrong", 9, testaudio.AudioWithSeed(30)))
	scanLib(t, ctx, lib)
	tome := bookNamed(t, lib, "Tome").Item.PID
	part, other := fileAt(t, lib, filepath.Join(root, "Lone", "one.mp3")), fileAt(t, lib, stray)
	if err := os.Remove(cp); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cp, 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: part, BookPID: tome, Part: 4}, {FilePID: other, BookPID: tome, Part: 5}})
	if err != nil {
		t.Fatalf("assign with an unreadable copy: %v", err)
	}
	if rep.Files[part] != tome || rep.Files[other] != tome {
		t.Errorf("report = %+v, want both named files in Tome", rep)
	}
}

// TestAssignBookPartsRefusesAudioNamedTwice and a file that is not audio: two files of one
// audio, which takes one place, and a file row that is no audio are refused.
func TestAssignBookPartsRefusesAudioNamedTwice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var db string
	lib, root := partsLibrary(t, model.MediaMixed, func(o *waxbin.Options) { db = o.DBPath })
	lone := testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Lone", Artist: "Writer", Album: "Lone",
		TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(50)})
	writeFile(t, filepath.Join(root, "Lone", "one.mp3"), lone)
	scanLib(t, ctx, lib)
	writeFile(t, filepath.Join(root, "Copy", "one.mp3"), lone)
	song := filepath.Join(root, "Song", "song.mp3")
	writeFile(t, song, testaudio.BuildMP3WithAudio("Song", "Band", "Record", 1, testaudio.AudioWithSeed(51)))
	scanLib(t, ctx, lib)
	tome, lonePID := bookNamed(t, lib, "Tome").Item.PID, bookNamed(t, lib, "Lone").Item.PID
	part, cp := fileAt(t, lib, filepath.Join(root, "Lone", "one.mp3")), fileAt(t, lib, filepath.Join(root, "Copy", "one.mp3"))
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: part, BookPID: tome, Part: 4},
		{FilePID: cp, BookPID: lonePID, Part: 1}}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("one audio sent two places = %v, want refused", err)
	}
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: part, BookPID: tome, Part: 4},
		{FilePID: cp, BookPID: tome, Part: 4}}); err != nil {
		t.Errorf("one audio's two files sent one place: %v", err)
	}
	rep, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: part}, {FilePID: cp}})
	if err != nil || len(rep.Withdrawn) != 1 {
		t.Errorf("one audio's two files withdrawn = %+v (err %v), want its one assignment withdrawn", rep, err)
	}
	image := fileAt(t, lib, song)
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("UPDATE file SET kind = 'image' WHERE pid = ?", string(image)); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: image, BookPID: tome}}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("a file that is no audio = %v, want refused", err)
	}
}
