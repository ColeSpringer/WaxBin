package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
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
	// userID is the resolved play_state user, set only by the play-derived specs
	// (which have to resolve it to build their join). A filtered browse reuses it
	// instead of resolving the same pid a second time for the query's user join.
	userID *int64
}

// BrowsePage returns one keyset-paginated window of a named discovery list. The
// vocabulary is canonical (read.DiscoveryList); play-derived lists read indexed
// play_state for opt.UserPID (empty selects the default user). Pagination is
// stable under concurrent mutation because each page resumes strictly after the
// cursor's (order value, pid) rather than skipping a fixed offset.
//
// opt.Query narrows the list through the shared query engine (see
// read.BrowseOptions.Query). Only a filtered browse validates opt.UserPID: an
// unfiltered one ignores the field entirely, so a bogus pid stays ignored there
// rather than newly erroring, while a filtered one validates it like Facet and
// QueryPage do.
func (s *Store) BrowsePage(ctx context.Context, list read.DiscoveryList, opt read.BrowseOptions) (*read.Page, error) {
	const op = "store.BrowsePage"
	limit := opt.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	stmt, args, err := s.browseStmt(ctx, list, opt, limit, op)
	if err != nil {
		return nil, err
	}

	rows, err := s.read.QueryContext(ctx, stmt, args...)
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

// browseStmt assembles one page's statement and bind args, asking for one row past
// limit to detect a further page. Split out of BrowsePage so the plan tests EXPLAIN
// the real statement rather than a lookalike that can drift.
func (s *Store) browseStmt(ctx context.Context, list read.DiscoveryList, opt read.BrowseOptions, limit int, op string) (string, []any, error) {
	if !list.Valid() {
		return "", nil, waxerr.New(waxerr.CodeInvalid, op, "unknown discovery list: "+string(list))
	}
	spec, err := s.browseSpecFor(ctx, list, opt)
	if err != nil {
		return "", nil, err
	}
	userJoin, leadArgs, compiled, err := s.browseFilter(ctx, opt, spec.userID, op)
	if err != nil {
		return "", nil, err
	}

	cmp, dir := ">", "ASC"
	if spec.desc {
		cmp, dir = "<", "DESC"
	}

	// Arg order must match clause order in the statement: the user join's ON clause
	// (leadArgs), then the list's own join, then the filter, then the keyset. One
	// hazard this ordering does not make obvious: spec.orderExpr is emitted in the
	// SELECT list before every join, so a spec that ever bound a placeholder there
	// would lead all of these, leadArgs included. Today only random varies and it
	// inlines its seed.
	//
	// The capacity covers every segment plus the trailing three: a keyset cursor's two
	// binds and the limit. Sized up front like Facet's, which assembles the same
	// four-segment shape.
	args := make([]any, 0, len(leadArgs)+len(spec.joinArgs)+len(spec.whereArgs)+len(compiled.args)+3)
	args = append(args, leadArgs...)
	args = append(args, spec.joinArgs...)
	where := andWhere(andWhere(spec.where, compiled.where), compiled.entityWhere)
	args = append(args, spec.whereArgs...)
	args = append(args, compiled.args...)
	if opt.Cursor != "" {
		ord, pid, ok := opt.Cursor.Decode()
		if !ok {
			return "", nil, waxerr.New(waxerr.CodeInvalid, op, "malformed browse cursor")
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
				return "", nil, waxerr.New(waxerr.CodeInvalid, op, "malformed browse cursor")
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
	sb.WriteString(userJoin)
	sb.WriteString(spec.join)
	if where != "" {
		sb.WriteString(" WHERE ")
		sb.WriteString(where)
	}
	fmt.Fprintf(&sb, " ORDER BY ord %s, pi.pid %s LIMIT ?", dir, dir)
	args = append(args, limit+1) // one extra row signals a further page
	return sb.String(), args, nil
}

// browseCompiled is the spliceable form of opt.Query: the compiled WHERE and its
// args, plus the entity's kind predicate kept separate so it ANDs on last. All three
// are empty for an unfiltered browse.
type browseCompiled struct {
	where       string
	entityWhere string
	args        []any
}

// browseFilter compiles opt.Query into the user join, its leading args, and the
// spliceable WHERE. A query with neither an entity nor a where clause contributes
// nothing and short-circuits to empty values, so an unfiltered browse's statement
// stays byte-identical to what it was before filters existed.
//
// Sorts are the one field that neither triggers compilation nor blocks the
// short-circuit. This path ignores them whether or not an entity is named (the list
// owns the order), so a caller reusing a smart-playlist rule doc that carries sorts
// gets the same answer either way rather than an error naming a field it never set.
//
// A where clause with no entity is rejected rather than defaulting to items.
// fieldMapFor's fail-closed default is the injection barrier, and giving the zero
// entity a second meaning would silently accept a caller who meant EntityFiles. The
// error mirrors "by-year browse requires a year" and keeps BrowseOptions' promise
// that its zero value is valid.
//
// Only the query's Entity and Where are compiled. query.Compile validates the limit
// mode before anything else, so passing the whole thing through would reject a
// caller reusing a smart-playlist query with LimitMode random over fields this path
// deliberately ignores.
//
// specUserID, when non-nil, is the play_state user a play-derived spec already
// resolved; reusing it keeps a filtered play browse to one user lookup per page.
// Otherwise the user resolves through userStateJoin, which is called only on the
// filtered path: it validates a non-empty userPID even when the query needs no join,
// so calling it unconditionally would make Browse(alphabetical, {UserPID: "bogus"})
// start erroring where the field is ignored today.
func (s *Store) browseFilter(ctx context.Context, opt read.BrowseOptions, specUserID *int64, op string) (userJoin string, leadArgs []any, out browseCompiled, err error) {
	q := opt.Query
	if q.Entity == "" && q.Where == nil {
		return "", nil, browseCompiled{}, nil
	}
	if q.Entity == "" {
		return "", nil, browseCompiled{}, waxerr.New(waxerr.CodeInvalid, op, "browse query requires an entity")
	}
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return "", nil, browseCompiled{}, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	c, err := query.Compile(query.Query{Entity: q.Entity, Where: q.Where}, fm)
	if err != nil {
		return "", nil, browseCompiled{}, err
	}
	switch {
	case specUserID == nil:
		if userJoin, leadArgs, err = s.userStateJoin(ctx, c, opt.UserPID, op); err != nil {
			return "", nil, browseCompiled{}, err
		}
	case c.NeedsUser:
		// The spec resolved this pid (and so already rejected an unknown user), so the
		// join is built directly rather than resolving it again.
		userJoin, leadArgs = userStateJoinClause, []any{*specUserID}
	}
	// entityPredicate is what narrows the browse to the entity's kind. Without it an
	// EntityTracks query passes fieldMapFor but never reaches pi.kind='track', so the
	// page would carry books and episodes with null track columns.
	return userJoin, leadArgs, browseCompiled{
		where: c.Where, entityWhere: entityPredicate(q.Entity), args: c.Args,
	}, nil
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
		// COALESCE to 0 sorts an undated item last under DESC and keeps the keyset
		// column non-NULL. Episodes are excluded because ep.year is yearOf(pub_date),
		// not a release year: every episode of a year would tie here, and
		// recent-episodes orders the same rows by the full date.
		return browseSpec{
			where:     notEpisodes,
			orderExpr: "COALESCE(" + itemYearExpr + ", 0)",
			orderInt:  true,
			desc:      true,
		}, nil
	case read.ListByYear:
		if opt.Year <= 0 {
			return browseSpec{}, waxerr.New(waxerr.CodeInvalid, op, "by-year browse requires a year")
		}
		// Keeps episodes, unlike newest: this is a filter and an episode genuinely is
		// from the year it was published. The year facet excludes them, like every
		// music dimension; a filter matches what the row displays, a facet does not.
		return browseSpec{
			where: itemYearExpr + " = ?", whereArgs: []any{opt.Year}, orderExpr: "pi.sort_key",
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
	case read.ListRecentEpisodes:
		// Excluding undated episodes rather than coalescing them is what lets this drive
		// off episode_pubdate; the excluded set is real, since manual episodes often
		// arrive with no date. The filter doubles as the kind scope, only an episode
		// having an ep row.
		//
		// Same-pubdate ties break on pid DESC, where EpisodesByPodcast uses pid ASC.
		// BrowsePage emits one direction for both key columns because a mixed-direction
		// keyset cannot use SQLite's row-value comparison.
		return browseSpec{
			where:     "ep.pub_date IS NOT NULL",
			orderExpr: "ep.pub_date",
			orderInt:  true,
			desc:      true,
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
//
// A play-derived list filtered by a per-user field joins play_state twice, as bps
// here and as ps through userStateJoin, and that is correct rather than redundant.
// This join is an inner join because it defines the list's membership: most-played is
// the items that have a play_state row. The filter's join has to be a left join,
// because itemFields coalesces a missing row to 0, which is what makes `played is 0`
// mean "unplayed". Collapsing them into one join would quietly give every filter
// inner semantics and drop every never-played item. The cost is one extra
// primary-key seek per candidate row; the user id is resolved once and shared.
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
		userID:   &userID,
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
