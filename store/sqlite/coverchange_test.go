package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/art"
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
// it stays while any part still states that picture, it goes once none does, and with
// the primary stating none it is the cover of the first part that states one.
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
	// With no part ahead of it stating one, a later part's cover is the book's.
	if res := part(3, true); !res.SidecarsChanged {
		t.Error("the put that gave the book its cover reports no change")
	}
	if got := frontHash(t, st, model.ArtTrack, book); got != cover.Hash {
		t.Errorf("front = %q after part three gained a cover, want it taken up", got)
	}
	// A part ahead of it that states another takes over, and a part behind does not.
	other := testPNG(t, 41, 41)
	in := bookSpecInput(lib.ID, bookSpec{
		path: filepath.Join("/lib/book", "2.mp3"), essence: "b2", content: "c2",
		title: "Tome", author: "Auth", asin: "B0010", position: 2, durationMS: 1000,
	})
	in.CoverArt = other
	if _, err := st.PutScannedBook(ctx, in); err != nil {
		t.Fatalf("put part 2: %v", err)
	}
	if got := frontHash(t, st, model.ArtTrack, book); got != other.Hash {
		t.Errorf("front = %q after part two stated another cover, want part two's", got)
	}
	part(3, true)
	if got := frontHash(t, st, model.ArtTrack, book); got != other.Hash {
		t.Errorf("front = %q after part three was read again, want part two's kept", got)
	}
}

