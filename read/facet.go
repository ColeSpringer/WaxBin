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
