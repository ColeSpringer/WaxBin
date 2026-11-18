package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// scanCover puts one file as a scan does, with the cover the scan found for it, nil for a
// scan that looked and found none, and returns the put's result.
func scanCover(t *testing.T, st *Store, libID int64, path, essence string, cover *model.ArtImage, preserveLocks bool) *model.ScanItemResult {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID:     libID,
		PreserveLocks: preserveLocks,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, ContentHash: "c" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "T" + essence,
			IdentityKey: "essence:" + essence,
		},
		Track:    model.Track{Artist: "X", Album: "Al", Year: 2000},
		CoverArt: cover,
		NoCover:  cover == nil,
	})
	if err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
	return res
}

func frontHash(t *testing.T, st *Store, typ model.ArtEntity, pid model.PID) string {
	t.Helper()
	got, err := st.ArtHashes(context.Background(), []model.EntityRef{{Type: typ, PID: pid}})
	if err != nil {
		t.Fatalf("ArtHashes: %v", err)
	}
	return got[model.EntityRef{Type: typ, PID: pid}]
}

// TestACoverGoneFromDiskLeavesTheCatalog rescans a file whose cover is gone, embedded or
// beside it. The scan read the file and found none, so the picture the disk gave goes
// with it, for the track and for the album that borrowed it, and the item says so.
func TestACoverGoneFromDiskLeavesTheCatalog(t *testing.T) {
	t.Parallel()
	for _, source := range []model.ProvenanceSource{model.SourceTag, model.SourceSidecar} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			st, lib := entityFixture(t)
			ctx := context.Background()
			cover := testPNG(t, 40, 40)
			cover.Source = source
			pid := scanCover(t, st, lib.ID, "/lib/al/1.flac", "e1", cover, true).ItemPID
			album := albumPID(t, st)
			if frontHash(t, st, model.ArtTrack, pid) != cover.Hash || frontHash(t, st, model.ArtAlbum, album) != cover.Hash {
				t.Fatal("the scanned cover does not answer for the track and its album")
			}
			seq, err := st.LatestChangeSeq(ctx)
			if err != nil {
				t.Fatalf("seq: %v", err)
			}

			res := scanCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil, true)
			if got := frontHash(t, st, model.ArtTrack, pid); got != "" {
				t.Errorf("the track still answers %s for a cover the disk no longer holds", got)
			}
			if got := frontHash(t, st, model.ArtAlbum, album); got != "" {
				t.Errorf("the album still answers %s from a member that lost its cover", got)
			}
			if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
				t.Errorf("ResolveArt = %v, want CodeNotFound", err)
			}
			if !res.SidecarsChanged {
				t.Error("the put reports no sidecar change for a cover it dropped")
			}
			changes, err := st.ChangesSince(ctx, seq)
			if err != nil {
				t.Fatalf("changes: %v", err)
			}
			named := false
			for _, c := range changes {
				named = named || c.EntityType == "item" && c.EntityPID == pid && c.Op == model.OpUpdate
			}
			if !named {
				t.Errorf("no item update for the dropped cover in %+v", changes)
			}
			// A second scan finds nothing to do.
			if res := scanCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil, true); res.SidecarsChanged {
				t.Error("a rescan of a coverless file reports a change")
			}
		})
	}
}

