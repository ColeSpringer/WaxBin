package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/internal/pathx"
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
		libraries                                  []string
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

			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			q, err := scopedQuery(ctx(cmd), lib, cmd, "query", rulePath, queryFlags{
				title: title, artist: artist, album: album, genre: genre, kind: kind, source: source,
				year: year, limit: limit, sortField: sortField, desc: desc,
				tagEq: tagEq, tagContains: tagContains, tagPresent: tagPresent, tagMissing: tagMissing,
				limitMode: limitMode, seed: seed,
			}, libraries)
			if err != nil {
				return err
			}

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
			if err := printItemTable(out(cmd), items); err != nil {
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
	f.StringVar(&kind, "kind", "", "match kind ("+kindList()+", exact)")
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
	f.StringArrayVar(&libraries, "library", nil, "restrict to a library, by pid or registered root path (repeatable)")
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
	if err := printItemTable(out(cmd), page.Items); err != nil {
		return err
	}
	fmt.Fprintf(out(cmd), "(%d items)\n", len(page.Items))
	if page.HasMore {
		fmt.Fprintf(out(cmd), "next cursor: %s\n", page.Next)
	}
	return nil
}

// validateKind rejects an unknown item kind, failing closed on a typo. An unknown
// kind matches no rows, so without this `--kind traks` reads as an empty library
// rather than a mistake, the same reason the facet group-by and the diagnostic enums
// validate. An empty kind means "any" and is accepted.
func validateKind(kind string) error {
	if kind == "" || model.Kind(kind).Valid() {
		return nil
	}
	return waxerr.New(waxerr.CodeInvalid, "query", "unknown kind "+kind+"; valid: "+kindList())
}

// kindList renders the item-kind vocabulary. Every --kind flag's help string and the
// validation error are built from it, so adding a kind to model.Kinds() updates all
// of them rather than leaving one behind (edit's help said "track|book" for as long
// as it had in fact accepted episodes).
func kindList() string {
	ks := model.Kinds()
	names := make([]string, len(ks))
	for i, k := range ks {
		names[i] = string(k)
	}
	return strings.Join(names, "|")
}

// resolveStateRefs turns each --state ref into an item state, rejecting an unknown
// one so a typo does not read as an empty catalog. The store validates too, for a
// caller reaching Search directly; this runs first so the rejection lands before the
// catalog is opened, and both quote model.ItemStateList. Deduplicated first-seen for
// symmetry with resolveLibraryRefs, though nothing downstream depends on the arity.
func resolveStateRefs(op string, refs []string) ([]model.ItemState, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]model.ItemState, 0, len(refs))
	seen := make(map[model.ItemState]bool, len(refs))
	for _, ref := range refs {
		st := model.ItemState(ref)
		if !st.Valid() {
			return nil, waxerr.New(waxerr.CodeInvalid, op,
				"unknown item state "+ref+"; valid: "+model.ItemStateList())
		}
		if seen[st] {
			continue
		}
		seen[st] = true
		out = append(out, st)
	}
	return out, nil
}

// pager is the subset of the library used by paged query (eases testing).
type pager interface {
	QueryPage(ctx context.Context, q query.Query, cursor read.Cursor, limit int, desc bool, userPID model.PID) (*read.Page, error)
}

// libraryLister is the subset of the library resolveLibraryRefs needs (eases testing).
type libraryLister interface {
	Libraries(ctx context.Context) ([]*model.Library, error)
}

// scopedQuery resolves the --library refs against the catalog and builds the query with
// the resulting pids applied. Resolving and applying live together so they cannot drift:
// a resolve whose result never reaches the query is a filter that silently matches
// everything, which is the failure this flag exists to prevent.
func scopedQuery(ctx context.Context, lister libraryLister, cmd *cobra.Command,
	op, rulePath string, qf queryFlags, refs []string) (query.Query, error) {
	// A rule document owns the whole where-clause, so --library cannot be layered on
	// top of one. Refused rather than ignored: this flag validates its input, so
	// silently dropping it would answer a wider question than the user asked for.
	if rulePath != "" && len(refs) > 0 {
		return query.Query{}, waxerr.New(waxerr.CodeInvalid, op,
			"--library does not combine with --rule; put a library condition in the rule document")
	}
	pids, err := resolveLibraryRefs(ctx, lister, op, refs)
	if err != nil {
		return query.Query{}, err
	}
	qf.library = pids
	return buildQuery(cmd, rulePath, qf)
}

