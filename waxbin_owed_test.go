package waxbin_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
)

// owedOn lists the owed keys on an item's files, sorted and without repeats.
func owedOn(t *testing.T, ctx context.Context, lib *waxbin.Library, pid model.PID) []string {
	t.Helper()
	diags, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{ItemPID: pid, Code: model.DiagTagWriteOwed})
	if err != nil {
		t.Fatalf("owed rows: %v", err)
	}
	var out []string
	for _, d := range diags {
		if !slices.Contains(out, d.TagKey) {
			out = append(out, d.TagKey)
		}
	}
	slices.Sort(out)
	return out
}

// solidPNG is a 4x4 PNG of one color, so two covers differ by their bytes.
func solidPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

// twoMemberAlbum scans two tracks of one album under root and returns the library and
// the two item pids, the first being "One".
func twoMemberAlbum(t *testing.T, ctx context.Context, txxx []testaudio.TXXXFrame) (*waxbin.Library, string, model.PID, model.PID) {
	t.Helper()
	root := t.TempDir()
	for i, title := range []string{"One", "Two"} {
		writeFile(t, filepath.Join(root, title+".mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: title, Artist: "Alpha", AlbumArtist: "Alpha", Album: "Both", Track: i + 1,
			Audio: testaudio.AudioWithSeed(byte(i + 1)), TXXX: txxx,
		}))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return lib, root, itemPIDByTitle(t, ctx, lib, "One"), itemPIDByTitle(t, ctx, lib, "Two")
}

// TestCatalogOnlyRenameOwesItsMembers: a rename without a write-back leaves every
// member's file behind on the keying field it rewrote, until a rename that writes back.
func TestCatalogOnlyRenameOwesItsMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, one, two := twoMemberAlbum(t, ctx, nil)
	v, err := lib.Get(ctx, one)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := lib.RenameEntity(ctx, model.MergeAlbum, v.AlbumPID, map[string]string{"album": "New Album"}, waxbin.RenameOptions{}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	for _, pid := range []model.PID{one, two} {
		if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"album"}) {
			t.Fatalf("owed on %s = %v, want [album]", pid, got)
		}
	}
	if _, err := lib.RenameEntity(ctx, model.MergeAlbum, v.AlbumPID, map[string]string{"album": "Newer Album"},
		waxbin.RenameOptions{WriteBack: true}); err != nil {
		t.Fatalf("rename with write-back: %v", err)
	}
	for _, pid := range []model.PID{one, two} {
		if got := owedOn(t, ctx, lib, pid); len(got) != 0 {
			t.Fatalf("owed on %s after the write-back = %v, want none", pid, got)
		}
	}
}

// TestCatalogOnlyAcquisitionOwesAWrite: an acquisition set without a write-back owes the
// file its tags until a write-back lands them, and a clear the lock does not hold is
// undone by the next full scan reading the tags back, which pays the row too.
func TestCatalogOnlyAcquisitionOwesAWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Song", Artist: "Band", Album: "Album",
		TXXX: []testaudio.TXXXFrame{{Desc: "SOURCE_URL", Value: "https://tagged.test/a"}},
	}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")
	fixed := model.AcquisitionInput{SourceType: model.SourceManual, SourceURL: "https://fixed.test/a"}
	if err := lib.SetAcquisition(ctx, pid, fixed, waxbin.AcquisitionEditOptions{}); err != nil {
		t.Fatalf("set acquisition: %v", err)
	}
	if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"acquisition"}) {
		t.Fatalf("owed = %v, want [acquisition]", got)
	}
	if err := lib.SetAcquisition(ctx, pid, fixed, waxbin.AcquisitionEditOptions{WriteBack: true}); err != nil {
		t.Fatalf("set acquisition with write-back: %v", err)
	}
	if got := owedOn(t, ctx, lib, pid); len(got) != 0 {
		t.Fatalf("owed after the write-back = %v, want none", got)
	}

	if err := lib.ClearAcquisition(ctx, pid, waxbin.AcquisitionEditOptions{Lock: model.LockOff}); err != nil {
		t.Fatalf("clear acquisition: %v", err)
	}
	if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"acquisition"}) {
		t.Fatalf("owed after the clear = %v, want [acquisition]", got)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if got := owedOn(t, ctx, lib, pid); len(got) != 0 {
		t.Fatalf("owed after the scan read the tags back = %v, want none", got)
	}
}

