package sqlite

import "github.com/colespringer/waxbin/query"

// itemFields whitelists the logical fields a query over items/tracks may
// reference, mapping each to a column expression in the items SELECT (aliases:
// pi=playable_item, t=track, f=primary file). A field absent here is rejected by
// the compiler, which is what keeps untrusted names out of SQL.
var itemFields = query.FieldMap{
	"pid":          {Expr: "pi.pid", Kind: query.KindText},
	"kind":         {Expr: "pi.kind", Kind: query.KindText},
	"state":        {Expr: "pi.state", Kind: query.KindText},
	"title":        {Expr: "pi.title", Kind: query.KindText},
	"sort_key":     {Expr: "pi.sort_key", Kind: query.KindText},
	"added":        {Expr: "pi.created_at", Kind: query.KindTime},
	"created_at":   {Expr: "pi.created_at", Kind: query.KindTime},
	"updated_at":   {Expr: "pi.updated_at", Kind: query.KindTime},
	"artist":       {Expr: "t.artist", Kind: query.KindText},
	"album_artist": {Expr: "t.album_artist", Kind: query.KindText},
	"albumartist":  {Expr: "t.album_artist", Kind: query.KindText},
	"album":        {Expr: "t.album", Kind: query.KindText},
	"genre":        {Expr: "t.genre", Kind: query.KindText},
	"year":         {Expr: "t.year", Kind: query.KindInt},
	"track":        {Expr: "t.track_no", Kind: query.KindInt},
	"track_no":     {Expr: "t.track_no", Kind: query.KindInt},
	"disc":         {Expr: "t.disc_no", Kind: query.KindInt},
	"disc_no":      {Expr: "t.disc_no", Kind: query.KindInt},
	"duration_ms":  {Expr: "f.duration_ms", Kind: query.KindInt},
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
