package main

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// TestDBVerifyReportsAndFixesAlbumYearDrift: an album year that is not its members' is
// reported by `db verify`, which fails, and `db verify --fix` puts it right.
func TestDBVerifyReportsAndFixesAlbumYearDrift(t *testing.T) {
	t.Parallel()
	db, root, _ := creditCLIFixture(t)
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	// Closed on a failure too: an open handle on Windows keeps the temp dir from going.
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec("UPDATE track SET year = 2015; UPDATE album SET year = 1900"); err != nil {
		t.Fatalf("set the years by hand: %v", err)
	}
	_ = raw.Close()

	verify := func(args ...string) (int, error) {
		t.Helper()
		out, err := runCLIJSON(t, db, root, append([]string{"db", "verify"}, args...)...)
		var env struct {
			Data struct {
				AlbumYearDrift int `json:"albumYearDrift"`
			} `json:"data"`
		}
		if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
			t.Fatalf("db verify %v printed %q: %v", args, out, jerr)
		}
		return env.Data.AlbumYearDrift, err
	}
	if n, err := verify(); n != 1 || err == nil {
		t.Fatalf("db verify = %d drift (err %v), want 1 and a failure", n, err)
	}
	if n, err := verify("--fix"); n != 0 || err != nil {
		t.Fatalf("db verify --fix = %d drift (err %v), want 0 and success", n, err)
	}
}

// TestDBVerifyReportsAndFixesReleaseYearDrift: a release year newest orders by that is
// not the item's year is reported by `db verify`, which fails, and --fix puts it right.
func TestDBVerifyReportsAndFixesReleaseYearDrift(t *testing.T) {
	t.Parallel()
	db, root, _ := creditCLIFixture(t)
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec("UPDATE playable_item SET release_year = 1900"); err != nil {
		t.Fatalf("set the release year by hand: %v", err)
	}
	_ = raw.Close()

	verify := func(args ...string) (int, error) {
		t.Helper()
		out, err := runCLIJSON(t, db, root, append([]string{"db", "verify"}, args...)...)
		var env struct {
			Data struct {
				ReleaseYearDrift int `json:"releaseYearDrift"`
			} `json:"data"`
		}
		if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
			t.Fatalf("db verify %v printed %q: %v", args, out, jerr)
		}
		return env.Data.ReleaseYearDrift, err
	}
	n, err := verify()
	if n != 1 || err == nil {
		t.Fatalf("db verify = %d drift (err %v), want 1 and a failure", n, err)
	}
	// A rescan does not rewrite an unchanged book, so --fix is the remedy to name.
	if !strings.Contains(err.Error(), "--fix") || strings.Contains(err.Error(), "re-scan") {
		t.Errorf("db verify's verdict = %q, want --fix as the remedy", err)
	}
	if n, err := verify("--fix"); n != 0 || err != nil {
		t.Fatalf("db verify --fix = %d drift (err %v), want 0 and success", n, err)
	}
}

