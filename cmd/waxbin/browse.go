package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newBrowseCmd(g *globals) *cobra.Command {
	var (
		user     string
		year     int
		genre    string
		seed     int64
		pageSize int
		cursor   string
	)
	cmd := &cobra.Command{
		Use:   "browse LIST",
		Short: "Page a canonical discovery list",
		Long: "Returns one keyset-paginated window of a discovery list in its canonical " +
			"order. Lists: " + discoveryList() + ". Play-derived lists (most-played, " +
			"recently-played, starred) read --user's state; by-year needs --year, by-genre " +
			"needs --genre PID, and random takes a --seed for a stable paginated shuffle.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			list := read.DiscoveryList(args[0])
			if !list.Valid() {
				return waxerr.New(waxerr.CodeInvalid, "browse",
					fmt.Sprintf("unknown list %q; valid: %s", args[0], discoveryList()))
			}

			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			page, err := lib.Browse(ctx(cmd), list, read.BrowseOptions{
				UserPID: model.PID(user), Year: year, GenrePID: model.PID(genre),
				Seed: seed, Cursor: read.Cursor(cursor), Limit: pageSize,
			})
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, toPageView(page))
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PID\tTITLE\tARTIST\tALBUM\tTRK\tYEAR")
			for _, v := range page.Items {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\n",
					v.PID, v.Title, v.Artist, v.Album, v.TrackNo, v.Year)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "(%d items)\n", len(page.Items))
			if page.HasMore {
				fmt.Fprintf(out(cmd), "next cursor: %s\n", page.Next)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&user, "user", "", "user pid for play-derived lists (empty = default user)")
	f.IntVar(&year, "year", 0, "year for the by-year list")
	f.StringVar(&genre, "genre", "", "genre pid for the by-genre list")
	f.Int64Var(&seed, "seed", 0, "stable shuffle seed for the random list")
	f.IntVar(&pageSize, "page-size", 0, "rows per page (0 = default)")
	f.StringVar(&cursor, "cursor", "", "keyset cursor from a prior page's nextCursor")
	return cmd
}

func discoveryList() string {
	ls := read.DiscoveryLists()
	names := make([]string, len(ls))
	for i, l := range ls {
		names[i] = string(l)
	}
	return strings.Join(names, "|")
}