// TestACoverlessScanKeepsWhatTheDiskNeverGave covers the fronts a coverless scan has no
// say over: one a user or a generator set, which no file ever stated, and one under a
// lock the scan was told to respect.
func TestACoverlessScanKeepsWhatTheDiskNeverGave(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		set           model.ProvenanceSource // "" keeps the scanned cover
		lock          bool
		preserveLocks bool
		kept          bool
	}{
		{name: "a user's cover", set: model.SourceUser, preserveLocks: true, kept: true},
		{name: "a generated cover", set: model.SourceGenerated, preserveLocks: true, kept: true},
		{name: "a locked scanned cover", lock: true, preserveLocks: true, kept: true},
		{name: "a locked scanned cover, locks ignored", lock: true, preserveLocks: false, kept: false},
		{name: "a user's cover, locks ignored", set: model.SourceUser, preserveLocks: false, kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, lib := entityFixture(t)
			ctx := context.Background()
			scanned := testPNG(t, 40, 40)
			pid := scanCover(t, st, lib.ID, "/lib/al/1.flac", "e1", scanned, true).ItemPID
			want := scanned.Hash
			if tc.set != "" {
				chosen := testPNG(t, 41, 41)
				if _, err := st.SetItemArt(ctx, pid, model.ArtRoleFront, chosen.Data, "", model.Attribution{Source: tc.set}, model.LockOf(false), false); err != nil {
					t.Fatalf("SetItemArt: %v", err)
				}
				want = chosen.Hash
			}
			if tc.lock {
				if _, err := st.SetArtLock(ctx, model.ArtTrack, pid, model.ArtRoleFront, true); err != nil {
					t.Fatalf("SetArtLock: %v", err)
				}
			}
			scanCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil, tc.preserveLocks)
			got := frontHash(t, st, model.ArtTrack, pid)
			if tc.kept && got != want {
				t.Errorf("front = %q after a coverless scan, want %s kept", got, want)
			}
			if !tc.kept && got != "" {
				t.Errorf("front = %s after a coverless scan, want it dropped", got)
			}
		})
	}
}

// TestABookCoverFollowsItsParts holds the book rule: the cover is the primary part's,
// it stays while any part still states that picture, and it goes once none does.
func TestABookCoverFollowsItsParts(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	cover := testPNG(t, 40, 40)
	part := func(n int, withCover bool) *model.ScanItemResult {
		t.Helper()
		in := bookSpecInput(lib.ID, bookSpec{
			path: filepath.Join("/lib/book", string(rune('0'+n))+".mp3"), essence: "b" + string(rune('0'+n)),
			content: "c" + string(rune('0'+n)), title: "Tome", author: "Auth", asin: "B0010", position: n, durationMS: 1000,
		})
		if withCover {
			in.CoverArt = cover
		}
		in.NoCover = !withCover
		res, err := st.PutScannedBook(ctx, in)
		if err != nil {
			t.Fatalf("put part %d: %v", n, err)
		}
		return res
	}
	book := part(1, true).ItemPID
	part(2, true)
	part(3, false)
	if got := frontHash(t, st, model.ArtTrack, book); got != cover.Hash {
		t.Fatalf("front = %q after a coverless third part, want the primary part's cover kept", got)
	}
	if res := part(1, false); res.SidecarsChanged {
		t.Error("the primary part's put reports a change while part two still states the cover")
	}
	if got := frontHash(t, st, model.ArtTrack, book); got != cover.Hash {
		t.Fatalf("front = %q after the primary part lost its cover, want it kept for part two", got)
	}
	if res := part(2, false); !res.SidecarsChanged {
		t.Error("the put that took the book's cover away reports no change")
	}
	if got := frontHash(t, st, model.ArtTrack, book); got != "" {
		t.Errorf("front = %s once no part states a cover, want none", got)
	}
	// The cover is the primary part's to give: a later part's is not taken up.
	part(3, true)
	if got := frontHash(t, st, model.ArtTrack, book); got != "" {
		t.Errorf("front = %q after part three gained a cover, want the book left without", got)
	}
}

// coverFiles is a store over a library rooted in a real folder, which the copy rules
// need to tell a file on disk from one gone, and put builds one encoding of one
// recording there with the cover its scan found (nil for a finding of none).
func coverFiles(t *testing.T) (st *Store, put func(name, codec string, bitrate int, cover *model.ArtImage) model.PutScannedTrackInput) {
	t.Helper()
	st, _ = entityFixture(t)
	root := t.TempDir()
	lib, err := st.EnsureLibrary(context.Background(), &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatalf("ensure library: %v", err)
	}
	return st, func(name, codec string, bitrate int, cover *model.ArtImage) model.PutScannedTrackInput {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		return model.PutScannedTrackInput{
			LibraryID: lib.ID, PreserveLocks: true,
			File: model.File{
				Path: []byte(path), DisplayPath: path, RelPath: []byte(name), Kind: model.FileAudio,
				ContentHash: "c" + name, EssenceHash: "e" + name, Codec: codec, Bitrate: bitrate, SampleRate: 44100,
				ScanState: model.ScanIndexed,
			},
			Item:     model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: "Song", IdentityKey: "mbid:rec-1"},
			Track:    model.Track{Artist: "X", Album: "Al"},
			CoverArt: cover, NoCover: cover == nil,
		}
	}
}

