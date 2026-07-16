package main

import (
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/spf13/cobra"
)

func newEnrichCmd(g *globals) *cobra.Command {
	var force bool
	var limit int
	cmd := &cobra.Command{
		Use:   "enrich",
		Short: "Enrich catalog metadata from MusicBrainz and key-free providers",
		Long: "Runs the resumable enrichment pass: resolves release groups and artists " +
			"against MusicBrainz (MBID-first), populates the release-group type, artist " +
			"aliases/relations, genres for untagged items (merged with community genres from " +
			"ListenBrainz), release-group cover art from the Cover Art Archive, and lyrics for " +
			"tracks that have none from LRCLIB. It is lock-respecting (never overwriting a " +
			"tagged or user-locked field), records the provider behind each value, and degrades " +
			"gracefully offline. Requires a MusicBrainz contact (config enrichment.contact or " +
			"WAXBIN_ENRICH_CONTACT). The optional AcoustID fallback additionally needs an API " +
			"key and fpcalc. An embedder can inject further providers via Options.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.open(cmd) // mutating: takes the write lock
			if err != nil {
				return err
			}
			defer lib.Close()

			res, err := lib.Enrich(ctx(cmd), waxbin.EnrichOptions{Force: force, Limit: limit})
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toEnrichView(res))
			}
			w := out(cmd)
			r := res.Result
			fmt.Fprintf(w, "artists:        %d enriched (%d matched)\n", r.ArtistsEnriched, r.ArtistsMatched)
			fmt.Fprintf(w, "release groups: %d enriched (%d matched)\n", r.ReleaseGroupsEnriched, r.ReleaseGroupsMatched)
			fmt.Fprintf(w, "books:          %d enriched (%d matched)\n", r.BooksEnriched, r.BooksMatched)
			fmt.Fprintf(w, "lyrics:         %d looked up (%d matched)\n", r.LyricsEnriched, r.LyricsMatched)
			fmt.Fprintf(w, "cover art:      %d fetched\n", r.ArtFetched)
			fmt.Fprintf(w, "job:            %s\n", res.JobPID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "re-enrich entities already looked up")
	cmd.Flags().IntVar(&limit, "limit", 0, "cap the number of entities processed (0 = all)")
	return cmd
}