// resolveLibraryRefs turns each --library ref into a library pid, accepting a pid or
// either spelling of a registered root. An unknown ref errors rather than matching
// nothing, so a mistyped root does not read as an empty catalog.
func resolveLibraryRefs(ctx context.Context, lister libraryLister, op string, refs []string) ([]model.PID, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	libs, err := lister.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	// Deduplicated, first-seen order. A pid and its root path are two spellings of one
	// library, and the arity is load-bearing: compileValueSubCond emits the indexed
	// `is` form only at one value, and repeats the whole subquery above that.
	out := make([]model.PID, 0, len(refs))
	seen := make(map[model.PID]bool, len(refs))
	for _, ref := range refs {
		pid, err := matchLibraryRef(libs, op, ref)
		if err != nil {
			return nil, err
		}
		if seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, pid)
	}
	return out, nil
}

// rootSpellings returns the distinct ways a library's root can be written. Every
// writer of the library row builds root and display_root from one string, so the two
// are the same bytes today and this yields one entry; model.Library types them apart
// (raw OS bytes against a UTF-8 rendering) so they are allowed to diverge, which is
// why both are still matched rather than one being picked.
func rootSpellings(l *model.Library) []string {
	if raw := string(l.Root); raw != l.DisplayRoot {
		return []string{l.DisplayRoot, raw}
	}
	return []string{l.DisplayRoot}
}

func matchLibraryRef(libs []*model.Library, op, ref string) (model.PID, error) {
	// Roots are stored absolute, so a relative ref is resolved against the working
	// directory before comparing, the same way `library add` resolves the path it
	// registers. A pid is matched before this ever matters, and Abs on a pid produces
	// a path no root can equal.
	abs := filepath.Clean(ref)
	if a, err := filepath.Abs(ref); err == nil {
		abs = a
	}
	for _, l := range libs {
		if ref == string(l.PID) {
			return l.PID, nil
		}
		for _, root := range rootSpellings(l) {
			if ref == root || abs == filepath.Clean(root) {
				return l.PID, nil
			}
		}
	}
	// A path under a registered root is the mistake a user will actually make. It is
	// not resolved to the parent, which would silently widen a subdirectory filter to
	// the whole library. Containment goes through pathx so this agrees with every
	// other containment check in the repo, including on a sibling like /music/..old.
	for _, l := range libs {
		for _, root := range rootSpellings(l) {
			if root != "" && pathx.UnderRoot(filepath.Clean(root), abs) {
				return "", waxerr.New(waxerr.CodeNotFound, op,
					"no such library: "+ref+"; it is inside root "+root+", so filter by path instead")
			}
		}
	}
	return "", waxerr.New(waxerr.CodeNotFound, op,
		"no such library: "+ref+"; registered: "+libraryRootList(libs))
}

func libraryRootList(libs []*model.Library) string {
	if len(libs) == 0 {
		return "(none)"
	}
	names := make([]string, len(libs))
	for i, l := range libs {
		names[i] = l.DisplayRoot
	}
	return strings.Join(names, ", ")
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
	// library holds already-resolved library pids, since buildQuery is deliberately
	// catalog-free and resolving a root path needs a catalog. Registering the flag on
	// a new command means extending that command's own selection check too; edit.go
	// enumerates these fields by hand.
	library []model.PID
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
		if err := validateKind(qf.kind); err != nil {
			return query.Query{}, err
		}
		b.Where("kind", query.OpIs, qf.kind)
	}
	if qf.source != "" {
		b.Where("source", query.OpIs, qf.source)
	}
	// `in` at arity 1 compiles to what `is` compiles to, so a single --library still
	// seeks file_library.
	if len(qf.library) > 0 {
		b.WhereValues("library", query.OpIn, query.Values(qf.library)...)
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
