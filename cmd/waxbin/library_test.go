package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// runLibraryCmd runs one command against db and returns what it printed.
func runLibraryCmd(t *testing.T, db string, jsonOut bool, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	full := []string{"--db", db}
	if jsonOut {
		full = append(full, "--json")
	}
	cmd.SetArgs(append(full, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

// TestLibrarySetFlagsThroughAServer: `library set` reaches a running server's catalog
// through the proxy, and `library list` shows the flag in both outputs.
func TestLibrarySetFlagsThroughAServer(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := serveCatalog(t, waxbin.Options{DBPath: db,
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	libs, err := lib.Libraries(ctx)
	if err != nil || len(libs) != 1 {
		t.Fatal(err)
	}
	pid := string(libs[0].PID)

	if _, err := runLibraryCmd(t, db, false, "library", "set", pid); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("library set with no flag = %v, want a usage error", err)
	}
	if _, err := runLibraryCmd(t, db, false, "library", "set", pid, "--read-only", "--writable"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("library set with both flags = %v, want a usage error", err)
	}
	out, err := runLibraryCmd(t, db, true, "library", "set", pid, "--read-only")
	if err != nil {
		t.Fatalf("library set --read-only: %v", err)
	}
	var set struct {
		Data struct {
			PID      string `json:"pid"`
			ReadOnly bool   `json:"readOnly"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &set); err != nil || set.Data.PID != pid || !set.Data.ReadOnly {
		t.Fatalf("library set printed %q (err %v), want the flagged library", out, err)
	}
	if libs, err := lib.Libraries(ctx); err != nil || !libs[0].ReadOnly {
		t.Fatalf("server libraries = %+v (err %v), want the flag set", libs, err)
	}

	text, err := runLibraryCmd(t, db, false, "library", "list")
	if err != nil {
		t.Fatalf("library list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "READ-ONLY") || !strings.Contains(lines[1], "yes") {
		t.Fatalf("library list =\n%s\nwant a READ-ONLY column saying yes", text)
	}

	if _, err := runLibraryCmd(t, db, false, "library", "set", pid, "--writable"); err != nil {
		t.Fatalf("library set --writable: %v", err)
	}
	out, err = runLibraryCmd(t, db, true, "library", "list")
	if err != nil {
		t.Fatalf("library list --json: %v", err)
	}
	if strings.Contains(out, `"readOnly"`) {
		t.Fatalf("library list --json = %s, want no readOnly key once cleared", out)
	}
}

// TestLibrarySetFolderFallbackThroughAServer: `library set` turns an in-place library's
// folder fallback on and off through a running server, one setting per call, and both
// list outputs show it.
func TestLibrarySetFolderFallbackThroughAServer(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := serveCatalog(t, waxbin.Options{DBPath: db,
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace, Profile: "waxbin-native"}}})
	libs, err := lib.Libraries(ctx)
	if err != nil || len(libs) != 1 {
		t.Fatal(err)
	}
	pid := string(libs[0].PID)

	for _, both := range [][]string{{"--folder-fallback", "--no-folder-fallback"}, {"--folder-fallback", "--read-only"}} {
		if _, err := runLibraryCmd(t, db, false, append([]string{"library", "set", pid}, both...)...); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Fatalf("library set %v = %v, want a usage error", both, err)
		}
	}
	if _, err := runLibraryCmd(t, db, false, "library", "set", pid, "--read-only"); err != nil {
		t.Fatalf("library set --read-only: %v", err)
	}
	out, err := runLibraryCmd(t, db, true, "library", "set", pid, "--folder-fallback")
	if err != nil {
		t.Fatalf("library set --folder-fallback: %v", err)
	}
	var set struct {
		Data struct {
			ReadOnly       bool `json:"readOnly"`
			FolderFallback bool `json:"folderFallback"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &set); err != nil || !set.Data.FolderFallback || !set.Data.ReadOnly {
		t.Fatalf("library set printed %q (err %v), want both flags on", out, err)
	}
	if libs, err := lib.Libraries(ctx); err != nil || !libs[0].FolderFallback {
		t.Fatalf("server libraries = %+v (err %v), want the folder fallback on", libs, err)
	}
	text, err := runLibraryCmd(t, db, false, "library", "list")
	if err != nil {
		t.Fatalf("library list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "FOLDER-FALLBACK") {
		t.Fatalf("library list =\n%s\nwant a FOLDER-FALLBACK column", text)
	}

	if _, err := runLibraryCmd(t, db, false, "library", "set", pid, "--no-folder-fallback"); err != nil {
		t.Fatalf("library set --no-folder-fallback: %v", err)
	}
	if libs, err := lib.Libraries(ctx); err != nil || libs[0].FolderFallback || !libs[0].ReadOnly {
		t.Fatalf("server libraries = %+v (err %v), want the fallback off and read-only untouched", libs, err)
	}
}
