package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newFacetCmd(g *globals) *cobra.Command {
	var (
		groupBy                           string
		title, artist, album, genre, kind string
		year                              int
		rulePath                          string
	)
	cmd := &cobra.Command{
		Use:   "facet --group-by DIM",
		Short: "Group items by a dimension and count each bucket",
		Long: "Groups the items matching the filters (or a --rule document) by one " +
			"dimension and returns each bucket's count. Dimensions: " + groupByList() + ".",
		RunE: func(cmd *cobra.Command, _ []string) error {
			gb := read.GroupBy(groupBy)
			if !gb.Valid() {
				return waxerr.New(waxerr.CodeInvalid, "facet",
					fmt.Sprintf("unknown --group-by %q; valid: %s", groupBy, groupByList()))
			}
			q, err := buildQuery(cmd, rulePath, queryFlags{
				title: title, artist: artist, album: album, genre: genre, kind: kind, year: year,
			})
			if err != nil {
				return err
			}

			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			res, err := lib.Facet(ctx(cmd), q, gb)
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, toFacetView(res))
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintf(tw, "%s\tCOUNT\n", strings.ToUpper(string(res.GroupBy)))
			for _, b := range res.Buckets {
				fmt.Fprintf(tw, "%s\t%d\n", b.Display, b.Count)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "(%d buckets)\n", len(res.Buckets))
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&groupBy, "group-by", "", "facet dimension: "+groupByList())
	f.StringVar(&title, "title", "", "match title (substring)")
	f.StringVar(&artist, "artist", "", "match artist (substring)")
	f.StringVar(&album, "album", "", "match album (substring)")
	f.StringVar(&genre, "genre", "", "match genre (exact)")
	f.StringVar(&kind, "kind", "", "match kind: track|book|episode (exact)")
	f.IntVar(&year, "year", 0, "match year (exact)")
	f.StringVar(&rulePath, "rule", "", "load a JSON rule document (overrides filter flags)")
	_ = cmd.MarkFlagRequired("group-by")
	return cmd
}

func groupByList() string {
	gs := read.GroupBys()
	names := make([]string, len(gs))
	for i, g := range gs {
		names[i] = string(g)
	}
	return strings.Join(names, "|")
}
