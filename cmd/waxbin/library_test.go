package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
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

// TestLibraryAddChecksTheFolder: `library add` resolves a relative path against the
// working directory and registers an existing folder, refuses a missing one, and
// registers it with --allow-absent; `init` refuses a missing configured root the same way.
func TestLibraryAddChecksTheFolder(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	db := filepath.Join(t.TempDir(), "catalog.db")
	if _, err := runLibraryCmd(t, db, false, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "music"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	out, err := runLibraryCmd(t, db, true, "library", "add", filepath.Join(".", "music")+":in-place")
	var added struct {
		Data struct {
			Root string `json:"root"`
		} `json:"data"`
	}
	if err != nil || json.Unmarshal([]byte(out), &added) != nil || added.Data.Root != filepath.Join(work, "music") {
		t.Fatalf("library add ./music = %q (err %v), want the absolute folder registered", out, err)
	}
	later := filepath.Join(t.TempDir(), "later")
	if _, err := runLibraryCmd(t, db, false, "library", "add", later+":in-place"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("library add of a missing folder = %v, want CodeInvalid", err)
	}
	if _, err := runLibraryCmd(t, db, false, "library", "add", later+":in-place", "--allow-absent"); err != nil {
		t.Fatalf("library add --allow-absent: %v", err)
	}

	fresh := filepath.Join(t.TempDir(), "fresh.db")
	missing := filepath.Join(t.TempDir(), "unmounted")
	if _, err := runLibraryCmd(t, fresh, false, "--root", missing, "init"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("init with a missing root = %v, want CodeInvalid", err)
	}
	if _, err := runLibraryCmd(t, fresh, false, "--root", missing, "init", "--allow-absent"); err != nil {
		t.Fatalf("init --allow-absent: %v", err)
	}
}

// TestLibraryRemove: `library remove` takes a library out of the catalog and says what
// it detached, in both outputs; a root the command line configures is refused without
// --force, and forced it goes with a warning that the configuration brings it back.
func TestLibraryRemove(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	rootA, rootB, rootC := t.TempDir(), t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeAsset(t, filepath.Join(rootB, "b.mp3"), testaudio.BuildMP3WithAudio("In B", "Artist", "Album", 1, testaudio.AudioWithSeed(61)))
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db,
		Roots: []config.Root{{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	c, err := lib.AddRoot(ctx, config.Root{Path: rootC, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var a model.PID
	for _, l := range libs {
		if l.DisplayRoot == rootA {
			a = l.PID
		}
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	text, err := runLibraryCmd(t, db, false, "library", "remove", string(b.PID))
	if err != nil {
		t.Fatalf("library remove: %v", err)
	}
	if !strings.Contains(text, "Removed library "+string(b.PID)) || !strings.Contains(text, "1 file detached, 1 item archived") {
		t.Errorf("library remove printed %q, want the library and what it detached", text)
	}
	out, err := runLibraryCmd(t, db, true, "library", "remove", string(c.PID))
	if err != nil {
		t.Fatalf("library remove --json: %v", err)
	}
	var removed struct {
		Data struct {
			LibraryPID    string `json:"libraryPid"`
			Root          string `json:"root"`
			FilesDetached *int   `json:"filesDetached"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &removed); err != nil || removed.Data.LibraryPID != string(c.PID) ||
		removed.Data.Root != rootC || removed.Data.FilesDetached == nil {
		t.Fatalf("library remove --json printed %q (err %v), want the removed library and its counts", out, err)
	}

	configured := rootA + ":managed:waxbin-native"
	if _, err := runLibraryCmd(t, db, false, "--root", configured, "library", "remove", string(a)); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("library remove of a configured root = %v, want CodeInvalid", err)
	}
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--db", db, "--root", configured, "library", "remove", string(a), "--force"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("library remove --force: %v", err)
	}
	if !strings.Contains(stderr.String(), "configuration") {
		t.Errorf("a forced removal of a configured root warned %q, want it to say the configuration brings it back", stderr.String())
	}
	list, err := runLibraryCmd(t, db, false, "library", "list")
	if err != nil || !strings.Contains(list, "no library roots registered") {
		t.Errorf("library list = %q (err %v), want every root gone", list, err)
	}
}

// TestLibraryRemoveThroughAServer: with a server running, `library remove` runs as the
// server's job, which the command follows to its report.
func TestLibraryRemoveThroughAServer(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeAsset(t, filepath.Join(rootB, "b.mp3"), testaudio.BuildMP3WithAudio("In B", "Artist", "Album", 1, testaudio.AudioWithSeed(62)))
	lib := serveCatalog(t, waxbin.Options{DBPath: db,
		Roots: []config.Root{{Path: rootA, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	b, err := lib.AddRoot(ctx, config.Root{Path: rootB, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{LibraryPID: b.PID}); err != nil {
		t.Fatal(err)
	}
	out, err := runLibraryCmd(t, db, true, "library", "remove", string(b.PID))
	if err != nil {
		t.Fatalf("library remove through a server: %v", err)
	}
	var removed struct {
		Data struct {
			Root          string `json:"root"`
			FilesDetached int    `json:"filesDetached"`
			ItemsArchived int    `json:"itemsArchived"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &removed); err != nil || removed.Data.Root != rootB ||
		removed.Data.FilesDetached != 1 || removed.Data.ItemsArchived != 1 {
		t.Fatalf("library remove printed %q (err %v), want the job's report", out, err)
	}
	if libs, err := lib.Libraries(ctx); err != nil || len(libs) != 1 || libs[0].DisplayRoot != rootA {
		t.Fatalf("server libraries = %+v (err %v), want only %s", libs, err, rootA)
	}
}
