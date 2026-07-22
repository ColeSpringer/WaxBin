package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// facetSpec is the SQL recipe for one faceting dimension: how to join the
// dimension to the item base, what to group by, and how to render each bucket.
// An absent key (NULL) is mapped to the canonical unknown sentinel.
type facetSpec struct {
	join     string // extra join(s), aliased to avoid clashing with itemJoins
	joinArgs []any  // bind args for join's placeholders (e.g. the tag key), before WHERE args
	groupBy  string // GROUP BY expression
	keyExpr  string // machine key (NULL => unknown bucket)
	display  string // display label (NULL => unknown bucket)
	sortExpr string // ORDER BY expression (NULLs sort last)
	entity   bool   // keyExpr is an entity pid (drilldown target)
	unknown  string // sentinel display when the dimension is absent
	// noEpisodes excludes podcast episodes from this dimension: they carry no
	// artist/album/genre/year in the music sense, so including them would pile every
	// episode into a single Unknown bucket. The kind facet keeps them (it groups them
	// correctly under 'episode').
	noEpisodes bool
}

func facetSpecFor(g read.GroupBy) (facetSpec, bool) {
	switch g {
	case read.GroupGenre:
		return facetSpec{
			join:    " LEFT JOIN item_genre fig ON fig.item_id = pi.id LEFT JOIN genre fg ON fg.id = fig.genre_id",
			groupBy: "fg.id", keyExpr: "fg.pid", display: "fg.name", sortExpr: "fg.sort_key",
			entity: true, unknown: read.NoGenre, noEpisodes: true,
		}, true
	case read.GroupArtist:
		// itemArtistIDExpr COALESCEs the book author so an audiobook groups under
		// its author in the same artist facet a track groups under its artist
		// (itemJoins provides bk). The expr is shared with the artist_pid query
		// field, so a bucket's EntityPID and a pid filter can never disagree.
		return facetSpec{
			join:    " LEFT JOIN artist fa ON fa.id = " + itemArtistIDExpr,
			groupBy: itemArtistIDExpr, keyExpr: "fa.pid", display: "fa.name", sortExpr: "fa.sort_key",
			entity: true, unknown: read.UnknownArtist, noEpisodes: true,
		}, true
	case read.GroupAlbumArtist:
		// Shares itemAlbumArtistIDExpr with the album_artist_pid query field (see
		// GroupArtist).
		return facetSpec{
			join:    " LEFT JOIN artist faa ON faa.id = " + itemAlbumArtistIDExpr,
			groupBy: itemAlbumArtistIDExpr, keyExpr: "faa.pid", display: "faa.name", sortExpr: "faa.sort_key",
			entity: true, unknown: read.UnknownArtist, noEpisodes: true,
		}, true
	case read.GroupYear:
		return facetSpec{
			groupBy: "COALESCE(t.year, bk.year)", keyExpr: "CAST(COALESCE(t.year, bk.year) AS TEXT)",
			display: "CAST(COALESCE(t.year, bk.year) AS TEXT)", sortExpr: "COALESCE(t.year, bk.year)",
			unknown: read.UnknownYear, noEpisodes: true,
		}, true
	case read.GroupKind:
		return facetSpec{
			groupBy: "pi.kind", keyExpr: "pi.kind", display: "pi.kind", sortExpr: "pi.kind",
		}, true
	case read.GroupLibrary:
		// The primary backing file's library, keyed by pid (drilldown pairs with
		// the `library` query field) and displayed by root. A fileless item, such
		// as an undownloaded episode, has a NULL f.library_id and lands in the
		// "[No File]" bucket, so episodes stay included. The library table has no
		// sort_key; display_root is the stable human order.
		return facetSpec{
			join:    " LEFT JOIN library flib ON flib.id = f.library_id",
			groupBy: "flib.id", keyExpr: "flib.pid", display: "flib.display_root", sortExpr: "flib.display_root",
			entity: true, unknown: read.NoFile,
		}, true
	}
	// A custom-tag dimension: group items by the values of one tag key. The INNER JOIN
	// means only items carrying the key contribute (correct for a value browse
	// dimension), and a multi-value item is counted once per distinct value via the
	// shared COUNT(DISTINCT pi.id). The key is bound (joinArgs), never inlined, for the
	// same reason the query resolver binds it: a canonical key may hold SQL
	// metacharacters. Value buckets are BINARY/case-sensitive (only tag keys are
	// canonicalized, not values), consistent with the equality query path.
	if key, ok := read.TagGroupKey(g); ok {
		// The `itf.value <> ''` guard mirrors the query presence predicate: it keeps the
		// value dimension independent of the write-path invariant that empty values are
		// never stored, so a stray empty value could never render as a blank-labeled
		// bucket (this dimension has no unknown sentinel).
		return facetSpec{
			join:     " INNER JOIN item_tag itf ON itf.item_id = pi.id AND itf.key = ? AND itf.value <> ''",
			joinArgs: []any{key},
			groupBy:  "itf.value", keyExpr: "itf.value", display: "itf.value", sortExpr: "itf.value",
		}, true
	}
	return facetSpec{}, false
}

