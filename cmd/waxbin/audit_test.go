package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/waxerr"
)

// TestAuditRejectsUnknownCheck ensures a mistyped --check name is rejected before
// the audit runs, rather than silently matching nothing and reporting no issues.
// Validation happens before the catalog is opened, so no database is needed.
func TestAuditRejectsUnknownCheck(t *testing.T) {
	t.Parallel()
	cmd := newAuditCmd(&globals{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"--check", "missing_replay_gain"}) // typo: real name is missing_replaygain
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error for an unknown check name, got nil")
	}
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("error code = %v, want CodeInvalid", err)
	}
	if !strings.Contains(err.Error(), "unknown check") {
		t.Errorf("error = %q, want it to mention the unknown check", err)
	}
}

// TestAuditDuplicateAlbumListsBothReasons: `audit --check duplicate_album` reports an album
// split across folders by its name and a pair enrichment gave one release id by the id.
func TestAuditDuplicateAlbumListsBothReasons(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, f := range []struct{ dir, artist, album string }{
		{"A/Hits", "A", "Hits"}, {"A/Hits", "A", "Hits"}, {"A/Hits Again", "A", "Hits"},
		{"B/Live", "B", "Live"}, {"B/Live Too", "B", "Live"},
	} {
		dir := filepath.Join(root, filepath.FromSlash(f.dir))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		mp3 := testaudio.BuildMP3WithAudio("Song "+strconv.Itoa(i), f.artist, f.album, i+1, testaudio.AudioWithSeed(byte(i+1)))
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)+".mp3"), mp3, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec("UPDATE album SET mbid = 'dddddddd-0000-4000-8000-000000000001' WHERE title = 'Live'"); err != nil {
		t.Fatalf("give the Live pair one release id: %v", err)
	}
	_ = raw.Close()

	out, err := runCLIJSON(t, db, root, "audit", "--check", "duplicate_album")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var env struct {
		Data struct {
			Findings []struct {
				Message string `json:"message"`
			} `json:"findings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("audit printed %q: %v", out, err)
	}
	var byName, byID int
	for _, f := range env.Data.Findings {
		switch {
		case strings.Contains(f.Message, "(same title and album artist): Hits / Hits"):
			byName++
		case strings.Contains(f.Message, "(shared MBID): Live / Live"):
			byID++
		}
	}
	if byName != 1 || byID != 1 || len(env.Data.Findings) != 2 {
		t.Errorf("findings = %+v, want one by name for Hits and one by id for Live", env.Data.Findings)
	}
}
