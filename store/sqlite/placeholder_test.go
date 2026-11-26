package sqlite

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// thumbPut is one file of its own recording on album Al, whose tags hold cover (nil for
// none). thumb says the import that brought it marked that picture a placeholder. The
// album carries a barcode, so the album-art backfill asks about it.
func thumbPut(libID int64, name string, cover *model.ArtImage, thumb bool) model.PutScannedTrackInput {
	path := "/lib/Al/" + name + ".flac"
	return model.PutScannedTrackInput{
		LibraryID: libID, PreserveLocks: true,
		File: model.File{Path: []byte(path), DisplayPath: path, RelPath: []byte(name + ".flac"),
			Kind: model.FileAudio, ContentHash: "c" + name, EssenceHash: "e" + name, ScanState: model.ScanIndexed},
		Item:     model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: name, IdentityKey: "essence:e" + name},
		Track:    model.Track{Artist: "X", AlbumArtist: "X", Album: "Al", Year: 2000, Barcode: "0075992739429"},
		CoverArt: cover, NoCover: cover == nil, PlaceholderCover: thumb,
	}
}

func frontOf(t *testing.T, st *Store, typ model.ArtEntity, pid model.PID) *model.ArtProvenance {
	t.Helper()
	p, err := st.ArtProvenance(context.Background(), model.EntityRef{Type: typ, PID: pid}, model.ArtRoleFront)
	if err != nil {
		t.Fatalf("ArtProvenance %s %s: %v", typ, pid, err)
	}
	return p
}

// wantFront is what a front-cover read answers: the picture, the level that answered,
// and whether it is a placeholder.
type wantFront struct {
	hash        string
	level       model.ArtEntity
	placeholder bool
}

func checkFront(t *testing.T, st *Store, when string, typ model.ArtEntity, pid model.PID, want wantFront) {
	t.Helper()
	p := frontOf(t, st, typ, pid)
	if p.SourceHash != want.hash || p.Level != want.level || p.Placeholder != want.placeholder {
		t.Errorf("%s: %s front = %s at %s (placeholder %v), want %s at %s (placeholder %v)", when, typ,
			p.SourceHash, p.Level, p.Placeholder, want.hash, want.level, want.placeholder)
	}
}

// TestAPlaceholderLeavesTheAlbumsCoverToFill: a download's thumbnail is marked at the
// import. It shows while nothing better does, but the album still reads as wanting a
// cover, so the backfill's fill lands, and the track then draws the album's cover. A
// second thumbnail does not take the fetched cover away, where a real one does.
func TestAPlaceholderLeavesTheAlbumsCoverToFill(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	thumb := testPNG(t, 40, 30)
	one := mustScan(t, st, thumbPut(lib.ID, "one", thumb, true)).ItemPID
	if n := scalarInt(t, st, "SELECT placeholder FROM art_source WHERE hash = ?", thumb.Hash); n != 1 {
		t.Fatalf("the thumbnail's mark = %d, want it marked at the put", n)
	}
	album := albumPID(t, st)
	albumID := int64(scalarInt(t, st, "SELECT id FROM album WHERE pid = ?", string(album)))
	checkFront(t, st, "imported", model.ArtTrack, one, wantFront{thumb.Hash, model.ArtTrack, true})
	checkFront(t, st, "imported", model.ArtAlbum, album, wantFront{thumb.Hash, model.ArtAlbum, true})

	targets, err := st.AlbumsNeedingArt(ctx, model.EnrichQueueOptions{}, 0, 10, model.ArtSlots{Front: true}, nil)
	if err != nil || len(targets) != 1 || !targets[0].FrontDue {
		t.Fatalf("album art queue = %+v (err %v), want the album's front due", targets, err)
	}
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: albumID, PID: album,
		Front: model.ArtHalf{Asked: true, Provider: "mock"}, Art: rgEnrichArt("al-front")}); err != nil {
		t.Fatalf("album art backfill: %v", err)
	}
	checkFront(t, st, "after the fill", model.ArtTrack, one, wantFront{"al-front", model.ArtAlbum, false})

	two := mustScan(t, st, thumbPut(lib.ID, "two", testPNG(t, 41, 30), true)).ItemPID
	checkFront(t, st, "after a second thumbnail", model.ArtAlbum, album, wantFront{"al-front", model.ArtAlbum, false})
	checkFront(t, st, "after a second thumbnail", model.ArtTrack, two, wantFront{"al-front", model.ArtAlbum, false})

	real := testPNG(t, 42, 30)
	mustScan(t, st, thumbPut(lib.ID, "three", real, false))
	checkFront(t, st, "after a real cover", model.ArtAlbum, album, wantFront{real.Hash, model.ArtAlbum, false})
	assertVerifyClean(t, st)
}

