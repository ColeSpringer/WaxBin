package sqlite

import "github.com/colespringer/waxbin/query"

// itemFields whitelists the logical fields a query over items/tracks may
// reference, mapping each to a column expression in the items SELECT (aliases:
// pi=playable_item, t=track, bk=book, srs=series, f=primary file). A field absent
// here is rejected by the compiler, which is what keeps untrusted names out of SQL.
//
// The artist/album_artist/album/genre/year columns COALESCE the track values with
// the book's author/series/year, mirroring itemViewCols, so a filter or sort over
// the items entity matches a book by the same values the row displays (e.g.
// `--artist Tolkien` or `--year 1937` finds an audiobook). The track-only entity
// excludes books via entityPredicate, so for it these still resolve to the track
// columns.
var itemFields = query.FieldMap{
	"pid":          {Expr: "pi.pid", Kind: query.KindText},
	"kind":         {Expr: "pi.kind", Kind: query.KindText},
	"state":        {Expr: "pi.state", Kind: query.KindText},
	"title":        {Expr: "pi.title", Kind: query.KindText},
	"sort_key":     {Expr: "pi.sort_key", Kind: query.KindText},
	"added":        {Expr: "pi.created_at", Kind: query.KindTime},
	"created_at":   {Expr: "pi.created_at", Kind: query.KindTime},
	"updated_at":   {Expr: "pi.updated_at", Kind: query.KindTime},
	"artist":       {Expr: "COALESCE(NULLIF(t.artist,''), bk.author, '')", Kind: query.KindText},
	"album_artist": {Expr: "COALESCE(NULLIF(t.album_artist,''), bk.author, '')", Kind: query.KindText},
	"albumartist":  {Expr: "COALESCE(NULLIF(t.album_artist,''), bk.author, '')", Kind: query.KindText},
	"album":        {Expr: "COALESCE(NULLIF(t.album,''), srs.name, '')", Kind: query.KindText},
	"genre":        {Expr: "COALESCE(NULLIF(t.genre,''), bk.genre, '')", Kind: query.KindText},
	"year":         {Expr: "COALESCE(t.year, bk.year)", Kind: query.KindInt},
	"track":        {Expr: "t.track_no", Kind: query.KindInt},
	"track_no":     {Expr: "t.track_no", Kind: query.KindInt},
	"disc":         {Expr: "t.disc_no", Kind: query.KindInt},
	"disc_no":      {Expr: "t.disc_no", Kind: query.KindInt},
	"duration_ms":  {Expr: "COALESCE(bk.total_duration_ms, f.duration_ms)", Kind: query.KindInt},
	"codec":        {Expr: "f.codec", Kind: query.KindText},
	"container":    {Expr: "f.container", Kind: query.KindText},
	"path":         {Expr: "f.display_path", Kind: query.KindText},
}

// fieldMapFor returns the whitelist for an entity and whether it is supported.
// items and tracks share the item view. Other entities are not queryable here
// and report false so callers can reject them rather than silently return item
// rows.
func fieldMapFor(e query.Entity) (query.FieldMap, bool) {
	switch e {
	case query.EntityItems, query.EntityTracks:
		return itemFields, true
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
