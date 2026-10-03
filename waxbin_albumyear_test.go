package waxbin_test

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/query"
)

// malibuMP3 is track n of Malibu, tagged with year.
func malibuMP3(n, year int) []byte {
	return testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Track " + strconv.Itoa(n), Artist: "Anderson .Paak", AlbumArtist: "Anderson .Paak",
		Album: "Malibu", Track: n, Year: year, Audio: testaudio.AudioWithSeed(byte(n)),
	})
}

// TestOrganizeFilesAStrayYearWithItsAlbum: organize renders {year} from the album's year,
// so a track tagged a year apart from the rest lands in its album's folder rather than a
// folder of its own that the next scan would key as a second album.
func TestOrganizeFilesAStrayYearWithItsAlbum(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for n, year := range map[int]int{1: 2015, 2: 2015, 3: 2015, 4: 2016} {
		writeFile(t, filepath.Join(root, "incoming", strconv.Itoa(n)+".mp3"), malibuMP3(n, year))
	}
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := filepath.Join(root, "Anderson .Paak", "Malibu (2015)")
	if len(plan.Actions) != 4 {
		t.Fatalf("actions = %d, want 4", len(plan.Actions))
	}
	for _, a := range plan.Actions {
		if filepath.Dir(a.Dst) != want {
			t.Errorf("%s plans into %s, want its album's folder %s", filepath.Base(a.Src), filepath.Dir(a.Dst), want)
		}
	}
	if _, err := lib.ApplyOrganize(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	albums, err := lib.Query(ctx, query.New(query.EntityItems).Where("album", query.OpIs, "Malibu").Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	seen := map[string]bool{}
	for _, it := range albums {
		seen[string(it.AlbumPID)] = true
	}
	if len(albums) != 4 || len(seen) != 1 {
		t.Errorf("after organize and a forced scan: %d tracks on %d albums, want 4 on 1", len(albums), len(seen))
	}
}

// TestImportFilesAStrayYearWithItsAlbum: the import planner renders {year} from the year
// the staged album's tracks give it together, the rule the catalog then keeps, so a stray
// year lands with its album. Two staged releases of one name in folders of their own keep
// their own years.
func TestImportFilesAStrayYearWithItsAlbum(t *testing.T) {
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	for n, year := range map[int]int{1: 2015, 2: 2015, 3: 2015, 4: 2016} {
		writeFile(t, filepath.Join(staging, "Malibu", strconv.Itoa(n)+".mp3"), malibuMP3(n, year))
	}
	for n, folder := range map[int]string{5: "Weezer 1994", 6: "Weezer 2001"} {
		year := 1994
		if folder == "Weezer 2001" {
			year = 2001
		}
		writeFile(t, filepath.Join(staging, folder, "1.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: "Track " + strconv.Itoa(n), Artist: "Weezer", AlbumArtist: "Weezer", Album: "Weezer",
			Track: 1, Year: year, Audio: testaudio.AudioWithSeed(byte(n)),
		}))
	}
	plan := importAll(t, ctx, lib, staging, 6)
	want := map[string]string{
		"Malibu":      filepath.Join(root, "Anderson .Paak", "Malibu (2015)"),
		"Weezer 1994": filepath.Join(root, "Weezer", "Weezer (1994)"),
		"Weezer 2001": filepath.Join(root, "Weezer", "Weezer (2001)"),
	}
	for _, a := range plan.Actions {
		if w := want[filepath.Base(filepath.Dir(a.Src))]; filepath.Dir(a.Dst) != w {
			t.Errorf("%s lands in %s, want %s", a.Src, filepath.Dir(a.Dst), w)
		}
	}
}