// TestRealCoversOutrankAPlaceholderAtEveryRung walks the chain with a thumbnail on the
// album's first track: a sibling's real cover, then the release group's, answer ahead of
// it, and it answers ahead of the artist's picture, for its own track, a coverless
// sibling and the album alike.
func TestRealCoversOutrankAPlaceholderAtEveryRung(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	thumb, real := testPNG(t, 40, 30), testPNG(t, 41, 30)
	a := mustScan(t, st, thumbPut(lib.ID, "a", thumb, true)).ItemPID
	mustScan(t, st, thumbPut(lib.ID, "b", real, false))
	c := mustScan(t, st, thumbPut(lib.ID, "c", nil, false)).ItemPID
	album := albumPID(t, st)
	check := func(when string, own, sibling, al wantFront) {
		t.Helper()
		checkFront(t, st, when, model.ArtTrack, a, own)
		checkFront(t, st, when, model.ArtTrack, c, sibling)
		checkFront(t, st, when, model.ArtAlbum, album, al)
	}
	fromAlbum := wantFront{real.Hash, model.ArtAlbum, false}
	check("beside a real sibling", fromAlbum, fromAlbum, fromAlbum)

	mustScan(t, st, thumbPut(lib.ID, "b", nil, false))
	check("standing in", wantFront{thumb.Hash, model.ArtTrack, true},
		wantFront{thumb.Hash, model.ArtAlbum, true}, wantFront{thumb.Hash, model.ArtAlbum, true})

	rg := model.PID(scalarStr(t, st, "SELECT pid FROM release_group LIMIT 1"))
	group := testPNG(t, 43, 30)
	if _, err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rg, model.ArtRoleFront, group.Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatal(err)
	}
	fromGroup := wantFront{group.Hash, model.ArtReleaseGroup, false}
	check("under a group cover", fromGroup, fromGroup, fromGroup)

	if _, err := st.SetEntityArt(ctx, model.ArtReleaseGroup, rg, model.ArtRoleFront, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatal(err)
	}
	artist := model.PID(scalarStr(t, st, "SELECT pid FROM artist WHERE name = 'X'"))
	if _, err := st.SetEntityArt(ctx, model.ArtArtist, artist, model.ArtRoleFront, testPNG(t, 44, 30).Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatal(err)
	}
	check("over an artist picture", wantFront{thumb.Hash, model.ArtTrack, true},
		wantFront{thumb.Hash, model.ArtAlbum, true}, wantFront{thumb.Hash, model.ArtAlbum, true})
	assertVerifyClean(t, st)
}