func mustScan(t *testing.T, st *Store, in model.PutScannedTrackInput) *model.ScanItemResult {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s: %v", in.File.DisplayPath, err)
	}
	return res
}

// TestACoverAnotherFileStatesStays is the item with two encodings. A coverless flac
// takes the item over from an mp3 that embeds the cover: the picture is still on disk in
// the mp3, so it stays, and goes only when the mp3 stops stating it too.
func TestACoverAnotherFileStatesStays(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	cover := testPNG(t, 40, 40)
	mp3 := mustScan(t, st, put("song.mp3", "mp3", 320000, cover))
	flac := mustScan(t, st, put("song.flac", "flac", 900000, nil))
	if it, err := st.ItemByPID(ctx, mp3.ItemPID); err != nil || it.FilePID != flac.FilePID {
		t.Fatalf("item = %+v (err %v), want the flac to have taken it over", it, err)
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != cover.Hash {
		t.Fatalf("front = %q after a coverless flac took over, want the mp3's cover kept", got)
	}
	mustScan(t, st, put("song.flac", "flac", 900000, nil))
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != cover.Hash {
		t.Fatalf("front = %q after the flac was read again, want the mp3's cover kept", got)
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}
	if res := mustScan(t, st, put("song.mp3", "mp3", 320000, nil)); !res.AttachedAsCopy || !res.SidecarsChanged {
		t.Fatalf("the mp3 read again = %+v, want it a copy whose read changed a cover", res)
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != "" {
		t.Errorf("front = %s once neither file states a cover, want none", got)
	}
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	named := false
	for _, c := range changes {
		named = named || c.EntityType == "item" && c.EntityPID == mp3.ItemPID
	}
	if !named {
		t.Errorf("the copy that took the cover away named no item change: %+v", changes)
	}
}

// TestACopysCoverIsNotTheItems: an item draws its primary file's cover. A copy's own
// picture is never taken up, on an item with no front or one with another.
func TestACopysCoverIsNotTheItems(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	cover, other := testPNG(t, 40, 40), testPNG(t, 41, 41)
	flac := mustScan(t, st, put("song.flac", "flac", 900000, nil))
	if res := mustScan(t, st, put("song.mp3", "mp3", 320000, cover)); !res.AttachedAsCopy || res.SidecarsChanged {
		t.Fatalf("the mp3 = %+v, want it a copy of the flac that changed no cover", res)
	}
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != "" {
		t.Fatalf("front = %q, want none: the primary states no cover", got)
	}
	mustScan(t, st, put("song.flac", "flac", 900000, other))
	mustScan(t, st, put("song.mp3", "mp3", 320000, cover))
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != other.Hash {
		t.Errorf("front = %q, want the primary's own cover", got)
	}
}

// TestATagWriteKeepsWhatTheFileStates follows a file's recorded cover through the
// writes that change the file without a read. A write of other tags leaves the picture
// and the record alone, so analyze and organize do not blind the catalog to it; a cover
// write leaves the file stating the picture written; and a strip leaves a folder cover
// or none, which only a read can tell.
func TestATagWriteKeepsWhatTheFileStates(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	cover, written := testPNG(t, 40, 40), testPNG(t, 41, 41)
	in := put("song.mp3", "mp3", 320000, cover)
	mp3 := mustScan(t, st, in)
	size, mtime := in.File.Size, in.File.MTimeNS
	write := func(front *string) {
		t.Helper()
		ok, err := st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
			FilePID: mp3.FilePID, ExpectedSize: size, ExpectedMTimeNS: mtime,
			NewSize: size + 1, NewMTimeNS: mtime + 1, NewContentHash: "written", Front: front,
		})
		if err != nil || !ok {
			t.Fatalf("file state update = %v (err %v), want it applied", ok, err)
		}
		size, mtime = size+1, mtime+1
	}
	stated := func() string {
		t.Helper()
		return scalarStr(t, st, "SELECT COALESCE(front_hash, 'unknown') FROM file WHERE pid = ?", string(mp3.FilePID))
	}
	write(nil)
	if got := stated(); got != cover.Hash {
		t.Errorf("after a write of other tags the file states %q, want its cover %s still", got, cover.Hash)
	}
	write(&written.Hash)
	if got := stated(); got != written.Hash {
		t.Errorf("after a cover write the file states %q, want the cover written %s", got, written.Hash)
	}
	none := ""
	write(&none)
	if got := stated(); got != "unknown" {
		t.Errorf("after a strip the file states %q, want unknown until it is read", got)
	}
}

