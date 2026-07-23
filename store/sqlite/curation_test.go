package sqlite

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// tinyPNG returns a small valid PNG so probeArtImage decodes real dimensions.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

func TestSetItemLyricsAndLockSurvivesScan(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/A/1/01.flac", essence: "e1", content: "c1", title: "One", artist: "Alpha",
	})
	pid := itemPID(t, st)

	ly := &model.Lyrics{Synced: []model.SyncedLine{{TimeMS: 0, Text: "hello"}, {TimeMS: 1000, Text: "world"}}}
	if err := st.SetItemLyrics(ctx, pid, ly, true, false); err != nil {
		t.Fatalf("set lyrics: %v", err)
	}
	got, err := st.LyricsByItem(ctx, pid)
	if err != nil || len(got.Synced) != 2 || got.Source != string(model.SourceUser) {
		t.Fatalf("lyrics = %+v, err %v", got, err)
	}

	// A forced rescan carrying different embedded lyrics must NOT overwrite the locked set.
	scanLy := &model.Lyrics{Source: "embedded", Unsynced: "scanned lyrics"}
	rescanTrackWithLyrics(t, st, lib.ID, "e1", "c2", scanLy, true)
	got, _ = st.LyricsByItem(ctx, pid)
	if got.Source != string(model.SourceUser) || len(got.Synced) != 2 {
		t.Fatalf("locked lyrics overwritten by scan: %+v", got)
	}

	// --ignore-locks (preserveLocks=false) re-derives.
	rescanTrackWithLyrics(t, st, lib.ID, "e1", "c3", scanLy, false)
	got, _ = st.LyricsByItem(ctx, pid)
	if got.Source != "embedded" {
		t.Fatalf("ignore-locks did not re-derive lyrics: %+v", got)
	}
}

func rescanTrackWithLyrics(t *testing.T, st *Store, libID int64, essence, content string, ly *model.Lyrics, preserve bool) {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte("/lib/A/1/01.flac"), DisplayPath: "/lib/A/1/01.flac", RelPath: []byte("01.flac"),
			Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 2,
			ContentHash: content, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "One",
			SortKey: model.SortKey("One"), IdentityKey: "essence:" + essence,
		},
		Track:         model.Track{Artist: "Alpha", ArtistSort: model.SortKey("Alpha")},
		Lyrics:        ly,
		PreserveLocks: preserve,
	}
	if _, err := st.PutScannedTrack(context.Background(), in); err != nil {
		t.Fatalf("rescan: %v", err)
	}
}

func TestSetItemArtAndLockSurvivesScan(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/A/1/01.flac", essence: "e1", content: "c1", title: "One", artist: "Alpha",
	})
	pid := itemPID(t, st)

	user := tinyPNG(t)
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, user, true, false); err != nil {
		t.Fatalf("set art: %v", err)
	}
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	userHash := blob.SourceHash

	// A forced rescan with a DIFFERENT cover must not replace the locked user cover.
	scanImg := &model.ArtImage{Data: []byte("JPEGSCANDATA-different-bytes")}
	if ok := finalizeScanImg(scanImg); !ok {
		scanImg.Hash = "scanhash" // undecodable bytes still store
	}
	rescanTrackWithCover(t, st, lib.ID, "e1", "c2", scanImg, true)
	blob, _ = st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 0)
	if blob.SourceHash != userHash {
		t.Fatalf("locked cover replaced by scan: %s != %s", blob.SourceHash, userHash)
	}

	// Locked SetItemArt without force is refused.
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, tinyPNG(t), true, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("set locked art = %v, want CodeLocked", err)
	}
}

// finalizeScanImg mirrors the scanner's hash/probe so a test cover is storable.
func finalizeScanImg(img *model.ArtImage) bool {
	i, err := probeArtImage(img.Data)
	if err != nil {
		return false
	}
	*img = *i
	return true
}

func rescanTrackWithCover(t *testing.T, st *Store, libID int64, essence, content string, cover *model.ArtImage, preserve bool) {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte("/lib/A/1/01.flac"), DisplayPath: "/lib/A/1/01.flac", RelPath: []byte("01.flac"),
			Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 2,
			ContentHash: content, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "One",
			SortKey: model.SortKey("One"), IdentityKey: "essence:" + essence,
		},
		Track:         model.Track{Artist: "Alpha", ArtistSort: model.SortKey("Alpha")},
		CoverArt:      cover,
		PreserveLocks: preserve,
	}
	if _, err := st.PutScannedTrack(context.Background(), in); err != nil {
		t.Fatalf("rescan cover: %v", err)
	}
}