// TestThePlaceholderMarkFollowsThePicture: the mark is the picture's, so a rescan that
// does not repeat it, the same picture moving between the tags and the folder, a copy of
// the file and an item folding into another keep it, while a new picture is unmarked.
func TestThePlaceholderMarkFollowsThePicture(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	lib, file := diskLibrary(t, st)
	thumb := testPNG(t, 40, 30)
	const mbid = "4e2b1b2a-0000-4000-8000-0000000000cc"
	enc := func(path, essence, codec, recording string, cover *model.ArtImage, mark bool) model.PutScannedTrackInput {
		in := trackSpecInput(lib.ID, trackSpec{path: path, essence: essence, content: essence + "-bytes", title: "One",
			artist: "A", album: "T", mbRecording: recording, durationMS: 1000})
		in.File.Codec, in.File.SampleRate = codec, 44100
		in.CoverArt, in.NoCover, in.PlaceholderCover = cover, cover == nil, mark
		return in
	}
	mp3Path := file("B/one.mp3", "m")
	item := putTrackInput(t, st, enc(mp3Path, "pf-mp3", "mp3", "", thumb, true)).ItemPID
	stands := func(when string, pid model.PID) {
		t.Helper()
		checkFront(t, st, when, model.ArtTrack, pid, wantFront{thumb.Hash, model.ArtTrack, true})
	}
	stands("imported", item)
	putTrackInput(t, st, enc(mp3Path, "pf-mp3", "mp3", "", thumb, false))
	stands("read again", item)
	beside := *thumb
	beside.Source = model.SourceSidecar
	putTrackInput(t, st, enc(mp3Path, "pf-mp3", "mp3", "", &beside, false))
	stands("found beside the file", item)

	copyPath := file("C/one.mp3", "m2")
	putTrackInput(t, st, enc(copyPath, "pf-mp3", "mp3", "", thumb, false))
	stands("with a copy", item)

	survivor := putTrackInput(t, st, enc(file("A/one.flac", "f"), "pf-flac", "flac", mbid, nil, false)).ItemPID
	out := putTrackInput(t, st, enc(mp3Path, "pf-mp3", "mp3", mbid, thumb, false))
	if out.ItemPID != survivor {
		t.Fatalf("the mp3 under the recording id landed on %s, want the flac's item %s", out.ItemPID, survivor)
	}
	stands("folded into another item", survivor)

	fresh := testPNG(t, 45, 30)
	putTrackInput(t, st, enc(mp3Path, "pf-mp3", "mp3", mbid, fresh, false))
	putTrackInput(t, st, enc(copyPath, "pf-mp3", "mp3", "", fresh, false))
	checkFront(t, st, "retagged with a new picture", model.ArtTrack, survivor, wantFront{fresh.Hash, model.ArtTrack, false})
	assertVerifyClean(t, st)
}

// TestAThumbnailRescanLeavesAChosenCover: a cover a provider or the user put on the
// track stays when a forced rescan reads the thumbnail the file still carries, where a
// real picture in the file would replace an unlocked one.
func TestAThumbnailRescanLeavesAChosenCover(t *testing.T) {
	t.Parallel()
	for _, attr := range []model.Attribution{
		{Source: model.SourceEnrichment, Provider: "coverartarchive"},
		{Source: model.SourceUser},
	} {
		t.Run(string(attr.Source), func(t *testing.T) {
			t.Parallel()
			st, lib := entityFixture(t)
			ctx := context.Background()
			thumb, chosen := testPNG(t, 40, 30), testPNG(t, 41, 30)
			item := mustScan(t, st, thumbPut(lib.ID, "one", thumb, true)).ItemPID
			if _, err := st.SetItemArt(ctx, item, model.ArtRoleFront, chosen.Data, "", attr, model.LockOff, false); err != nil {
				t.Fatal(err)
			}
			mustScan(t, st, thumbPut(lib.ID, "one", thumb, false))
			checkFront(t, st, "after the rescan", model.ArtTrack, item, wantFront{chosen.Hash, model.ArtTrack, false})
			if got := scalarStr(t, st, "SELECT source FROM art_map WHERE entity_type = 'track' AND role = 'front'"); got != string(attr.Source) {
				t.Errorf("the cover's source = %q, want %q", got, attr.Source)
			}
			assertVerifyClean(t, st)
		})
	}
}

