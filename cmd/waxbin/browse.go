package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
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
		rulePath string
		kind     string
	)
	cmd := &cobra.Command{
		Use:   "browse LIST",
		Short: "Page a canonical discovery list",
		Long: "Returns one keyset-paginated window of a discovery list in its canonical " +
			"order. Lists: " + discoveryList() + ". Play-derived lists (most-played, " +
			"recently-played, starred, in-progress) read --user's state; by-year needs --year, " +
			"by-genre needs --genre PID, and random takes a --seed for a stable paginated " +
			"shuffle.\n\n" +
			"--kind scopes a list to one media type, so `recently-added --kind book` is the " +
			"audiobooks added most recently. Most lists span every kind on their own. The two " +
			"that do not are counterparts, each ordering by something only its kinds carry: " +
			"newest covers tracks and books (by release year) and recent-episodes covers " +
			"episodes (by publication date). Asking one of those for a kind it cannot hold is " +
			"an error rather than an empty page.\n\n" +
			"--rule scopes a list to the items a JSON rule document matches, using the same " +
			"filter engine `query` uses, so a list can be narrowed to one genre, media type, " +
			"or per-user state while keeping its own ordering. It is mutually exclusive with " +
			"--kind; a rule carries its own kind predicate. A cursor is only valid for the " +
			"exact options it was issued under, --rule and --kind included.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			list := read.DiscoveryList(args[0])
			if !list.Valid() {
				return waxerr.New(waxerr.CodeInvalid, "browse",
					fmt.Sprintf("unknown list %q; valid: %s", args[0], discoveryList()))
			}
			// Only a --rule or --kind browse carries a query; without either, the zero
			// Query leaves the unfiltered path exactly as it was.
			//
			// The kind filter is built here rather than through buildQuery, which reads
			// the command's own --year flag via Changed("year"). On this command --year
			// is the by-year list's parameter, not a filter, so routing through it would
			// AND a `year is 0` predicate onto `by-year --year 2000 --kind track` and
			// return an empty page with no error. The two flags mean different things on
			// the two commands and must not share the builder.
			var rule query.Query
			switch {
			case rulePath != "":
				var err error
				if rule, err = buildQuery(cmd, rulePath, queryFlags{}); err != nil {
					return err
				}
			case kind != "":
				if err := validateKind(kind); err != nil {
					return err
				}
				if err := checkListKind(list, kind); err != nil {
					return err
				}
				rule = query.New(query.EntityItems).Where("kind", query.OpIs, kind).Build()
			}

			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			page, err := lib.Browse(ctx(cmd), list, read.BrowseOptions{
				UserPID: model.PID(user), Year: year, GenrePID: model.PID(genre),
				Seed: seed, Cursor: read.Cursor(cursor), Limit: pageSize, Query: rule,
			})
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, toPageView(page))
			}
			if err := printItemTable(out(cmd), page.Items); err != nil {
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
	f.StringVar(&rulePath, "rule", "", "load a JSON rule document to scope the list")
	f.StringVar(&kind, "kind", "", "scope to one media type ("+kindList()+"): track is music, book audiobooks, episode podcasts")
	// A rule already carries whatever kind predicate it wants, so accepting both would
	// mean silently dropping one. Rejecting the pair also keeps an invalid --kind from
	// going unreported when a rule is present.
	cmd.MarkFlagsMutuallyExclusive("rule", "kind")
	return cmd
}

// checkListKind rejects a --kind the list can never hold, since an empty page reads
// the same as a library holding none of that kind.
func checkListKind(list read.DiscoveryList, kind string) error {
	covers := list.Kinds()
	if covers == nil || slices.Contains(covers, model.Kind(kind)) {
		return nil
	}
	names := make([]string, len(covers))
	for i, k := range covers {
		names[i] = string(k)
	}
	hint := ""
	switch list {
	case read.ListNewest:
		hint = "; recent-episodes orders episodes by publication date"
	case read.ListRecentEpisodes:
		hint = "; newest orders tracks and books by release year"
	}
	return waxerr.New(waxerr.CodeInvalid, "browse", fmt.Sprintf(
		"the %s list holds no %s items, it covers %s%s", list, kind, strings.Join(names, "|"), hint))
}

func discoveryList() string {
	ls := read.DiscoveryLists()
	names := make([]string, len(ls))
	for i, l := range ls {
		names[i] = string(l)
	}
	return strings.Join(names, "|")
}