// TestACoverWrittenToACopyKeepsTheItemsCover is why the write keeps the record current:
// the copy now embeds the cover the catalog holds, so the primary read again without one
// takes nothing away, though no scan has read the copy since.
func TestACoverWrittenToACopyKeepsTheItemsCover(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	old, chosen := testPNG(t, 40, 40), testPNG(t, 41, 41)
	mp3in := put("song.mp3", "mp3", 320000, old)
	mp3 := mustScan(t, st, mp3in)
	mustScan(t, st, put("song.flac", "flac", 900000, old))
	// The chosen cover lands in the catalog and is written into both files; the scan then
	// reads the flac, which carries it, and the mp3 is left on the fast path.
	if _, err := st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID: mp3.FilePID, ExpectedSize: mp3in.File.Size, ExpectedMTimeNS: mp3in.File.MTimeNS,
		NewSize: 9, NewMTimeNS: 9, NewContentHash: "written", Front: &chosen.Hash,
	}); err != nil {
		t.Fatal(err)
	}
	mustScan(t, st, put("song.flac", "flac", 900000, chosen))
	mustScan(t, st, put("song.flac", "flac", 900000, nil))
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != chosen.Hash {
		t.Errorf("front = %q, want the cover the mp3 still carries from the write", got)
	}
}

// TestAFileNotReadSinceIsNotTakenForCoverless covers a catalog from before a read's
// cover was kept per file: the other file of the item may still hold the picture, so a
// coverless read of this one takes nothing away.
func TestAFileNotReadSinceIsNotTakenForCoverless(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	cover := testPNG(t, 40, 40)
	mp3 := mustScan(t, st, put("song.mp3", "mp3", 320000, cover))
	if _, err := st.wdb().ExecContext(ctx, "UPDATE file SET front_hash = NULL"); err != nil {
		t.Fatal(err)
	}
	mustScan(t, st, put("song.flac", "flac", 900000, nil))
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != cover.Hash {
		t.Errorf("front = %q, want the cover kept while the mp3 has not been read again", got)
	}
}

// TestAReadThatSaysNothingKeepsTheCover is the put with no picture and no finding of
// none: a scan that could not read a cover folder, or an import whose cover follows.
func TestAReadThatSaysNothingKeepsTheCover(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	cover := testPNG(t, 40, 40)
	res := mustScan(t, st, put("song.mp3", "mp3", 320000, cover))
	in := put("song.mp3", "mp3", 320000, nil)
	in.NoCover = false
	if again := mustScan(t, st, in); again.SidecarsChanged {
		t.Error("a read that says nothing about the cover reports a change")
	}
	if got := frontHash(t, st, model.ArtTrack, res.ItemPID); got != cover.Hash {
		t.Fatalf("front = %q after a read that says nothing, want the cover kept", got)
	}
	// And what the file last stated stands, so a later finding of none still counts.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM file WHERE front_hash = ?", cover.Hash); n != 1 {
		t.Errorf("%d files recorded as stating the cover, want the one", n)
	}
}

// TestADroppedMemberCoverReopensTheAlbumsFront: the album's front marker says its vacancy
// was asked about, and a member losing the cover that answered for the album is a new
// vacancy.
func TestADroppedMemberCoverReopensTheAlbumsFront(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	scanCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 40, 40), true)
	albumID := scalarInt(t, st, "SELECT id FROM album")
	if _, err := st.wdb().ExecContext(ctx,
		"INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, enriched_at) VALUES ('album_front', ?, 'none', 0, 1)", albumID); err != nil {
		t.Fatal(err)
	}
	scanCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil, true)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = 'album_front' AND entity_id = ?", albumID); n != 0 {
		t.Error("the album's front marker outlived the member cover that answered for it")
	}
}

