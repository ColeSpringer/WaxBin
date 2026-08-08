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
// An absent key (NULL) is mapped to the canonical unknown sentinel. A spec is a
// function of the dimension alone, except on the user-scoped dimensions, where
// joinArgs also carries the calling user's id (see facetSpecFor).
type facetSpec struct {
	// join is the extra join(s) this dimension needs, aliased to avoid clashing with
	// itemJoins. Empty when the dimension reads the item base directly (year, kind) or
	// an alias itemJoins already binds (album reads alb, releaseGroup reads rg; a
	// second join on the same condition is a per-row seek SQLite does not elide). The
	// facet tests are what catch an itemJoins alias rename, since it fails at runtime.
	join     string
	joinArgs []any  // bind args for join's placeholders (e.g. the tag key), before WHERE args
	groupBy  string // GROUP BY expression
	keyExpr  string // machine key (NULL => unknown bucket)
	display  string // display label (NULL => unknown bucket)
	sortExpr string // ORDER BY expression (NULLs sort last)
	entity   bool   // keyExpr is an entity pid (drilldown target)
	unknown  string // sentinel display when the dimension is absent
	// kindWhere scopes the dimension to the item kinds it is meaningful for, ANDed
	// onto the caller's WHERE ("" for a kind-agnostic dimension). The five music
	// dimensions exclude episodes ("pi.kind <> 'episode'"), which carry no
	// artist/album/genre/year in the music sense and would otherwise pile into one
	// Unknown bucket; the podcast dimension is the converse, keeping only episodes.
	// It holds the predicate rather than a flag because the two scopes are mutually
	// exclusive and a pair of booleans could encode a state that means nothing.
	kindWhere string
}

// Kind scopes shared by the facet specs. notEpisodes is the music dimensions' scope;
// onlyEpisodes is the podcast dimension's.
const (
	notEpisodes  = "pi.kind <> 'episode'"
	onlyEpisodes = "pi.kind = 'episode'"
)

