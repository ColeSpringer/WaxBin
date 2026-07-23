package sqlite

import (
	"bytes"
	"context"
	"database/sql"
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

// threePartBook builds one book from three parts of 1000/2000/3000 ms (total
// 6000), each carrying one scanned chapter, and returns its pid.
func threePartBook(t *testing.T, st *Store, libID int64) model.PID {
	t.Helper()
	var pid model.PID
	for i, dur := range []int64{1000, 2000, 3000} {
		n := string(rune('1' + i))
		res := putBook(t, st, libID, bookSpec{
			path: "/lib/mb/p" + n + ".m4b", essence: "mbe" + n, content: "mbc" + n,
			title: "Multi", author: "Auth", asin: "BMULTI", position: i + 1, durationMS: dur,
			chapters: []model.Chapter{{Position: 0, Title: "Scanned " + n}},
		})
		pid = res.ItemPID
	}
	return pid
}

func startTitles(chs []model.Chapter) [][2]any {
	out := make([][2]any, len(chs))
	for i, c := range chs {
		out[i] = [2]any{c.StartMS, c.Title}
	}
	return out
}

func TestSetItemChaptersMultiFileRoundTrip(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := threePartBook(t, st, lib.ID)

	// A flat book-timeline list: one chapter per part plus one mid-part start.
	in := []model.Chapter{
		{Title: "One", StartMS: 0},
		{Title: "Two", StartMS: 1500},   // inside part 2
		{Title: "Three", StartMS: 3000}, // exactly the part-3 boundary
		{Title: "Four", StartMS: 4500},
	}
	if err := st.SetItemChapters(ctx, pid, in, true, false); err != nil {
		t.Fatalf("set chapters: %v", err)
	}
	got, err := st.Chapters(ctx, pid)
	if err != nil {
		t.Fatalf("read chapters: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("chapters = %+v, want the 4 user chapters (scanned ones suppressed)", got)
	}
	for i := range in {
		if got[i].StartMS != in[i].StartMS || got[i].Title != in[i].Title {
			t.Fatalf("round-trip mismatch at %d: got %v, want %v", i, startTitles(got), startTitles(in))
		}
	}
	// Boundary start lands in the part it opens: chapter Three is backed by part 3.
	var p3pid model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM file WHERE path=?", []byte("/lib/mb/p3.m4b")).Scan(&p3pid); err != nil {
		t.Fatalf("p3 pid: %v", err)
	}
	if got[2].FilePID != p3pid {
		t.Fatalf("boundary chapter backed by %s, want part 3 (%s)", got[2].FilePID, p3pid)
	}
	// Open ends fill from the next start across parts; the last runs to the total.
	if got[1].EndMS != 3000 || got[3].EndMS != 6000 {
		t.Fatalf("filled ends = %d/%d, want 3000/6000", got[1].EndMS, got[3].EndMS)
	}
}

func TestSetItemChaptersSpanningContiguous(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := threePartBook(t, st, lib.ID)

	// Chapter one spans parts 1 and 2 with an explicit end equal to the next
	// start (contiguous): it round-trips exactly even though it crosses a
	// boundary, because contiguity is stored open and refilled on read.
	in := []model.Chapter{
		{Title: "Spanning", StartMS: 0, EndMS: 2500},
		{Title: "Rest", StartMS: 2500},
	}
	if err := st.SetItemChapters(ctx, pid, in, true, false); err != nil {
		t.Fatalf("set chapters: %v", err)
	}
	got, _ := st.Chapters(ctx, pid)
	if len(got) != 2 || got[0].StartMS != 0 || got[0].EndMS != 2500 || got[1].StartMS != 2500 {
		t.Fatalf("spanning round-trip = %v", startTitles(got))
	}
	if got[0].EndMS != 2500 {
		t.Fatalf("spanning end = %d, want 2500", got[0].EndMS)
	}
}

func TestSetItemChaptersClampsGapAcrossBoundary(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := threePartBook(t, st, lib.ID)

	// A non-contiguous explicit end that crosses the part-1/part-2 boundary
	// (ends at 1400, next starts at 2000) clamps to the starting part's end: a
	// continuation row in part 2 would render a phantom duplicate chapter.
	in := []model.Chapter{
		{Title: "Clamped", StartMS: 500, EndMS: 1400},
		{Title: "Later", StartMS: 2000},
	}
	if err := st.SetItemChapters(ctx, pid, in, true, false); err != nil {
		t.Fatalf("set chapters: %v", err)
	}
	got, _ := st.Chapters(ctx, pid)
	if len(got) != 2 {
		t.Fatalf("chapters = %v", startTitles(got))
	}
	if got[0].EndMS != 1000 {
		t.Fatalf("clamped end = %d, want 1000 (part-1 boundary)", got[0].EndMS)
	}
	// An in-part explicit end is preserved as given.
	in2 := []model.Chapter{{Title: "InPart", StartMS: 200, EndMS: 700}}
	if err := st.SetItemChapters(ctx, pid, in2, true, true); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	got2, _ := st.Chapters(ctx, pid)
	if len(got2) != 1 || got2[0].EndMS != 700 {
		t.Fatalf("in-part end = %v, want 700", got2)
	}
}

func TestSetItemChaptersRejectsBadTimelines(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := threePartBook(t, st, lib.ID)

	for name, in := range map[string][]model.Chapter{
		"non-monotonic": {{Title: "A", StartMS: 1000}, {Title: "B", StartMS: 500}},
		"duplicate":     {{Title: "A", StartMS: 1000}, {Title: "B", StartMS: 1000}},
		"negative":      {{Title: "A", StartMS: -5}},
		"end<=start":    {{Title: "A", StartMS: 1000, EndMS: 1000}},
	} {
		if err := st.SetItemChapters(ctx, pid, in, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Fatalf("%s = %v, want CodeInvalid", name, err)
		}
	}
	// Nothing was written by the rejects: the scanned chapters still read back.
	got, _ := st.Chapters(ctx, pid)
	if len(got) != 3 || got[0].Title != "Scanned 1" {
		t.Fatalf("rejects disturbed chapters: %v", startTitles(got))
	}
}

func TestSetItemChaptersShrinkClearsUncoveredParts(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := threePartBook(t, st, lib.ID)

	full := []model.Chapter{
		{Title: "One", StartMS: 0},
		{Title: "Two", StartMS: 1500},
		{Title: "Three", StartMS: 4000},
	}
	if err := st.SetItemChapters(ctx, pid, full, true, false); err != nil {
		t.Fatalf("set full: %v", err)
	}
	// The re-set covers only part 1; parts 2 and 3 lose their user rows, and the
	// book reads exactly the shrunken user list (no scanned fallback).
	if err := st.SetItemChapters(ctx, pid, []model.Chapter{{Title: "Only", StartMS: 0}}, true, true); err != nil {
		t.Fatalf("shrink: %v", err)
	}
	got, _ := st.Chapters(ctx, pid)
	if len(got) != 1 || got[0].Title != "Only" || got[0].EndMS != 6000 {
		t.Fatalf("after shrink = %v (end %d), want just Only ending at 6000", startTitles(got), got[0].EndMS)
	}
	var stray int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chapter c JOIN item_file itf ON itf.file_id = c.file_id AND itf.item_id = c.book_item_id
		 WHERE c.source='user' AND itf.position > 1`).Scan(&stray); err != nil {
		t.Fatalf("stray count: %v", err)
	}
	if stray != 0 {
		t.Fatalf("uncovered parts kept %d user rows, want 0", stray)
	}

	// A clear loops all parts and falls back to the scanned chapters.
	if err := st.SetItemChapters(ctx, pid, nil, true, true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = st.Chapters(ctx, pid)
	if len(got) != 3 || got[0].Title != "Scanned 1" {
		t.Fatalf("after clear = %v, want the 3 scanned chapters", startTitles(got))
	}
}

func TestSetItemChaptersMultiFileSurvivesScan(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := threePartBook(t, st, lib.ID)

	in := []model.Chapter{{Title: "Curated", StartMS: 0}, {Title: "Late", StartMS: 5000}}
	if err := st.SetItemChapters(ctx, pid, in, true, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	// A forced rescan of every part re-imports the scanned chapters; the user
	// timeline still wins book-wide.
	for i, dur := range []int64{1000, 2000, 3000} {
		n := string(rune('1' + i))
		putBook(t, st, lib.ID, bookSpec{
			path: "/lib/mb/p" + n + ".m4b", essence: "mbe" + n, content: "mbc" + n + "x",
			title: "Multi", author: "Auth", asin: "BMULTI", position: i + 1, durationMS: dur,
			chapters: []model.Chapter{{Position: 0, Title: "Rescanned " + n}},
		})
	}
	got, _ := st.Chapters(ctx, pid)
	if len(got) != 2 || got[0].Title != "Curated" || got[1].StartMS != 5000 {
		t.Fatalf("user chapters lost to rescan: %v", startTitles(got))
	}
}

func TestSetItemChaptersZeroDurationPartKeepsTimeline(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Part 1 has an unknown (0) file duration; only its scanned chapter extent
	// (500 ms) advances the timeline. Part 2 is 1000 ms.
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/zb/p1.m4b", essence: "zb1", content: "zbc1",
		title: "ZeroDur", author: "Auth", asin: "BZD", position: 1, durationMS: 0,
		chapters: []model.Chapter{{Position: 0, Title: "D1", FileStartMS: 0, FileEndMS: 500}},
	})
	res := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/zb/p2.m4b", essence: "zb2", content: "zbc2",
		title: "ZeroDur", author: "Auth", asin: "BZD", position: 2, durationMS: 1000,
	})
	pid := res.ItemPID

	// User curation suppresses the scanned chapters on read, but the timeline
	// they advanced must survive: a chapter placed past part 1's 500 ms extent
	// still reads back where it was authored, backed by part 2.
	in := []model.Chapter{{Title: "A", StartMS: 0}, {Title: "B", StartMS: 600}}
	if err := st.SetItemChapters(ctx, pid, in, true, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := st.Chapters(ctx, pid)
	if len(got) != 2 || got[0].StartMS != 0 || got[1].StartMS != 600 {
		t.Fatalf("zero-duration part collapsed the timeline: %v", startTitles(got))
	}
	var p2pid model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM file WHERE path=?", []byte("/lib/zb/p2.m4b")).Scan(&p2pid); err != nil {
		t.Fatalf("p2 pid: %v", err)
	}
	if got[1].FilePID != p2pid {
		t.Fatalf("chapter B backed by %s, want part 2 (%s)", got[1].FilePID, p2pid)
	}
}

func TestSetItemChaptersMapsAgainstDisplayedSource(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Part 1 (unknown duration) carries embedded chapters reaching 400 ms plus a
	// transient cue row reaching 900 ms. The read shows only the preferred
	// embedded source, so the user authored against a 400 ms part 1; the split
	// must map against that, not the 900 ms extent of the hidden source.
	r1 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/ds/p1.m4b", essence: "ds1", content: "dsc1",
		title: "TwoSrc", author: "Auth", asin: "BDS", position: 1, durationMS: 0,
		chapters: []model.Chapter{{Position: 0, Title: "Emb", FileStartMS: 0, FileEndMS: 400}},
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/ds/p2.m4b", essence: "ds2", content: "dsc2",
		title: "TwoSrc", author: "Auth", asin: "BDS", position: 2, durationMS: 1000,
		chapters: []model.Chapter{{Position: 0, Title: "P2", FileStartMS: 0}},
	})
	pid := r1.ItemPID
	var itemID, fileID int64
	if err := st.read.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE pid=?", string(pid)).Scan(&itemID); err != nil {
		t.Fatalf("item id: %v", err)
	}
	if err := st.read.QueryRowContext(ctx, "SELECT id FROM file WHERE path=?", []byte("/lib/ds/p1.m4b")).Scan(&fileID); err != nil {
		t.Fatalf("file id: %v", err)
	}
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO chapter(book_item_id, file_id, position, title, start_ms, end_ms, source) VALUES (?,?,?,?,?,?,'cue')",
			itemID, fileID, 0, "Cue", 0, 900)
		return err
	}); err != nil {
		t.Fatalf("inject cue row: %v", err)
	}

	// The displayed pre-curation timeline: part 2 opens at 400 (the embedded
	// extent), not 900.
	pre, _ := st.Chapters(ctx, pid)
	if len(pre) != 2 || pre[1].StartMS != 400 {
		t.Fatalf("displayed timeline = %v, want part 2 at 400", startTitles(pre))
	}

	in := []model.Chapter{{Title: "A", StartMS: 0}, {Title: "B", StartMS: 600, EndMS: 950}}
	if err := st.SetItemChapters(ctx, pid, in, true, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := st.Chapters(ctx, pid)
	if len(got) != 2 || got[1].StartMS != 600 || got[1].EndMS != 950 {
		t.Fatalf("split diverged from the displayed timeline: %v (end %d)", startTitles(got), got[1].EndMS)
	}
	var p2pid model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM file WHERE path=?", []byte("/lib/ds/p2.m4b")).Scan(&p2pid); err != nil {
		t.Fatalf("p2 pid: %v", err)
	}
	if got[1].FilePID != p2pid {
		t.Fatalf("chapter B backed by %s, want part 2 (%s)", got[1].FilePID, p2pid)
	}
}

func TestSetItemChaptersMultiFileIgnoresStaleFileOffsets(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := threePartBook(t, st, lib.ID)

	// A single chapter whose timeline fields are zero but whose File* offsets
	// carry stale values (a round-tripped Chapter, edited and passed back). On a
	// multi-file book the legacy sniff must not fire: the input reads as a
	// timeline chapter at 0 with an open end, and the stale offsets are ignored.
	in := []model.Chapter{{Title: "Whole", FileStartMS: 0, FileEndMS: 700}}
	if err := st.SetItemChapters(ctx, pid, in, true, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := st.Chapters(ctx, pid)
	if len(got) != 1 || got[0].StartMS != 0 {
		t.Fatalf("chapters = %v", startTitles(got))
	}
	if got[0].EndMS != 6000 {
		t.Fatalf("end = %d, want the 6000 book total (stale FileEndMS ignored)", got[0].EndMS)
	}
}

func TestSetItemChaptersBeyondTotalExtendsDuration(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := threePartBook(t, st, lib.ID)

	// A start past the 6000 ms total attaches to the last part and extends the
	// book's effective duration (the single-file precedent).
	in := []model.Chapter{{Title: "One", StartMS: 0}, {Title: "Epilogue", StartMS: 7000}}
	if err := st.SetItemChapters(ctx, pid, in, true, false); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := st.Chapters(ctx, pid)
	if len(got) != 2 || got[1].StartMS != 7000 {
		t.Fatalf("beyond-total chapter = %v", startTitles(got))
	}
	v, err := st.ItemByPID(ctx, pid)
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	if v.DurationMS < 7000 {
		t.Fatalf("book duration = %d, want extended past 7000", v.DurationMS)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Fatalf("derived data inconsistent after extension: %+v (err %v)", rep, err)
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
