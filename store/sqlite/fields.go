package sqlite

import (
	"context"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// itemArtistIDExpr / itemAlbumArtistIDExpr resolve an item's effective artist /
// album-artist entity id, COALESCEing the book author so an audiobook carries its
// author where a track carries its artist. Both the artist facet specs
// (facetSpecFor) and the artist_pid/album_artist_pid query fields consume the
// same expression, so a facet bucket's EntityPID and a pid filter can never
// disagree about which entity an item belongs to.
const (
	itemArtistIDExpr      = "COALESCE(t.artist_id, bk.author_id)"
	itemAlbumArtistIDExpr = "COALESCE(t.album_artist_id, bk.author_id)"
)

// itemYearExpr is an item's effective release year: a track's, else a book's, else an
// episode's (derived from its pub date). Shared by the year field, itemViewCols, and
// the newest/by-year specs so they cannot disagree. The year facet deliberately does
// not use it; see GroupYear.
const itemYearExpr = "COALESCE(t.year, bk.year, ep.year)"

// itemArtSlotExpr selects the art_map entity_type slot an item's own art lives
// under. Tracks and books both store item art under the 'track' slot (the shared
// attachArtTx path treats the entity id as the playable_item id), but an
// episode's cover attaches under 'episode' (attachEntityArtTx(..., "episode",
// itemID, ...) on download). Any read predicate probing item-own art must switch
// the slot by kind through this expression; a track-only predicate would read 0
// for every covered episode.
const itemArtSlotExpr = "CASE WHEN pi.kind='episode' THEN 'episode' ELSE 'track' END"

// The value-side lowering subqueries behind the entity-handle query fields: each
// resolves one caller-supplied pid to the internal id the item row carries, and is
// emitted where the value's placeholder would otherwise go (query.Column.ValueSub).
// Each holds exactly one ?, which binds the pid; the SQL around it is constant, so
// no caller text ever reaches the statement.
const (
	artistIDByPIDSub       = "(SELECT apq.id FROM artist apq WHERE apq.pid = ?)"
	albumIDByPIDSub        = "(SELECT albq.id FROM album albq WHERE albq.pid = ?)"
	releaseGroupIDByPIDSub = "(SELECT rgq.id FROM release_group rgq WHERE rgq.pid = ?)"
	podcastIDByPIDSub      = "(SELECT podq.id FROM podcast podq WHERE podq.pid = ?)"
	libraryIDByPIDSub      = "(SELECT libq.id FROM library libq WHERE libq.pid = ?)"
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
// play_count, played, finished, starred, and position_ms coalesce a missing
// play_state row to 0 (unplayed). That is what lets `played is 0`, `play_count is 0`,
// and `starred is 0` match an item with no row, which is how you ask for "unplayed".
// The tradeoff is that these expressions are never NULL, so isMissing and isPresent
// are useless on them (isMissing never matches, isPresent always does). Use `is 0`
// or `gt 0`.
//
// position_ms is the in-progress predicate: `position_ms gt 0` with `finished is 0`,
// which excludes a finished item whose position was never reset.
//
// rating, last_played, and last_progress stay raw NULL, so isMissing and isPresent do
// work there: an unrated, never-played, or never-touched item reads NULL. Write "never
// play disliked" as `rating isMissing OR rating gt N`. A plain `rating lte N` drops
// unrated items, since a comparison against NULL is never true. The relative-time
// operators follow the same contract: `last_played notInTheLast <window>` matches NULL
// ("not played in 30 days" includes never-played), while `inTheLast` never matches NULL.
//
// last_progress is the wider of the two playback stamps: a play moves both, while a
// progress checkpoint moves only this one, so a checkpointed-but-never-played item
// reads NULL for last_played and non-NULL here. A star or a rating moves neither (they
// move updated_at), which is what makes last_progress the ordering key for "where was I".
//
// The entity-handle fields filter by normalized-entity identity instead of display
// text, so a facet drilldown can query by the bucket's EntityPID. There are nine, and
// they split two ways: artist_pid, album_artist_pid, album_pid, release_group_pid,
// podcast_pid, and library are scalar columns lowered on the value side, while
// genre_pid, credit_artist_pid, and playlist_pid are set fields, over item_genre,
// item_contributor, and playlist_item, because those dimensions hold many rows per
// item (the paragraphs below say why they stay set fields).
//
// The artist exprs share itemArtistIDExpr/itemAlbumArtistIDExpr with the facet specs,
// so a facet bucket's EntityPID and a pid filter can never disagree (a book matches by
// its author, like the artist facet). album_pid and release_group_pid are track-only
// (NULL for books/episodes), podcast_pid is episode-only, and library is the primary
// file's library pid (NULL for a fileless item, so `library isMissing` matches
// undownloaded episodes). release_group_pid reads the album joined by itemJoins, so
// it is additionally NULL for a track whose album carries no release group:
// isMissing there covers all three of no album, no release group, and not a track.
//
// Value-side lowering (query.Column.ValueSub) means the compiled column is the item
// row's internal id and the caller's pid is resolved by a subquery on the other side
// of the comparison. Doing it the other way round, a per-row correlated subquery
// projecting each item's pid, forced a full scan and one pid lookup per row.
// Lowering resolves the pid once per query and lets the planner
// use the id column's index where one exists: podcast_pid seeks episode_podcast,
// album_pid seeks track_album_id, and library reaches file_library. Those three
// narrow the outer loop, so they cost O(matches).
//
// artist_pid, album_artist_pid, and release_group_pid still scan playable_item; what
// they gain from lowering is N pid lookups collapsing to one, not an indexed seek.
// The artist pair scan because their COALESCE across track and book has no single
// index. release_group_pid scans despite the joined alb alias looking indexed: the
// plan is SCAN pi with a per-row alb probe, and a plan line naming album_rg is that
// probe, not a narrowing drive. Only
// is/isNot/in/notIn/isPresent/isMissing are accepted on a lowered field, sorting by one
// is rejected, and isNot against a pid no entity holds matches nothing, because the
// lowered value is NULL. See Column.ValueSub.
//
// The indexed seeks above are `is` and `in` only. `in` seeks the same index `is` does
// at every arity, including one, and an empty list matches nothing. notIn is an
// anti-join and scans at any arity, on every one of the six, so a deny-list costs
// O(catalog) even on podcast_pid and library.
//
// in and notIn are not complements over an item with no handle at all. notIn keeps
// such a row, so `library notIn [every root]` returns every fileless item (an
// undownloaded episode) while `library in [every root]` returns none. That is the
// deny-list contract a stale entry needs; see query.Column.ValueSub. A caller
// scoping visibility by library has to decide what a fileless item means to it.
//
// genre_pid, credit_artist_pid, and playlist_pid stay set fields (set-field
// semantics: isNot is a deny-list, ordered operators are rejected) and are
// deliberately not converted. Their EXISTS correlates on pi.id whatever side the
// value sits, so lowering would save the per-row join but not the scan, and it would
// need a second lowering mechanism on SetColumn for no plan change. playlist_pid has
// no scalar form to lower to at all: an item carries no playlist id column.
//
// playlist_pid matches static playlist membership only. A smart playlist stores no
// playlist_item rows, so its pid never matches, which is also what keeps the field
// non-recursive: a smart rule can reference a playlist without a rule ever evaluating
// a rule. Matching nothing for a wrong-kind pid is this family's existing contract,
// the same one stated above for a pid no entity holds. A compiler check is not
// available, since query.Compile is storage-agnostic by design (see query/query.go)
// and the value is an untyped any, so checking would mean a database round trip
// inside the query layer. There is no CLI flag to check in either: like album_mbid,
// this reaches users through --rule and smart playlists. A consumer building a rule
// editor should offer only static playlists in its picker.
//
// isNot and notIn are the deny-list here, inherited from set-field semantics, so an
// item in no playlist matches `playlist_pid isNot P`. That is exactly the shape
// "tracks not in my Archive list" needs.
//
// isPresent and isMissing are catalog-wide, not caller-scoped. They compile to
// EXISTS/NOT EXISTS over the whole playlist_item table, so on a multi-user catalog
// `playlist_pid isPresent` is true for an item sitting only in another user's private
// playlist, and isMissing excludes it. The usability trap is the sharper edge: "tracks
// I have not filed anywhere" is a natural rule and this is not it. The owner-scoped
// form is `playlist_pid notIn [the caller's playlist pids]`, which already works and
// puts the visibility decision where the caller's grants live.
//
// The field is not owner-scoped where the playlist facet is. Scoping it would need the
// user at query.Compile time, but fieldMapFor takes no user and all four callers
// compile before resolving one; threading it in would make every compiled query
// user-dependent and break faceting on a read-only catalog that never ran
// ensureDefaultUser (store.go). A pid is an unguessable ULID, so a pid-targeted filter
// leaks nothing a caller does not already hold.
//
// A rule referencing playlist_pid changes membership when the *referenced* playlist
// changes, which bumps that playlist's updated_at and emits its delta, never the smart
// playlist's. A consumer memoizing a smart playlist's membership on its own UpdatedAt
// was already unsound (a `starred is 1` rule changes membership on a star with no
// playlist write at all); this is one more way.
//
// has_art and has_lyrics are presence probes: EXISTS lowered to 0/1, never NULL,
// so like play_count the presence ops are useless on them; use `is 0` / `is 1`.
// has_art covers only the item's own front cover, through the kind-switched art
// slot (itemArtSlotExpr). Chain-inherited album/artist art reads 0 on purpose,
// since the field exists to find items missing their own cover.
//
// explicit and podcast_explicit are advisory-flag probes, episode-sourced and
// show-sourced. Both COALESCE to 0, so a track or book reads 0; use `is 0` / `is 1`.
// They stay separate because the feed parser reads the channel and item
// <itunes:explicit> independently with no inheritance, and a show routinely marks
// itself once and leaves every episode unmarked. A restricted browse is therefore
// `explicit is 0 AND podcast_explicit is 0`.
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
	// The composer pair and the book author sort mirror their itemViewCols exprs
	// (never NULL; '' for the kinds that lack them), so filter, sort, and display
	// agree. composer_sort/author_sort are the collation keys a sorted list wants.
	"composer":      {Expr: "COALESCE(t.composer,'')", Kind: query.KindText},
	"composer_sort": {Expr: "COALESCE(t.composer_sort,'')", Kind: query.KindText},
	"author_sort":   {Expr: "COALESCE(bk.author_sort,'')", Kind: query.KindText},
	"year":          {Expr: itemYearExpr, Kind: query.KindInt},
	"track":         {Expr: "t.track_no", Kind: query.KindInt},
	"track_no":      {Expr: "t.track_no", Kind: query.KindInt},
	"disc":          {Expr: "t.disc_no", Kind: query.KindInt},
	"disc_no":       {Expr: "t.disc_no", Kind: query.KindInt},
	"season":        {Expr: "ep.season", Kind: query.KindInt},
	"published":     {Expr: "ep.pub_date", Kind: query.KindTime},
	"source":        {Expr: "COALESCE(acq.source_type, pod.source_type, 'local')", Kind: query.KindText},
	"duration_ms":   {Expr: "COALESCE(bk.total_duration_ms, " + itemEffectiveDurationExpr + ", ep.duration_ms)", Kind: query.KindInt},
	"codec":         {Expr: "f.codec", Kind: query.KindText},
	"container":     {Expr: "f.container", Kind: query.KindText},
	"path":          {Expr: "f.display_path", Kind: query.KindText},

	// Entity handles (see the header block): the item's internal id column compared
	// against a subquery that resolves the caller's pid once, except genre_pid, which
	// is a set field over item_genre. Kind describes the Expr, which since the lowering
	// is an integer id column and not the pid text the caller passes. Nothing reads it
	// today (compileValueSubCond handles every operator these fields accept), but
	// declaring KindText would leave a live trap: routed back through the generic path,
	// `library isMissing` would compile the text presence check and test an integer
	// column against ''.
	"artist_pid":       {Expr: itemArtistIDExpr, ValueSub: artistIDByPIDSub, Kind: query.KindInt},
	"album_artist_pid": {Expr: itemAlbumArtistIDExpr, ValueSub: artistIDByPIDSub, Kind: query.KindInt},
	// album_pid stays t.album_id, not alb.id: track_album_id is the better seek and
	// TestLoweredIdentityFieldPlans pins it.
	"album_pid":         {Expr: "t.album_id", ValueSub: albumIDByPIDSub, Kind: query.KindInt},
	"release_group_pid": {Expr: "alb.release_group_id", ValueSub: releaseGroupIDByPIDSub, Kind: query.KindInt},
	"podcast_pid":       {Expr: "ep.podcast_id", ValueSub: podcastIDByPIDSub, Kind: query.KindInt},
	"library":           {Expr: "f.library_id", ValueSub: libraryIDByPIDSub, Kind: query.KindInt},
	"genre_pid": {Set: &query.SetColumn{
		Sub:       "SELECT 1 FROM item_genre igq JOIN genre gq ON gq.id = igq.genre_id WHERE igq.item_id = pi.id",
		ValueExpr: "gq.pid",
	}},
	"credit_artist_pid": {Set: &query.SetColumn{
		Sub: "SELECT 1 FROM item_contributor icq JOIN artist caq ON caq.id = icq.artist_id" +
			" WHERE icq.item_id = pi.id AND icq.role = 'artist'",
		ValueExpr: "caq.pid",
	}},
	"playlist_pid": {Set: &query.SetColumn{
		Sub: "SELECT 1 FROM playlist_item pliq JOIN playlist plq ON plq.id = pliq.playlist_id" +
			" WHERE pliq.item_id = pi.id",
		ValueExpr: "plq.pid",
	}},

	// External identifiers. Each COALESCEs to '' so isPresent and isMissing read as
	// "the catalog knows this id" rather than tripping over the nullable columns behind
	// them, and each shares its expression with itemViewCols so a filter and a displayed
	// value cannot disagree. Only ids reachable from a bound alias are here: an artist
	// MBID would need a correlated subquery in the WHERE, which
	// TestLoweredIdentityFieldPlans rejects anywhere in a compiled item plan. Filter by
	// release_group_pid, which lowers to an indexed id, and read the artist MBIDs off
	// the projected item view.
	//
	// release_group_mbid completes the chain audit's missing_mbid walks. Without it that
	// check is inexpressible as a query on an enriched-but-untagged library, where
	// enrichment resolved release groups and no release id exists to match. The three
	// together still need `kind in (track, book)` to match the audit: an episode is
	// missing all of them and always will be, so the audit excludes it by kind.
	"mbid":               {Expr: "COALESCE(NULLIF(t.mbid,''), bk.mbid, '')", Kind: query.KindText},
	"isrc":               {Expr: "COALESCE(t.isrc,'')", Kind: query.KindText},
	"album_mbid":         {Expr: "COALESCE(NULLIF(alb.mbid,''), bk.mbid, '')", Kind: query.KindText},
	"release_group_mbid": {Expr: "COALESCE(rg.mbid,'')", Kind: query.KindText},

	// The album entity's remaining release columns, off the alb alias itemJoins binds.
	// They compare raw text, which is the trap: a scan stores whatever the tag said while
	// an entity edit normalizes, so `album_country is "US"` misses albums scanned "USA".
	// album_media has no normalizer at all, so "CD", "2xCD", and "CD, Album, Reissue" are
	// three values. Prefer `contains` unless the catalog is uniformly tagged; the
	// enrichment matcher folds separately and shares nothing with these.
	//
	// Deliberately not mirrored into itemViewCols: rows.go documents a per-column cost
	// budget there, and these are entity-scoped values `entity info album` already serves.
	// No CLI flags either, like album_mbid; they reach users through --rule and smart
	// playlists.
	"album_barcode":        {Expr: "COALESCE(alb.barcode,'')", Kind: query.KindText},
	"album_label":          {Expr: "COALESCE(alb.label,'')", Kind: query.KindText},
	"album_catalog_number": {Expr: "COALESCE(alb.catalog_number,'')", Kind: query.KindText},
	"album_media":          {Expr: "COALESCE(alb.media,'')", Kind: query.KindText},
	"album_country":        {Expr: "COALESCE(alb.country,'')", Kind: query.KindText},

	// Presence probes (0/1, never NULL; use `is 0`/`is 1`, not presence ops).
	"has_art": {Expr: "CASE WHEN EXISTS(SELECT 1 FROM art_map amq WHERE amq.entity_type = " + itemArtSlotExpr +
		" AND amq.entity_id = pi.id AND amq.role = 'front') THEN 1 ELSE 0 END", Kind: query.KindInt},
	"has_lyrics": {Expr: "CASE WHEN EXISTS(SELECT 1 FROM lyrics lyq WHERE lyq.item_id = pi.id) THEN 1 ELSE 0 END", Kind: query.KindInt},

	// Advisory flags (see the header). The COALESCE covers the LEFT JOIN's NULL for a
	// non-episode; without it `explicit is 0` would skip every track and book.
	"explicit":         {Expr: "COALESCE(ep.explicit, 0)", Kind: query.KindInt},
	"podcast_explicit": {Expr: "COALESCE(pod.explicit, 0)", Kind: query.KindInt},

	// Per-user playback state (bound via userStateJoin when referenced).
	"starred":     {Expr: "CASE WHEN ps.starred_at IS NOT NULL THEN 1 ELSE 0 END", Kind: query.KindInt, NeedsUser: true},
	"starred_at":  {Expr: "ps.starred_at", Kind: query.KindTime, NeedsUser: true},
	"rating":      {Expr: "ps.rating", Kind: query.KindInt, NeedsUser: true},
	"play_count":  {Expr: "COALESCE(ps.play_count, 0)", Kind: query.KindInt, NeedsUser: true},
	"position_ms": {Expr: "COALESCE(ps.position_ms, 0)", Kind: query.KindInt, NeedsUser: true},
	"played":      {Expr: "COALESCE(ps.played, 0)", Kind: query.KindInt, NeedsUser: true},
	"finished":    {Expr: "COALESCE(ps.finished, 0)", Kind: query.KindInt, NeedsUser: true},
	"last_played": {Expr: "ps.last_played_at", Kind: query.KindTime, NeedsUser: true},
	// The ps alias joins on play_state's primary key, so a filter or sort here cannot
	// use play_state_progress; that index serves the in-progress browse list alone.
	"last_progress": {Expr: "ps.last_progress_at", Kind: query.KindTime, NeedsUser: true},
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
