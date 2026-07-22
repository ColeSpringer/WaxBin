package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newQueryCmd(g *globals) *cobra.Command {
	var (
		title, artist, album, genre, kind, source  string
		year, limit                                int
		sortField                                  string
		desc                                       bool
		rulePath                                   string
		pageSize                                   int
		cursor                                     string
		user                                       string
		tagEq, tagContains, tagPresent, tagMissing []string
		limitMode                                  string
		seed                                       int64
	)
	cmd := &cobra.Command{
		Use:     "query",
		Aliases: []string{"ls"},
		Short:   "Select items with the shared query engine",
		Long: "Builds a query from flags (or a JSON rule document via --rule) and " +
			"returns matching items. Text flags match by substring; year/kind/genre match exactly. " +
			"With --page-size, results are paged in collation-correct order using a keyset --cursor.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The keyset-paged read ignores limit modes entirely (the canonical
			// order owns the page and its window), so an explicit --limit-mode or
			// --seed there is rejected rather than silently returning the full
			// match set unshuffled.
			if (pageSize > 0 || cursor != "") &&
				(cmd.Flags().Changed("limit-mode") || cmd.Flags().Changed("seed")) {
				return waxerr.New(waxerr.CodeInvalid, "query",
					"--limit-mode/--seed do not apply to keyset-paged mode (--page-size/--cursor)")
			}

			q, err := buildQuery(cmd, rulePath, queryFlags{
				title: title, artist: artist, album: album, genre: genre, kind: kind, source: source,
				year: year, limit: limit, sortField: sortField, desc: desc,
				tagEq: tagEq, tagContains: tagContains, tagPresent: tagPresent, tagMissing: tagMissing,
				limitMode: limitMode, seed: seed,
			})
			if err != nil {
				return err
			}

			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			// Keyset pagination mode: stable, collation-correct windows by sort_key.
			// --sort/--limit do not apply here (the canonical order owns the page).
			if pageSize > 0 || cursor != "" {
				return runQueryPage(cmd, g, lib, q, pageSize, cursor, desc, model.PID(user))
			}

			items, err := lib.Query(ctx(cmd), q, model.PID(user))
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
	f.StringVar(&source, "source", "", "match acquisition source: local|rss|youtube|manual (exact)")
	f.IntVar(&year, "year", 0, "match year (exact)")
	f.IntVar(&limit, "limit", 0, "limit results (0 = no limit)")
	f.StringVar(&sortField, "sort", "", "sort field (e.g. title, artist, year)")
	f.BoolVar(&desc, "desc", false, "sort descending")
	f.StringVar(&rulePath, "rule", "", "load a JSON rule document (overrides filter flags)")
	f.IntVar(&pageSize, "page-size", 0, "keyset pagination: rows per page (enables paged mode)")
	f.StringVar(&cursor, "cursor", "", "keyset pagination: cursor from a prior page's nextCursor")
	f.StringVar(&user, "user", "", "user pid for per-user fields (e.g. rating, starred, play_count); empty = default user")
	f.StringArrayVar(&tagEq, "tag", nil, "match a custom tag exactly: KEY=VALUE (repeatable; equality is case-sensitive)")
	f.StringArrayVar(&tagContains, "tag-contains", nil, "match a custom tag by substring: KEY=SUBSTR (repeatable; case-insensitive)")
	f.StringArrayVar(&tagPresent, "tag-present", nil, "require a custom tag key to be present (repeatable)")
	f.StringArrayVar(&tagMissing, "tag-missing", nil, "require a custom tag key to be absent (repeatable)")
	f.StringVar(&limitMode, "limit-mode", "", "interpret --limit as: count|random|minutes|megabytes")
	f.Int64Var(&seed, "seed", 0, "pin the shuffle order for --limit-mode random or a sortless budget mode (0 = fresh per run)")
	return cmd
}

// runQueryPage serves one keyset-paginated window and prints the next cursor.
func runQueryPage(cmd *cobra.Command, g *globals, lib pager, q query.Query, pageSize int, cursor string, desc bool, userPID model.PID) error {
	page, err := lib.QueryPage(ctx(cmd), q, read.Cursor(cursor), pageSize, desc, userPID)
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
}

// pager is the subset of the library used by paged query (eases testing).
type pager interface {
	QueryPage(ctx context.Context, q query.Query, cursor read.Cursor, limit int, desc bool, userPID model.PID) (*read.Page, error)
}

type queryFlags struct {
	title, artist, album, genre, kind, source string
	year, limit                               int
	sortField                                 string
	desc                                      bool
	// Custom-tag filters. Each tagEq/tagContains entry is KEY=VALUE; tagPresent and
	// tagMissing are bare keys. Empty when a command does not expose tag flags (facet),
	// which is why buildQuery ranges over them without gating.
	tagEq, tagContains, tagPresent, tagMissing []string
	// limitMode reinterprets limit (random/minutes/megabytes; "count" and "" are
	// the plain row cap) and seed pins its shuffle order. Zero when a command does
	// not expose the flags (facet ignores modes anyway).
	limitMode string
	seed      int64
}

// tagField builds the tag.<KEY> query field for a user-supplied key, giving a clear
// error at the point of use for each way a key is rejected: empty, malformed, or
// reserved. Reusing the same model helpers the resolver and the tag editor use keeps
// the CLI's notion of a valid custom-tag key identical to the compiler's; the resolver
// remains the ultimate injection barrier, so this only turns its generic "unknown
// field" into an actionable message (a reserved key like tag.ISRC is not a typo).
func tagField(flag, key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", waxerr.New(waxerr.CodeInvalid, "query", flag+" needs a non-empty tag key")
	}
	canon, ok := model.CanonicalTagKey(key)
	if !ok {
		return "", waxerr.New(waxerr.CodeInvalid, "query", flag+" has an invalid tag key: "+key)
	}
	if model.IsReservedTagKey(canon) {
		return "", waxerr.New(waxerr.CodeInvalid, "query",
			flag+": tag key "+canon+" is reserved (WaxBin owns it), not a custom tag")
	}
	return "tag." + key, nil
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
	if qf.source != "" {
		b.Where("source", query.OpIs, qf.source)
	}
	if cmd.Flags().Changed("year") {
		b.Where("year", query.OpIs, qf.year)
	}
	// Custom-tag filters. Split each KEY=VALUE on the FIRST '=' so a value that legally
	// contains '=' survives (e.g. DISCOGS_RELEASE=id=12345 -> key DISCOGS_RELEASE, value
	// id=12345). The tag.<KEY> field passes through the builder opaquely; the resolver
	// uppercases/canonicalizes the key and validates it at Compile, and the value is
	// bound verbatim. Equality is case-sensitive; substring (contains) is
	// case-insensitive, mirroring the scalar text fields.
	for _, kv := range qf.tagEq {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			return query.Query{}, waxerr.New(waxerr.CodeInvalid, "query",
				"--tag needs KEY=VALUE; use --tag-present for presence")
		}
		field, err := tagField("--tag", key)
		if err != nil {
			return query.Query{}, err
		}
		b.Where(field, query.OpIs, val)
	}
	for _, kv := range qf.tagContains {
		key, sub, ok := strings.Cut(kv, "=")
		if !ok {
			return query.Query{}, waxerr.New(waxerr.CodeInvalid, "query",
				"--tag-contains needs KEY=SUBSTR")
		}
		field, err := tagField("--tag-contains", key)
		if err != nil {
			return query.Query{}, err
		}
		b.Where(field, query.OpContains, sub)
	}
	for _, key := range qf.tagPresent {
		field, err := tagField("--tag-present", key)
		if err != nil {
			return query.Query{}, err
		}
		b.WherePresence(field, query.OpIsPresent)
	}
	for _, key := range qf.tagMissing {
		field, err := tagField("--tag-missing", key)
		if err != nil {
			return query.Query{}, err
		}
		b.WherePresence(field, query.OpIsMissing)
	}
	if qf.sortField != "" {
		b.OrderBy(qf.sortField, qf.desc)
	}
	if qf.limit > 0 {
		b.Limit(qf.limit)
	}
	// "count" is the explicit spelling of the default row-cap mode; anything else
	// passes through opaquely for the compiler to validate (fail closed).
	if qf.limitMode != "" && qf.limitMode != "count" {
		b.LimitBy(query.LimitMode(qf.limitMode))
	}
	if qf.seed != 0 {
		b.Seed(qf.seed)
	}
	return b.Build(), nil
}