// TestCatalogOnlyDetachOwesAWrite: a detach without a write-back leaves the file naming
// the album it left, so the release ids are owed, and a scan that re-resolves the member
// from those ids puts it back on the album, which pays them.
func TestCatalogOnlyDetachOwesAWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root, one, _ := twoMemberAlbum(t, ctx, []testaudio.TXXXFrame{
		{Desc: "MusicBrainz Album Id", Value: "14141414-1414-1414-1414-141414141414"},
		{Desc: "MusicBrainz Release Group Id", Value: "15151515-1515-1515-1515-151515151515"},
	})
	if _, err := lib.Detach(ctx, one, waxbin.DetachOptions{}); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); !slices.Equal(got, []string{"album.mbid", "release_group.mbid"}) {
		t.Fatalf("owed = %v, want the two release ids", got)
	}
	if _, err := meta.NewWriter().Apply(ctx, filepath.Join(root, "One.mp3"),
		[]meta.TagEdit{{Key: "COMMENT", Values: []string{"touched"}}}); err != nil {
		t.Fatalf("retag: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); len(got) != 0 {
		t.Fatalf("owed after the member re-joined its album = %v, want none", got)
	}
	if _, err := lib.Detach(ctx, one, waxbin.DetachOptions{WriteBack: true}); err != nil {
		t.Fatalf("detach with write-back: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); len(got) != 0 {
		t.Fatalf("owed after a detach that wrote back = %v, want none", got)
	}
}

// TestCatalogOnlyEntityEditOwesMemberFiles: an album field edited without a write-back
// owes every member file the fanned tag, and an edit that writes back pays them all.
func TestCatalogOnlyEntityEditOwesMemberFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, one, two := twoMemberAlbum(t, ctx, nil)
	v, err := lib.Get(ctx, one)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := lib.EditEntity(ctx, model.MergeAlbum, v.AlbumPID, map[string]string{"label": "Harvest"}, waxbin.EntityEditOptions{}); err != nil {
		t.Fatalf("edit label: %v", err)
	}
	for _, pid := range []model.PID{one, two} {
		if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"album.label"}) {
			t.Fatalf("owed on %s = %v, want [album.label]", pid, got)
		}
	}
	if _, err := lib.EditEntity(ctx, model.MergeAlbum, v.AlbumPID, map[string]string{"label": "Harvest"},
		waxbin.EntityEditOptions{WriteBack: true}); err != nil {
		t.Fatalf("edit label with write-back: %v", err)
	}
	for _, pid := range []model.PID{one, two} {
		if got := owedOn(t, ctx, lib, pid); len(got) != 0 {
			t.Fatalf("owed on %s after the write-back = %v, want none", pid, got)
		}
	}
}

