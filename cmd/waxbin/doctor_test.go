package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
)

// TestDoctorPrintsEnrichmentPhases: the phase line sits under the enrichment line, so an
// operator reading doctor sees what this build's enrichment pass walks.
func TestDoctorPrintsEnrichmentPhases(t *testing.T) {
	db, root, _ := creditCLIFixture(t)
	t.Setenv("WAXBIN_ENRICH_CONTACT", "t@e.com")
	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", db, "--root", root + ":managed:waxbin-native", "doctor"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	want := "enrichment:     enabled"
	out := stdout.String()
	i := strings.Index(out, want)
	if i < 0 {
		t.Fatalf("doctor output has no enabled enrichment line:\n%s", out)
	}
	line := "\n                phases: artist, release-group, album-release, group-art, album-art, book, lyrics\n"
	if !strings.Contains(out[i:], line) {
		t.Errorf("doctor output lacks the phase line %q under enrichment:\n%s", strings.TrimSpace(line), out)
	}
}

// TestDoctorPrintsLyricsCoverage: the lyrics line counts every track holding lyrics,
// whatever supplied them, and the tracks a lookup found none for.
func TestDoctorPrintsLyricsCoverage(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	files := map[string][]byte{
		"song.mp3": testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(1)),
		"song.lrc": []byte("[00:01.00]la la\n"),
		"break.mp3": testaudio.BuildMP3WithAudio("Instrumental Break", "Artist", "Album", 2,
			testaudio.AudioWithSeed(2)),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/artist":
			_, _ = w.Write([]byte(`{"artists":[]}`))
		case "/release-group":
			_, _ = w.Write([]byte(`{"release-groups":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mb.Close)
	lrc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instrumental":true,"syncedLyrics":null,"plainLyrics":null}`))
	}))
	t.Cleanup(lrc.Close)
	off := false
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Enrichment: config.EnrichConfig{
			Contact: "t@e.com", MusicBrainzBaseURL: mb.URL, LRCLibBaseURL: lrc.URL,
			CoverArt: &off, CommunityGenres: &off, MatchReleases: &off,
		},
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	// A failure below still releases the catalog; the close before the command is the
	// one that matters, and a second close is a no-op.
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := lib.Enrich(ctx, waxbin.EnrichOptions{}); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", db, "--root", root + ":managed:waxbin-native", "doctor"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if want := "\nlyrics:         1 of 2 tracks (1 looked up, none found)\n"; !strings.Contains(stdout.String(), want) {
		t.Errorf("doctor output lacks %q:\n%s", strings.TrimSpace(want), stdout.String())
	}
}
