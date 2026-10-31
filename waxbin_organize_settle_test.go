package waxbin_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/query"
)

// TestOrganizePlansSettledAlbumYears: a plan files tracks under the album's settled year
// even when a scan's settle failed and left the album's year stale behind it.
func TestOrganizePlansSettledAlbumYears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, year := range []int{2001, 2002, 2002} {
		writeFile(t, filepath.Join(root, "in", string(rune('a'+i))+".mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: "Song " + string(rune('A'+i)), Artist: "X", AlbumArtist: "X", Album: "A", Track: i + 1, Year: year,
			Audio: testaudio.AudioWithSeed(byte(i + 1)),
		}))
	}
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+db+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE album SET year = 2001;
		INSERT INTO rollup_dirty(kind, entity_id) SELECT 'album', id FROM album`); err != nil {
		t.Fatalf("leave a failed settle behind: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Pending() != 3 {
		t.Fatalf("planned moves = %+v, want the three tracks", plan.Actions)
	}
	for _, a := range plan.Actions {
		if !strings.Contains(filepath.ToSlash(a.Dst), "A (2002)/") {
			t.Errorf("%s planned to %s, want the album's settled year 2002", a.Src, a.Dst)
		}
	}
}
