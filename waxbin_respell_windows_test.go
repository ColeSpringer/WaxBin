//go:build windows

package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/query"
)

// TestOrganizeRespellsAnAuthorFolder: on a case-insensitive filesystem, organizing one book
// into its author's folder spelled anew renames the folder, and the catalog's path of a
// book left in it follows, so the next scan reads nothing again.
func TestOrganizeRespellsAnAuthorFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	for i, title := range []string{"Book A", "Book B"} {
		writeFile(t, filepath.Join(root, "tolkien", title+" {Reader}", title+".mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Artist: "Tolkien", Album: title, TXXX: narrated, Audio: testaudio.AudioWithSeed(byte(i + 1))}))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Where("title", query.OpIs, "Book A").Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("PlanOrganize: %v", err)
	}
	if rep, err := lib.ApplyOrganize(ctx, plan); err != nil || rep.Errored != 0 {
		t.Fatalf("ApplyOrganize: %+v, %v", rep, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "Tolkien" {
		t.Fatalf("root holds %v (err %v), want the folder respelled Tolkien", entries, err)
	}
	res, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil || res.Total.Reread != 0 || res.Total.Relinked != 0 || res.Total.Unchanged != 2 {
		t.Errorf("rescan = %+v (err %v), want both books unchanged where the catalog says", res, err)
	}
}

// TestImportRespellsAnAuthorFolder: an import into the folder of an author the library
// spells otherwise gives the folder the import's spelling, and the catalog's paths of the
// books already in it follow.
func TestImportRespellsAnAuthorFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	writeFile(t, filepath.Join(root, "tolkien", "Book A {Reader}", "Book A.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Artist: "Tolkien", Album: "Book A", TXXX: narrated, Audio: testaudio.AudioWithSeed(1)}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	staging := t.TempDir()
	writeFile(t, filepath.Join(staging, "Book B.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Artist: "Tolkien", Album: "Book B", TXXX: narrated, Audio: testaudio.AudioWithSeed(2)}))
	importAll(t, ctx, lib, staging, 1)
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "Tolkien" {
		t.Fatalf("root holds %v (err %v), want the folder respelled Tolkien", entries, err)
	}
	res, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil || res.Total.Reread != 0 || res.Total.Relinked != 0 || res.Total.Unchanged != 2 {
		t.Errorf("rescan = %+v (err %v), want both books unchanged where the catalog says", res, err)
	}
}