// Facet groups the items matching q by one dimension and counts each group. It
// reuses the shared query engine's WHERE so `facet --group-by genre` honors the
// same filters as a plain query; q's sort/limit/offset/limit-mode are ignored (a
// facet is an aggregation over the full match set, not a row window). A filter
// over a per-user field scopes to userPID's play_state (empty selects the
// default user).
func (s *Store) Facet(ctx context.Context, q query.Query, g read.GroupBy, userPID model.PID) (*read.FacetResult, error) {
	const op = "store.Facet"
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	spec, ok := facetSpecFor(g)
	if !ok {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported group-by: "+string(g))
	}
	c, err := query.Compile(q, fm)
	if err != nil {
		return nil, err
	}
	userJoin, leadArgs, err := s.userStateJoin(ctx, c, userPID, op)
	if err != nil {
		return nil, err
	}
	where := andWhere(c.Where, entityPredicate(q.Entity))
	if spec.noEpisodes {
		where = andWhere(where, "pi.kind <> 'episode'")
	}
	if where == "" {
		where = "1=1"
	}

	// Arg order follows the statement's clause order: the user join's ON clause (its
	// user id, in leadArgs) comes right after itemJoins, then the facet dimension join's
	// placeholders (spec.joinArgs, e.g. a tag key), then the WHERE args (c.Args). A
	// custom-tag facet's join binds the tag key here, which is why the ordering is
	// spelled out rather than assuming the dimension join binds nothing.
	stmt := fmt.Sprintf(
		"SELECT %s, %s, COUNT(DISTINCT pi.id)%s%s%s WHERE %s GROUP BY %s ORDER BY (%s IS NULL), %s, %s",
		spec.keyExpr, spec.display, itemJoins, userJoin, spec.join, where, spec.groupBy,
		spec.sortExpr, spec.sortExpr, spec.display)

	// Assemble args in clause order: user id (the join ON clause), then the facet
	// dimension join's args (spec.joinArgs, e.g. the tag key), then the WHERE args. A
	// fresh slice is needed because spec.joinArgs sits between leadArgs and c.Args; the
	// sibling readers (QueryItems/CountItems/QueryPage) have no middle args and append
	// c.Args onto leadArgs directly.
	args := make([]any, 0, len(leadArgs)+len(spec.joinArgs)+len(c.Args))
	args = append(args, leadArgs...)
	args = append(args, spec.joinArgs...)
	args = append(args, c.Args...)
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()

	res := &read.FacetResult{GroupBy: g}
	for rows.Next() {
		var key, display sql.NullString
		var count int
		if err := rows.Scan(&key, &display, &count); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		b := read.Bucket{Key: key.String, Display: display.String, Count: count}
		if !key.Valid {
			b.IsUnknown = true
			b.Display = spec.unknown
			b.Key = ""
		} else if spec.entity {
			b.EntityPID = model.PID(key.String)
		}
		res.Buckets = append(res.Buckets, b)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return res, nil
}

// TagKeys returns every custom-tag key in the catalog with the number of distinct
// items carrying it, most-used first (ties broken by key). It is the "which tag.<KEY>
// browse dimensions exist" discovery primitive: a consumer lists these, then facets or
// filters on the ones it wants. A multi-valued tag on one item still counts that item
// once (COUNT(DISTINCT item_id)). Keys are stored canonical, so no folding is needed.
func (s *Store) TagKeys(ctx context.Context) ([]read.TagKeyCount, error) {
	const op = "store.TagKeys"
	rows, err := s.read.QueryContext(ctx,
		"SELECT key, COUNT(DISTINCT item_id) AS n FROM item_tag GROUP BY key ORDER BY n DESC, key")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []read.TagKeyCount
	for rows.Next() {
		var tk read.TagKeyCount
		if err := rows.Scan(&tk.Key, &tk.Count); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, tk)
	}
	return out, rows.Err()
}

