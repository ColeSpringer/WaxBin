//go:build windows

package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
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

// TestOrganizeRespellsInEachManagedLibrary: with a second managed library the plan spans
// two roots, and the author folder is still respelled in the library the book lives in.
func TestOrganizeRespellsInEachManagedLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	music, books := t.TempDir(), t.TempDir()
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	for i, title := range []string{"Book A", "Book B"} {
		writeFile(t, filepath.Join(books, "tolkien", title+" {Reader}", title+".mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Artist: "Tolkien", Album: title, TXXX: narrated, Audio: testaudio.AudioWithSeed(byte(i + 1))}))
	}
	writeFile(t, filepath.Join(music, "Artist", "Album", "01 - Song.mp3"), testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(3)))
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: music, Mode: model.ModeManaged, Media: model.MediaMusic, Profile: "waxbin-native"},
			{Path: books, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"},
		}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Where("title", query.OpIs, "Book A").Build(), waxbin.OrganizeOptions{})
	if err != nil {
		t.Fatalf("PlanOrganize: %v", err)
	}
	if rep, err := lib.ApplyOrganize(ctx, plan); err != nil || rep.Errored != 0 {
		t.Fatalf("ApplyOrganize: %+v, %v", rep, err)
	}
	entries, err := os.ReadDir(books)
	if err != nil || len(entries) != 1 || entries[0].Name() != "Tolkien" {
		t.Fatalf("books root holds %v (err %v), want the folder respelled Tolkien", entries, err)
	}
}