// TestCatalogOnlyCoverOwesAWrite: a cover set without a write-back owes the file its
// picture. A scan re-deriving an unlocked cover from the file undoes the set and pays the
// row; a locked one keeps it owed until a write-back embeds it. An album cover owes every
// member file the same way.
func TestCatalogOnlyCoverOwesAWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root, one, two := twoMemberAlbum(t, ctx, nil)
	red, blue := solidPNG(t, color.RGBA{R: 255, A: 255}), solidPNG(t, color.RGBA{B: 255, A: 255})
	if _, err := meta.NewWriter().ApplyPicture(ctx, filepath.Join(root, "One.mp3"), meta.PictureEdit{Data: red}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if err := lib.SetItemArt(ctx, one, "", blue, waxbin.ArtEditOptions{}); err != nil {
		t.Fatalf("set cover: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); !slices.Equal(got, []string{"art"}) {
		t.Fatalf("owed = %v, want [art]", got)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); len(got) != 0 {
		t.Fatalf("owed after the scan re-derived the cover = %v, want none", got)
	}

	if err := lib.SetItemArt(ctx, one, "", blue, waxbin.ArtEditOptions{Lock: model.LockOn}); err != nil {
		t.Fatalf("set locked cover: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); !slices.Equal(got, []string{"art"}) {
		t.Fatalf("owed under a lock = %v, want [art] kept while the file shows another cover", got)
	}
	// Another tool embeds the locked cover, so the file agrees with the catalog.
	if _, err := meta.NewWriter().ApplyPicture(ctx, filepath.Join(root, "One.mp3"), meta.PictureEdit{Data: blue}); err != nil {
		t.Fatalf("embed the catalog's cover: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); len(got) != 0 {
		t.Fatalf("owed once the file carries the cover = %v, want none", got)
	}
	if err := lib.SetItemArt(ctx, one, "", red, waxbin.ArtEditOptions{Lock: model.LockOn, Force: true}); err != nil {
		t.Fatalf("set another locked cover: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); !slices.Equal(got, []string{"art"}) {
		t.Fatalf("owed = %v, want [art]", got)
	}
	if err := lib.SetItemArt(ctx, one, "", red, waxbin.ArtEditOptions{Lock: model.LockOn, Force: true, WriteBack: true}); err != nil {
		t.Fatalf("set cover with write-back: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); len(got) != 0 {
		t.Fatalf("owed after the write-back = %v, want none", got)
	}

	// Two embeds nothing, so a scan takes its cover from cover.png beside it: the set is
	// undone all the same, though the file holds no picture to compare.
	writeFile(t, filepath.Join(root, "cover.png"), red)
	if err := lib.SetItemArt(ctx, two, "", blue, waxbin.ArtEditOptions{}); err != nil {
		t.Fatalf("set cover on two: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if got := owedOn(t, ctx, lib, two); len(got) != 0 {
		t.Fatalf("owed on two after the scan took the folder cover = %v, want none", got)
	}

	v, err := lib.Get(ctx, two)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := lib.SetEntityArt(ctx, model.ArtAlbum, v.AlbumPID, "", red, waxbin.ArtEditOptions{}); err != nil {
		t.Fatalf("set album cover: %v", err)
	}
	for _, pid := range []model.PID{one, two} {
		if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"album.art"}) {
			t.Fatalf("owed on %s = %v, want [album.art]", pid, got)
		}
	}
	if err := lib.SetEntityArt(ctx, model.ArtAlbum, v.AlbumPID, "", red, waxbin.ArtEditOptions{WriteBack: true}); err != nil {
		t.Fatalf("set album cover with write-back: %v", err)
	}
	for _, pid := range []model.PID{one, two} {
		if got := owedOn(t, ctx, lib, pid); len(got) != 0 {
			t.Fatalf("owed on %s after the write-back = %v, want none", pid, got)
		}
	}
}

// TestCatalogOnlyAlbumIDClearOwesTheStrip: clearing an album's MusicBrainz id without a
// write-back leaves the id on every member file, so its strip is owed, and a scan that
// re-resolves a member from that id puts it back on an identified album, which pays it.
func TestCatalogOnlyAlbumIDClearOwesTheStrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root, one, two := twoMemberAlbum(t, ctx, []testaudio.TXXXFrame{
		{Desc: "MusicBrainz Album Id", Value: "14141414-1414-1414-1414-141414141414"},
	})
	v, err := lib.Get(ctx, one)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := lib.EditEntity(ctx, model.MergeAlbum, v.AlbumPID, map[string]string{"mbid": ""}, waxbin.EntityEditOptions{}); err != nil {
		t.Fatalf("clear album id: %v", err)
	}
	for _, pid := range []model.PID{one, two} {
		if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"album.mbid"}) {
			t.Fatalf("owed on %s = %v, want [album.mbid]", pid, got)
		}
	}
	if _, err := meta.NewWriter().Apply(ctx, filepath.Join(root, "One.mp3"),
		[]meta.TagEdit{{Key: "COMMENT", Values: []string{"touched"}}}); err != nil {
		t.Fatalf("retag: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); len(got) != 0 {
		t.Fatalf("owed on the re-resolved member = %v, want none", got)
	}
	if got := owedOn(t, ctx, lib, two); !slices.Equal(got, []string{"album.mbid"}) {
		t.Fatalf("owed on the untouched member = %v, want [album.mbid] still", got)
	}
}

// TestOwedLockedValueSettlesOnlyOnATag: a locked catalog value that only a fallback
// reproduces (here an album taken from ALBUMSORT) is still owed after a forced rescan,
// since the file's ALBUM tag is empty. An unlocked edit the rescan replaces with the
// fallback value is gone, so its row goes with it.
func TestOwedLockedValueSettlesOnlyOnATag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	p := filepath.Join(root, "song.mp3")
	writeFile(t, p, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Song", Artist: "Band", Audio: testaudio.AudioWithSeed(4)}))
	if _, err := meta.NewWriter().Apply(ctx, p, []meta.TagEdit{{Key: "ALBUMSORT", Values: []string{"Tome"}}}); err != nil {
		t.Fatalf("stage sort tag: %v", err)
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")
	edit := func(value string, lock model.LockChange) {
		t.Helper()
		if err := lib.EditField(ctx, pid, "album", value, waxbin.EditOptions{Lock: lock, Force: true}); err != nil {
			t.Fatalf("edit album %q: %v", value, err)
		}
	}
	rescan := func() {
		t.Helper()
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
			t.Fatalf("forced scan: %v", err)
		}
	}

	edit("Other", model.LockOn)
	edit("Tome", model.LockOn)
	rescan()
	if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"album"}) {
		t.Fatalf("owed after a rescan that only a sort tag agrees with = %v, want [album]", got)
	}

	edit("Other", model.LockOff)
	rescan()
	if got := owedOn(t, ctx, lib, pid); len(got) != 0 {
		t.Fatalf("owed after a rescan replaced the unlocked edit = %v, want none", got)
	}
}