const defaultPageSize = 100

// QueryPage returns one keyset-paginated window of items in collation-correct
// order (the generated sort_key, then pid as a tiebreak). Pagination is stable
// under concurrent mutation because it resumes strictly after the cursor row
// rather than skipping a fixed offset. q's own sort/limit/offset/limit-mode are
// ignored; the canonical sort_key ordering owns the page. A non-empty but
// malformed cursor is rejected rather than silently restarting.
func (s *Store) QueryPage(ctx context.Context, q query.Query, cursor read.Cursor, limit int, desc bool, userPID model.PID) (*read.Page, error) {
	const op = "store.QueryPage"
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	c, err := query.Compile(q, fm)
	if err != nil {
		return nil, err
	}
	userJoin, leadArgs, err := s.userStateJoin(ctx, c, userPID, op)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultPageSize
	}

	// leadArgs (the join user id, or empty) leads the args: its ON clause precedes
	// WHERE and the keyset comparison.
	args := append(leadArgs, c.Args...)
	where := andWhere(c.Where, entityPredicate(q.Entity))
	cmp := ">"
	order := "ASC"
	if desc {
		cmp, order = "<", "DESC"
	}
	if cursor != "" {
		sk, pid, decodeOK := cursor.Decode()
		if !decodeOK {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "malformed page cursor")
		}
		// SQLite row-value comparison: (a, b) > (x, y) is exactly
		// a > x OR (a = x AND b > y), but the planner can drive it off an index
		// directly, and it needs only two binds.
		keyset := fmt.Sprintf("(pi.sort_key, pi.pid) %s (?, ?)", cmp)
		if where != "" {
			where = "(" + where + ") AND " + keyset
		} else {
			where = keyset
		}
		args = append(args, sk, string(pid))
	}

	var sb strings.Builder
	sb.WriteString(pageItemSelect)
	sb.WriteString(userJoin)
	if where != "" {
		sb.WriteString(" WHERE ")
		sb.WriteString(where)
	}
	fmt.Fprintf(&sb, " ORDER BY pi.sort_key %s, pi.pid %s LIMIT ?", order, order)
	args = append(args, limit+1) // fetch one extra to detect a further page

	rows, err := s.read.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()

	page := &read.Page{}
	var sortKeys []string // parallel to page.Items, for building the next cursor
	for rows.Next() {
		v, sortKey, err := scanPageItem(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		page.Items = append(page.Items, v)
		sortKeys = append(sortKeys, sortKey)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// We fetched limit+1 rows; the extra one only signals a further page. The
	// next cursor must point at the last *returned* row, not the dropped probe.
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
		page.Next = read.EncodeCursor(sortKeys[limit-1], page.Items[limit-1].PID)
	}
	return page, nil
}