// TestDBVerifyReportsAndFixesPlaylistPositionDrift: playlist positions that are not the
// listing indexes are reported by `db verify` without failing it (indexes resolve by
// rank), and `db verify --fix` renumbers them in listing order.
func TestDBVerifyReportsAndFixesPlaylistPositionDrift(t *testing.T) {
	t.Parallel()
	db, root, pid := creditCLIFixture(t)
	if _, err := runCLIJSON(t, db, root, "playlist", "create", "Mix"); err != nil {
		t.Fatalf("playlist create: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`INSERT INTO playlist_item(playlist_id, position, item_id)
		SELECT (SELECT id FROM playlist), 5, id FROM playable_item WHERE pid = ?`, string(pid)); err != nil {
		t.Fatalf("add a sparse entry by hand: %v", err)
	}
	_ = raw.Close()

	verify := func(args ...string) (int, error) {
		t.Helper()
		out, err := runCLIJSON(t, db, root, append([]string{"db", "verify"}, args...)...)
		var env struct {
			Data struct {
				PlaylistPositionDrift int `json:"playlistPositionDrift"`
			} `json:"data"`
		}
		if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
			t.Fatalf("db verify %v printed %q: %v", args, out, jerr)
		}
		return env.Data.PlaylistPositionDrift, err
	}
	if n, err := verify(); n != 1 || err != nil {
		t.Fatalf("db verify = %d drift (err %v), want 1 and success", n, err)
	}
	if n, err := verify("--fix"); n != 0 || err != nil {
		t.Fatalf("db verify --fix = %d drift (err %v), want 0 and success", n, err)
	}
}

// TestDBVerifyFixRebuildsTheSearchIndex: a search row gone stale is something `db
// verify` cannot see (it counts rows), so --fix rebuilds the index and says how many
// rows it wrote.
func TestDBVerifyFixRebuildsTheSearchIndex(t *testing.T) {
	t.Parallel()
	db, root, _ := creditCLIFixture(t)
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec("UPDATE search_fts SET title = 'outdated words'"); err != nil {
		t.Fatalf("stale the row by hand: %v", err)
	}
	_ = raw.Close()

	out, err := runCLIJSON(t, db, root, "db", "verify", "--fix")
	if err != nil {
		t.Fatalf("db verify --fix: %v", err)
	}
	var env struct {
		Data struct {
			SearchRowsRebuilt int `json:"searchRowsRebuilt"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("db verify --fix printed %q: %v", out, err)
	}
	if env.Data.SearchRowsRebuilt != 1 {
		t.Errorf("searchRowsRebuilt = %d, want 1", env.Data.SearchRowsRebuilt)
	}
	for q, want := range map[string]int{"outdated": 0, "song": 1} {
		out, err := runCLIJSON(t, db, root, "search", q)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		var res struct {
			Data struct {
				Tracks []struct{} `json:"tracks"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("search printed %q: %v", out, err)
		}
		if len(res.Data.Tracks) != want {
			t.Errorf("search %q = %d tracks, want %d", q, len(res.Data.Tracks), want)
		}
	}
}

// TestDBVerifyFixReportsWhatItRewrote: `db verify --fix` says how many rows each repair
// rewrote, in its text and under "refreshed" in its JSON, which a plain verify leaves out.
func TestDBVerifyFixReportsWhatItRewrote(t *testing.T) {
	t.Parallel()
	db, root, _ := creditCLIFixture(t)
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec("UPDATE album SET year = 1900; UPDATE playable_item SET release_year = 1900"); err != nil {
		t.Fatalf("break the years by hand: %v", err)
	}
	_ = raw.Close()

	type refreshed struct {
		Artists      int `json:"artists"`
		AlbumYears   int `json:"albumYears"`
		ReleaseYears int `json:"releaseYears"`
	}
	read := func(args ...string) (*refreshed, bool) {
		t.Helper()
		out, _ := runCLIJSON(t, db, root, append([]string{"db", "verify"}, args...)...)
		var env struct {
			Data struct {
				RollupsPending *int       `json:"rollupsPending"`
				Refreshed      *refreshed `json:"refreshed"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("db verify %v printed %q: %v", args, out, err)
		}
		return env.Data.Refreshed, env.Data.RollupsPending != nil
	}
	if got, pending := read(); got != nil || !pending {
		t.Errorf("plain verify: refreshed %+v, rollupsPending present %t, want no refreshed and the pending count", got, pending)
	}
	if got, _ := read("--fix"); got == nil || *got != (refreshed{AlbumYears: 1, ReleaseYears: 1}) {
		t.Errorf("verify --fix refreshed = %+v, want one album year and one release year", got)
	}

	text, err := runCLIText(t, db, root, "db", "verify", "--fix")
	if err != nil {
		t.Fatalf("db verify --fix: %v (%s)", err, text)
	}
	if !strings.Contains(text, "years rewritten:") || !strings.Contains(text, "0 albums, 0 items") {
		t.Errorf("text = %q, want the years line reporting nothing left to rewrite", text)
	}
}

// damageThumbsCLI seeds a thumbnail into the file beside db and writes garbage over its
// pages past the schema page and the pointer map, which an attach never reads.
func damageThumbsCLI(t *testing.T, db string) {
	t.Helper()
	thumbs := sqlite.ThumbFile(db)
	raw, err := sql.Open("sqlite", "file:"+thumbs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`INSERT INTO thumb(source_hash, size, format, width, height, data, created_at)
		VALUES ('h1', 128, 'jpeg', 128, 128, x'0405', 1)`); err != nil {
		t.Fatalf("seed a thumbnail: %v", err)
	}
	_ = raw.Close()
	f, err := os.OpenFile(thumbs, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var header [2]byte
	if _, err := f.ReadAt(header[:], 16); err != nil {
		t.Fatal(err)
	}
	page := int64(binary.BigEndian.Uint16(header[:]))
	info, err := f.Stat()
	if err != nil || info.Size() <= 2*page {
		t.Fatalf("the thumbnail file: %v, want pages past the pointer map", err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xa5}, int(info.Size()-2*page)), 2*page); err != nil {
		t.Fatal(err)
	}
}

// verifyData runs `db verify` with args and returns its JSON data.
func verifyData(t *testing.T, db, root string, args ...string) map[string]any {
	t.Helper()
	out, err := runCLIJSON(t, db, root, append([]string{"db", "verify"}, args...)...)
	if err != nil {
		t.Fatalf("db verify %v: %v (%s)", args, err, out)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("db verify %v printed %q: %v", args, out, err)
	}
	return env.Data
}

// TestDBVerifyOverADamagedThumbFile: a thumbnail file whose table pages are garbage reads
// as a damaged cache, not a failed check: `db verify` says so in both forms, counts no
// orphans it could not read, passes, and points at --fix.
func TestDBVerifyOverADamagedThumbFile(t *testing.T) {
	t.Parallel()
	db, root, _ := creditCLIFixture(t)
	damageThumbsCLI(t, db)
	data := verifyData(t, db, root)
	if data["thumbnailsDamaged"] != true || data["orphanThumbnails"] != float64(0) || data["consistent"] != true {
		t.Errorf("db verify = %v, want the damage reported, no orphans counted, and a consistent catalog", data)
	}
	text, err := runCLIText(t, db, root, "db", "verify")
	if err != nil {
		t.Fatalf("db verify (text): %v (%s)", err, text)
	}
	for _, want := range []string{"orphan thumbnails:        not counted", "`db verify --fix` replaces it"} {
		if !strings.Contains(text, want) {
			t.Errorf("db verify text is missing %q:\n%s", want, text)
		}
	}
}

// TestDBVerifyFixReplacesADamagedThumbFile: --fix leaves a sound thumbnail file in place
// of a damaged one, the damaged one set aside, and says it did.
func TestDBVerifyFixReplacesADamagedThumbFile(t *testing.T) {
	t.Parallel()
	db, root, _ := creditCLIFixture(t)
	damageThumbsCLI(t, db)
	data := verifyData(t, db, root, "--fix")
	if data["thumbnailsDamaged"] != false || data["thumbnailsReplaced"] != true {
		t.Errorf("db verify --fix = %v, want a sound cache it replaced", data)
	}
	if _, err := os.Stat(sqlite.ThumbFile(db) + ".unusable"); err != nil {
		t.Errorf("the damaged file was not set aside: %v", err)
	}
	if data := verifyData(t, db, root); data["thumbnailsDamaged"] != false {
		t.Errorf("db verify after the fix = %v, want a sound cache", data)
	}
}

// TestDBVerifyFixSaysCursorsMoveOnlyWhenAKeyDid: the fix's note to re-page open cursors and
// its sortKeysRewritten count come from the sort keys it moved, so a sort spelling it
// repairs under a key that stays put leaves both quiet.
func TestDBVerifyFixSaysCursorsMoveOnlyWhenAKeyDid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, seed string
		moved      int
	}{
		{"spelling only", "UPDATE album SET title_sort = 'stale'", 0},
		{"spelling and key", "UPDATE album SET title_sort = 'stale', sort_key = '" + model.SortKey("stale") + "'", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, root, _ := creditCLIFixture(t)
			seed := func() {
				t.Helper()
				raw, err := sql.Open("sqlite", "file:"+db)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				if _, err := raw.Exec(tc.seed); err != nil {
					t.Fatalf("break the spelling by hand: %v", err)
				}
			}
			seed()
			text, err := runCLIText(t, db, root, "db", "verify", "--fix")
			if err != nil {
				t.Fatalf("db verify --fix: %v (%s)", err, text)
			}
			if strings.Contains(text, "re-page") != (tc.moved > 0) ||
				!strings.Contains(text, fmt.Sprintf("sort keys rewritten:      %d\n", tc.moved)) {
				t.Errorf("text = %q, want %d sort keys rewritten and the cursor note only for a moved key", text, tc.moved)
			}
			seed()
			data := verifyData(t, db, root, "--fix")
			moved, _ := data["sortKeysRewritten"].(float64)
			refreshed, _ := data["refreshed"].(map[string]any)
			if int(moved) != tc.moved || refreshed["sortSpellings"] != float64(1) || refreshed["sortSpellingKeys"] != float64(tc.moved) {
				t.Errorf("db verify --fix = %v, want one spelling rewritten and %d keys moved", data, tc.moved)
			}
		})
	}
}
