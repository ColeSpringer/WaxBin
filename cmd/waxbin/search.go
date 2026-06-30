package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin/read"
	"github.com/spf13/cobra"
)

func newSearchCmd(g *globals) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "search QUERY",
		Short: "Grouped, BM25-ranked search across artists, albums, and tracks",
		Long: "Searches catalog metadata and returns grouped, relevance-ranked results " +
			"(artists, albums, tracks). Field weighting makes a title match outrank an " +
			"artist/album match. Multiple words narrow the result (implicit AND, prefix-matched).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := strings.Join(args, " ")
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			res, err := lib.Search(ctx(cmd), q, read.SearchOptions{Limit: limit})
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, toSearchView(res))
			}
			if res.Empty() {
				fmt.Fprintln(out(cmd), "(no matches)")
				return nil
			}
			printHits(cmd, "ARTISTS", res.Artists)
			printHits(cmd, "ALBUMS", res.Albums)
			printHits(cmd, "TRACKS", res.Tracks)
			printHits(cmd, "BOOKS", res.Books)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max results per group (0 = default)")
	return cmd
}

func printHits(cmd *cobra.Command, header string, hits []read.SearchHit) {
	if len(hits) == 0 {
		return
	}
	fmt.Fprintf(out(cmd), "%s\n", header)
	tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
	for _, h := range hits {
		if h.Subtitle != "" {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", h.PID, h.Title, h.Subtitle)
		} else {
			fmt.Fprintf(tw, "  %s\t%s\t\n", h.PID, h.Title)
		}
	}
	_ = tw.Flush()
}