// coverFiles is a store over a library rooted in a real folder, which the copy rules
// need to tell a file on disk from one gone, and put builds one encoding of one
// recording there with the cover its scan found (nil for a finding of none).
func coverFiles(t *testing.T) (st *Store, put func(name, codec string, bitrate int, cover *model.ArtImage) model.PutScannedTrackInput) {
	t.Helper()
	st, _ = entityFixture(t)
	// However a test moves files and covers about, it ends with every cover from disk
	// being the one its item's files state.
	t.Cleanup(func() {
		if rep, err := st.VerifyDerived(context.Background()); err != nil || rep.CoverDrift != 0 {
			t.Errorf("the test ends with %d covers adrift (err %v)", rep.CoverDrift, err)
		}
	})
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

// TestTheFirstFileToStateACoverGivesIt: an item draws its primary file's cover, and a
// copy's while the primary states none, whichever of them was read first. A copy's
// picture never replaces one the primary states.
func TestTheFirstFileToStateACoverGivesIt(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	cover, other := testPNG(t, 40, 40), testPNG(t, 41, 41)
	flac := mustScan(t, st, put("song.flac", "flac", 900000, nil))
	if res := mustScan(t, st, put("song.mp3", "mp3", 320000, cover)); !res.AttachedAsCopy || !res.SidecarsChanged {
		t.Fatalf("the mp3 = %+v, want it a copy of the flac whose cover the item took", res)
	}
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != cover.Hash {
		t.Fatalf("front = %q, want the copy's cover: the primary states none", got)
	}
	mustScan(t, st, put("song.flac", "flac", 900000, other))
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != other.Hash {
		t.Fatalf("front = %q, want the primary's own cover", got)
	}
	if res := mustScan(t, st, put("song.mp3", "mp3", 320000, cover)); res.SidecarsChanged {
		t.Error("a copy read again behind a primary that states a cover reports a change")
	}
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != other.Hash {
		t.Errorf("front = %q, want the primary's own cover kept", got)
	}
	// The primary loses its picture, and the cover passes to the copy that still has one.
	mustScan(t, st, put("song.flac", "flac", 900000, nil))
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != cover.Hash {
		t.Errorf("front = %q once the primary states none, want the copy's cover", got)
	}
}

// TestATagWriteKeepsWhatTheFileStates follows a file's recorded cover through the
// writes that change the file without a read. A write of other tags leaves the picture
// and the record alone, so analyze and organize do not blind the catalog to it; a cover
// write leaves the file stating what the writer says it states now, a picture or none;
// and one that cannot say leaves it unknown.
func TestATagWriteKeepsWhatTheFileStates(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	cover, written := testPNG(t, 40, 40), testPNG(t, 41, 41)
	in := put("song.mp3", "mp3", 320000, cover)
	mp3 := mustScan(t, st, in)
	size, mtime := in.File.Size, in.File.MTimeNS
	write := func(cover *model.FileCover) {
		t.Helper()
		ok, err := st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
			FilePID: mp3.FilePID, ExpectedSize: size, ExpectedMTimeNS: mtime,
			NewSize: size + 1, NewMTimeNS: mtime + 1, NewContentHash: "written", Cover: cover,
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
	write(&model.FileCover{Art: written})
	if got := stated(); got != written.Hash {
		t.Errorf("after a cover write the file states %q, want the cover written %s", got, written.Hash)
	}
	// The item's cover was the file's, so it follows the write as it would a read.
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != written.Hash {
		t.Errorf("front = %q after the file's picture was rewritten, want the one written", got)
	}
	write(&model.FileCover{})
	if got := stated(); got != "unknown" {
		t.Errorf("after a write that cannot say the file states %q, want unknown", got)
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != written.Hash {
		t.Errorf("front = %q while the file's cover is unknown, want it kept", got)
	}
	write(&model.FileCover{None: true})
	if got := stated(); got != "" {
		t.Errorf("after a strip with nothing beside the file it states %q, want none", got)
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != "" {
		t.Errorf("front = %q after the strip, want none", got)
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
		NewSize: 9, NewMTimeNS: 9, NewContentHash: "written", Cover: &model.FileCover{Art: chosen},
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

// TestAReadThatFoundNoAnswerKeepsTheCover is the put with no picture and no finding of
// none: the tags hold no picture and the folders did not say, as when a cover file would
// not read or an import's cover is still to land. The item keeps its cover, and the file
// no longer counts as stating the one its tags held before, so what stands beside it can
// be told later and settles the cover. A read that left the tags unread says nothing at
// all, and the file goes on stating what it did.
func TestAReadThatFoundNoAnswerKeepsTheCover(t *testing.T) {
	t.Parallel()
	for _, tagsUnread := range []bool{false, true} {
		st, put := coverFiles(t)
		ctx := context.Background()
		cover := testPNG(t, 40, 40)
		res := mustScan(t, st, put("song.mp3", "mp3", 320000, cover))
		in := put("song.mp3", "mp3", 320000, nil)
		in.NoCover, in.TagsUnread = false, tagsUnread
		if again := mustScan(t, st, in); again.SidecarsChanged {
			t.Errorf("tags unread %v: a read that found no answer reports a change", tagsUnread)
		}
		if got := frontHash(t, st, model.ArtTrack, res.ItemPID); got != cover.Hash {
			t.Fatalf("tags unread %v: front = %q after a read that found no answer, want the cover kept", tagsUnread, got)
		}
		want := "unknown"
		if tagsUnread {
			want = cover.Hash
		}
		if got := scalarStr(t, st, "SELECT COALESCE(front_hash, 'unknown') FROM file"); got != want {
			t.Errorf("tags unread %v: the file states %q, want %q", tagsUnread, got, want)
		}
		// The folders say there is none, and the cover the tags no longer hold goes.
		changed, err := st.PutFolderCover(ctx, model.FolderCoverInput{FilePID: res.FilePID, Cover: model.FileCover{None: true}, PreserveLocks: true})
		if err != nil || changed == tagsUnread {
			t.Errorf("tags unread %v: the folder finding changed a cover = %v (err %v)", tagsUnread, changed, err)
		}
		if got := frontHash(t, st, model.ArtTrack, res.ItemPID); (got == "") == tagsUnread {
			t.Errorf("tags unread %v: front = %q after the folders said none", tagsUnread, got)
		}
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

// named reports whether the change log names an update of the entity since seq.
func named(t *testing.T, st *Store, seq int64, typ string, pid model.PID) bool {
	t.Helper()
	changes, err := st.ChangesSince(context.Background(), seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	for _, c := range changes {
		if c.EntityType == typ && c.EntityPID == pid && c.Op == model.OpUpdate {
			return true
		}
	}
	return false
}

// TestACoverGoesWithTheFileThatStatedIt is the item whose cover only a copy states: a
// coverless flac is the primary and the mp3 behind it embeds the picture. However the
// mp3 leaves the item, the cover goes with it, for the item and the album that borrowed
// it, with no read of the flac. And the mp3 put back gives the cover again.
func TestACoverGoesWithTheFileThatStatedIt(t *testing.T) {
	t.Parallel()
	for name, leave := range map[string]func(t *testing.T, st *Store, mp3 model.PutScannedTrackInput, file model.PID){
		"detached": func(t *testing.T, st *Store, _ model.PutScannedTrackInput, file model.PID) {
			if _, err := st.DetachFile(context.Background(), file, model.DeletePrune); err != nil {
				t.Fatalf("detach: %v", err)
			}
		},
		"trashed": func(t *testing.T, st *Store, mp3 model.PutScannedTrackInput, file model.PID) {
			if _, err := st.TrashFile(context.Background(), model.TrashFileInput{
				FilePID: file, TrashPath: []byte(mp3.File.DisplayPath + ".trashed"), TrashDisplay: mp3.File.DisplayPath + ".trashed",
			}); err != nil {
				t.Fatalf("trash: %v", err)
			}
		},
		"gone from disk": func(t *testing.T, st *Store, mp3 model.PutScannedTrackInput, file model.PID) {
			if err := os.Remove(mp3.File.DisplayPath); err != nil {
				t.Fatal(err)
			}
			if _, err := st.MarkFilesMissing(context.Background(), []model.PID{file}); err != nil {
				t.Fatalf("mark missing: %v", err)
			}
		},
		"retagged to another recording": func(t *testing.T, st *Store, mp3 model.PutScannedTrackInput, _ model.PID) {
			mp3.Item.IdentityKey, mp3.Item.Title = "mbid:rec-2", "Other Song"
			mp3.Track.Album = "Other Album"
			mustScan(t, st, mp3)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st, put := coverFiles(t)
			ctx := context.Background()
			cover := testPNG(t, 40, 40)
			mp3in := put("song.mp3", "mp3", 320000, cover)
			mp3 := mustScan(t, st, mp3in)
			mustScan(t, st, put("song.flac", "flac", 900000, nil))
			album := albumPID(t, st)
			if frontHash(t, st, model.ArtTrack, mp3.ItemPID) != cover.Hash || frontHash(t, st, model.ArtAlbum, album) != cover.Hash {
				t.Fatal("the copy's cover does not answer for the item and its album")
			}
			seq, err := st.LatestChangeSeq(ctx)
			if err != nil {
				t.Fatal(err)
			}

			leave(t, st, mp3in, mp3.FilePID)
			if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != "" {
				t.Errorf("the item still answers %s for a cover none of its files states", got)
			}
			if got := frontHash(t, st, model.ArtAlbum, album); got != "" {
				t.Errorf("the album still answers %s", got)
			}
			if !named(t, st, seq, "item", mp3.ItemPID) || !named(t, st, seq, "album", album) {
				t.Error("the change log does not name the item and the album whose cover went")
			}

			// The file comes back as the copy it was, and its cover with it.
			if name == "gone from disk" {
				if err := os.WriteFile(mp3in.File.DisplayPath, []byte("audio"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if res := mustScan(t, st, mp3in); res.ItemPID != mp3.ItemPID || !res.AttachedAsCopy {
				t.Fatalf("the mp3 put back = %+v, want it a copy of the item again", res)
			}
			if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != cover.Hash {
				t.Errorf("front = %q once the mp3 is back, want its cover", got)
			}
		})
	}
}

// TestACoverPassesToTheNextFileThatStatesOne: the primary gave the item its cover and a
// copy states another. When the primary goes, the copy's picture is the item's at once,
// from the bytes kept for it, before any read of the copy.
func TestACoverPassesToTheNextFileThatStatesOne(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	own, copys := testPNG(t, 40, 40), testPNG(t, 41, 41)
	copys.Source = model.SourceSidecar
	flac := mustScan(t, st, put("song.flac", "flac", 900000, own))
	if res := mustScan(t, st, put("song.mp3", "mp3", 320000, copys)); !res.AttachedAsCopy || res.SidecarsChanged {
		t.Fatalf("the mp3 = %+v, want a copy that changed no cover", res)
	}
	album := albumPID(t, st)
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.DetachFile(ctx, flac.FilePID, model.DeletePrune)
	if err != nil || len(res.Promoted) != 1 {
		t.Fatalf("detach = %+v (err %v), want the mp3 promoted", res, err)
	}
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != copys.Hash {
		t.Fatalf("front = %q once the primary went, want the copy's cover", got)
	}
	if got := scalarStr(t, st, "SELECT source FROM art_map WHERE entity_type = 'track' AND role = 'front'"); got != string(model.SourceSidecar) {
		t.Errorf("the cover's source = %q, want the one the copy's read gave it", got)
	}
	if !named(t, st, seq, "album", album) {
		t.Error("the album that borrows the cover was not named")
	}
}

// TestACoverTheDiskDidNotGiveOutlastsItsFiles covers what a departing file does not take:
// a front under a lock, one a user set, and the cover of an item left with no file at
// all, which is kept for the file that may be restored.
func TestACoverTheDiskDidNotGiveOutlastsItsFiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		prepare  func(t *testing.T, st *Store, item model.PID) (want string)
		lastFile bool
	}{
		{name: "locked", prepare: func(t *testing.T, st *Store, item model.PID) string {
			if _, err := st.SetArtLock(context.Background(), model.ArtTrack, item, model.ArtRoleFront, true); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{name: "set by a user", prepare: func(t *testing.T, st *Store, item model.PID) string {
			chosen := testPNG(t, 42, 42)
			if _, err := st.SetItemArt(context.Background(), item, model.ArtRoleFront, chosen.Data, "",
				model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
				t.Fatal(err)
			}
			return chosen.Hash
		}},
		{name: "the last file", lastFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, put := coverFiles(t)
			ctx := context.Background()
			cover := testPNG(t, 40, 40)
			mp3 := mustScan(t, st, put("song.mp3", "mp3", 320000, cover))
			want := cover.Hash
			if !tc.lastFile {
				mustScan(t, st, put("song.flac", "flac", 900000, nil))
			}
			if tc.prepare != nil {
				if w := tc.prepare(t, st, mp3.ItemPID); w != "" {
					want = w
				}
			}
			if _, err := st.DetachFile(ctx, mp3.FilePID, model.DeletePrune); err != nil {
				t.Fatalf("detach: %v", err)
			}
			if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != want {
				t.Errorf("front = %q after the file that stated it left, want %s kept", got, want)
			}
		})
	}
}

// TestAStatedCoverIsKeptForItsFile: a copy's picture is mapped to nothing while the
// primary states another, and it is not garbage. The art collector keeps it and the
// derived-data check counts it live for as long as the file states it.
func TestAStatedCoverIsKeptForItsFile(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	own, copys := testPNG(t, 40, 40), testPNG(t, 41, 41)
	mustScan(t, st, put("song.flac", "flac", 900000, own))
	mp3 := mustScan(t, st, put("song.mp3", "mp3", 320000, copys))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_map WHERE source_hash = ?", copys.Hash); n != 0 {
		t.Fatalf("%d rows map the copy's cover, want none", n)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || rep.OrphanArtSources != 0 {
		t.Fatalf("verify = %+v (err %v), want the copy's cover counted live", rep, err)
	}
	if sources, _, err := st.GCArt(ctx); err != nil || sources != 0 {
		t.Fatalf("GCArt collected %d sources (err %v), want none", sources, err)
	}
	if _, err := st.DetachFile(ctx, mp3.FilePID, model.DeletePrune); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if sources, _, err := st.GCArt(ctx); err != nil || sources != 1 {
		t.Errorf("GCArt collected %d sources (err %v) once the copy is gone, want its cover", sources, err)
	}
}

// TestAFolderCoverSettlesAFileWhoseCoverIsUnknown follows PutFolderCover. A file whose
// read said nothing has no picture in its tags, so the cover beside it is the one it
// states and the item's, and a finding of none beside it is what lets a cover no other
// file states go. A file whose cover the catalog knows is left as it is.
func TestAFolderCoverSettlesAFileWhoseCoverIsUnknown(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	beside := testPNG(t, 40, 40)
	beside.Source = model.SourceSidecar
	in := put("song.mp3", "mp3", 320000, nil)
	in.NoCover = false
	mp3 := mustScan(t, st, in)
	folder := func(c model.FileCover) bool {
		t.Helper()
		changed, err := st.PutFolderCover(ctx, model.FolderCoverInput{FilePID: mp3.FilePID, Cover: c})
		if err != nil {
			t.Fatalf("PutFolderCover: %v", err)
		}
		return changed
	}
	if folder(model.FileCover{}) {
		t.Error("a statement that says nothing changed a cover")
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !folder(model.FileCover{Art: beside}) {
		t.Fatal("the cover beside the file changed nothing")
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != beside.Hash {
		t.Fatalf("front = %q, want the cover beside the file", got)
	}
	if !named(t, st, seq, "item", mp3.ItemPID) {
		t.Error("the item whose cover arrived was not named")
	}
	// The catalog knows the file's cover now, so another look beside it is not taken.
	if folder(model.FileCover{None: true}) {
		t.Error("a file whose cover is known took a folder finding")
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != beside.Hash {
		t.Errorf("front = %q, want it kept", got)
	}
	if changed, err := st.PutFolderCover(ctx, model.FolderCoverInput{FilePID: "NOSUCHFILE", Cover: model.FileCover{None: true}}); err != nil || changed {
		t.Errorf("a file the catalog does not hold = %v (err %v), want it left alone", changed, err)
	}

	// The unknown file is what kept another file's finding of none from taking the cover
	// back. Its own finding of none settles that.
	if _, err := st.wdb().ExecContext(ctx, "UPDATE file SET front_hash = NULL, front_source = NULL"); err != nil {
		t.Fatal(err)
	}
	mustScan(t, st, put("song.flac", "flac", 900000, nil))
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != beside.Hash {
		t.Fatalf("front = %q with the mp3's cover unknown, want it kept", got)
	}
	if !folder(model.FileCover{None: true}) {
		t.Error("the finding of none beside the unknown file changed nothing")
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != "" {
		t.Errorf("front = %s once every file states none, want none", got)
	}
}

// TestAReadThatLeftTheTagsUnreadSpeaksForTheFolders: a file the reader has no parser for
// states what is beside it and nothing of its tags. A cover it took from its folder goes
// when the folder has none, and one its tags stated at an earlier read stands, whatever
// the folder holds.
func TestAReadThatLeftTheTagsUnreadSpeaksForTheFolders(t *testing.T) {
	t.Parallel()
	for _, source := range []model.ProvenanceSource{model.SourceTag, model.SourceSidecar} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			st, put := coverFiles(t)
			cover, beside := testPNG(t, 40, 40), testPNG(t, 41, 41)
			cover.Source, beside.Source = source, model.SourceSidecar
			res := mustScan(t, st, put("song.mp3", "mp3", 320000, cover))
			unread := func(c *model.ArtImage) {
				t.Helper()
				in := put("song.mp3", "mp3", 320000, c)
				in.TagsUnread = true
				mustScan(t, st, in)
			}
			if source == model.SourceTag {
				unread(beside)
				if got := frontHash(t, st, model.ArtTrack, res.ItemPID); got != cover.Hash {
					t.Errorf("front = %q after a folder cover arrived beside the unread file, want its tags' cover kept", got)
				}
			}
			unread(nil)
			want := ""
			if source == model.SourceTag {
				want = cover.Hash
			}
			if got := frontHash(t, st, model.ArtTrack, res.ItemPID); got != want {
				t.Errorf("front = %q after an unread file with nothing beside it, want %q", got, want)
			}
		})
	}
}

// TestACoverWrittenIntoAFileBecomesItsItems: an album's cover fanned into its members'
// files replaces the picture each file embedded, so each member's own cover, which was
// its file's, follows the write with no scan to read it back.
func TestACoverWrittenIntoAFileBecomesItsItems(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	old, written := testPNG(t, 40, 40), testPNG(t, 41, 41)
	in := put("song.mp3", "mp3", 320000, old)
	mp3 := mustScan(t, st, in)
	album := albumPID(t, st)
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID: mp3.FilePID, ExpectedSize: in.File.Size, ExpectedMTimeNS: in.File.MTimeNS,
		NewSize: 9, NewMTimeNS: 9, NewContentHash: "written", Cover: &model.FileCover{Art: written},
	}); err != nil || !ok {
		t.Fatalf("file state update = %v (err %v)", ok, err)
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != written.Hash {
		t.Errorf("front = %q, want the picture written into the file", got)
	}
	if !named(t, st, seq, "item", mp3.ItemPID) || !named(t, st, seq, "album", album) {
		t.Error("the change log does not name the item and the album whose cover moved")
	}
	// A cover a user chose for the item is not the file's to replace.
	chosen := testPNG(t, 42, 42)
	if _, err := st.SetItemArt(ctx, mp3.ItemPID, model.ArtRoleFront, chosen.Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID: mp3.FilePID, ExpectedSize: 9, ExpectedMTimeNS: 9,
		NewSize: 10, NewMTimeNS: 10, NewContentHash: "again", Cover: &model.FileCover{Art: old},
	}); err != nil || !ok {
		t.Fatalf("file state update = %v (err %v)", ok, err)
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != chosen.Hash {
		t.Errorf("front = %q, want the locked cover kept", got)
	}
}

// TestADiskCoverIsTheFirstStatedOne: a cover the disk gave is the picture of the first
// file that states one, and not merely one some file states. A lock held the primary's
// old picture while the primary came to state another, and a copy states the old one
// still. With the lock gone, the next read of any of the item's files puts the primary's
// picture back.
func TestADiskCoverIsTheFirstStatedOne(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	old, now := testPNG(t, 40, 40), testPNG(t, 41, 41)
	flac := mustScan(t, st, put("song.flac", "flac", 900000, old))
	mustScan(t, st, put("song.mp3", "mp3", 320000, old))
	if _, err := st.SetArtLock(ctx, model.ArtTrack, flac.ItemPID, model.ArtRoleFront, true); err != nil {
		t.Fatal(err)
	}
	mustScan(t, st, put("song.flac", "flac", 900000, now))
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != old.Hash {
		t.Fatalf("front = %q under the lock, want the old cover held", got)
	}
	if _, err := st.SetArtLock(ctx, model.ArtTrack, flac.ItemPID, model.ArtRoleFront, false); err != nil {
		t.Fatal(err)
	}
	if res := mustScan(t, st, put("song.mp3", "mp3", 320000, old)); !res.SidecarsChanged {
		t.Error("the copy's read that settled the cover reports no change")
	}
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != now.Hash {
		t.Errorf("front = %q, want the picture the primary states", got)
	}
}

// TestAUsersCoverWrittenBackStaysTheirs: a cover a user chose and left unlocked is
// written into the first of the item's two files. That file states the picture then,
// and the front holds it already, so the front keeps the user's name and the file the
// write has not reached goes on owing the cover, through the write and through a read of
// either file.
func TestAUsersCoverWrittenBackStaysTheirs(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	old, chosen := testPNG(t, 40, 40), testPNG(t, 41, 41)
	flacIn := put("song.flac", "flac", 900000, old)
	flac := mustScan(t, st, flacIn)
	mp3 := mustScan(t, st, put("song.mp3", "mp3", 320000, old))
	if _, err := st.SetItemArt(ctx, flac.ItemPID, model.ArtRoleFront, chosen.Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("SetItemArt: %v", err)
	}
	owed := func(file model.PID) int {
		t.Helper()
		return scalarInt(t, st, `SELECT COUNT(*) FROM file_diagnostic d JOIN file f ON f.id = d.file_id
			WHERE d.code = ? AND d.tag_key = 'art' AND f.pid = ?`, string(model.DiagTagWriteOwed), string(file))
	}
	if owed(flac.FilePID) != 1 || owed(mp3.FilePID) != 1 {
		t.Fatal("the set cover is not owed to both files")
	}
	check := func(when string) {
		t.Helper()
		if got := scalarStr(t, st, "SELECT source FROM art_map WHERE entity_type = 'track' AND role = 'front'"); got != string(model.SourceUser) {
			t.Errorf("%s: the cover's source = %q, want it the user's still", when, got)
		}
		if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != chosen.Hash {
			t.Errorf("%s: front = %q, want the chosen cover", when, got)
		}
		if n := owed(mp3.FilePID); n != 1 {
			t.Errorf("%s: the file the write did not reach owes the cover %d times, want once", when, n)
		}
	}
	// The write lands in the flac alone, as the write-back records it.
	if ok, err := st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID: flac.FilePID, ExpectedSize: flacIn.File.Size, ExpectedMTimeNS: flacIn.File.MTimeNS,
		NewSize: 9, NewMTimeNS: 9, NewContentHash: "written", Cover: &model.FileCover{Art: chosen},
	}); err != nil || !ok {
		t.Fatalf("file state update = %v (err %v)", ok, err)
	}
	if err := st.SettleTagWriteOwed(ctx, flac.FilePID, []string{model.OwedArt}); err != nil {
		t.Fatal(err)
	}
	check("after the write")
	mustScan(t, st, put("song.flac", "flac", 900000, chosen))
	check("after the written file was read")
	mustScan(t, st, put("song.mp3", "mp3", 320000, old))
	check("after the other file was read")
}

// TestAFileThatFollowsAnotherBringsItsCover: a kind change sends a copy it cannot read to
// the item the file it copies went to. The copy joins with no read, so the cover its row
// last stated is what it brings, and the item it joins draws it when its own file states
// none.
func TestAFileThatFollowsAnotherBringsItsCover(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	cover := testPNG(t, 40, 40)
	flac := mustScan(t, st, put("song.flac", "flac", 900000, nil))
	apart := put("song.mp3", "mp3", 320000, cover)
	apart.Item.IdentityKey, apart.Item.Title, apart.Track.Album = "mbid:rec-2", "Other Song", "Other Album"
	mp3 := mustScan(t, st, apart)
	if mp3.ItemPID == flac.ItemPID || frontHash(t, st, model.ArtTrack, flac.ItemPID) != "" {
		t.Fatal("the two files do not start as two items, the flac's with no cover")
	}
	album := model.PID(scalarStr(t, st, `SELECT al.pid FROM album al JOIN track t ON t.album_id = al.id
		JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(flac.ItemPID)))
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	into, _, err := st.FollowFile(ctx, mp3.FilePID, flac.FilePID, "")
	if err != nil || into != flac.ItemPID {
		t.Fatalf("FollowFile = %s (err %v), want the mp3 on the flac's item", into, err)
	}
	if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != cover.Hash {
		t.Errorf("front = %q after the copy joined, want the cover it states", got)
	}
	if !named(t, st, seq, "album", album) {
		t.Error("the album that borrows the cover was not named")
	}
}

// TestAWriteNeverReplacesACoverTheDiskDidNotGive: a user's cover is in a format no
// decoder here reads, so once it is written into the file a read of the file would take
// the folder's cover ahead of it, and the write-back says so. That is news of the file
// and not a reason to take the cover the user just set away: only a read of the file
// replaces an unlocked cover, as it always has.
func TestAWriteNeverReplacesACoverTheDiskDidNotGive(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	old, chosen, beside := testPNG(t, 40, 40), testPNG(t, 41, 41), testPNG(t, 42, 42)
	beside.Source = model.SourceSidecar
	in := put("song.mp3", "mp3", 320000, old)
	mp3 := mustScan(t, st, in)
	if _, err := st.SetItemArt(ctx, mp3.ItemPID, model.ArtRoleFront, chosen.Data, "",
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("SetItemArt: %v", err)
	}
	if ok, err := st.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID: mp3.FilePID, ExpectedSize: in.File.Size, ExpectedMTimeNS: in.File.MTimeNS,
		NewSize: 9, NewMTimeNS: 9, NewContentHash: "written", Cover: &model.FileCover{Art: beside},
	}); err != nil || !ok {
		t.Fatalf("file state update = %v (err %v)", ok, err)
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != chosen.Hash {
		t.Fatalf("front = %q after the write-back, want the cover the user set", got)
	}
	if got := scalarStr(t, st, "SELECT front_hash FROM file"); got != beside.Hash {
		t.Errorf("the file states %q, want the folder's cover a read would take", got)
	}
	mustScan(t, st, put("song.mp3", "mp3", 320000, beside))
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != beside.Hash {
		t.Errorf("front = %q after a read of the file, want the cover it states", got)
	}
}

// TestAFileWhoseCoverIsUnknownSpeaksFirst: the primary's cover is unknown, as an import
// leaves it until its covers land, and a copy states a picture. The primary would speak
// first, so the copy's picture is not taken up while the catalog cannot tell, and the
// item's cover settles once, when the primary's is known: to the folder's cover, or to
// the copy's when there is none beside the primary.
func TestAFileWhoseCoverIsUnknownSpeaksFirst(t *testing.T) {
	t.Parallel()
	for _, folder := range []bool{true, false} {
		st, put := coverFiles(t)
		ctx := context.Background()
		copys, beside := testPNG(t, 40, 40), testPNG(t, 41, 41)
		beside.Source = model.SourceSidecar
		pending := put("song.flac", "flac", 900000, nil)
		pending.NoCover = false
		flac := mustScan(t, st, pending)
		seq, err := st.LatestChangeSeq(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res := mustScan(t, st, put("song.mp3", "mp3", 320000, copys)); res.SidecarsChanged {
			t.Errorf("folder %v: the copy's read changed a cover behind a primary whose cover is unknown", folder)
		}
		if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != "" {
			t.Fatalf("folder %v: front = %q while the primary's cover is unknown, want none yet", folder, got)
		}
		in := model.FolderCoverInput{FilePID: flac.FilePID, Cover: model.FileCover{None: true}, PreserveLocks: true}
		want := copys.Hash
		if folder {
			in.Cover, want = model.FileCover{Art: beside}, beside.Hash
		}
		if changed, err := st.PutFolderCover(ctx, in); err != nil || !changed {
			t.Fatalf("folder %v: PutFolderCover = %v (err %v), want the cover settled", folder, changed, err)
		}
		if got := frontHash(t, st, model.ArtTrack, flac.ItemPID); got != want {
			t.Errorf("folder %v: front = %q, want %s", folder, got, want)
		}
		changes, err := st.ChangesSince(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		albums := 0
		for _, c := range changes {
			if c.EntityType == "album" {
				albums++
			}
		}
		if albums != 1 {
			t.Errorf("folder %v: the album was named %d times, want once, for the one cover it came to draw", folder, albums)
		}
	}
}

// TestAFolderCoverAnswersToTheScansLocks: a look beside a file honours a cover's lock or
// not as the scan that made it was told, as a read of the file does.
func TestAFolderCoverAnswersToTheScansLocks(t *testing.T) {
	t.Parallel()
	for _, preserve := range []bool{true, false} {
		st, put := coverFiles(t)
		ctx := context.Background()
		beside := testPNG(t, 40, 40)
		beside.Source = model.SourceSidecar
		in := put("song.mp3", "mp3", 320000, nil)
		in.NoCover = false
		mp3 := mustScan(t, st, in)
		if _, err := st.SetArtLock(ctx, model.ArtTrack, mp3.ItemPID, model.ArtRoleFront, true); err != nil {
			t.Fatal(err)
		}
		changed, err := st.PutFolderCover(ctx, model.FolderCoverInput{FilePID: mp3.FilePID, Cover: model.FileCover{Art: beside}, PreserveLocks: preserve})
		if err != nil || changed == preserve {
			t.Errorf("preserve locks %v: PutFolderCover changed the cover = %v (err %v)", preserve, changed, err)
		}
		if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); (got == "") != preserve {
			t.Errorf("preserve locks %v: front = %q", preserve, got)
		}
	}
}

// TestAFolderCoverForAKnownFileCostsNoWrite: an import sends every file it placed, most
// of whose covers the catalog knows already. Such a file is turned away before its
// picture is examined, which for an oversized one is a decode.
func TestAFolderCoverForAKnownFileCostsNoWrite(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	mp3 := mustScan(t, st, put("song.mp3", "mp3", 320000, testPNG(t, 40, 40)))
	raw := widePNG(t, art.MaxSourceDim+200, 300)
	big := &model.ArtImage{Data: raw, Attribution: model.Attribution{Source: model.SourceSidecar}}
	if changed, err := st.PutFolderCover(ctx, model.FolderCoverInput{FilePID: mp3.FilePID, Cover: model.FileCover{Art: big}}); err != nil || changed {
		t.Fatalf("PutFolderCover on a known file = %v (err %v), want it left alone", changed, err)
	}
	if _, ok := st.examined.get(art.Hash(raw), model.Attribution{}); ok {
		t.Error("the picture was examined for a file whose cover is known")
	}
}

// TestVerifyCountsAndSettlesACoverAdrift: a write that takes a file from its item and
// settles no cover leaves the item drawing a picture none of its files states. The
// derived-data check counts it, the audit reads the same count, and the repair settles
// it by the rule a file's leaving applies.
func TestVerifyCountsAndSettlesACoverAdrift(t *testing.T) {
	t.Parallel()
	st, put := coverFiles(t)
	ctx := context.Background()
	cover := testPNG(t, 40, 40)
	mp3 := mustScan(t, st, put("song.mp3", "mp3", 320000, cover))
	mustScan(t, st, put("song.flac", "flac", 900000, nil))
	if rep, err := st.VerifyDerived(ctx); err != nil || rep.CoverDrift != 0 || !rep.Consistent() {
		t.Fatalf("verify = %+v (err %v), want no cover adrift", rep, err)
	}
	// The copy's edge goes with no settle, as a path that forgot the call would leave it.
	if _, err := st.wdb().ExecContext(ctx, "DELETE FROM item_file WHERE role = 'alternate'"); err != nil {
		t.Fatal(err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil || rep.CoverDrift != 1 || rep.Consistent() {
		t.Fatalf("verify = %+v (err %v), want the one cover adrift and the catalog inconsistent", rep, err)
	}
	if drift, err := st.DerivedDrift(ctx); err != nil || drift.CoverDrift != 1 || drift.Consistent() {
		t.Fatalf("audit drift = %+v (err %v), want the one cover", drift, err)
	}
	// A lock holds the cover as it is, and the check does not count what the rule leaves.
	if _, err := st.SetArtLock(ctx, model.ArtTrack, mp3.ItemPID, model.ArtRoleFront, true); err != nil {
		t.Fatal(err)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || rep.CoverDrift != 0 {
		t.Fatalf("verify = %+v (err %v), want a locked cover left out", rep, err)
	}
	if _, err := st.SetArtLock(ctx, model.ArtTrack, mp3.ItemPID, model.ArtRoleFront, false); err != nil {
		t.Fatal(err)
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := st.RefreshRollups(ctx)
	if err != nil || fixed.Covers != 1 {
		t.Fatalf("refresh = %+v (err %v), want the one cover settled", fixed, err)
	}
	if got := frontHash(t, st, model.ArtTrack, mp3.ItemPID); got != "" {
		t.Errorf("front = %s after the repair, want none", got)
	}
	if !named(t, st, seq, "item", mp3.ItemPID) || !named(t, st, seq, "album", albumPID(t, st)) {
		t.Error("the repair did not name the item and the album whose cover went")
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || rep.CoverDrift != 0 || !rep.Consistent() {
		t.Errorf("verify after the repair = %+v (err %v), want it clean", rep, err)
	}
	if again, err := st.RefreshRollups(ctx); err != nil || again.Covers != 0 {
		t.Errorf("a second refresh = %+v (err %v), want nothing left to settle", again, err)
	}
}
