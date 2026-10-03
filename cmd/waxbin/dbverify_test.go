package main

import (
	"database/sql"
	"encoding/json"
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
