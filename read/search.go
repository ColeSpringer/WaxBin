package read

import "github.com/colespringer/waxbin/model"

// SearchOptions tunes a cross-entity search. Limit caps each result group.
type SearchOptions struct {
	Limit int // per-group cap (0 uses a default)
}

// SearchHit is one ranked search result: an entity reference plus its display
// fields and BM25 score. Score is the SQLite bm25 value, where a lower (more
// negative) score is a better match; consumers order ascending.
type SearchHit struct {
	PID      model.PID
	Kind     string  // artist|album|track|episode
	Title    string  // primary display (track/album title, or artist name)
	Subtitle string  // secondary display (artist for a track/album; empty for an artist)
	Score    float64 // bm25; lower is a better match
}

// SearchResult is the grouped, BM25-ranked answer for one query string. Metadata
// hits (artists/albums/tracks) come from the metadata FTS with field weighting so
// a title hit outranks artist and album hits. Episodes are reserved for
// transcript-backed podcast search and stay empty until transcript indexing exists.
type SearchResult struct {
	Query    string
	Artists  []SearchHit
	Albums   []SearchHit
	Tracks   []SearchHit
	Episodes []SearchHit
	// Truncated is set when the search hit its internal ranked-row scan cap, so the
	// groups may omit lower-ranked albums/artists. A consumer wanting fuller coverage
	// can narrow the query.
	Truncated bool
}

// Empty reports whether the search produced no hits in any group.
func (r *SearchResult) Empty() bool {
	return len(r.Artists) == 0 && len(r.Albums) == 0 && len(r.Tracks) == 0 && len(r.Episodes) == 0
}
