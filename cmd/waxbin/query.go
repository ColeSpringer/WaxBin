package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newQueryCmd(g *globals) *cobra.Command {
	var (
		title, artist, album, genre, kind string
		year, limit                       int
		sortField                         string
		desc                              bool
		rulePath                          string
	)
	cmd := &cobra.Command{
		Use:     "query",
		Aliases: []string{"ls"},
		Short:   "Select items with the shared query engine",
		Long: "Builds a query from flags (or a JSON rule document via --rule) and " +
			"returns matching items. Text flags match by substring; year/kind/genre match exactly.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			q, err := buildQuery(cmd, rulePath, queryFlags{
				title: title, artist: artist, album: album, genre: genre, kind: kind,
				year: year, limit: limit, sortField: sortField, desc: desc,
			})
			if err != nil {
				return err
			}

			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			items, err := lib.Query(ctx(cmd), q)
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, itemViews(items))
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PID\tTITLE\tARTIST\tALBUM\tTRK\tYEAR")
			for _, v := range items {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\n",
					v.PID, v.Title, v.Artist, v.Album, v.TrackNo, v.Year)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "(%d items)\n", len(items))
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&title, "title", "", "match title (substring)")
	f.StringVar(&artist, "artist", "", "match artist (substring)")
	f.StringVar(&album, "album", "", "match album (substring)")
	f.StringVar(&genre, "genre", "", "match genre (exact)")
	f.StringVar(&kind, "kind", "", "match kind: track|book|episode (exact)")
	f.IntVar(&year, "year", 0, "match year (exact)")
	f.IntVar(&limit, "limit", 0, "limit results (0 = no limit)")
	f.StringVar(&sortField, "sort", "", "sort field (e.g. title, artist, year)")
	f.BoolVar(&desc, "desc", false, "sort descending")
	f.StringVar(&rulePath, "rule", "", "load a JSON rule document (overrides filter flags)")
	return cmd
}

type queryFlags struct {
	title, artist, album, genre, kind string
	year, limit                       int
	sortField                         string
	desc                              bool
}

// buildQuery constructs a query from a --rule file (if given) or from flags.
func buildQuery(cmd *cobra.Command, rulePath string, qf queryFlags) (query.Query, error) {
	if rulePath != "" {
		data, err := os.ReadFile(rulePath)
		if err != nil {
			return query.Query{}, waxerr.Wrapf(waxerr.CodeIO, "query", err, "reading rule %s", rulePath)
		}
		return query.ParseRule(data)
	}

	b := query.New(query.EntityItems)
	if qf.title != "" {
		b.Where("title", query.OpContains, qf.title)
	}
	if qf.artist != "" {
		b.Where("artist", query.OpContains, qf.artist)
	}
	if qf.album != "" {
		b.Where("album", query.OpContains, qf.album)
	}
	if qf.genre != "" {
		b.Where("genre", query.OpIs, qf.genre)
	}
	if qf.kind != "" {
		b.Where("kind", query.OpIs, qf.kind)
	}
	if cmd.Flags().Changed("year") {
		b.Where("year", query.OpIs, qf.year)
	}
	if qf.sortField != "" {
		b.OrderBy(qf.sortField, qf.desc)
	}
	if qf.limit > 0 {
		b.Limit(qf.limit)
	}
	return b.Build(), nil
}
