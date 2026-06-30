package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// browseSpec is the SQL recipe for one discovery list: the extra join (with its
// own args), the extra filter (with its own args), the single-column order
// expression, whether that column is integer-typed (so the keyset cursor binds an
// int64), and the sort direction. The order expression must never be NULL for a
// matched row, so single-column keyset pagination stays well defined; each spec
// either uses a non-null column or filters NULLs out.
type browseSpec struct {
	join      string
	joinArgs  []any
	where     string // without a leading AND; "" means no filter
	whereArgs []any
	orderExpr string
	orderInt  bool
	desc      bool
}

// BrowsePage returns one keyset-paginated window of a named discovery list. The
// vocabulary is canonical (read.DiscoveryList); play-derived lists read indexed
// play_state for opt.UserPID (empty selects the default user). Pagination is
// stable under concurrent mutation because each page resumes strictly after the
// cursor's (order value, pid) rather than skipping a fixed offset.
func (s *Store) BrowsePage(ctx context.Context, list read.DiscoveryList, opt read.BrowseOptions) (*read.Page, error) {
	const op = "store.BrowsePage"
	if !list.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown discovery list: "+string(list))
	}
	spec, err := s.browseSpecFor(ctx, list, opt)
	if err != nil {
		return nil, err
	}
	limit := opt.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}

	cmp, dir := ">", "ASC"
	if spec.desc {
		cmp, dir = "<", "DESC"
	}

	// Arg order must match clause order in the statement: join, then filter, then
	// the keyset comparison.
	args := append([]any(nil), spec.joinArgs...)
	where := spec.where
	args = append(args, spec.whereArgs...)
	if opt.Cursor != "" {
		ord, pid, ok := opt.Cursor.Decode()
		if !ok {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "malformed browse cursor")
		}
		keyset := fmt.Sprintf("(%s, pi.pid) %s (?, ?)", spec.orderExpr, cmp)
		if where != "" {
			where = "(" + where + ") AND " + keyset
		} else {
			where = keyset
		}
		if spec.orderInt {
			n, perr := strconv.ParseInt(ord, 10, 64)
			if perr != nil {
				return nil, waxerr.New(waxerr.CodeInvalid, op, "malformed browse cursor")
			}
			args = append(args, n, string(pid))
		} else {
			args = append(args, ord, string(pid))
		}
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(spec.orderExpr)
	sb.WriteString(" AS ord, ")
	sb.WriteString(itemViewCols)
	sb.WriteString(itemJoins)
	sb.WriteString(spec.join)
	if where != "" {
		sb.WriteString(" WHERE ")
		sb.WriteString(where)
	}
	fmt.Fprintf(&sb, " ORDER BY ord %s, pi.pid %s LIMIT ?", dir, dir)
	args = append(args, limit+1) // one extra row signals a further page

	rows, err := s.read.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()

	page := &read.Page{}
	var ords []string // parallel to page.Items, for the next cursor
	for rows.Next() {
		// The leading column is the order value; scanPageItem scans a leading string
		// plus the item view, which is exactly this row shape.
		v, ord, err := scanPageItem(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		page.Items = append(page.Items, v)
		ords = append(ords, ord)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
		page.Next = read.EncodeCursor(ords[limit-1], page.Items[limit-1].PID)
	}
	return page, nil
}

// browseSpecFor builds the SQL recipe for a discovery list, resolving the per-list
// parameters (user for play lists, year/genre filters, the shuffle seed).
func (s *Store) browseSpecFor(ctx context.Context, list read.DiscoveryList, opt read.BrowseOptions) (browseSpec, error) {
	const op = "store.BrowsePage"
	switch list {
	case read.ListAlphabetical:
		return browseSpec{orderExpr: "pi.sort_key"}, nil
	case read.ListRecentlyAdded:
		return browseSpec{orderExpr: "pi.created_at", orderInt: true, desc: true}, nil
	case read.ListNewest:
		// Newest release first; an untagged year coalesces to 0 so it sorts last
		// under DESC and the order column is never NULL (keyset stays well defined).
		// The book's year stands in for a track's, matching the field map and facets.
		return browseSpec{orderExpr: "COALESCE(t.year, bk.year, 0)", orderInt: true, desc: true}, nil
	case read.ListByYear:
		if opt.Year <= 0 {
			return browseSpec{}, waxerr.New(waxerr.CodeInvalid, op, "by-year browse requires a year")
		}
		return browseSpec{
			where: "COALESCE(t.year, bk.year) = ?", whereArgs: []any{opt.Year}, orderExpr: "pi.sort_key",
		}, nil
	case read.ListByGenre:
		gid, err := s.genreIDByPID(ctx, opt.GenrePID, op)
		if err != nil {
			return browseSpec{}, err
		}
		return browseSpec{
			where:     "EXISTS (SELECT 1 FROM item_genre big WHERE big.item_id = pi.id AND big.genre_id = ?)",
			whereArgs: []any{gid},
			orderExpr: "pi.sort_key",
		}, nil
	case read.ListRandom:
		// The seed is an int we control, so inlining it as a literal is injection-safe
		// and lets the identical expression appear in SELECT, ORDER BY, and the keyset.
		return browseSpec{orderExpr: fmt.Sprintf("wb_shuffle(%d, pi.pid)", opt.Seed), orderInt: true}, nil
	case read.ListMostPlayed, read.ListRecentlyPlayed, read.ListStarred:
		return s.playBrowseSpec(ctx, list, opt.UserPID, op)
	}
	return browseSpec{}, waxerr.New(waxerr.CodeInvalid, op, "unknown discovery list: "+string(list))
}

// playBrowseSpec builds the recipe for a per-user play-derived list: an inner join
// to the user's play_state plus the list's NULL-excluding filter and order column.
func (s *Store) playBrowseSpec(ctx context.Context, list read.DiscoveryList, userPID model.PID, op string) (browseSpec, error) {
	userID, err := userIDByPID(ctx, s.read, userPID, op)
	if err != nil {
		return browseSpec{}, err
	}
	spec := browseSpec{
		join:     " JOIN play_state bps ON bps.item_id = pi.id AND bps.user_id = ?",
		joinArgs: []any{userID},
		orderInt: true,
		desc:     true,
	}
	switch list {
	case read.ListMostPlayed:
		spec.where, spec.orderExpr = "bps.play_count > 0", "bps.play_count"
	case read.ListRecentlyPlayed:
		spec.where, spec.orderExpr = "bps.last_played_at IS NOT NULL", "bps.last_played_at"
	case read.ListStarred:
		spec.where, spec.orderExpr = "bps.starred_at IS NOT NULL", "bps.starred_at"
	}
	return spec, nil
}

// genreIDByPID resolves a genre pid to its rowid, or CodeNotFound/CodeInvalid.
func (s *Store) genreIDByPID(ctx context.Context, pid model.PID, op string) (int64, error) {
	if pid == "" {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "by-genre browse requires a genre pid")
	}
	var id int64
	err := s.read.QueryRowContext(ctx, "SELECT id FROM genre WHERE pid = ?", string(pid)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such genre: "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, nil
}
