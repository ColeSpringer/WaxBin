package waxbin_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// librivoxPart writes chapter n of a LibriVox-style recording: the author is the artist,
// the book the album, the reader rides COMPOSER and the genre is Speech, none of which
// makes a mixed root file it as a book.
func librivoxPart(t *testing.T, path string, n int) {
	t.Helper()
	writeFile(t, path, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: fmt.Sprintf("Chapter %02d", n), Artist: "Jane Austen", AlbumArtist: "Jane Austen",
		Album: "Persuasion", Composer: "Elizabeth Klett", Genre: "Speech", Track: n,
		Audio: testaudio.AudioWithSeed(byte(n * 11)),
	}))
}

// librivoxTrack opens a mixed root holding one LibriVox-style chapter, scans it as a track
// and returns the library, the track and its file's path.
func librivoxTrack(t *testing.T, ctx context.Context) (*waxbin.Library, model.PID, string, string) {
	t.Helper()
	root, db := t.TempDir(), filepath.Join(t.TempDir(), "catalog.db")
	path := filepath.Join(root, "Jane Austen", "Persuasion", "01.mp3")
	librivoxPart(t, path, 1)
	lib := openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)
	pid := itemPIDByTitle(t, ctx, lib, "Chapter 01")
	if v, err := lib.Get(ctx, pid); err != nil || v.Kind != model.KindTrack {
		t.Fatalf("item = %+v (err %v), want a track", v, err)
	}
	return lib, pid, path, db
}

// TestSetItemKindMakesATrackABook: a LibriVox-style MP3 a mixed root files as a track
// becomes a book in place. It keeps its pid and resume position, reads back with one
// whole-file chapter and the reader as its narrator, carries a kind lock, and a forced
// scan leaves it a book. The change runs as a set-kind job.
func TestSetItemKindMakesATrackABook(t *testing.T) {
	ctx := context.Background()
	lib, pid, _, _ := librivoxTrack(t, ctx)
	if err := lib.Playback().Checkpoint(ctx, "", pid, 300, nil); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	rep, err := lib.SetItemKind(ctx, []model.PID{pid}, model.KindBook, waxbin.KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if !slices.Equal(rep.Converted, []model.PID{pid}) || len(rep.Absorbed) != 0 || len(rep.Created) != 0 {
		t.Errorf("report = %+v, want the track converted and nothing absorbed or created", rep)
	}
	assertPersuasionBook(t, ctx, lib, pid)
	if row := kindRow(t, ctx, lib, pid); row == nil || !row.Locked || row.Source != model.SourceUser {
		t.Errorf("kind row = %+v, want a user lock", row)
	}
	if jobs, err := lib.Jobs(ctx, 10); err != nil || !hasDoneJob(jobs, "set-kind") {
		t.Errorf("jobs (err %v): want a finished set-kind job", err)
	}

	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	assertPersuasionBook(t, ctx, lib, pid)
}

// assertPersuasionBook checks the converted chapter reads back as the book it names.
func assertPersuasionBook(t *testing.T, ctx context.Context, lib *waxbin.Library, pid model.PID) {
	t.Helper()
	book, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if book.Item.Kind != model.KindBook || book.Item.Title != "Persuasion" {
		t.Errorf("book item = %s %q, want the book Persuasion", book.Item.Kind, book.Item.Title)
	}
	if !slices.Equal(book.Narrators, []string{"Elizabeth Klett"}) || !slices.Equal(book.Authors, []string{"Jane Austen"}) {
		t.Errorf("credits = authors %v narrators %v, want Jane Austen read by Elizabeth Klett", book.Authors, book.Narrators)
	}
	if len(book.Chapters) != 1 || book.Chapters[0].Title != "Chapter 01" {
		t.Errorf("chapters = %+v, want the one whole-file chapter", book.Chapters)
	}
	if st, err := lib.Playback().State(ctx, "", pid); err != nil || st.PositionMS != 300 {
		t.Errorf("play state = %+v (err %v), want the position kept", st, err)
	}
}

// TestSetItemKindWritesTheMediaType: with write-back the kind reaches the files (MEDIATYPE 2
// in an MP3, the stik atom in an M4A), so a forced scan that ignores locks still reads a
// book, and turning the book back into a track clears it, so that scan reads a track.
func TestSetItemKindWritesTheMediaType(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mp3 := filepath.Join(root, "Jane Austen", "Persuasion", "01.mp3")
	librivoxPart(t, mp3, 1)
	m4a := filepath.Join(root, "Sample", "sample.m4a")
	writeFile(t, m4a, testaudio.Fixture(t, "sample.m4a"))
	if _, err := meta.NewWriter().Apply(ctx, m4a, []meta.TagEdit{
		{Key: "TITLE", Values: []string{"Sample Chapter"}}, {Key: "ALBUM", Values: []string{"Sample Book"}},
		{Key: "ARTIST", Values: []string{"Some Author"}},
	}); err != nil {
		t.Fatalf("stage m4a: %v", err)
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	pids := []model.PID{itemPIDByTitle(t, ctx, lib, "Chapter 01"), itemPIDByTitle(t, ctx, lib, "Sample Chapter")}

	rep, err := lib.SetItemKind(ctx, pids, model.KindBook, waxbin.KindOptions{WriteBack: true})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if len(rep.Converted) != 2 || len(rep.WriteBackFailures) != 0 {
		t.Fatalf("report = %+v, want both converted and written", rep)
	}
	for _, path := range []string{mp3, m4a} {
		if fm, err := meta.NewReader().Read(ctx, path); err != nil || fm.Tags.BookSignal != model.BookTagSignal {
			t.Errorf("%s book signal = %v (err %v), want the written media type", filepath.Base(path), fm.Tags.BookSignal, err)
		}
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, IgnoreLocks: true}); err != nil {
		t.Fatalf("scan ignoring locks: %v", err)
	}
	for _, pid := range pids {
		if v, err := lib.Get(ctx, pid); err != nil || v.Kind != model.KindBook {
			t.Errorf("%s after a scan ignoring locks = %+v (err %v), want a book", pid, v, err)
		}
	}

	// The change locked them as books, so changing them back takes force.
	if _, err := lib.SetItemKind(ctx, pids, model.KindTrack, waxbin.KindOptions{WriteBack: true, Force: true}); err != nil {
		t.Fatalf("set kind back: %v", err)
	}
	for _, path := range []string{mp3, m4a} {
		if fm, err := meta.NewReader().Read(ctx, path); err != nil || fm.Tags.BookSignal != model.NoBookSignal {
			t.Errorf("%s book signal = %v (err %v), want the media type cleared", filepath.Base(path), fm.Tags.BookSignal, err)
		}
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, IgnoreLocks: true}); err != nil {
		t.Fatalf("scan ignoring locks: %v", err)
	}
	for _, pid := range pids {
		if v, err := lib.Get(ctx, pid); err != nil || v.Kind != model.KindTrack {
			t.Errorf("%s after the second scan = %+v (err %v), want a track", pid, v, err)
		}
	}
}

