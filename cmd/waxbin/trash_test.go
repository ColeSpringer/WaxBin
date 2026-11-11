package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
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
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"0d", 0},
		{"36h", 36 * time.Hour},
		{"90m", 90 * time.Minute},
		// More days than a duration holds is the longest age, not one wrapped around to
		// minutes (213504 days wrapped to about 25 of them).
		{"213504d", math.MaxInt64},
		{"9999999999d", math.MaxInt64},
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
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Song", "Band", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := openSeeded(t, ctx, waxbin.Options{DBPath: db,
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

// TestTrashListShowsWhatARipHeld: a trashed rip's entry lists the audio it held, the
// library it left and each track with its window, and the text row counts the tracks the
// ITEM column does not name.
func TestTrashListShowsWhatARipHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const rate = 22050
	if err := os.WriteFile(filepath.Join(root, "album.wav"),
		testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 30, testaudio.AltPartials, 5)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "album.cue"), []byte("PERFORMER \"Rip Artist\"\nTITLE \"Rip Album\"\nFILE \"album.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:10:00\n"+
		"  TRACK 03 AUDIO\n    TITLE \"Three\"\n    INDEX 01 00:20:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).OrderBy("track", false).Build(), "")
	if err != nil || len(items) != 3 {
		t.Fatalf("items = %d (err %v), want the rip's three tracks", len(items), err)
	}
	f, err := lib.File(ctx, items[0].FilePID)
	if err != nil {
		t.Fatal(err)
	}
	libPID := items[0].LibraryPID
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLIJSON(t, db, root, "rm", string(items[0].PID), string(items[1].PID), string(items[2].PID), "--apply"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	out, err := runCLIJSON(t, db, root, "trash", "list")
	if err != nil {
		t.Fatalf("trash list: %v", err)
	}
	type itemJSON struct {
		ItemPID     string `json:"itemPid"`
		Virtual     bool   `json:"virtual"`
		StartFrames int64  `json:"startFrames"`
		EndFrames   int64  `json:"endFrames"`
	}
	var listed struct {
		Data []struct {
			ItemPID     string     `json:"itemPid"`
			EssenceHash string     `json:"essenceHash"`
			LibraryPID  string     `json:"libraryPid"`
			Items       []itemJSON `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed.Data) != 1 {
		t.Fatalf("trash list printed %q (err %v), want one entry", out, err)
	}
	e := listed.Data[0]
	want := []itemJSON{
		{string(items[0].PID), true, 0, 750},
		{string(items[1].PID), true, 750, 1500},
		{string(items[2].PID), true, 1500, 0},
	}
	if e.EssenceHash == "" || e.EssenceHash != f.EssenceHash || e.LibraryPID != string(libPID) ||
		e.ItemPID != want[0].ItemPID || !slices.Equal(e.Items, want) {
		t.Errorf("entry = %+v, want essence %q, library %s and items %+v", e, f.EssenceHash, libPID, want)
	}
	text, err := runCLIText(t, db, root, "trash", "list")
	if err != nil || !strings.Contains(text, want[0].ItemPID+" +2") {
		t.Errorf("trash list printed %q (err %v), want the opening track and +2", text, err)
	}
}
