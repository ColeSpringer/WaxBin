package sqlite

import (
	"context"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// itemFields whitelists the logical fields a query over items/tracks may
// reference, mapping each to a column expression in the items SELECT (aliases:
// pi=playable_item, t=track, bk=book, srs=series, f=primary file, ps=the current
// user's play_state). A field absent here is rejected by the compiler, which is
// what keeps untrusted names out of SQL.
//
// The artist/album_artist/album/genre/year columns COALESCE the track values with
// the book's author/series/year and the podcast's title, mirroring itemViewCols, so
// a filter or sort over the items entity matches a book or episode by the same
// values the row displays (e.g. `--artist Tolkien` or `--year 1937` finds an
// audiobook; `--album "My Show"` finds its episodes). The track-only entity
// excludes books/episodes via entityPredicate, so for it these still resolve to the
// track columns.
//
// The user-state fields (starred, rating, play_count, and the rest) read the ps
// alias and carry NeedsUser. When a query references one, the compiler sets
// Compiled.NeedsUser and the store binds the current user's play_state with a LEFT
// JOIN (see userStateJoin). The LEFT join keeps unplayed and unrated items visible,
// so `played is 0`, `rating isMissing`, and "never play disliked" all work.
//
// The two groups handle NULL differently, which changes how you query them.
//
// play_count, played, finished, and starred coalesce a missing play_state row to 0
// (unplayed). That is what lets `played is 0`, `play_count is 0`, and `starred is 0`
// match an item with no row, which is how you ask for "unplayed". The tradeoff is
// that these expressions are never NULL, so isMissing and isPresent are useless on
// them (isMissing never matches, isPresent always does). Use `is 0` or `gt 0`.
//
// rating and last_played stay raw NULL, so isMissing and isPresent do work there: an
// unrated or never-played item reads NULL. Write "never play disliked" as
// `rating isMissing OR rating gt N`. A plain `rating lte N` drops unrated items,
// since a comparison against NULL is never true.
var itemFields = query.FieldMap{
	"pid":          {Expr: "pi.pid", Kind: query.KindText},
	"kind":         {Expr: "pi.kind", Kind: query.KindText},
	"state":        {Expr: "pi.state", Kind: query.KindText},
	"title":        {Expr: "pi.title", Kind: query.KindText},
	"sort_key":     {Expr: "pi.sort_key", Kind: query.KindText},
	"added":        {Expr: "pi.created_at", Kind: query.KindTime},
	"created_at":   {Expr: "pi.created_at", Kind: query.KindTime},
	"updated_at":   {Expr: "pi.updated_at", Kind: query.KindTime},
	"artist":       {Expr: "COALESCE(NULLIF(t.artist,''), bk.author, pod.title, '')", Kind: query.KindText},
	"album_artist": {Expr: "COALESCE(NULLIF(t.album_artist,''), bk.author, pod.title, '')", Kind: query.KindText},
	"albumartist":  {Expr: "COALESCE(NULLIF(t.album_artist,''), bk.author, pod.title, '')", Kind: query.KindText},
	"album":        {Expr: "COALESCE(NULLIF(t.album,''), srs.name, pod.title, '')", Kind: query.KindText},
	"podcast":      {Expr: "COALESCE(pod.title, '')", Kind: query.KindText},
	"genre":        {Expr: "COALESCE(NULLIF(t.genre,''), bk.genre, '')", Kind: query.KindText},
	"year":         {Expr: "COALESCE(t.year, bk.year, ep.year)", Kind: query.KindInt},
	"track":        {Expr: "t.track_no", Kind: query.KindInt},
	"track_no":     {Expr: "t.track_no", Kind: query.KindInt},
	"disc":         {Expr: "t.disc_no", Kind: query.KindInt},
	"disc_no":      {Expr: "t.disc_no", Kind: query.KindInt},
	"season":       {Expr: "ep.season", Kind: query.KindInt},
	"published":    {Expr: "ep.pub_date", Kind: query.KindTime},
	"source":       {Expr: "COALESCE(acq.source_type, pod.source_type, 'local')", Kind: query.KindText},
	"duration_ms":  {Expr: "COALESCE(bk.total_duration_ms, " + itemEffectiveDurationExpr + ", ep.duration_ms)", Kind: query.KindInt},
	"codec":        {Expr: "f.codec", Kind: query.KindText},
	"container":    {Expr: "f.container", Kind: query.KindText},
	"path":         {Expr: "f.display_path", Kind: query.KindText},

	// Per-user playback state (bound via userStateJoin when referenced).
	"starred":     {Expr: "CASE WHEN ps.starred_at IS NOT NULL THEN 1 ELSE 0 END", Kind: query.KindInt, NeedsUser: true},
	"starred_at":  {Expr: "ps.starred_at", Kind: query.KindTime, NeedsUser: true},
	"rating":      {Expr: "ps.rating", Kind: query.KindInt, NeedsUser: true},
	"play_count":  {Expr: "COALESCE(ps.play_count, 0)", Kind: query.KindInt, NeedsUser: true},
	"played":      {Expr: "COALESCE(ps.played, 0)", Kind: query.KindInt, NeedsUser: true},
	"finished":    {Expr: "COALESCE(ps.finished, 0)", Kind: query.KindInt, NeedsUser: true},
	"last_played": {Expr: "ps.last_played_at", Kind: query.KindTime, NeedsUser: true},
}

// userStateJoinClause binds the current user's play_state as the ps alias. The user
// predicate belongs in the JOIN ON clause, not in WHERE. Putting it in WHERE would
// quietly turn the LEFT join into an INNER join, dropping unplayed items, and would
// also let one user match another user's row. In the ON clause each item matches
// only this user's row or NULL. The join uses play_state's (user_id, item_id)
// primary key, so it resolves by index seek and needs no extra index.
const userStateJoinClause = " LEFT JOIN play_state ps ON ps.item_id = pi.id AND ps.user_id = ?"

// userStateJoin returns the play_state LEFT JOIN clause and the leading bind args
// (the resolved user id) for a caller to prepend to its own query args. It produces
// a join only when the compiled query references a user-state field. An empty
// userPID means the default user, matching the rest of the API.
//
// A caller builds its full arg list with append(leadArgs, c.Args...). Because the
// join's ON clause comes before WHERE in the statement, the user id has to bind
// first, so it goes at the front. Keeping that ordering in one place stops the four
// callers (QueryItems, CountItems, Facet, QueryPage) from each getting it wrong.
//
// A query with no user-state field needs no join, but a non-empty userPID is still
// looked up so a typo returns "no such user" rather than silently falling back to
// default-scoped results. The default user ("") is always valid and skips the lookup.
func (s *Store) userStateJoin(ctx context.Context, c *query.Compiled, userPID model.PID, op string) (clause string, leadArgs []any, err error) {
	if !c.NeedsUser {
		if userPID != "" {
			if _, err := userIDByPID(ctx, s.read, userPID, op); err != nil {
				return "", nil, err
			}
		}
		return "", nil, nil
	}
	uid, err := userIDByPID(ctx, s.read, userPID, op)
	if err != nil {
		return "", nil, err
	}
	return userStateJoinClause, []any{uid}, nil
}

// tagFields is the query field resolver for the item/track entities. It resolves the
// static itemFields plus dynamic tag.<KEY> custom-tag fields, which compile to a
// correlated EXISTS over item_tag. It is the injection barrier for tag queries: an
// unknown static field, or a tag key that is not canonical or is reserved (owned by a
// scalar/credit/identifier surface), is rejected by returning false. The canonical key
// is bound as a positional arg, never inlined, precisely because model.CanonicalTagKey
// legally permits SQL metacharacters (quote, semicolon, --) in a key.
//
// The correlated subquery keys on pi.id, which is valid for both tracks and books, so a
// tag.<KEY> filter works on either kind. On the tracks alias entityPredicate still adds
// pi.kind='track', so a book carrying the tag is excluded there, consistent with every
// other field on that alias. The CLI defaults to EntityItems (all kinds), so --tag on the
// CLI matches tracks, books, and episodes alike.
type tagFields struct{ static query.FieldMap }

func (f tagFields) Column(field string) (query.Column, bool) {
	if c, ok := f.static[field]; ok {
		return c, true
	}
	raw, ok := model.CutTagPrefix(field)
	if !ok {
		return query.Column{}, false
	}
	canon, ok := model.CanonicalTagKey(raw)
	if !ok || model.IsReservedTagKey(canon) {
		return query.Column{}, false
	}
	return query.Column{Set: &query.SetColumn{
		Sub:       "SELECT 1 FROM item_tag itq WHERE itq.item_id = pi.id AND itq.key = ?",
		ValueExpr: "itq.value",
		Args:      []any{canon}, // bound, never inlined: a canonical key may hold SQL metacharacters
	}}, true
}

// fieldMapFor returns the query field resolver for an entity and whether it is
// supported. items and tracks share the item view (and gain tag.<KEY> fields). Other
// entities are not queryable here and report false so callers can reject them rather
// than silently return item rows.
func fieldMapFor(e query.Entity) (query.Fields, bool) {
	switch e {
	case query.EntityItems, query.EntityTracks:
		return tagFields{static: itemFields}, true
	default:
		return nil, false
	}
}

// entityPredicate returns an extra WHERE predicate scoping a compiled item query to
// its entity's kind, or "" for the kind-agnostic items entity. The tracks entity is
// the music alias: now that the shared item view LEFT JOINs the book subtype, a
// bare query would otherwise return book rows with NULL track columns, so tracks is
// constrained to kind='track'.
func entityPredicate(e query.Entity) string {
	if e == query.EntityTracks {
		return "pi.kind = 'track'"
	}
	return ""
}

// andWhere combines two SQL boolean expressions with AND, tolerating an empty one.
func andWhere(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return "(" + a + ") AND " + b
	}
}