// TestSetItemKindHonoursAKindLock: an item whose kind is locked is refused without Force
// and left as it was; with Force it converts and the lock pins the new kind.
func TestSetItemKindHonoursAKindLock(t *testing.T) {
	ctx := context.Background()
	lib, pid, _, _ := librivoxTrack(t, ctx)
	if err := lib.Lock(ctx, pid, model.KindLockField); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := lib.SetItemKind(ctx, []model.PID{pid}, model.KindBook, waxbin.KindOptions{}); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("set kind over a lock = %v, want CodeLocked", err)
	}
	if v, _ := lib.Get(ctx, pid); v.Kind != model.KindTrack {
		t.Fatalf("kind after the refusal = %s, want the track left alone", v.Kind)
	}
	if _, err := lib.SetItemKind(ctx, []model.PID{pid}, model.KindBook, waxbin.KindOptions{Force: true}); err != nil {
		t.Fatalf("forced set kind: %v", err)
	}
	if v, _ := lib.Get(ctx, pid); v.Kind != model.KindBook {
		t.Errorf("kind = %s, want the forced book", v.Kind)
	}
	if row := kindRow(t, ctx, lib, pid); row == nil || !row.Locked {
		t.Errorf("kind row = %+v, want the book locked", row)
	}
	// A kind the item already has needs no force.
	if _, err := lib.SetItemKind(ctx, []model.PID{pid}, model.KindBook, waxbin.KindOptions{}); err != nil {
		t.Errorf("set kind to the locked kind: %v", err)
	}
}

// TestSetItemKindRefusals: what a kind change cannot apply to is refused before anything
// changes: an episode, a cue track, a file another item shares, a book whose chapters a
// cue sheet gives, a file not on disk, a target kind other than track or book, and an
// unknown item.
func TestSetItemKindRefusals(t *testing.T) {
	ctx := context.Background()
	root, pod := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, title := range []string{"Plain", "Windowed", "Shared", "Sharer", "Gone"} {
		writeFile(t, filepath.Join(root, title+".mp3"), testaudio.BuildMP3WithAudio(title, "Artist", "Album "+title, 1,
			testaudio.AudioWithSeed(byte(40+i))))
	}
	books := t.TempDir()
	writeFile(t, filepath.Join(books, "Austen", "Emma", "emma.mp3"), testaudio.BuildMP3WithAudio("Emma", "Jane Austen", "Emma", 1,
		testaudio.AudioWithSeed(70)))
	writeFile(t, filepath.Join(books, "Austen", "Emma", "emma.cue"), []byte("FILE \"emma.mp3\" MP3\n"+
		"  TRACK 01 AUDIO\n    TITLE \"Volume One\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Volume Two\"\n    INDEX 01 00:00:20\n"))
	lib := openMediaTyped(t, ctx, db, root, books, pod)
	scanLib(t, ctx, lib)
	cued := itemPIDByTitle(t, ctx, lib, "Emma")
	if chapters, err := lib.Chapters(ctx, cued); err != nil || len(chapters) != 2 {
		t.Fatalf("Emma chapters = %d (err %v), want the sheet's two", len(chapters), err)
	}
	episode := filepath.Join(t.TempDir(), "ep.mp3")
	writeFile(t, episode, testaudio.BuildMP3WithAudio("Episode", "Host", "Show", 1, testaudio.AudioWithSeed(60)))
	ep, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: episode}, model.KindEpisode, waxbin.AcquiredMeta{
		ShowTitle: "Show", SourceType: model.SourceManual, Title: "Episode"})
	if err != nil {
		t.Fatalf("import episode: %v", err)
	}
	plain := itemPIDByTitle(t, ctx, lib, "Plain")
	windowed := itemPIDByTitle(t, ctx, lib, "Windowed")
	makeBackingFileVirtual(t, ctx, db, windowed)
	shared := itemPIDByTitle(t, ctx, lib, "Shared")
	shareFileWith(t, ctx, db, shared, itemPIDByTitle(t, ctx, lib, "Sharer"))
	gone := itemPIDByTitle(t, ctx, lib, "Gone")
	if err := os.Remove(filepath.Join(root, "Gone.mp3")); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		pids []model.PID
		kind model.Kind
		code waxerr.Code
	}{
		{"episode", []model.PID{ep.EpisodePID}, model.KindBook, waxerr.CodeInvalid},
		{"cue track", []model.PID{windowed}, model.KindBook, waxerr.CodeInvalid},
		{"shared file", []model.PID{shared}, model.KindBook, waxerr.CodeInvalid},
		// As a track the sheet would carve the file into cue tracks, which no lock holds.
		{"book chaptered by a cue sheet", []model.PID{cued}, model.KindTrack, waxerr.CodeInvalid},
		{"file not on disk", []model.PID{gone}, model.KindBook, waxerr.CodeConflict},
		{"episode kind", []model.PID{plain}, model.KindEpisode, waxerr.CodeInvalid},
		{"no items", nil, model.KindBook, waxerr.CodeInvalid},
		{"unknown item", []model.PID{"01ZZZZZZZZZZZZZZZZZZZZZZZZ"}, model.KindBook, waxerr.CodeNotFound},
		// One bad item refuses the whole request, the good one included.
		{"mixed", []model.PID{plain, windowed}, model.KindBook, waxerr.CodeInvalid},
	} {
		if _, err := lib.SetItemKind(ctx, c.pids, c.kind, waxbin.KindOptions{}); !waxerr.Is(err, c.code) {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
	for _, pid := range []model.PID{plain, windowed, shared} {
		if v, err := lib.Get(ctx, pid); err != nil || v.Kind != model.KindTrack {
			t.Errorf("%s after the refusals = %+v (err %v), want it still a track", pid, v, err)
		}
	}
	if v, err := lib.Get(ctx, cued); err != nil || v.Kind != model.KindBook {
		t.Errorf("Emma after the refusals = %+v (err %v), want it still a book", v, err)
	}
}

// shareFileWith gives other an edge on pid's primary file too, the state a file two
// items share.
func shareFileWith(t *testing.T, ctx context.Context, db string, pid, other model.PID) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `INSERT INTO item_file(item_id, file_id, role, position)
		SELECT (SELECT id FROM playable_item WHERE pid = ?), file_id, 'alternate', 0
		FROM item_file WHERE role = 'primary' AND item_id = (SELECT id FROM playable_item WHERE pid = ?)`,
		string(other), string(pid)); err != nil {
		t.Fatalf("share file: %v", err)
	}
}