// TestACopysRealCoverOutranksAPlaceholder: the primary carries a download's thumbnail
// and a copy carries the real cover. The copy's cover is the item's whichever file was
// read first, and unmarking the thumbnail gives the item the primary's picture back.
func TestACopysRealCoverOutranksAPlaceholder(t *testing.T) {
	t.Parallel()
	for _, thumbFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "thumbnail first", false: "cover first"}[thumbFirst], func(t *testing.T) {
			t.Parallel()
			st, put := coverFiles(t)
			ctx := context.Background()
			thumb, real := testPNG(t, 40, 30), testPNG(t, 41, 30)
			flac := put("song.flac", "flac", 900000, thumb)
			flac.PlaceholderCover = true
			mp3 := put("song.mp3", "mp3", 320000, real)
			var item model.PID
			if thumbFirst {
				item = mustScan(t, st, flac).ItemPID
				mustScan(t, st, mp3)
			} else {
				item = mustScan(t, st, mp3).ItemPID
				mustScan(t, st, flac)
			}
			if it, err := st.ItemByPID(ctx, item); err != nil || it.FilePID == "" {
				t.Fatalf("item = %+v (err %v)", it, err)
			}
			checkFront(t, st, "both read", model.ArtTrack, item, wantFront{real.Hash, model.ArtTrack, false})
			if changed, err := st.SetArtPlaceholder(ctx, thumb.Hash, false); err != nil || !changed {
				t.Fatalf("unmark = %v (err %v), want the mark to move", changed, err)
			}
			checkFront(t, st, "unmarked", model.ArtTrack, item, wantFront{thumb.Hash, model.ArtTrack, false})
		})
	}
}

// TestMarkingAPictureNamesWhatItMoves: marking a picture a placeholder, or unmarking it,
// names each item whose own cover it is and the album whose cover moved. A mark re-opens
// the album's front for the backfill, and an unmark gives back the album's cover from the
// member over a fetched one, as a real cover arriving from disk does. A mark that is
// already standing names nothing.
func TestMarkingAPictureNamesWhatItMoves(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pic := testPNG(t, 40, 30)
	item := mustScan(t, st, thumbPut(lib.ID, "one", pic, false)).ItemPID
	album := albumPID(t, st)
	albumID := int64(scalarInt(t, st, "SELECT id FROM album WHERE pid = ?", string(album)))
	due := func() bool {
		t.Helper()
		targets, err := st.AlbumsNeedingArt(ctx, model.EnrichQueueOptions{}, 0, 10, model.ArtSlots{Front: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return len(targets) == 1 && targets[0].FrontDue
	}
	// The album's front was asked about once, and nothing came back.
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: albumID, PID: album,
		Front: model.ArtHalf{Asked: true, Provider: "mock"}}); err != nil {
		t.Fatal(err)
	}
	if due() {
		t.Fatal("the album's front is due before the mark")
	}
	mark := func(on bool) int64 {
		t.Helper()
		seq, err := st.LatestChangeSeq(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := st.SetArtPlaceholder(ctx, pic.Hash, on); err != nil || !changed {
			t.Fatalf("SetArtPlaceholder(%v) = %v (err %v), want the mark moved", on, changed, err)
		}
		if !named(t, st, seq, "item", item) || !named(t, st, seq, "album", album) {
			t.Errorf("SetArtPlaceholder(%v) did not name the item and the album", on)
		}
		return seq
	}
	mark(true)
	if !due() {
		t.Error("the album's front is not due once its only cover is a placeholder")
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetArtPlaceholder(ctx, pic.Hash, true); err != nil || changed {
		t.Errorf("a second mark = %v (err %v), want no change", changed, err)
	}
	if changes, err := st.ChangesSince(ctx, seq); err != nil || len(changes) != 0 {
		t.Errorf("a second mark logged %+v (err %v), want nothing", changes, err)
	}

	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: albumID, PID: album,
		Front: model.ArtHalf{Asked: true, Provider: "mock"}, Art: rgEnrichArt("al-front")}); err != nil {
		t.Fatalf("album art backfill: %v", err)
	}
	checkFront(t, st, "filled", model.ArtAlbum, album, wantFront{"al-front", model.ArtAlbum, false})
	mark(false)
	checkFront(t, st, "unmarked", model.ArtAlbum, album, wantFront{pic.Hash, model.ArtAlbum, false})
	checkFront(t, st, "unmarked", model.ArtTrack, item, wantFront{pic.Hash, model.ArtTrack, false})
	assertVerifyClean(t, st)
}

