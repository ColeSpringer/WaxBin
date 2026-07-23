package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/spf13/cobra"
)

func newSearchCmd(g *globals) *cobra.Command {
	var (
		limit         int
		maxCandidates int
		libraries     []string
	)
	cmd := &cobra.Command{
		Use:   "search QUERY",
		Short: "Grouped, BM25-ranked search across artists, albums, tracks, books, and episodes",
		Long: "Searches catalog metadata (and podcast transcripts) and returns grouped, " +
			"relevance-ranked results (artists, albums, tracks, books, episodes). Field " +
			"weighting makes a title match outrank an artist/album match, which outranks a " +
			"transcript-body match. Multiple words narrow the result (implicit AND, prefix-matched). " +
			"--max-candidates bounds how many matches are ranked (the newest ones win under " +
			"truncation) and --library scopes the search to items playable from those libraries.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := strings.Join(args, " ")
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			opt := read.SearchOptions{Limit: limit, MaxCandidates: maxCandidates}
			for _, pid := range libraries {
				opt.Libraries = append(opt.Libraries, model.PID(pid))
			}
			res, err := lib.Search(ctx(cmd), q, opt)
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
			printHits(cmd, "EPISODES", res.Episodes)
			if res.Truncated {
				fmt.Fprintln(out(cmd), "(truncated)")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max results per group (0 = default)")
	cmd.Flags().IntVar(&maxCandidates, "max-candidates", 0, "cap the ranked match pool; the newest matches win under truncation (0 = no cap)")
	cmd.Flags().StringArrayVar(&libraries, "library", nil, "scope to items playable from this library pid (repeatable)")
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
