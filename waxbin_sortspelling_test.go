package waxbin_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// TestScannedSortSpellingsKeepTheirCase: the sort spellings a file states reach the
// item view as written, and a list sorted by composer_sort collates by their keys.
func TestScannedSortSpellingsKeepTheirCase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "prelude.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Prelude", Artist: "Glenn Gould", Album: "Bach", Composer: "J. S. Bach", Track: 1,
		TXXX:  []testaudio.TXXXFrame{{Desc: "COMPOSERSORT", Value: "Bach, Johann Sebastian"}},
		Audio: testaudio.AudioWithSeed(1),
	}))
	writeFile(t, filepath.Join(root, "adagio.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Adagio", Artist: "Glenn Gould", Album: "Bach", Composer: "Tomaso Albinoni", Track: 2,
		TXXX:  []testaudio.TXXXFrame{{Desc: "COMPOSERSORT", Value: "Albinoni, Tomaso"}},
		Audio: testaudio.AudioWithSeed(2),
	}))
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	v, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "Prelude"))
	if err != nil {
		t.Fatal(err)
	}
	if v.ComposerSort != "Bach, Johann Sebastian" {
		t.Errorf("ComposerSort = %q, want the file's spelling with its case", v.ComposerSort)
	}
	items, err := lib.Query(ctx, query.New(query.EntityTracks).OrderBy("composer_sort", false).Build(), "")
	if err != nil || len(items) != 2 || items[0].Title != "Adagio" || items[1].Title != "Prelude" {
		t.Errorf("composer_sort order = %v (err %v), want Adagio (albinoni) then Prelude (bach)", titlesOf(items), err)
	}
}

func titlesOf(items []*model.ItemView) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

// TestRebuildKeepsAFannedOutAlbumSort: an album sort an entity edit wrote to the member
// files keys the album in a catalog built from those files, which a curated sort alone
// never survived, and the catalog that wrote it follows the files at once, so clearing
// the curated sort leaves the order the files state.
func TestRebuildKeepsAFannedOutAlbumSort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	for i, title := range []string{"One", "Two"} {
		writeFile(t, filepath.Join(root, "Wall", title+".mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: title, Artist: "Pink Floyd", AlbumArtist: "Pink Floyd", Album: "Wall", Track: i + 1,
			Audio: testaudio.AudioWithSeed(byte(i + 1)),
		}))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	album := albumPIDOf(t, ctx, lib)
	if _, err := lib.EditEntity(ctx, model.MergeAlbum, album, map[string]string{"sort": "Zz Wall"},
		waxbin.EntityEditOptions{WriteBack: true}); err != nil {
		t.Fatalf("edit album sort: %v", err)
	}
	if _, err := lib.EditEntity(ctx, model.MergeAlbum, album, map[string]string{"sort": ""}, waxbin.EntityEditOptions{}); err != nil {
		t.Fatalf("clear the curated sort: %v", err)
	}
	if info, err := lib.EntityByPID(ctx, read.EntityAlbum, album); err != nil || info.SortKey != model.SortKey("Zz Wall") {
		t.Errorf("after the clear, album = %+v (err %v), want the files' spelling's key", info, err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	rebuilt := openManaged(t, ctx, filepath.Join(t.TempDir(), "rebuilt.db"), root)
	if _, err := rebuilt.Scan(ctx, waxbin.ScanRequest{AdoptStampedPIDs: true}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	info, err := rebuilt.EntityByPID(ctx, read.EntityAlbum, albumPIDOf(t, ctx, rebuilt))
	if err != nil || info.SortKey != model.SortKey("Zz Wall") {
		t.Errorf("rebuilt album = %+v (err %v), want it keyed by the fanned spelling", info, err)
	}
}

// albumPIDOf reads the one album a library's items sit on.
func albumPIDOf(t *testing.T, ctx context.Context, lib *waxbin.Library) model.PID {
	t.Helper()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) == 0 || items[0].AlbumPID == "" {
		t.Fatalf("items = %v (err %v), want them on an album", items, err)
	}
	return items[0].AlbumPID
}