// TestSetArtPlaceholderTakesTheArrivingHash: a picture stored scaled down is marked by
// the hash it arrived under as well as by its own, and a hash the catalog does not hold
// is not found.
func TestSetArtPlaceholderTakesTheArrivingHash(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	big := taggedCover(t, widePNG(t, 2100, 20))
	item := mustScan(t, st, thumbPut(lib.ID, "one", big, false)).ItemPID
	stored := frontOf(t, st, model.ArtTrack, item).SourceHash
	if stored == big.Hash {
		t.Fatal("the oversized picture was stored as it arrived")
	}
	if changed, err := st.SetArtPlaceholder(ctx, big.Hash, true); err != nil || !changed {
		t.Fatalf("mark by the arriving hash = %v (err %v)", changed, err)
	}
	checkFront(t, st, "marked", model.ArtTrack, item, wantFront{stored, model.ArtTrack, true})
	if _, err := st.SetArtPlaceholder(ctx, "no-such-picture", true); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("an unknown hash = %v, want CodeNotFound", err)
	}
}

// TestThePlaceholderMarkIsReadInTheTrackSlotOnly: the album holding the same picture as
// its own cover answers it as a cover, and its roles say so, while the track's roles and
// the art_placeholder field report the stand-in.
func TestThePlaceholderMarkIsReadInTheTrackSlotOnly(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	thumb := testPNG(t, 40, 30)
	marked := mustScan(t, st, thumbPut(lib.ID, "one", thumb, true)).ItemPID
	mustScan(t, st, thumbPut(lib.ID, "two", testPNG(t, 41, 30), false))
	album := albumPID(t, st)
	if _, err := st.SetEntityArt(ctx, model.ArtAlbum, album, model.ArtRoleFront, thumb.Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatal(err)
	}
	checkFront(t, st, "own row", model.ArtAlbum, album, wantFront{thumb.Hash, model.ArtAlbum, false})
	roles := func(typ model.ArtEntity, pid model.PID) bool {
		t.Helper()
		got, err := st.ArtRoles(ctx, model.EntityRef{Type: typ, PID: pid})
		if err != nil || len(got) != 1 {
			t.Fatalf("ArtRoles %s = %+v (err %v)", typ, got, err)
		}
		return got[0].Placeholder
	}
	if !roles(model.ArtTrack, marked) || roles(model.ArtAlbum, album) {
		t.Error("ArtRoles reports the mark outside the track slot or not in it")
	}
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Where("art_placeholder", query.OpIs, 1).Build(), "")
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, it := range items {
		titles = append(titles, it.Title)
	}
	if !slices.Equal(titles, []string{"one"}) {
		t.Errorf("art_placeholder is 1 = %v, want [one]", titles)
	}
}

