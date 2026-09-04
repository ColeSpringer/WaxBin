package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/enrich"
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
		LyricsEnriched: 1, AuxArtEnriched: 1, ArtistArtEnriched: 1,
		TrackFieldsEnriched: 1, BookFieldsEnriched: 1, AlbumFieldsEnriched: 1,
	}}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := renderEnrichResult(cmd, &globals{}, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"artists:", "release groups:", "album releases:", "books:", "lyrics:",
		"aux art:", "artist art:", "track fields:", "book fields:", "album fields:",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("summary is missing the %q line:\n%s", want, buf.String())
		}
	}
}
