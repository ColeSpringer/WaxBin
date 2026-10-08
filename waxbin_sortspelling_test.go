package waxbin_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
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
