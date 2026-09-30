package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

func TestParseAge(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"0d", 0},
		{"36h", 36 * time.Hour},
		{"90m", 90 * time.Minute},
	} {
		got, err := parseAge("trash empty", tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseAge(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	// A negative retention age has no meaning on either caller: it would purge
	// everything newer than a moment in the future, which is everything.
	for _, in := range []string{"", "d", "x d", "thirty days", "1.5x", "-30d", "-1h"} {
		if _, err := parseAge("trash empty", in); err == nil {
			t.Errorf("parseAge(%q) should fail", in)
		}
	}
}

// TestTrashEmptySaysWhatItLeft: entries kept because their library is read-only are
// counted in the summary, or emptying the trash would look like it missed them.
func TestTrashEmptySaysWhatItLeft(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Song", "Band", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db,
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v)", len(items), err)
	}
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{items[0].PID}, model.DeleteTrash)
	if err == nil {
		_, err = lib.ApplyDelete(ctx, plan)
	}
	if err != nil {
		t.Fatal(err)
	}
	libs, err := lib.Libraries(ctx)
	if err == nil {
		_, err = lib.SetLibraryReadOnly(ctx, libs[0].PID, true)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = lib.Close()

	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", db, "trash", "empty"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("trash empty: %v", err)
	}
	if !strings.Contains(stdout.String(), "1 left in read-only libraries") {
		t.Fatalf("trash empty printed %q, want the entry it left counted", stdout.String())
	}
}
