package read

import "github.com/colespringer/waxbin/model"

// Bucket is one group of a faceted aggregation: a key, a human display label,
// and the count of matching items. An entity-keyed dimension also carries the
// entity's public id so a consumer can drill in.
type Bucket struct {
	Key       string // stable machine key (entity pid, year, or kind)
	Display   string // human label; a canonical sentinel when IsUnknown
	Count     int    // matching items in this bucket
	IsUnknown bool   // the dimension was absent (mapped to a sentinel)
	// EntityPID is the bucket's entity pid on the entity-keyed dimensions
	// (genre, artist, albumArtist, album, library, podcast) and "" on the rest,
	// including the unknown bucket of a dimension that has one.
	//
	// It is a pid, not a typed handle, so the dimension is what says how to resolve
	// it. Four of the six pair with an EntityKind and read back through EntityByPID
	// or EntityByPIDs; the exceptions are library, whose pid is a model.Library, and
	// podcast, whose pid reads back through PodcastByPID. EntityKind deliberately
	// covers neither, so a consumer walking buckets generically has to switch on
	// GroupBy rather than assume every EntityPID is an EntityByPID argument.
	EntityPID model.PID
}

// FacetResult is the grouped, counted answer for one Query and dimension.
type FacetResult struct {
	GroupBy GroupBy
	Buckets []Bucket
}

// FacetOrder selects how a facet's buckets are ordered. The empty value means
// FacetOrderLabel, so a caller that does not care passes "" and gets the canonical
// order.
type FacetOrder string

const (
	// FacetOrderLabel is canonical collation order: the dimension's sort key, then
	// its display label, with the unknown bucket last. It is the default.
	FacetOrderLabel FacetOrder = "label"
	// FacetOrderCount is biggest bucket first, ties broken in collation order so the
	// result is deterministic. Paired with a limit it is the "top N artists" shelf.
	FacetOrderCount FacetOrder = "count"
)

// Valid reports whether o is a known facet order. The empty string is valid and
// means FacetOrderLabel.
func (o FacetOrder) Valid() bool {
	switch o {
	case "", FacetOrderLabel, FacetOrderCount:
		return true
	default:
		return false
	}
}

// TagGroupKey reports whether g is a custom-tag faceting dimension ("tag.<KEY>") and,
// if so, returns the canonical tag key to group by. It is the discovery/validation
// point shared by GroupBy.Valid and the store's facet spec: a reserved or malformed
// key is rejected (ok=false), keeping the same fail-closed barrier the query resolver
// uses. Tag keys are open-ended (discovered via TagKeys), so they are not enumerated
// in GroupBys the way the fixed dimensions are.
func TagGroupKey(g GroupBy) (string, bool) {
	raw, ok := model.CutTagPrefix(string(g))
	if !ok {
		return "", false
	}
	canon, ok := model.CanonicalTagKey(raw)
	if !ok || model.IsReservedTagKey(canon) {
		return "", false
	}
	return canon, true
}

// TagKeyCount is one custom-tag key and the number of distinct items carrying it. It
// answers "which tag.<KEY> browse dimensions exist" for a consumer building tag facets.
type TagKeyCount struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}
