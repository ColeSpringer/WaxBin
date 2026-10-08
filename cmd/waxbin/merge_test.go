package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

func TestDedupLosers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		args     []string
		survivor model.PID
		want     []model.PID
	}{
		{"distinct", []string{"b", "c"}, "a", []model.PID{"b", "c"}},
		{"drops duplicate loser", []string{"b", "b", "c"}, "a", []model.PID{"b", "c"}},
		{"drops survivor", []string{"a", "b"}, "a", []model.PID{"b"}},
		{"all collapse", []string{"a", "a"}, "a", []model.PID{}},
		{"preserves order", []string{"c", "b", "c", "b"}, "a", []model.PID{"c", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupLosers(tc.args, tc.survivor)
			if len(got) != len(tc.want) {
				t.Fatalf("dedupLosers(%v, %q) = %v, want %v", tc.args, tc.survivor, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("dedupLosers(%v, %q) = %v, want %v", tc.args, tc.survivor, got, tc.want)
				}
			}
		})
	}
}

// TestMergeSaysWhatCanUndoIt: a merge in text ends with a note that the files still carry
// the spellings it merged, which a rebuild (or, for albums, a retag or a move) can split
// off again; with --json it prints its document alone.
func TestMergeSaysWhatCanUndoIt(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, artist := range []string{"Kept Spelling", "Second Spelling", "Third Spelling"} {
		writeAsset(t, filepath.Join(root, artist+".mp3"),
			testaudio.BuildMP3WithAudio("Song "+artist, artist, "Album "+artist, 1, testaudio.AudioWithSeed(byte(81+i))))
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db,
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 3 {
		t.Fatalf("items = %d (err %v), want 3", len(items), err)
	}
	artists, albums := map[string]model.PID{}, map[string]model.PID{}
	for _, it := range items {
		artists[it.Artist] = it.ArtistPID
		albums[it.Album] = it.AlbumPID
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	text, err := runLibraryCmd(t, db, false, "merge", "artist", string(artists["Kept Spelling"]), string(artists["Second Spelling"]))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !strings.Contains(text, "note:") || !strings.Contains(text, "db reset") {
		t.Errorf("merge printed %q, want a note on what can undo it", text)
	}
	text, err = runLibraryCmd(t, db, false, "merge", "album", string(albums["Album Kept Spelling"]), string(albums["Album Second Spelling"]))
	if err != nil {
		t.Fatalf("merge albums: %v", err)
	}
	if !strings.Contains(text, "note:") || !strings.Contains(text, "folder") {
		t.Errorf("album merge printed %q, want a note that a move can split it", text)
	}
	out, err := runLibraryCmd(t, db, true, "merge", "artist", string(artists["Kept Spelling"]), string(artists["Third Spelling"]))
	if err != nil {
		t.Fatalf("merge --json: %v", err)
	}
	if _, why := oneEnvelope(out); why != "" || strings.Contains(out, "note:") {
		t.Errorf("merge --json printed %q (%s), want one envelope and no note", out, why)
	}
}

// TestMergeNoteNamesTheReset: the caveat a merge prints names what drops a fold, a db
// reset, since a rebuild scans into the catalog that holds it.
func TestMergeNoteNamesTheReset(t *testing.T) {
	t.Parallel()
	for _, et := range []model.MergeEntity{model.MergeArtist, model.MergeAlbum} {
		if n := mergeNote(et); !strings.Contains(n, "db reset") || strings.Contains(n, "rebuild") {
			t.Errorf("note for %s = %q, want it to name a db reset and no rebuild", et, n)
		}
	}
}