// TestUnchangedEditsOweNothing: an edit owes a file only a value it changed, so a form that
// saves every field, a credit set to the names already credited, and an entity value, an
// origin or a cover set to what the catalog holds owe nothing. A write-back that lands
// leaves nothing owed, and one a read-only library refuses leaves its value owed.
func TestUnchangedEditsOweNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, one, _ := twoMemberAlbum(t, ctx, nil)
	v, err := lib.Get(ctx, one)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	owes := func(step, key string, want bool) {
		t.Helper()
		if got := owedOn(t, ctx, lib, one); slices.Contains(got, key) != want {
			t.Errorf("%s: owed = %v, want %s owed %v", step, got, key, want)
		}
	}
	check := func(step string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	check("same fields", lib.EditFields(ctx, one, map[string]string{"title": "One", "album": "Both", "track_no": "1"}, waxbin.EditOptions{}))
	for _, f := range []string{"title", "album", "track_no"} {
		owes("same fields", f, false)
	}
	_, _, err = lib.SetCredits(ctx, one, model.RoleArtist, []string{"Alpha"}, waxbin.CreditEditOptions{})
	check("same credit", err)
	owes("same credit", "credit.artist", false)
	_, _, err = lib.SetCredits(ctx, one, model.RoleProducer, []string{"Pat"}, waxbin.CreditEditOptions{WriteBack: true})
	check("producer with write-back", err)
	owes("producer with write-back", "credit.producer", false)

	_, err = lib.EditEntity(ctx, model.MergeAlbum, v.AlbumPID, map[string]string{"label": "Harvest"}, waxbin.EntityEditOptions{WriteBack: true})
	check("label with write-back", err)
	owes("label with write-back", "album.label", false)
	_, err = lib.EditEntity(ctx, model.MergeAlbum, v.AlbumPID, map[string]string{"label": "Harvest"}, waxbin.EntityEditOptions{})
	check("same label", err)
	owes("same label", "album.label", false)

	origin := model.AcquisitionInput{SourceType: model.SourceManual, SourceURL: "https://fixed.test/one", AcquiredAt: 1_700_000_000_000_000_000}
	check("origin with write-back", lib.SetAcquisition(ctx, one, origin, waxbin.AcquisitionEditOptions{WriteBack: true}))
	owes("origin with write-back", "acquisition", false)
	check("same origin", lib.SetAcquisition(ctx, one, origin, waxbin.AcquisitionEditOptions{}))
	owes("same origin", "acquisition", false)

	red := solidPNG(t, color.RGBA{R: 255, A: 255})
	check("cover with write-back", lib.SetItemArt(ctx, one, "", red, waxbin.ArtEditOptions{WriteBack: true}))
	owes("cover with write-back", "art", false)
	check("same cover", lib.SetItemArt(ctx, one, "", red, waxbin.ArtEditOptions{}))
	owes("same cover", "art", false)

	check("genre with write-back", lib.EditField(ctx, one, "genre", "Rock", waxbin.EditOptions{WriteBack: true}))
	owes("genre with write-back", "genre", false)

	libs, err := lib.Libraries(ctx)
	check("libraries", err)
	_, err = lib.SetLibraryReadOnly(ctx, libs[0].PID, true)
	check("read-only", err)
	var wbErr *waxbin.WriteBackError
	if err := lib.EditField(ctx, one, "genre", "Jazz", waxbin.EditOptions{WriteBack: true}); !errors.As(err, &wbErr) {
		t.Fatalf("write-back on a read-only library = %v, want a WriteBackError", err)
	}
	owes("refused write-back", "genre", true)
}