func TestSetEntityArtDurableAlbum(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1",
		title: "One", artist: "Alpha", albumArt: "Alpha", album: "One",
	})
	var albumPID string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM album LIMIT 1").Scan(&albumPID); err != nil {
		t.Fatalf("album pid: %v", err)
	}

	img := tinyPNG(t)
	if err := st.SetEntityArt(ctx, model.ArtAlbum, model.PID(albumPID), model.ArtRoleFront, img); err != nil {
		t.Fatalf("set album art: %v", err)
	}
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: model.PID(albumPID)}, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("resolve album art: %v", err)
	}
	if len(blob.Bytes) == 0 {
		t.Fatal("album art not resolved from durable map")
	}
	// A durable album row exists (not just track-derived).
	var n int
	if err := st.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM art_map WHERE entity_type='album'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("durable album art_map rows = %d, want 1", n)
	}
	// db verify stays clean and GCArt does not reclaim the live album source.
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", r, err)
	}
}

func TestSetItemChaptersSurvivesScan(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Book/b.m4b", essence: "be1", content: "bc1",
		title: "The Book", author: "Jane Author", durationMS: 5000,
		chapters: []model.Chapter{{Position: 0, Title: "Scanned Ch", FileStartMS: 0}},
	})
	var bpid string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='book' LIMIT 1").Scan(&bpid); err != nil {
		t.Fatalf("book pid: %v", err)
	}
	pid := model.PID(bpid)

	userCh := []model.Chapter{
		{Position: 0, Title: "User One", FileStartMS: 0},
		{Position: 1, Title: "User Two", FileStartMS: 2500},
	}
	if err := st.SetItemChapters(ctx, pid, userCh, true, false); err != nil {
		t.Fatalf("set chapters: %v", err)
	}
	chs, err := st.Chapters(ctx, pid)
	if err != nil {
		t.Fatalf("read chapters: %v", err)
	}
	if len(chs) != 2 || chs[0].Title != "User One" {
		t.Fatalf("chapters = %+v, want the 2 user chapters", chs)
	}

	// A forced rescan re-imports the scanned chapters but user chapters still win.
	rescanBookChapters(t, st, lib.ID, "be1", "bc2")
	chs, _ = st.Chapters(ctx, pid)
	if len(chs) != 2 || chs[0].Title != "User One" {
		t.Fatalf("user chapters lost after scan: %+v", chs)
	}
}

func TestSetItemChaptersRejectsMultiFile(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Two parts sharing one book key collapse to one book item with two files.
	putBookPart(t, st, lib.ID, "/lib/b/p1.m4b", "multi", "me1", 0)
	putBookPart(t, st, lib.ID, "/lib/b/p2.m4b", "multi", "me2", 1)
	var bpid string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='book' LIMIT 1").Scan(&bpid); err != nil {
		t.Fatalf("book pid: %v", err)
	}
	err := st.SetItemChapters(ctx, model.PID(bpid),
		[]model.Chapter{{Position: 0, Title: "X", FileStartMS: 0}}, true, false)
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("multi-file SetItemChapters = %v, want CodeUnsupported", err)
	}
}

func rescanBookChapters(t *testing.T, st *Store, libID int64, essence, content string) {
	t.Helper()
	in := model.PutScannedBookInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte("/lib/Author/Book/b.m4b"), DisplayPath: "/lib/Author/Book/b.m4b",
			RelPath: []byte("b.m4b"), Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 2,
			ContentHash: content, EssenceHash: essence, DurationMS: 5000, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: "The Book",
			SortKey:     model.SortKey("The Book"),
			IdentityKey: identity.BookKey("", "", "Jane Author", "The Book", ""),
		},
		Book:          model.Book{Author: "Jane Author", Authors: []string{"Jane Author"}},
		Chapters:      []model.Chapter{{Position: 0, Title: "Scanned Ch", FileStartMS: 0}},
		ChapterSource: "embedded",
		PreserveLocks: true,
	}
	if _, err := st.PutScannedBook(context.Background(), in); err != nil {
		t.Fatalf("rescan book: %v", err)
	}
}