// TestAMarkAtTheImportReachesThePictureWhereverItShows: an import marks a thumbnail the
// catalog already shows as another track's cover. That track's cover is a stand-in from
// then on and the change log says so, and a book's marked cover is still its own answer,
// since a book climbs no further.
func TestAMarkAtTheImportReachesThePictureWhereverItShows(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	thumb := testPNG(t, 40, 30)
	earlier := mustScan(t, st, thumbPut(lib.ID, "one", thumb, false)).ItemPID
	checkFront(t, st, "unmarked", model.ArtTrack, earlier, wantFront{thumb.Hash, model.ArtTrack, false})
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := thumbPut(lib.ID, "two", thumb, true)
	in.Track.Album = "Elsewhere"
	mustScan(t, st, in)
	checkFront(t, st, "after the import", model.ArtTrack, earlier, wantFront{thumb.Hash, model.ArtTrack, true})
	if !named(t, st, seq, "item", earlier) {
		t.Error("the track whose cover became a stand-in was not named")
	}

	book := bookSpecInput(lib.ID, bookSpec{path: "/lib/B/book.m4b", essence: "bk", content: "bk-bytes",
		title: "Book", author: "Writer", durationMS: 1000})
	book.CoverArt, book.PlaceholderCover = testPNG(t, 42, 30), true
	res, err := st.PutScannedBook(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	checkFront(t, st, "a book", model.ArtTrack, res.ItemPID, wantFront{book.CoverArt.Hash, model.ArtTrack, true})
	assertVerifyClean(t, st)
}

// TestADerivedCoverNamesItsMember: an album with no cover of its own answers from its
// earliest member with a real cover, else its earliest stand-in, and names that member;
// so does a track answered through its album, and a thumbnail of the album's cover.
func TestADerivedCoverNamesItsMember(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	thumb, real := testPNG(t, 40, 30), testPNG(t, 41, 30)
	a := mustScan(t, st, thumbPut(lib.ID, "a", thumb, true)).ItemPID
	b := mustScan(t, st, thumbPut(lib.ID, "b", real, false)).ItemPID
	c := mustScan(t, st, thumbPut(lib.ID, "c", nil, false)).ItemPID
	album := albumPID(t, st)
	member := func(when string, typ model.ArtEntity, pid, want model.PID) {
		t.Helper()
		if got := frontOf(t, st, typ, pid).MemberPID; got != want {
			t.Errorf("%s: %s %s names member %q, want %q", when, typ, pid, got, want)
		}
	}
	member("a real member", model.ArtAlbum, album, b)
	member("a real member", model.ArtTrack, c, b)
	member("a real member", model.ArtTrack, a, b)
	member("its own cover", model.ArtTrack, b, "")
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: album}, model.ArtRoleFront, 32)
	if err != nil || !blob.Thumbnail || blob.MemberPID != b {
		t.Errorf("album thumbnail = member %q, thumbnail %v (err %v), want %s", blob.MemberPID, blob.Thumbnail, err, b)
	}

	mustScan(t, st, thumbPut(lib.ID, "b", nil, false))
	member("a stand-in", model.ArtAlbum, album, a)
	member("a stand-in", model.ArtTrack, c, a)
	member("its own stand-in", model.ArtTrack, a, "")
}

// TestAnImportMarksOnlyAnUndecidedPicture: an import marks a picture the catalog already
// shows unmarked when nobody decided about it, but one the user unmarked by hand stays
// unmarked when the same download is imported again.
func TestAnImportMarksOnlyAnUndecidedPicture(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	scanned, unmarked := testPNG(t, 40, 30), testPNG(t, 41, 30)
	mustScan(t, st, thumbPut(lib.ID, "one", scanned, false))
	mustScan(t, st, thumbPut(lib.ID, "two", scanned, true))
	if n := scalarInt(t, st, "SELECT placeholder FROM art_source WHERE hash = ?", scanned.Hash); n != 1 {
		t.Errorf("an undecided picture's mark after an import = %d, want 1", n)
	}
	mustScan(t, st, thumbPut(lib.ID, "three", unmarked, true))
	if _, err := st.SetArtPlaceholder(ctx, unmarked.Hash, false); err != nil {
		t.Fatal(err)
	}
	mustScan(t, st, thumbPut(lib.ID, "four", unmarked, true))
	if n := scalarInt(t, st, "SELECT COALESCE(placeholder, -1) FROM art_source WHERE hash = ?", unmarked.Hash); n != 0 {
		t.Errorf("a hand-unmarked picture's mark after a second import = %d, want 0", n)
	}
	assertVerifyClean(t, st)
}

// TestMarkingAPictureSeeksTheFilesStatingIt: the files that state a picture are found
// through an index on what they state, not by reading every edge in the catalog.
func TestMarkingAPictureSeeksTheFilesStatingIt(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	plan := explainPlan(t, st, statingItemsQ, "x")
	if !strings.Contains(plan, "file_front_hash") || strings.Contains(plan, "SCAN e") {
		t.Errorf("the files stating a picture are not sought by it:\n%s", plan)
	}
}