// TestWriteBackPaysBothSpellingsOfAColumn: the scalar and the credit spelling of a column
// are one set of tags on disk (ARTIST, COMPOSER), so a write-back that lands one of them
// pays an owed row about the other, the way a scan that reads the file back does.
func TestWriteBackPaysBothSpellingsOfAColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	track := func(t *testing.T) (*waxbin.Library, model.PID) {
		t.Helper()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Album"}))
		lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
			t.Fatalf("scan: %v", err)
		}
		return lib, itemPIDByTitle(t, ctx, lib, "Song")
	}

	t.Run("a scalar write pays a credit row", func(t *testing.T) {
		lib, pid := track(t)
		if _, _, err := lib.SetCredits(ctx, pid, model.RoleArtist, []string{"Xavier", "Yolanda"}, waxbin.CreditEditOptions{}); err != nil {
			t.Fatalf("set credit: %v", err)
		}
		if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"credit.artist"}) {
			t.Fatalf("owed after the credit = %v, want [credit.artist]", got)
		}
		if err := lib.EditFields(ctx, pid, map[string]string{"artist": "Scalar Artist"}, waxbin.EditOptions{WriteBack: true}); err != nil {
			t.Fatalf("scalar write-back: %v", err)
		}
		if got := owedOn(t, ctx, lib, pid); len(got) != 0 {
			t.Errorf("owed after the scalar write-back = %v, want none", got)
		}
	})

	t.Run("a credit write pays a scalar row", func(t *testing.T) {
		lib, pid := track(t)
		if err := lib.EditFields(ctx, pid, map[string]string{"composer": "Composer One"}, waxbin.EditOptions{}); err != nil {
			t.Fatalf("set composer: %v", err)
		}
		if got := owedOn(t, ctx, lib, pid); !slices.Equal(got, []string{"composer"}) {
			t.Fatalf("owed after the composer = %v, want [composer]", got)
		}
		if _, _, err := lib.SetCredits(ctx, pid, model.RoleComposer, []string{"Composer Two", "Composer Three"},
			waxbin.CreditEditOptions{WriteBack: true}); err != nil {
			t.Fatalf("credit write-back: %v", err)
		}
		if got := owedOn(t, ctx, lib, pid); len(got) != 0 {
			t.Errorf("owed after the credit write-back = %v, want none", got)
		}
	})
}

