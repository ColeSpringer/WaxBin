package read

import "github.com/colespringer/waxbin/model"

// Bucket is one group of a faceted aggregation: a key, a human display label,
// and the count of matching items. For entity dimensions (genre/artist) the
// bucket also carries the entity's public id so a consumer can drill in.
type Bucket struct {
	Key       string    // stable machine key (entity pid, year, or kind)
	Display   string    // human label; a canonical sentinel when IsUnknown
	Count     int       // matching items in this bucket
	IsUnknown bool      // the dimension was absent (mapped to a sentinel)
	EntityPID model.PID // entity pid for genre/artist dimensions, else ""
}

// FacetResult is the grouped, counted answer for one Query and dimension.
type FacetResult struct {
	GroupBy GroupBy
	Buckets []Bucket
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
