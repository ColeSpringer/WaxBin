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
		// COALESCE the book author so an audiobook groups under its author in the
		// same artist facet a track groups under its artist (itemJoins provides bk).
		return facetSpec{
			join:    " LEFT JOIN artist fa ON fa.id = COALESCE(t.artist_id, bk.author_id)",
			groupBy: "COALESCE(t.artist_id, bk.author_id)", keyExpr: "fa.pid", display: "fa.name", sortExpr: "fa.sort_key",
			entity: true, unknown: read.UnknownArtist, noEpisodes: true,
		}, true
	case read.GroupAlbumArtist:
		return facetSpec{
			join:    " LEFT JOIN artist faa ON faa.id = COALESCE(t.album_artist_id, bk.author_id)",
			groupBy: "COALESCE(t.album_artist_id, bk.author_id)", keyExpr: "faa.pid", display: "faa.name", sortExpr: "faa.sort_key",
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
	}
	return facetSpec{}, false
}

// Facet groups the items matching q by one dimension and counts each group. It
// reuses the shared query engine's WHERE so `facet --group-by genre` honors the
// same filters as a plain query; q's sort/limit/offset are ignored (a facet is
// an aggregation, not a row window).
func (s *Store) Facet(ctx context.Context, q query.Query, g read.GroupBy) (*read.FacetResult, error) {
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
	where := andWhere(c.Where, entityPredicate(q.Entity))
	if spec.noEpisodes {
		where = andWhere(where, "pi.kind <> 'episode'")
	}
	if where == "" {
		where = "1=1"
	}

	stmt := fmt.Sprintf(
		"SELECT %s, %s, COUNT(DISTINCT pi.id)%s%s WHERE %s GROUP BY %s ORDER BY (%s IS NULL), %s, %s",
		spec.keyExpr, spec.display, itemJoins, spec.join, where, spec.groupBy,
		spec.sortExpr, spec.sortExpr, spec.display)

	rows, err := s.read.QueryContext(ctx, stmt, c.Args...)
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

const defaultPageSize = 100

// QueryPage returns one keyset-paginated window of items in collation-correct
// order (the generated sort_key, then pid as a tiebreak). Pagination is stable
// under concurrent mutation because it resumes strictly after the cursor row
// rather than skipping a fixed offset. q's own sort/limit/offset are ignored;
// the canonical sort_key ordering owns the page. A non-empty but malformed
// cursor is rejected rather than silently restarting.
func (s *Store) QueryPage(ctx context.Context, q query.Query, cursor read.Cursor, limit int, desc bool) (*read.Page, error) {
	const op = "store.QueryPage"
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	c, err := query.Compile(q, fm)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultPageSize
	}

	args := append([]any(nil), c.Args...)
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