// TestSetItemKindInAReadOnlyLibrary: a read-only library's item converts in the catalog,
// and the write-back it cannot make is reported and recorded as drift, the file left as
// it was.
func TestSetItemKindInAReadOnlyLibrary(t *testing.T) {
	ctx := context.Background()
	lib, pid, path, _ := librivoxTrack(t, ctx)
	libs, err := lib.Libraries(ctx)
	if err != nil || len(libs) != 1 {
		t.Fatalf("libraries = %d (err %v)", len(libs), err)
	}
	if _, err := lib.SetLibraryReadOnly(ctx, libs[0].PID, true); err != nil {
		t.Fatalf("read-only: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := lib.SetItemKind(ctx, []model.PID{pid}, model.KindBook, waxbin.KindOptions{WriteBack: true})
	if err != nil {
		var wb *waxbin.WriteBackError
		if errors.As(err, &wb) {
			t.Fatalf("set kind returned the write-back as an error (%v); the report carries it", err)
		}
		t.Fatalf("set kind: %v", err)
	}
	if v, _ := lib.Get(ctx, pid); v.Kind != model.KindBook {
		t.Errorf("kind = %s, want the catalog converted", v.Kind)
	}
	if len(rep.WriteBackFailures) != 1 || rep.WriteBackFailures[0].Path != path {
		t.Errorf("write-back failures = %+v, want the read-only file", rep.WriteBackFailures)
	}
	ds, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{ItemPID: pid, Code: model.DiagTagWriteUnsynced})
	if err != nil || len(ds) != 1 {
		t.Errorf("drift = %+v (err %v), want the refused write recorded", ds, err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the read-only file was rewritten")
	}
}

// TestSetItemKindLocksTheKindTheRuleGives: an item turned back into the kind its library
// gives it is still pinned, so a root declared otherwise later keeps it.
func TestSetItemKindLocksTheKindTheRuleGives(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	librivoxPart(t, filepath.Join(root, "Jane Austen", "Persuasion", "01.mp3"), 1)
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	pid := itemPIDByTitle(t, ctx, lib, "Persuasion")
	for _, kind := range []model.Kind{model.KindTrack, model.KindBook} {
		if _, err := lib.SetItemKind(ctx, []model.PID{pid}, kind, waxbin.KindOptions{Force: true}); err != nil {
			t.Fatalf("set kind %s: %v", kind, err)
		}
		if v, _ := lib.Get(ctx, pid); v.Kind != kind {
			t.Fatalf("kind = %s, want %s", v.Kind, kind)
		}
		if row := kindRow(t, ctx, lib, pid); row == nil || !row.Locked || row.Source != model.SourceUser {
			t.Errorf("as a %s: kind row = %+v, want a user lock", kind, row)
		}
	}
}

// persuasionChapters opens a mixed root holding three LibriVox-style chapters of one book,
// the third with a byte copy in another folder, scans them as tracks and returns the
// library, the tracks in chapter order and the catalog's path.
func persuasionChapters(t *testing.T, ctx context.Context) (*waxbin.Library, []model.PID, string) {
	t.Helper()
	root, db := t.TempDir(), filepath.Join(t.TempDir(), "catalog.db")
	for n := 1; n <= 3; n++ {
		librivoxPart(t, filepath.Join(root, "Jane Austen", "Persuasion", fmt.Sprintf("%02d.mp3", n)), n)
	}
	librivoxPart(t, filepath.Join(root, "Backup", "03.mp3"), 3)
	lib := openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)
	var pids []model.PID
	for n := 1; n <= 3; n++ {
		pids = append(pids, itemPIDByTitle(t, ctx, lib, fmt.Sprintf("Chapter %02d", n)))
	}
	if files, err := lib.ItemFiles(ctx, pids[2]); err != nil || len(files) != 2 {
		t.Fatalf("chapter 3 files = %d (err %v), want it and its copy", len(files), err)
	}
	return lib, pids, db
}

// partStarts returns where each part of a book starts on its timeline.
func partStarts(t *testing.T, ctx context.Context, lib *waxbin.Library, pid model.PID) []int64 {
	t.Helper()
	book, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	var out []int64
	var at int64
	for _, f := range book.Files {
		out = append(out, at)
		at += f.DurationMS
	}
	return out
}

// TestSetItemKindMergesTracksIntoOneBook: three tracks of one book converted together are
// one book of three parts under the first track's pid. The other two are absorbed, their
// plays, stars, playlist entries and sessions folding into the book, and their pids are
// gone; the copy of the third chapter follows it into the book.
func TestSetItemKindMergesTracksIntoOneBook(t *testing.T) {
	ctx := context.Background()
	lib, pids, db := persuasionChapters(t, ctx)
	t1, t2, t3 := pids[0], pids[1], pids[2]
	pb := lib.Playback()
	for _, starred := range []bool{true, false} {
		if _, err := pb.SetStar(ctx, "", t1, starred, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pb.SetStar(ctx, "", t2, true, nil); err != nil {
		t.Fatal(err)
	}
	for pid, r := range map[model.PID]int{t1: 60, t2: 80} {
		if _, err := pb.SetRating(ctx, "", pid, &r, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, pid := range []model.PID{t2, t2, t3} {
		if err := pb.MarkPlayed(ctx, "", pid, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := pb.Checkpoint(ctx, "", t3, 100, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.RecordSession(ctx, "", t3, "test", 1, 2, 1); err != nil {
		t.Fatal(err)
	}
	pl, err := lib.Playlists().CreateStatic(ctx, "Listen", "", model.VisibilityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.Playlists().Add(ctx, pl, t2, t3); err != nil {
		t.Fatal(err)
	}
	seq, err := lib.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := lib.SetItemKind(ctx, []model.PID{t1, t2, t3}, model.KindBook, waxbin.KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	// A feed reader sees the absorbed items deleted before the book's last update, so
	// reloading the book on that update finds every part.
	changes, err := lib.Changes(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	lastUpdate, lastDelete := -1, -1
	for i, c := range changes {
		switch {
		case c.EntityType == "item" && c.EntityPID == t1 && c.Op == model.OpUpdate:
			lastUpdate = i
		case c.EntityType == "item" && (c.EntityPID == t2 || c.EntityPID == t3) && c.Op == model.OpDelete:
			lastDelete = i
		}
	}
	if lastDelete < 0 || lastUpdate < lastDelete {
		t.Errorf("feed = %+v, want the book's update after both deletes", changes)
	}
	if !slices.Equal(rep.Converted, []model.PID{t1}) || len(rep.Created) != 0 ||
		len(rep.Absorbed) != 2 || rep.Absorbed[t2] != t1 || rep.Absorbed[t3] != t1 {
		t.Errorf("report = %+v, want the first converted and the other two absorbed into it", rep)
	}
	for _, gone := range []model.PID{t2, t3} {
		if _, err := lib.Get(ctx, gone); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Errorf("absorbed %s: err %v, want NotFound", gone, err)
		}
	}
	book, err := lib.Book(ctx, t1)
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if book.Item.Kind != model.KindBook || len(book.Files) != 3 {
		t.Fatalf("book = %s with %d parts, want a book of three parts", book.Item.Kind, len(book.Files))
	}
	files, err := lib.ItemFiles(ctx, t1)
	if err != nil || len(files) != 4 || files[3].Role != "alternate" || files[3].Position != book.Files[2].Position {
		t.Errorf("files = %+v (err %v), want the copy an alternate of part 3", files, err)
	}
	st, err := pb.State(ctx, "", t1)
	if err != nil {
		t.Fatal(err)
	}
	starts := partStarts(t, ctx, lib, t1)
	if !st.Starred || st.Rating != 60 || !st.Played || st.PlayCount != 3 || st.PositionMS != starts[2]+100 {
		t.Errorf("book state = %+v, want starred, rated 60, played 3 times, at %d inside part 3", st, starts[2]+100)
	}
	items, err := lib.Playlists().Items(ctx, pl, "")
	if err != nil || len(items) != 1 || items[0].PID != t1 {
		t.Errorf("playlist = %d entries (err %v), want the book once", len(items), err)
	}
	if n := catalogScalar[int](t, ctx, db, `SELECT COUNT(*) FROM play_session
		WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)`, string(t1)); n != 1 {
		t.Errorf("sessions on the book = %d, want the third chapter's", n)
	}
	if row := kindRow(t, ctx, lib, t1); row == nil || !row.Locked {
		t.Errorf("kind row = %+v, want the book locked", row)
	}
}

// TestSetItemKindMergeKeepsTheFirstNamedItem: the item named first keeps its pid even when
// its chapter is not the book's first, and its place moves on by the parts that join
// ahead of it, so it still points at the same audio.
func TestSetItemKindMergeKeepsTheFirstNamedItem(t *testing.T) {
	ctx := context.Background()
	lib, pids, _ := persuasionChapters(t, ctx)
	t1, t2, t3 := pids[0], pids[1], pids[2]
	if err := lib.Playback().Checkpoint(ctx, "", t3, 100, nil); err != nil {
		t.Fatal(err)
	}
	rep, err := lib.SetItemKind(ctx, []model.PID{t3, t1, t2}, model.KindBook, waxbin.KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if !slices.Equal(rep.Converted, []model.PID{t3}) || rep.Absorbed[t1] != t3 || rep.Absorbed[t2] != t3 {
		t.Fatalf("report = %+v, want the third chapter's item kept", rep)
	}
	starts := partStarts(t, ctx, lib, t3)
	if st, err := lib.Playback().State(ctx, "", t3); err != nil || st.PositionMS != starts[2]+100 {
		t.Errorf("book state = %+v (err %v), want %d inside part 3", st, err, starts[2]+100)
	}
}

// TestSetItemKindSplitsABookIntoTracks: a book of three parts turned into tracks keeps its
// pid on its primary part and mints a locked track for each other part; a place inside a
// part, a listener's resume position or a bookmark, lands on that part's track as an
// offset into it.
func TestSetItemKindSplitsABookIntoTracks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for n := 1; n <= 3; n++ {
		librivoxPart(t, filepath.Join(root, "Jane Austen", "Persuasion", fmt.Sprintf("%02d.mp3", n)), n)
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	pid := itemPIDByTitle(t, ctx, lib, "Persuasion")
	starts := partStarts(t, ctx, lib, pid)
	if len(starts) != 3 {
		t.Fatalf("parts = %d, want 3", len(starts))
	}
	pb := lib.Playback()
	bob, err := lib.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := pb.Checkpoint(ctx, "", pid, starts[1]+100, nil); err != nil {
		t.Fatal(err)
	}
	if err := pb.Checkpoint(ctx, bob.PID, pid, starts[2]+50, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.AddBookmark(ctx, "", pid, starts[1]+200, "mark"); err != nil {
		t.Fatal(err)
	}

	rep, err := lib.SetItemKind(ctx, []model.PID{pid}, model.KindTrack, waxbin.KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if !slices.Equal(rep.Converted, []model.PID{pid}) || len(rep.Created) != 2 || len(rep.Absorbed) != 0 {
		t.Fatalf("report = %+v, want the book converted and two tracks created", rep)
	}
	byChapter := map[string]model.PID{}
	for _, p := range append([]model.PID{pid}, rep.Created...) {
		v, err := lib.Get(ctx, p)
		if err != nil || v.Kind != model.KindTrack {
			t.Fatalf("%s = %+v (err %v), want a track", p, v, err)
		}
		byChapter[v.Title] = p
		if row := kindRow(t, ctx, lib, p); row == nil || !row.Locked {
			t.Errorf("%s kind row = %+v, want it locked", v.Title, row)
		}
	}
	if byChapter["Chapter 01"] != pid {
		t.Errorf("chapters = %v, want the book's pid on chapter 1", byChapter)
	}
	two, three := byChapter["Chapter 02"], byChapter["Chapter 03"]
	for _, c := range []struct {
		user, item model.PID
		want       int64
		set        bool
	}{{"", two, 100, true}, {bob.PID, three, 50, true}, {"", pid, 0, false}, {bob.PID, pid, 0, false}} {
		st, err := pb.State(ctx, c.user, c.item)
		if err != nil {
			t.Fatal(err)
		}
		if st.PositionMS != c.want || (st.LastProgressAt != 0) != c.set {
			t.Errorf("user %q on %s = %d (progress %d), want %d set %v", c.user, c.item, st.PositionMS, st.LastProgressAt, c.want, c.set)
		}
	}
	if marks, err := pb.Bookmarks(ctx, "", two); err != nil || len(marks) != 1 || marks[0].PositionMS != 200 {
		t.Errorf("chapter 2 marks = %+v (err %v), want the mark at 200", marks, err)
	}
}

// TestScanSplitMovesTheBookPosition: a book split by a scan, its root no longer declared
// audiobook, leaves its resume position on the audio it pointed at, as an offset into the
// part that holds it, not past the end of the track that keeps the pid.
func TestScanSplitMovesTheBookPosition(t *testing.T) {
	ctx := context.Background()
	root, db := t.TempDir(), filepath.Join(t.TempDir(), "catalog.db")
	for n := 1; n <= 2; n++ {
		librivoxPart(t, filepath.Join(root, "Jane Austen", "Persuasion", fmt.Sprintf("%02d.mp3", n)), n)
	}
	open := func(media model.MediaType) *waxbin.Library {
		lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db,
			Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Media: media, Profile: "waxbin-native"}}})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return lib
	}
	lib := open(model.MediaAudiobook)
	scanLib(t, ctx, lib)
	pid := itemPIDByTitle(t, ctx, lib, "Persuasion")
	starts := partStarts(t, ctx, lib, pid)
	if err := lib.Playback().Checkpoint(ctx, "", pid, starts[1]+100, nil); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	lib = open(model.MediaMusic)
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	two := itemPIDByTitle(t, ctx, lib, "Chapter 02")
	if st, err := lib.Playback().State(ctx, "", two); err != nil || st.PositionMS != 100 {
		t.Errorf("chapter 2 = %+v (err %v), want the place 100 into it", st, err)
	}
	if one := itemPIDByTitle(t, ctx, lib, "Chapter 01"); one != pid && two != pid {
		t.Errorf("neither chapter kept the book's pid %s", pid)
	}
}

// TestSetItemKindReportsAPartialChange: a file that fails partway leaves what already
// changed reported, in the call's answer and in the failed job's result, so a host can
// still map the pids it lost.
func TestSetItemKindReportsAPartialChange(t *testing.T) {
	ctx := context.Background()
	root, db := t.TempDir(), filepath.Join(t.TempDir(), "catalog.db")
	for n := 1; n <= 3; n++ {
		librivoxPart(t, filepath.Join(root, "Jane Austen", "Persuasion", fmt.Sprintf("%02d.mp3", n)), n)
	}
	lib := openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)
	var pids []model.PID
	for n := 1; n <= 3; n++ {
		pids = append(pids, itemPIDByTitle(t, ctx, lib, fmt.Sprintf("Chapter %02d", n)))
	}
	// Chapter 3's file turns into a folder: it is there, but it cannot be read.
	three := filepath.Join(root, "Jane Austen", "Persuasion", "03.mp3")
	if err := os.Remove(three); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(three, 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := lib.SetItemKind(ctx, pids, model.KindBook, waxbin.KindOptions{})
	if err == nil {
		t.Fatal("set kind succeeded, want the unreadable chapter to fail it")
	}
	check := func(label string, rep *waxbin.KindReport) {
		t.Helper()
		if rep == nil || !slices.Equal(rep.Converted, []model.PID{pids[0]}) || rep.Absorbed[pids[1]] != pids[0] {
			t.Errorf("%s report = %+v, want chapter 1 converted and chapter 2 absorbed into it", label, rep)
		}
	}
	check("returned", rep)
	jobs, err := lib.Jobs(ctx, 5)
	if err != nil || len(jobs) == 0 || jobs[0].Kind != "set-kind" || jobs[0].State != model.JobFailed {
		t.Fatalf("jobs = %+v (err %v), want the failed set-kind job first", jobs, err)
	}
	var recorded waxbin.KindReport
	if err := json.Unmarshal([]byte(jobs[0].Result), &recorded); err != nil {
		t.Fatalf("job result %q: %v", jobs[0].Result, err)
	}
	check("recorded", &recorded)
	if row := kindRow(t, ctx, lib, pids[0]); row == nil || !row.Locked {
		t.Errorf("kind row = %+v, want the converted book pinned", row)
	}
}

// TestSetItemKindChecksTheSheetOnDisk: a cue sheet written beside a book's file after its
// last scan still refuses the change to a track, since the read would carve the file into
// cue tracks; a sheet the catalog remembers but the disk no longer has refuses nothing.
func TestSetItemKindChecksTheSheetOnDisk(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	audio := filepath.Join(root, "Austen", "Emma", "emma.mp3")
	writeFile(t, audio, testaudio.BuildMP3WithAudio("Emma", "Jane Austen", "Emma", 1, testaudio.AudioWithSeed(80)))
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	book := itemPIDByTitle(t, ctx, lib, "Emma")
	if err := lib.Playback().Checkpoint(ctx, "", book, 300, nil); err != nil {
		t.Fatal(err)
	}
	cue := filepath.Join(root, "Austen", "Emma", "emma.cue")
	writeFile(t, cue, []byte("FILE \"emma.mp3\" MP3\n"+
		"  TRACK 01 AUDIO\n    TITLE \"Volume One\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Volume Two\"\n    INDEX 01 00:00:20\n"))
	if _, err := lib.SetItemKind(ctx, []model.PID{book}, model.KindTrack, waxbin.KindOptions{Force: true}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("set kind beside a new sheet = %v, want CodeInvalid", err)
	}
	if st, err := lib.Playback().State(ctx, "", book); err != nil || st.PositionMS != 300 {
		t.Fatalf("book state = %+v (err %v), want it kept", st, err)
	}

	// Scanned, the catalog holds the sheet's chapters; with the sheet gone the change goes
	// ahead and the file is a plain track.
	scanLib(t, ctx, lib)
	if chapters, err := lib.Chapters(ctx, book); err != nil || len(chapters) != 2 {
		t.Fatalf("chapters = %d (err %v), want the sheet's two", len(chapters), err)
	}
	if err := os.Remove(cue); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.SetItemKind(ctx, []model.PID{book}, model.KindTrack, waxbin.KindOptions{Force: true}); err != nil {
		t.Fatalf("set kind with the sheet gone: %v", err)
	}
	if v, err := lib.Get(ctx, book); err != nil || v.Kind != model.KindTrack {
		t.Errorf("item = %+v (err %v), want the book a track under its pid", v, err)
	}
}

// TestSetItemKindReportsWhereTheBookFolded: a book whose primary part's audio an existing
// track already holds folds into that track when split, and the report names that track
// rather than the new track of the book's first part.
func TestSetItemKindReportsWhereTheBookFolded(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	narrated := func(title string, track int, seed byte) []byte {
		return testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: title, Artist: "Author", AlbumArtist: "Author",
			Album: "Tome", Track: track, Audio: testaudio.AudioWithSeed(seed),
			TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}})
	}
	// The walk reaches part 2 first, so it is the book's primary.
	writeFile(t, filepath.Join(root, "Author", "Tome", "a-part2.mp3"), narrated("Part Two", 2, 91))
	writeFile(t, filepath.Join(root, "Author", "Tome", "b-part1.mp3"), narrated("Part One", 1, 92))
	writeFile(t, filepath.Join(root, "Loose", "two.mp3"), testaudio.BuildMP3WithAudio("Loose Two", "Someone", "Singles", 1,
		testaudio.AudioWithSeed(91)))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	book, loose := itemPIDByTitle(t, ctx, lib, "Tome"), itemPIDByTitle(t, ctx, lib, "Loose Two")

	rep, err := lib.SetItemKind(ctx, []model.PID{book}, model.KindTrack, waxbin.KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if rep.Absorbed[book] != loose || len(rep.Converted) != 0 || len(rep.Created) != 1 {
		t.Errorf("report = %+v, want the book absorbed into the loose track and part 1's track created", rep)
	}
}

// TestSetItemKindSplitPlacesFollowThePart: splitting a book whose part has another encoding
// with no recording id in common leaves the encoding a track of its own (a track's
// encodings are linked by recording id), and a place inside the part lands on the part's
// own track.
func TestSetItemKindSplitPlacesFollowThePart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const rate = 22050
	one := testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 11)
	two := testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 12)
	files := []struct {
		rel, format string
		samples     []float32
		track       string
	}{
		{"Dune/01.flac", "flac", one, "1"},
		{"Dune/02.flac", "flac", two, "2"},
		{"Dune MP3/02.mp3", "mp3", two, "2"},
	}
	for _, f := range files {
		path := filepath.Join(root, f.rel)
		writeFile(t, path, testaudio.EncodeAs(t, f.format, "", rate, f.samples))
		if _, err := meta.NewWriter().Apply(ctx, path, []meta.TagEdit{
			{Key: "TITLE", Values: []string{"Part " + f.track}}, {Key: "ALBUM", Values: []string{"Dune"}},
			{Key: "ARTIST", Values: []string{"Frank Herbert"}}, {Key: "TRACKNUMBER", Values: []string{f.track}},
		}); err != nil {
			t.Fatalf("tag %s: %v", f.rel, err)
		}
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	book := itemPIDByTitle(t, ctx, lib, "Dune")
	refs, err := lib.ItemFiles(ctx, book)
	if err != nil || len(refs) != 3 || refs[2].Role != "alternate" {
		t.Fatalf("book files = %+v (err %v), want two parts and the MP3 an alternate", refs, err)
	}
	starts := partStarts(t, ctx, lib, book)
	if err := lib.Playback().Checkpoint(ctx, "", book, starts[1]+300, nil); err != nil {
		t.Fatal(err)
	}

	rep, err := lib.SetItemKind(ctx, []model.PID{book}, model.KindTrack, waxbin.KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if len(rep.Created) != 2 {
		t.Fatalf("report = %+v, want a track for part 2 and one for its MP3", rep)
	}
	for _, p := range rep.Created {
		files, err := lib.ItemFiles(ctx, p)
		if err != nil || len(files) != 1 {
			t.Fatalf("%s files = %+v (err %v)", p, files, err)
		}
		st, err := lib.Playback().State(ctx, "", p)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if filepath.Ext(files[0].DisplayPath) == ".flac" {
			want = 300
		}
		if st.PositionMS != want {
			t.Errorf("%s = %d, want %d", files[0].DisplayPath, st.PositionMS, want)
		}
	}
}

// TestSetItemKindWritesOnlyTheNamedItems: a track merged into a book it did not name gets
// the kind written into its own file, and the book's other parts are left as they are.
func TestSetItemKindWritesOnlyTheNamedItems(t *testing.T) {
	ctx := context.Background()
	lib, pids, _ := persuasionChapters(t, ctx)
	if _, err := lib.SetItemKind(ctx, pids[:2], model.KindBook, waxbin.KindOptions{}); err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if _, err := lib.SetItemKind(ctx, pids[2:], model.KindBook, waxbin.KindOptions{WriteBack: true}); err != nil {
		t.Fatalf("set kind with write-back: %v", err)
	}
	files, err := lib.ItemFiles(ctx, pids[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		fm, err := meta.NewReader().Read(ctx, f.DisplayPath)
		if err != nil {
			t.Fatal(err)
		}
		want := model.NoBookSignal
		if filepath.Base(f.DisplayPath) == "03.mp3" {
			want = model.BookTagSignal
		}
		if fm.Tags.BookSignal != want {
			t.Errorf("%s book signal = %v, want %v", f.DisplayPath, fm.Tags.BookSignal, want)
		}
	}
}

// TestSetItemKindCarriesAnOfflineCopy: a copy the change cannot read, its file not on disk,
// follows the file it copies: merged, its track is absorbed and the copy is the book's
// alternate of that part; split, it goes to that part's own track.
func TestSetItemKindCarriesAnOfflineCopy(t *testing.T) {
	ctx := context.Background()
	root, db := t.TempDir(), filepath.Join(t.TempDir(), "catalog.db")
	for n := 1; n <= 3; n++ {
		librivoxPart(t, filepath.Join(root, "Jane Austen", "Persuasion", fmt.Sprintf("%02d.mp3", n)), n)
	}
	copyPath := filepath.Join(root, "Zbackup", "02.mp3")
	librivoxPart(t, copyPath, 2)
	lib := openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)
	var pids []model.PID
	for n := 1; n <= 3; n++ {
		pids = append(pids, itemPIDByTitle(t, ctx, lib, fmt.Sprintf("Chapter %02d", n)))
	}
	if err := os.Remove(copyPath); err != nil {
		t.Fatal(err)
	}
	copyRole := func(item model.PID) (string, int) {
		t.Helper()
		files, err := lib.ItemFiles(ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if f.DisplayPath == copyPath {
				return f.Role, f.Position
			}
		}
		return "", -1
	}

	rep, err := lib.SetItemKind(ctx, pids, model.KindBook, waxbin.KindOptions{})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if rep.Absorbed[pids[1]] != pids[0] {
		t.Errorf("report = %+v, want chapter 2 absorbed despite its offline copy", rep)
	}
	starts, _ := lib.Book(ctx, pids[0])
	if role, pos := copyRole(pids[0]); role != "alternate" || pos != starts.Files[1].Position {
		t.Errorf("copy on the book = %s at %d, want part 2's alternate", role, pos)
	}

	if _, err := lib.SetItemKind(ctx, []model.PID{pids[0]}, model.KindTrack, waxbin.KindOptions{Force: true}); err != nil {
		t.Fatalf("split: %v", err)
	}
	two := itemPIDByTitle(t, ctx, lib, "Chapter 02")
	if role, _ := copyRole(two); role != "alternate" {
		t.Errorf("copy on chapter 2's track = %q, want its alternate", role)
	}
	if role, _ := copyRole(pids[0]); role != "" {
		t.Errorf("copy still on chapter 1's track as %q", role)
	}
}

// TestSetItemKindKeepsThePidWhenThePrimarysEncodingSharesItsRecording: splitting a book
// whose primary part has another encoding tagged with the same recording id keeps the
// book's pid on that part's track, the encoding joining it, rather than folding the book
// into a track the encoding made first.
func TestSetItemKindKeepsThePidWhenThePrimarysEncodingSharesItsRecording(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const rate = 22050
	one := testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 21)
	two := testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 22)
	const recOne, recTwo = "4e2b1b2a-0000-4000-8000-0000000000c1", "4e2b1b2a-0000-4000-8000-0000000000c2"
	for _, f := range []struct {
		rel, format, track, mbid string
		samples                  []float32
	}{
		{"Dune/01.flac", "flac", "1", recOne, one},
		{"Dune/02.flac", "flac", "2", recTwo, two},
		{"Dune MP3/01.mp3", "mp3", "1", recOne, one},
	} {
		path := filepath.Join(root, f.rel)
		writeFile(t, path, testaudio.EncodeAs(t, f.format, "", rate, f.samples))
		if _, err := meta.NewWriter().Apply(ctx, path, []meta.TagEdit{
			{Key: "TITLE", Values: []string{"Part " + f.track}}, {Key: "ALBUM", Values: []string{"Dune"}},
			{Key: "ARTIST", Values: []string{"Frank Herbert"}}, {Key: "TRACKNUMBER", Values: []string{f.track}},
			{Key: "MUSICBRAINZ_TRACKID", Values: []string{f.mbid}},
		}); err != nil {
			t.Fatalf("tag %s: %v", f.rel, err)
		}
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	book := itemPIDByTitle(t, ctx, lib, "Dune")
	if refs, err := lib.ItemFiles(ctx, book); err != nil || len(refs) != 3 || refs[0].Role != "primary" ||
		filepath.Base(refs[0].DisplayPath) != "01.flac" || refs[2].Role != "alternate" {
		t.Fatalf("book files = %+v (err %v), want 01.flac the primary and the MP3 its alternate", refs, err)
	}
	rep, err := lib.SetItemKind(ctx, []model.PID{book}, model.KindTrack, waxbin.KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if !slices.Equal(rep.Converted, []model.PID{book}) || len(rep.Absorbed) != 0 {
		t.Errorf("report = %+v, want the book converted under its pid", rep)
	}
	files, err := lib.ItemFiles(ctx, book)
	if err != nil || len(files) != 2 {
		t.Fatalf("track files = %+v (err %v), want part 1 and its MP3", files, err)
	}
}

// TestSetItemKindPassesOverItemsAlreadyOfTheKind: an item already of the kind asked for is
// not read, so a gone part of it refuses nothing, and the other items change.
func TestSetItemKindPassesOverItemsAlreadyOfTheKind(t *testing.T) {
	ctx := context.Background()
	lib, track, _, _ := librivoxTrack(t, ctx)
	books := t.TempDir()
	part := filepath.Join(books, "Austen", "Emma", "01.mp3")
	writeFile(t, part, testaudio.BuildMP3WithAudio("Emma One", "Jane Austen", "Emma", 1, testaudio.AudioWithSeed(90)))
	writeFile(t, filepath.Join(books, "Austen", "Emma", "02.mp3"), testaudio.BuildMP3WithAudio("Emma Two", "Jane Austen", "Emma", 2, testaudio.AudioWithSeed(91)))
	if _, err := lib.AddRoot(ctx, config.Root{Path: books, Mode: model.ModeManaged, Media: model.MediaAudiobook}); err != nil {
		t.Fatalf("add root: %v", err)
	}
	scanLib(t, ctx, lib)
	emma := itemPIDByTitle(t, ctx, lib, "Emma")
	if err := os.Remove(part); err != nil {
		t.Fatal(err)
	}
	rep, err := lib.SetItemKind(ctx, []model.PID{emma, track}, model.KindBook, waxbin.KindOptions{})
	if err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if !slices.Equal(rep.Converted, []model.PID{track}) {
		t.Errorf("report = %+v, want the track converted", rep)
	}
}

// TestSetItemKindSplitsBesideAOneTrackSheet: a cue sheet that a read would not carve into
// cue tracks (a single track) refuses nothing, and the book becomes a plain track.
func TestSetItemKindSplitsBesideAOneTrackSheet(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Austen", "Emma", "emma.mp3"), testaudio.BuildMP3WithAudio("Emma", "Jane Austen", "Emma", 1, testaudio.AudioWithSeed(85)))
	writeFile(t, filepath.Join(root, "Austen", "Emma", "emma.cue"), []byte("FILE \"emma.mp3\" MP3\n"+
		"  TRACK 01 AUDIO\n    TITLE \"Emma\"\n    INDEX 01 00:00:00\n"))
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	book := itemPIDByTitle(t, ctx, lib, "Emma")
	if _, err := lib.SetItemKind(ctx, []model.PID{book}, model.KindTrack, waxbin.KindOptions{Force: true}); err != nil {
		t.Fatalf("set kind beside a one-track sheet: %v", err)
	}
	if v, err := lib.Get(ctx, book); err != nil || v.Kind != model.KindTrack {
		t.Errorf("item = %+v (err %v), want the book a track under its pid", v, err)
	}
}
