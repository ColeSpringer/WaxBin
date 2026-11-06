package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// TestPrimaryLibraryFlagsScopeToPlacement: on search, query and facet alike, --library
// finds an item through its copy in a root and --primary-library only through the root
// its primary file sits in.
func TestPrimaryLibraryFlagsScopeToPlacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	song := testaudio.BuildMP3("Harbor", "Artist", "Album", 1)
	for _, p := range []string{filepath.Join(rootA, "harbor.mp3"), filepath.Join(rootB, "harbor.mp3")} {
		if err := os.WriteFile(p, song, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := openSeeded(t, ctx, waxbin.Options{DBPath: db, Roots: []config.Root{
		{Path: rootA, Mode: model.ModeInPlace}, {Path: rootB, Mode: model.ModeInPlace},
	}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// The scan reads rootA first, so its file holds the item and rootB's copy attaches as
	// an alternate; pin that here, since every assertion below reads through it.
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var placedIn model.PID
	for _, l := range libs {
		if l.DisplayRoot == rootA {
			placedIn = l.PID
		}
	}
	scanned, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(scanned) != 1 || scanned[0].LibraryPID != placedIn || placedIn == "" {
		t.Fatalf("items = %d (err %v), want one item placed in %s", len(scanned), err, rootA)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	// run executes one read command against both roots and returns its JSON data.
	run := func(args ...string) json.RawMessage {
		t.Helper()
		cmd := newRootCmd(&globals{})
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(append([]string{"--db", db, "--root", rootA + ":in-place", "--root", rootB + ":in-place", "--json"}, args...))
		if err := cmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, stderr.String())
		}
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("%v printed %q: %v", args, stdout.String(), err)
		}
		return env.Data
	}
	tracks := func(args ...string) int {
		t.Helper()
		var res struct {
			Tracks []json.RawMessage `json:"tracks"`
		}
		if err := json.Unmarshal(run(append([]string{"search", "harbor"}, args...)...), &res); err != nil {
			t.Fatal(err)
		}
		return len(res.Tracks)
	}
	items := func(args ...string) int {
		t.Helper()
		var res []json.RawMessage
		if err := json.Unmarshal(run(append([]string{"query"}, args...)...), &res); err != nil {
			t.Fatal(err)
		}
		return len(res)
	}
	buckets := func(args ...string) []string {
		t.Helper()
		var res struct {
			Buckets []struct {
				Display string `json:"display"`
				Count   int    `json:"count"`
			} `json:"buckets"`
		}
		if err := json.Unmarshal(run(append([]string{"facet"}, args...)...), &res); err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(res.Buckets))
		for i, b := range res.Buckets {
			out[i] = filepath.Base(b.Display)
			if b.Count != 1 {
				t.Errorf("facet %v: bucket %s counts %d, want 1", args, b.Display, b.Count)
			}
		}
		return out
	}

	if n := tracks("--library", rootB); n != 1 {
		t.Errorf("search --library B found %d tracks, want the item through its copy", n)
	}
	if n := tracks("--primary-library", rootB); n != 0 {
		t.Errorf("search --primary-library B found %d tracks, want none: the copy does not place the item there", n)
	}
	if n := tracks("--primary-library", rootA); n != 1 {
		t.Errorf("search --primary-library A found %d tracks, want the item under its primary", n)
	}
	if n := tracks("--primary-library", rootA, "--library", rootB); n != 1 {
		t.Errorf("search with both flags found %d tracks, want the one item placed in A and playable from B", n)
	}
	if n := items("--library", rootB); n != 1 {
		t.Errorf("query --library B listed %d items, want the item through its copy", n)
	}
	if n := items("--primary-library", rootB); n != 0 {
		t.Errorf("query --primary-library B listed %d items, want none", n)
	}
	if n := items("--primary-library", rootA); n != 1 {
		t.Errorf("query --primary-library A listed %d items, want the item under its primary", n)
	}
	a, b := filepath.Base(rootA), filepath.Base(rootB)
	if got := buckets("--group-by", "primaryLibrary"); len(got) != 1 || got[0] != a {
		t.Errorf("facet primaryLibrary = %v, want one bucket for %s", got, a)
	}
	if got := buckets("--group-by", "library"); len(got) != 2 {
		t.Errorf("facet library = %v, want a bucket under each of %s and %s", got, a, b)
	}
	if got := buckets("--group-by", "library", "--primary-library", rootB); len(got) != 0 {
		t.Errorf("facet library under --primary-library B = %v, want no buckets", got)
	}
}