// TestARipCoverFollowsItsFile is the rip's side: one file's cover maps onto each track
// carved from it, and a cover that changes or goes while the sheet stands still reaches
// every one of them, as it does a plain track whose audio did not change.
func TestARipCoverFollowsItsFile(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	cover, second := testPNG(t, 40, 40), testPNG(t, 41, 41)
	rip := func(c *model.ArtImage) *model.ScanItemResult {
		t.Helper()
		in := model.PutScannedVirtualTracksInput{
			LibraryID: lib.ID,
			File: model.File{
				Path: []byte("/lib/rip.flac"), DisplayPath: "/lib/rip.flac", RelPath: []byte("rip.flac"),
				Kind: model.FileAudio, Size: 2, MTimeNS: 1, ContentHash: "CR", EssenceHash: "ER",
				DurationMS: 600000, ScanState: model.ScanIndexed,
			},
			CoverArt:      c,
			NoCover:       c == nil,
			PreserveLocks: true,
		}
		for n, start := range []int64{0, 7500} {
			in.Tracks = append(in.Tracks, model.VirtualTrack{
				Item: model.PlayableItem{
					Kind: model.KindTrack, State: model.StatePresent, Title: "Track",
					IdentityKey: identity.VirtualTrackKey("ER", n+1, start),
				},
				Track:       model.Track{Artist: "Rip", AlbumArtist: "Rip", Album: "Rip Album", TrackNo: n + 1},
				StartFrames: start, EndFrames: (start + 7500) % 15000,
			})
		}
		res, err := st.PutScannedVirtualTracks(ctx, in)
		if err != nil {
			t.Fatalf("put rip: %v", err)
		}
		return res
	}
	covered := func(hash string) int {
		t.Helper()
		return scalarInt(t, st, "SELECT COUNT(*) FROM art_map WHERE entity_type = 'track' AND role = 'front' AND source_hash = ?", hash)
	}
	rip(cover)
	if n := covered(cover.Hash); n != 2 {
		t.Fatalf("%d tracks carry the rip's cover, want both", n)
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}
	if res := rip(second); !res.SidecarsChanged {
		t.Error("the put reports no sidecar change for a cover it replaced")
	}
	if n := covered(second.Hash); n != 2 {
		t.Errorf("%d tracks carry the rip's new cover, want both", n)
	}
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	named := map[string]int{}
	for _, c := range changes {
		named[c.EntityType]++
	}
	if len(changes) != 3 || named["item"] != 2 || named["album"] != 1 {
		t.Errorf("a replaced cover named %+v, want an update per track and one for the album that borrows it", changes)
	}
	if res := rip(second); res.SidecarsChanged {
		t.Error("a rescan of an unchanged rip reports a change")
	}
	rip(nil)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_map WHERE entity_type = 'track'"); n != 0 {
		t.Errorf("%d tracks still carry a cover the rip no longer states", n)
	}
}

// TestAShowCoverThatLandsLaterNamesTheShow syncs a show whose picture did not download
// the first time. The sync that brings it changes nothing else about the show, and has
// to say the show changed all the same; the one after it has nothing to say.
func TestAShowCoverThatLandsLaterNamesTheShow(t *testing.T) {
	t.Parallel()
	const imageURL = "http://feed.example/cover.png"
	for name, sync := range map[string]func(st *Store, image *model.ArtImage) (model.PID, error){
		"feed": func(st *Store, image *model.ArtImage) (model.PID, error) {
			res, err := st.UpsertFeed(context.Background(), model.UpsertFeedInput{
				FeedURL: "http://feed.example/f", IdentityKey: identity.PodcastKey("", "http://feed.example/f"),
				Feed: model.Feed{Title: "Cast", ImageURL: imageURL}, FetchedAtNS: 1, Image: image,
			})
			if err != nil {
				return "", err
			}
			return res.PodcastPID, nil
		},
		"show": func(st *Store, image *model.ArtImage) (model.PID, error) {
			pid, _, err := st.UpsertShow(context.Background(), model.UpsertShowInput{
				FeedURL: "http://feed.example/f", IdentityKey: identity.PodcastKey("", "http://feed.example/f"),
				Title: "Cast", ImageURL: imageURL, Image: image,
			})
			return pid, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st, _ := entityFixture(t)
			ctx := context.Background()
			show, err := sync(st, nil)
			if err != nil {
				t.Fatalf("first sync: %v", err)
			}
			named := func() int {
				t.Helper()
				seq, err := st.LatestChangeSeq(ctx)
				if err != nil {
					t.Fatalf("seq: %v", err)
				}
				image := testPNG(t, 40, 40)
				image.Attribution = model.Attribution{Source: model.SourceFeed, SourceURL: imageURL}
				if _, err := sync(st, image); err != nil {
					t.Fatalf("sync: %v", err)
				}
				changes, err := st.ChangesSince(ctx, seq)
				if err != nil {
					t.Fatalf("changes: %v", err)
				}
				n := 0
				for _, c := range changes {
					if c.EntityType == "podcast" && c.EntityPID == show && c.Op == model.OpUpdate {
						n++
					}
				}
				return n
			}
			if n := named(); n != 1 {
				t.Errorf("the sync that brought the cover named the show %d times, want once", n)
			}
			if frontHash(t, st, model.ArtPodcast, show) == "" {
				t.Fatal("the show holds no cover after the sync that brought one")
			}
			if n := named(); n != 0 {
				t.Errorf("a sync that changed nothing named the show %d times", n)
			}
		})
	}
}

