package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestProfilesListsTheTemplates: the command shows each profile's templates, and its
// JSON carries every field. It reads the configuration alone, so it answers before
// any catalog exists and while a root names a profile nobody defined, which is when
// someone needs the list.
func TestProfilesListsTheTemplates(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", db, "--root", root + ":managed:no-such-profile", "profiles"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("profiles: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "PODCAST") ||
		!strings.Contains(lines[0], "COMPILATIONS") || !strings.HasPrefix(lines[1], "waxbin-native") ||
		!strings.Contains(lines[1], "{albumartist}/{album}") || !strings.Contains(lines[1], "Various Artists") {
		t.Fatalf("profiles =\n%s\nwant a header and the native profile's templates", stdout.String())
	}

	out, err := runCLIJSON(t, db, root, "profiles")
	if err != nil {
		t.Fatalf("profiles --json: %v", err)
	}
	var env struct {
		Data []struct {
			Name      string `json:"name"`
			Music     string `json:"music"`
			Audiobook string `json:"audiobook"`
			Podcast   string `json:"podcast"`
			TagWrite  bool   `json:"tagWrite"`
			Folder    string `json:"compilationFolder"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || len(env.Data) != 1 {
		t.Fatalf("profiles --json printed %q (err %v), want one profile", out, err)
	}
	if p := env.Data[0]; p.Name != "waxbin-native" || p.Music == "" || p.Audiobook == "" || p.Podcast == "" || p.Folder != "Various Artists" {
		t.Errorf("profile = %+v, want the native profile in full", p)
	}
}
