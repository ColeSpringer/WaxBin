package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
	line := "\n                phases: artist, release-group, album-release, album-art, book, lyrics\n"
	if !strings.Contains(out[i:], line) {
		t.Errorf("doctor output lacks the phase line %q under enrichment:\n%s", strings.TrimSpace(line), out)
	}
}