// TestAnEnrichmentAlbumCoverYieldsToAMembersOwn: enrichment fills an album only while it
// resolves no cover, so a fill made in a gap, the folder cover still on its way or
// briefly gone, has to give way when a track's own cover arrives, or it would outrank
// that cover for good. A cover a user chose for the album, one under a lock, and one
// holding the same picture all stay.
func TestAnEnrichmentAlbumCoverYieldsToAMembersOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		source model.ProvenanceSource
		lock   bool
		same   bool
		yields bool
	}{
		{name: "enrichment", source: model.SourceEnrichment, yields: true},
		{name: "enrichment, locked", source: model.SourceEnrichment, lock: true},
		{name: "enrichment, the same picture", source: model.SourceEnrichment, same: true},
		{name: "a user's", source: model.SourceUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, lib := entityFixture(t)
			ctx := context.Background()
			own, fetched := testPNG(t, 40, 40), testPNG(t, 41, 41)
			if tc.same {
				fetched = testPNG(t, 40, 40)
			}
			fetched.Attribution = model.Attribution{Source: tc.source}
			if tc.source == model.SourceEnrichment {
				fetched.Provider = "caa"
			}
			pid := scanCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil, true).ItemPID
			album := albumPID(t, st)
			albumID := int64(scalarInt(t, st, "SELECT id FROM album"))
			pic, err := st.examineArt(ctx, fetched)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.writeTx(ctx, func(tx *sql.Tx) error {
				_, err := setEntityArtRoleTx(ctx, tx, "album", albumID, "front", pic)
				return err
			}); err != nil {
				t.Fatalf("fill the album: %v", err)
			}
			if tc.lock {
				if _, err := st.SetArtLock(ctx, model.ArtAlbum, album, model.ArtRoleFront, true); err != nil {
					t.Fatal(err)
				}
			}
			seq, err := st.LatestChangeSeq(ctx)
			if err != nil {
				t.Fatal(err)
			}

			scanCover(t, st, lib.ID, "/lib/al/1.flac", "e1", own, true)
			if got := frontHash(t, st, model.ArtTrack, pid); got != own.Hash {
				t.Fatalf("the track draws %q, want its own cover", got)
			}
			want := fetched.Hash
			if tc.yields {
				want = own.Hash
			}
			if got := frontHash(t, st, model.ArtAlbum, album); got != want {
				t.Errorf("the album draws %q, want %q", got, want)
			}
			rows := scalarInt(t, st, "SELECT COUNT(*) FROM art_map WHERE entity_type = 'album'")
			if tc.yields != (rows == 0) {
				t.Errorf("the album holds %d rows of its own, want yields=%v", rows, tc.yields)
			}
			changes, err := st.ChangesSince(ctx, seq)
			if err != nil {
				t.Fatal(err)
			}
			named := false
			for _, c := range changes {
				named = named || c.EntityType == "album" && c.EntityPID == album
			}
			if named != tc.yields {
				t.Errorf("album named in the change log = %v, want %v: %+v", named, tc.yields, changes)
			}
		})
	}
}