// TestAStandInGivesWayToTheCoverBesideItsFile: a download's tags carry a thumbnail and a
// cover sits beside the file. The import marks the thumbnail and the file states the
// cover beside it, keeping the thumbnail (and its mark) as the picture it passed over.
// An unmark gives the file its thumbnail back at once, and a mark sends the file to be
// read again, since only a read sees the cover beside it.
func TestAStandInGivesWayToTheCoverBesideItsFile(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	thumb, beside := testPNG(t, 40, 30), testPNG(t, 50, 50)
	beside.Source = model.SourceSidecar
	in := thumbPut(lib.ID, "one", thumb, true)
	in.SidecarCover = beside
	in.AuxObservations = []model.AuxObservation{{Kind: model.AuxCover, Path: []byte("/lib/Al/cover.png"), Size: 1, MTimeNS: 1, Hash: beside.Hash}}
	one := mustScan(t, st, in).ItemPID
	checkFront(t, st, "imported", model.ArtTrack, one, wantFront{beside.Hash, model.ArtTrack, false})
	if got := scalarStr(t, st, "SELECT front_stand_in FROM file"); got != thumb.Hash {
		t.Errorf("the picture passed over = %q, want the thumbnail", got)
	}
	if _, _, err := st.GCArt(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT COALESCE(placeholder, -1) FROM art_source WHERE hash = ?", thumb.Hash); n != 1 {
		t.Errorf("the thumbnail's mark after a sweep = %d, want it kept marked", n)
	}
	assertVerifyClean(t, st)

	if _, err := st.SetArtPlaceholder(ctx, thumb.Hash, false); err != nil {
		t.Fatal(err)
	}
	checkFront(t, st, "unmarked", model.ArtTrack, one, wantFront{thumb.Hash, model.ArtTrack, false})
	if got := scalarStr(t, st, "SELECT front_stand_in FROM file"); got != "" {
		t.Errorf("the picture passed over after the unmark = %q, want none", got)
	}
	assertVerifyClean(t, st)

	if _, err := st.SetArtPlaceholder(ctx, thumb.Hash, true); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT mtime_ns FROM file"); n != -1 {
		t.Errorf("the file's stamp after the mark = %d, want it cleared so the next scan reads it", n)
	}
	in.PlaceholderCover = false
	mustScan(t, st, in)
	checkFront(t, st, "read again", model.ArtTrack, one, wantFront{beside.Hash, model.ArtTrack, false})
	assertVerifyClean(t, st)
}

// TestUnmarkingAHandSetCoverKeepsTheAlbumsFetchedOne: a user set a track's cover to a
// picture that was then marked, so the album took a fetched cover. Unmarking the picture
// leaves the fetched cover in place, as a hand-set track cover does; only a cover from
// the disk takes the album's back.
func TestUnmarkingAHandSetCoverKeepsTheAlbumsFetchedOne(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	one := mustScan(t, st, thumbPut(lib.ID, "one", nil, false)).ItemPID
	pic := testPNG(t, 40, 30)
	if _, err := st.SetItemArt(ctx, one, model.ArtRoleFront, pic.Data, pic.Format, model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetArtPlaceholder(ctx, pic.Hash, true); err != nil {
		t.Fatal(err)
	}
	album := albumPID(t, st)
	albumID := int64(scalarInt(t, st, "SELECT id FROM album WHERE pid = ?", string(album)))
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: albumID, PID: album,
		Front: model.ArtHalf{Asked: true, Provider: "mock"}, Art: rgEnrichArt("al-front")}); err != nil {
		t.Fatalf("album art backfill: %v", err)
	}
	checkFront(t, st, "after the fill", model.ArtAlbum, album, wantFront{"al-front", model.ArtAlbum, false})
	if _, err := st.SetArtPlaceholder(ctx, pic.Hash, false); err != nil {
		t.Fatal(err)
	}
	checkFront(t, st, "after the unmark", model.ArtAlbum, album, wantFront{"al-front", model.ArtAlbum, false})
	assertVerifyClean(t, st)
}

