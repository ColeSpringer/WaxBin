package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

// TestEnrichScopeFlagValidation ensures the scope flag-shape errors fire in the
// command itself, before a server is dialed or the catalog opened (and its
// write lock taken); the facade re-validates for embedders and the proxy.
// Validation happens with no database configured, so reaching it proves the
// early path.
func TestEnrichScopeFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"both scopes", []string{"--item", "01J0X", "--entity", "artist:01J0Y"}, "not both"},
		{"malformed entity", []string{"--entity", "artistonly"}, "wants type:pid"},
		{"non-enrichable entity type", []string{"--entity", "genre:01J0Y"}, "non-enrichable entity type"},
		{"unknown phase", []string{"--force-phase", "nope"}, "unknown enrichment phase"},
		{"retired aux-art key", []string{"--force-phase", "aux-art"}, "unknown enrichment phase \"aux-art\" (want one of artist|release-group|album-release|group-art|"},
		{"phase with force", []string{"--force", "--force-phase", "artist"}, "exclusive"},
		{"phase with a scope", []string{"--item", "01J0X", "--force-phase", "lyrics"}, "cannot combine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newEnrichCmd(&globals{})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("args %v: expected a validation error, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("args %v: error = %q, want it to mention %q", tc.args, err, tc.want)
			}
		})
	}
}

// TestEnrichSummaryCoversEveryPhase: every phase that spends the --limit budget has to
// appear in the text summary, or a capped run reads as having done less than it did. The
// gated phases print only when they walked something, so this drives a result with all of
// them non-zero and checks each line is there. The album fields phase was added without
// its line, which is what this catches.
func TestEnrichSummaryCoversEveryPhase(t *testing.T) {
	res := &waxbin.EnrichResult{Result: enrich.Result{
		ArtistsEnriched: 1, ReleaseGroupsEnriched: 1, AlbumsSearched: 1, BooksEnriched: 1,
		LyricsEnriched: 1, GroupArtEnriched: 1, ArtistArtEnriched: 1, AlbumArtEnriched: 1,
		TrackFieldsEnriched: 1, BookFieldsEnriched: 1, AlbumFieldsEnriched: 1,
		ArtFetched: 1, ArtReused: 1,
	}}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := renderEnrichResult(cmd, &globals{}, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"artists:", "release groups:", "album releases:", "books:", "lyrics:",
		"group art:", "artist art:", "album art:", "track fields:", "book fields:", "album fields:",
		"reused from the group cover",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("summary is missing the %q line:\n%s", want, buf.String())
		}
	}
}

// TestEnrichViewOmitsArtReusedAtZero: the reuse count is new, so a payload from a run
// that reused nothing has to keep the shape it had.
func TestEnrichViewOmitsArtReusedAtZero(t *testing.T) {
	zero, err := json.Marshal(toEnrichView(&waxbin.EnrichResult{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(zero), "artReused") {
		t.Errorf("zero payload = %s, want no artReused key", zero)
	}
	one, err := json.Marshal(toEnrichView(&waxbin.EnrichResult{Result: enrich.Result{ArtReused: 1}}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(one), `"artReused":1`) {
		t.Errorf("payload = %s, want artReused 1", one)
	}
}

// TestEnrichSummaryReportsDeferred: a run that left lookups owed says how many targets it
// left queued, in the text summary and the JSON view, and a run with none keeps the
// shape it had.
func TestEnrichSummaryReportsDeferred(t *testing.T) {
	render := func(r enrich.Result) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := renderEnrichResult(cmd, &globals{}, &waxbin.EnrichResult{Result: r}); err != nil {
			t.Fatalf("render: %v", err)
		}
		return buf.String()
	}
	if out := render(enrich.Result{LyricsEnriched: 3, Deferred: 2}); !strings.Contains(out, "deferred:       2 left queued for the next pass\n") {
		t.Errorf("summary lacks the deferred line:\n%s", out)
	}
	if out := render(enrich.Result{LyricsEnriched: 3}); strings.Contains(out, "deferred:") {
		t.Errorf("summary with nothing deferred prints a deferred line:\n%s", out)
	}
	payload, err := json.Marshal(toEnrichView(&waxbin.EnrichResult{Result: enrich.Result{Deferred: 2}}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"deferred":2`) {
		t.Errorf("payload = %s, want deferred 2", payload)
	}
	zero, err := json.Marshal(toEnrichView(&waxbin.EnrichResult{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(zero), "deferred") {
		t.Errorf("zero payload = %s, want no deferred key", zero)
	}
}

// TestEnrichSummaryReportsStalled: a phase that ran out of live providers is named, so a
// run that stopped short of its targets does not read as having finished them.
func TestEnrichSummaryReportsStalled(t *testing.T) {
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	res := &waxbin.EnrichResult{Result: enrich.Result{LyricsEnriched: 3,
		Stalled: []model.EnrichPhase{model.EnrichPhaseLyrics, model.EnrichPhaseAlbumArt}}}
	if err := renderEnrichResult(cmd, &globals{}, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "stalled:        lyrics, album-art (every provider serving them was out of this pass)\n"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("summary lacks the stalled line:\n%s", buf.String())
	}
	payload, err := json.Marshal(toEnrichView(res))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"stalled":["lyrics","album-art"]`) {
		t.Errorf("payload = %s, want the stalled phases", payload)
	}
	zero, err := json.Marshal(toEnrichView(&waxbin.EnrichResult{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(zero), "stalled") {
		t.Errorf("zero payload = %s, want no stalled key", zero)
	}
}