// bigCoverPNG is a textured picture over art.MaxSourceDim, distinct per seed.
func bigCoverPNG(t *testing.T, seed uint32) []byte {
	t.Helper()
	w, h := art.MaxSourceDim+200, (art.MaxSourceDim+200)/2
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8(x*255/w) + uint8(seed&7)
			img.Pix[i+1] = uint8(y*255/h) + uint8((seed>>8)&7)
			img.Pix[i+2] = 64 + uint8((seed>>16)&7)
			img.Pix[i+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

// TestArtWriteBackEmbedsTheBoundedCover: the write-back embeds the cover the catalog
// holds, which for an oversized picture is the scaled copy, so the file and the catalog
// agree byte for byte and the owed write is paid.
func TestArtWriteBackEmbedsTheBoundedCover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root, one, _ := twoMemberAlbum(t, ctx, nil)
	if err := lib.SetItemArt(ctx, one, "", bigCoverPNG(t, 1), waxbin.ArtEditOptions{WriteBack: true}); err != nil {
		t.Fatalf("set with write-back: %v", err)
	}
	blob, err := lib.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: one}, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, w, _, err := art.Probe(blob.Bytes); err != nil || w != art.MaxSourceDim {
		t.Fatalf("the catalog holds a cover %d wide (err %v), want %d", w, err, art.MaxSourceDim)
	}
	fm, err := meta.NewReader().Read(ctx, filepath.Join(root, "One.mp3"))
	if err != nil {
		t.Fatalf("re-read the file: %v", err)
	}
	if fm.CoverArt == nil || !bytes.Equal(fm.CoverArt.Data, blob.Bytes) {
		t.Errorf("the file embeds %d bytes, want the catalog's %d-byte cover", len(fm.CoverArt.Data), len(blob.Bytes))
	}
	if got := owedOn(t, ctx, lib, one); len(got) != 0 {
		t.Errorf("owed after the write-back = %v, want none", got)
	}
}

// TestOwedArtSettlesWhenTheFileCarriesAnOversizedCover: a locked, catalog-only cover is
// owed until the file carries it. The file may come to carry the very original the
// person chose, which the catalog scaled, or the catalog's own bytes; a scan settles the
// debt either way.
func TestOwedArtSettlesWhenTheFileCarriesAnOversizedCover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root, one, _ := twoMemberAlbum(t, ctx, nil)
	original := bigCoverPNG(t, 7)
	if err := lib.SetItemArt(ctx, one, "", original, waxbin.ArtEditOptions{Lock: model.LockOn}); err != nil {
		t.Fatalf("set a locked cover: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); !slices.Equal(got, []string{"art"}) {
		t.Fatalf("owed after a catalog-only set = %v, want [art]", got)
	}
	// Another tool embeds the original the person chose.
	if _, err := meta.NewWriter().ApplyPicture(ctx, filepath.Join(root, "One.mp3"), meta.PictureEdit{Data: original}); err != nil {
		t.Fatalf("embed the original: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); len(got) != 0 {
		t.Fatalf("owed once the file carries the original = %v, want none", got)
	}

	// Another locked cover, and this time the file gets the catalog's own bytes.
	if err := lib.SetItemArt(ctx, one, "", bigCoverPNG(t, 9), waxbin.ArtEditOptions{Lock: model.LockOn, Force: true}); err != nil {
		t.Fatalf("set another locked cover: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); !slices.Equal(got, []string{"art"}) {
		t.Fatalf("owed after the second set = %v, want [art]", got)
	}
	blob, err := lib.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: one}, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := meta.NewWriter().ApplyPicture(ctx, filepath.Join(root, "One.mp3"), meta.PictureEdit{Data: blob.Bytes}); err != nil {
		t.Fatalf("embed the catalog's bytes: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := owedOn(t, ctx, lib, one); len(got) != 0 {
		t.Errorf("owed once the file carries the catalog's bytes = %v, want none", got)
	}
}
