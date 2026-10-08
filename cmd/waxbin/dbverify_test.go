package main

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// TestDBVerifyReportsAndFixesAlbumYearDrift: an album year that is not its members' is
// reported by `db verify`, which fails, and `db verify --fix` puts it right.
func TestDBVerifyReportsAndFixesAlbumYearDrift(t *testing.T) {
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