// TestHandSettingAPlaceholderReopensTheAlbumsFront: a track's real cover answered its
// album, which was asked about once. Setting the track's cover by hand to a marked
// picture leaves the album no real cover, so its front is due again, as a cleared cover
// leaves it.
func TestHandSettingAPlaceholderReopensTheAlbumsFront(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	one := mustScan(t, st, thumbPut(lib.ID, "one", testPNG(t, 40, 30), false)).ItemPID
	thumb := testPNG(t, 41, 30)
	mustScan(t, st, thumbPut(lib.ID, "two", thumb, true))
	album := albumPID(t, st)
	albumID := int64(scalarInt(t, st, "SELECT id FROM album WHERE pid = ?", string(album)))
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: albumID, PID: album,
		Front: model.ArtHalf{Asked: true, Provider: "mock"}}); err != nil {
		t.Fatal(err)
	}
	due := func() bool {
		t.Helper()
		targets, err := st.AlbumsNeedingArt(ctx, model.EnrichQueueOptions{}, 0, 10, model.ArtSlots{Front: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return len(targets) == 1 && targets[0].FrontDue
	}
	if due() {
		t.Fatal("the album's front is due while a real member cover answers it")
	}
	if _, err := st.SetItemArt(ctx, one, model.ArtRoleFront, thumb.Data, thumb.Format, model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatal(err)
	}
	if !due() {
		t.Error("the album's front is not due once a hand-set placeholder took its only real cover")
	}
}

// TestALargeCoverBesideAFileIsExaminedOnlyToPassOver: a put hands the store the cover
// beside a file whose tags hold a picture, and the store examines it, which scales a large
// one, only when the picture in the tags is a placeholder it may take the place of.
func TestALargeCoverBesideAFileIsExaminedOnlyToPassOver(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pic, big := testPNG(t, 40, 30), testPNG(t, art.MaxSourceDim+10, 4)
	big.Source = model.SourceSidecar
	in := thumbPut(lib.ID, "one", pic, false)
	in.SidecarCover = big
	mustScan(t, st, in)
	if _, ok := st.examined.get(big.Hash, model.Attribution{}); ok {
		t.Fatal("the cover beside the file was scaled though the picture in its tags is a real one")
	}
	if _, err := st.SetArtPlaceholder(ctx, pic.Hash, true); err != nil {
		t.Fatal(err)
	}
	mustScan(t, st, in)
	if _, ok := st.examined.get(big.Hash, model.Attribution{}); !ok {
		t.Error("the cover beside the file was not examined to pass the placeholder over")
	}
	if got := scalarStr(t, st, "SELECT front_stand_in FROM file"); got != pic.Hash {
		t.Errorf("the picture passed over = %q, want the marked one", got)
	}
}

// TestAMarkAfterThePutLookedSendsTheFileBack: the put read the picture in a file's tags
// as no placeholder and left the cover beside the file unexamined, and a mark landed
// before its transaction. The statement keeps the tags' picture, and the file is read
// again at the next scan to put the cover beside it in its place.
func TestAMarkAfterThePutLookedSendsTheFileBack(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pic := testPNG(t, 40, 30)
	mustScan(t, st, thumbPut(lib.ID, "one", pic, false))
	if _, err := st.SetArtPlaceholder(ctx, pic.Hash, true); err != nil {
		t.Fatal(err)
	}
	examined, err := st.examineArt(ctx, pic)
	if err != nil {
		t.Fatal(err)
	}
	fileID := int64(scalarInt(t, st, "SELECT id FROM file"))
	c := scannedCover{pic: examined, sidecarSkipped: true}
	if err := st.writeTx(ctx, func(tx *sql.Tx) error { return passOverStandInTx(ctx, tx, fileID, &c) }); err != nil {
		t.Fatal(err)
	}
	if c.pic.img.Hash != pic.Hash || c.standIn.present() {
		t.Errorf("statement = %+v, want the tags' picture kept", c)
	}
	if n := scalarInt(t, st, "SELECT mtime_ns FROM file"); n != -1 {
		t.Errorf("the file's stamp = %d, want it cleared for a read at the next scan", n)
	}
}