// facetSpecFor returns the SQL recipe for one dimension. userID is the resolved
// calling user and is read only by the dimensions read.GroupBy.UserScoped reports,
// which today is GroupPlaylist alone; every other spec ignores it and is a pure
// function of g. TestFacetSpecUserScopedMatchesFlag pins that correspondence, so a
// new user-scoped dimension cannot silently bind a zero id and return an empty
// bucket set.
func facetSpecFor(g read.GroupBy, userID int64) (facetSpec, bool) {
	switch g {
	case read.GroupGenre:
		return facetSpec{
			join:    " LEFT JOIN item_genre fig ON fig.item_id = pi.id LEFT JOIN genre fg ON fg.id = fig.genre_id",
			groupBy: "fg.id", keyExpr: "fg.pid", display: "fg.name", sortExpr: "fg.sort_key",
			entity: true, unknown: read.NoGenre, kindWhere: notEpisodes,
		}, true
	case read.GroupArtist:
		// itemArtistIDExpr COALESCEs the book author so an audiobook groups under
		// its author in the same artist facet a track groups under its artist
		// (itemJoins provides bk). The expr is shared with the artist_pid query
		// field, so a bucket's EntityPID and a pid filter can never disagree.
		return facetSpec{
			join:    " LEFT JOIN artist fa ON fa.id = " + itemArtistIDExpr,
			groupBy: itemArtistIDExpr, keyExpr: "fa.pid", display: "fa.name", sortExpr: "fa.sort_key",
			entity: true, unknown: read.UnknownArtist, kindWhere: notEpisodes,
		}, true
	case read.GroupCreditArtist:
		// The many-per-item shape genre already has: the join fans a track across one
		// bucket per credited artist, so counts sum above the item count. Scoped to
		// role='artist' because that is the only role a scan writes; a book's author
		// shares the table and already has a bucket under GroupArtist.
		return facetSpec{
			join: " LEFT JOIN item_contributor fic ON fic.item_id = pi.id AND fic.role = 'artist'" +
				" LEFT JOIN artist fca ON fca.id = fic.artist_id",
			groupBy: "fca.id", keyExpr: "fca.pid", display: "fca.name", sortExpr: "fca.sort_key",
			entity: true, unknown: read.UnknownArtist, kindWhere: notEpisodes,
		}, true
	case read.GroupAlbumArtist:
		// Shares itemAlbumArtistIDExpr with the album_artist_pid query field (see
		// GroupArtist).
		return facetSpec{
			join:    " LEFT JOIN artist faa ON faa.id = " + itemAlbumArtistIDExpr,
			groupBy: itemAlbumArtistIDExpr, keyExpr: "faa.pid", display: "faa.name", sortExpr: "faa.sort_key",
			entity: true, unknown: read.UnknownArtist, kindWhere: notEpisodes,
		}, true
	case read.GroupAlbum:
		// Albums are track-only (t.album_id), so a book or episode has no album and
		// falls into the [Non-Album] bucket, consistent with the album_pid query
		// field a bucket's EntityPID drills down through. kindWhere keeps episodes
		// out entirely rather than piling them into that bucket.
		return facetSpec{
			groupBy: "t.album_id", keyExpr: "alb.pid", display: "alb.title", sortExpr: "alb.sort_key",
			entity: true, unknown: read.NonAlbum, kindWhere: notEpisodes,
		}, true
	case read.GroupReleaseGroup:
		// The dimension above album: a record's editions under one release group.
		// Track-only and episode-excluded like GroupAlbum. A track with an album but no
		// release group lands in [No Release Group], which is not [Non-Album].
		// Reads the rg alias itemJoins binds, like album reads alb.
		return facetSpec{
			groupBy: "alb.release_group_id", keyExpr: "rg.pid", display: "rg.title", sortExpr: "rg.sort_key",
			entity: true, unknown: read.NoReleaseGroup, kindWhere: notEpisodes,
		}, true
	case read.GroupYear:
		// Deliberately narrower than itemYearExpr, which the year field and item view
		// use: every music dimension here pairs with a wider field the same way (an
		// `--artist "My Show"` filter finds a podcast's episodes; this has no such
		// bucket). read.Stats is built from these and is music-scoped throughout, its
		// Items count being tracks alone, so adding ep.year would change what
		// `facet --group-by year` and `stats` mean rather than close a drift.
		return facetSpec{
			groupBy: "COALESCE(t.year, bk.year)", keyExpr: "CAST(COALESCE(t.year, bk.year) AS TEXT)",
			display: "CAST(COALESCE(t.year, bk.year) AS TEXT)", sortExpr: "COALESCE(t.year, bk.year)",
			unknown: read.UnknownYear, kindWhere: notEpisodes,
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
	case read.GroupPodcast:
		// The episode's feed, keyed by pid so a bucket drills down through the
		// podcast_pid query field. There is no unknown bucket because keyExpr cannot be
		// null: episode.podcast_id is NOT NULL, and the inner join then drops any row
		// that somehow failed to reach a feed rather than rendering it as a bucket with
		// a blank label. The join shape carries that guarantee, not the foreign key
		// alone, which is the same reason the tag dimension joins inner.
		return facetSpec{
			join:    " INNER JOIN podcast fpod ON fpod.id = ep.podcast_id",
			groupBy: "ep.podcast_id", keyExpr: "fpod.pid", display: "fpod.title", sortExpr: "fpod.sort_key",
			entity: true, kindWhere: onlyEpisodes,
		}, true
	case read.GroupPlaylist:
		// The only dimension whose bucket set varies by user: a playlist contributes
		// when the caller owns it or it is shared. That visibility clause is a listing
		// filter and not an access boundary. It matches what ListPlaylists applies,
		// while PlaylistByPID, PlaylistItems and every mutator stay unscoped and
		// model.Playlist defers ACLs past v1.0, so nothing multi-tenant should be built
		// on it.
		//
		// The measured plan, with no ANALYZE (which is production and every fixture),
		// drives off a full scan of playlist_item and applies the visibility filter at
		// the fpl rowid seek, so the cost is O(all playlist entries) plus three temp
		// b-trees. That is still the cheapest dimension here, because playlist_item is
		// small next to playable_item. Reordering the join text changes nothing, since
		// SQLite reorders freely; only a CROSS JOIN with playlist leading the FROM
		// clause forces the seek, and facetSpec appends after itemJoins by design.
		//
		// No unknown bucket, for three reasons and the third is decisive: it matches the
		// tag dimension's membership shape, a LEFT JOIN would turn the playlist_item
		// drive into a full playable_item scan, and under owner scoping "[Not in a
		// Playlist]" would mean "in no playlist you can see", making a catalog-shaped
		// number user-varying. The complement stays expressible as `playlist_pid
		// isMissing`, which is catalog-wide (see the itemFields header).
		//
		// Static only, structurally: a smart playlist stores no playlist_item rows and
		// every membership writer goes through staticPlaylistIDTx, so no kind guard is
		// needed. Bucket labels can collide across owners because playlist has no
		// sort_key, so display and sort are both the name and the caller's private
		// "Favorites" renders like someone else's shared one; EntityPID disambiguates.
		// ListPlaylists avoids this by ordering on the owner name and carrying
		// OwnerName, which read.Bucket has no room for.
		//
		// The shared COUNT(DISTINCT pi.id) means an item held at two positions counts
		// once here, where CountPlaylistItems counts entries. See the count triangle on
		// playlist.CountItems.
		//
		// kindWhere is empty: a playlist holds tracks, books and episodes alike.
		return facetSpec{
			join: " INNER JOIN playlist_item fpli ON fpli.item_id = pi.id" +
				" INNER JOIN playlist fpl ON fpl.id = fpli.playlist_id" +
				" AND (fpl.owner_user_id = ? OR fpl.visibility = 'shared')",
			joinArgs: []any{userID},
			groupBy:  "fpl.id", keyExpr: "fpl.pid", display: "fpl.name", sortExpr: "fpl.name",
			entity: true,
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
//
// userPID also selects the bucket set on a dimension read.GroupBy.UserScoped
// reports, which is GroupPlaylist alone: two users faceting the same query by
// playlist see different playlists. On every other dimension the buckets are a
// function of the query, so userPID reaches only the per-user filters.
//
// order selects the bucket order (empty = collation order; see read.FacetOrder) and
// limit truncates the result (<= 0 = every bucket). Together they are the top-N
// shelf: `facet artist --order count --limit 5`.
//
// Understand what limit does and does not cost. A facet aggregates the whole match
// set no matter what: the GROUP BY, the COUNT(DISTINCT), and the sort of every
// bucket all run in full, and limit bounds only the rows returned. That is also why
// paging a facet is not offered: a cursor would re-run the entire aggregation for
// every page. For an index over a large dimension, enumerate the entities instead
// (EntityPage), which is O(page) off the entity table's sort_key index.
func (s *Store) Facet(ctx context.Context, q query.Query, g read.GroupBy, order read.FacetOrder, limit int, userPID model.PID) (*read.FacetResult, error) {
	const op = "store.Facet"
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	if !g.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported group-by: "+string(g))
	}
	if !order.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported facet order: "+string(order))
	}
	c, err := query.Compile(q, fm)
	if err != nil {
		return nil, err
	}
	userJoin, leadArgs, err := s.userStateJoin(ctx, c, userPID, op)
	if err != nil {
		return nil, err
	}

	// Resolve the user id only for a dimension whose bucket set needs it. A read-only
	// catalog never opened read-write has no user row (ensureDefaultUser runs on a
	// read-write Open), and faceting by genre has to keep working there, which is the
	// same reason userStateJoin resolves one only when the compiled query asks.
	//
	// Every argument-shaped error is already returned above, so a caller passing both a
	// bogus dimension and a bogus user gets CodeInvalid naming the dimension rather
	// than "no such user", which would send them after the wrong argument.
	var userID int64
	if g.UserScoped() {
		id, uerr := userIDByPID(ctx, s.read, userPID, op)
		if uerr != nil {
			return nil, uerr
		}
		userID = id
	}
	spec, ok := facetSpecFor(g, userID)
	if !ok {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported group-by: "+string(g))
	}
	where := andWhere(andWhere(c.Where, entityPredicate(q.Entity)), spec.kindWhere)
	if where == "" {
		where = "1=1"
	}

	// The label order's three terms: the unknown bucket last, then the dimension's
	// sort key, then its display as a final tiebreak. The count order prefixes the
	// descending count onto the same three, so ties stay in collation order and the
	// result is deterministic rather than whatever order the grouping happened to
	// produce.
	orderBy := fmt.Sprintf("(%s IS NULL), %s, %s", spec.sortExpr, spec.sortExpr, spec.display)
	if order == read.FacetOrderCount {
		orderBy = "COUNT(DISTINCT pi.id) DESC, " + orderBy
	}

	// Arg order follows the statement's clause order: the user join's ON clause (its
	// user id, in leadArgs) comes right after itemJoins, then the facet dimension join's
	// placeholders (spec.joinArgs, e.g. a tag key), then the WHERE args (c.Args), and
	// last the limit. A custom-tag facet's join binds the tag key here, which is why
	// the ordering is spelled out rather than assuming the dimension join binds nothing.
	limitClause := ""
	if limit > 0 {
		limitClause = " LIMIT ?"
	}
	stmt := fmt.Sprintf(
		"SELECT %s, %s, COUNT(DISTINCT pi.id)%s%s%s WHERE %s GROUP BY %s ORDER BY %s%s",
		spec.keyExpr, spec.display, itemJoins, userJoin, spec.join, where, spec.groupBy, orderBy, limitClause)

	// Assemble args in clause order: user id (the join ON clause), then the facet
	// dimension join's args (spec.joinArgs, e.g. the tag key), then the WHERE args, and
	// last the limit. A fresh slice is needed because spec.joinArgs sits between
	// leadArgs and c.Args; the sibling readers (QueryItems/CountItems/QueryPage) have no
	// middle args and append c.Args onto leadArgs directly.
	args := make([]any, 0, len(leadArgs)+len(spec.joinArgs)+len(c.Args)+1)
	args = append(args, leadArgs...)
	args = append(args, spec.joinArgs...)
	args = append(args, c.Args...)
	if limitClause != "" {
		args = append(args, limit)
	}
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
