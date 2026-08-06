package read

import "github.com/colespringer/waxbin/model"

// SearchOptions tunes a cross-entity search. Limit caps each result group.
type SearchOptions struct {
	Limit int // per-group cap (0 uses a default)

	// MaxCandidates, when positive, caps how many matching rows are considered
	// at all. The pool is the newest MaxCandidates matches (insertion order),
	// not the best-ranked ones: bounding the ranked set exactly is what the cap
	// exists to avoid, and biasing the pool toward recent additions beats
	// systematically dropping them. With States set the pool is the newest N
	// matches in those states, which is the point of the pairing: without it a
	// bulk deletion matches its own names best, takes the top of the ranking,
	// and spends the pool on rows the caller discards afterwards. 0 ranks every
	// match up to the internal scan cap, exactly as before the option existed.
	// Exhausting the metadata pool sets SearchResult.Truncated; the
	// transcript-body rung is capped too but does not report its own exhaustion
	// (see Truncated).
	MaxCandidates int

	// Libraries, when non-empty, scopes the search to items playable from these
	// libraries: an item counts when its primary backing file lives in one of
	// them. A fileless item, such as an undownloaded episode, has no library and
	// drops out of a scoped search (its transcript hits included). An unknown
	// library pid is an error, not an empty scope.
	Libraries []model.PID

	// States, when non-empty, narrows the search to items in these lifecycle
	// states. Empty means no narrowing, which is the behavior from before the
	// option existed: an archived item keeps its FTS row and stays searchable.
	// An unknown state is an error, not an empty scope.
	//
	// It is orthogonal to Libraries in both directions, which is worth spelling
	// out because neither half is obvious. Marking an item missing preserves its
	// file row and its item_file edges, so a missing item is still inside a
	// library scope and only States can exclude it. An archived or remote item
	// has no file at all and is already outside every library scope, so only
	// States can include it: States{StateRemote} searches the unfetched backlog,
	// which no library scope can reach.
	//
	// It is an allow-list, so a caller that spells out today's four states will
	// not see a fifth if one is ever added. A caller wanting a stable "everything
	// a listing shows" set should name it once as a constant rather than
	// spelling it at each call site: the default stays "no narrowing" for
	// compatibility, so a call site forgotten at the next addition silently
	// reverts to today's behavior rather than failing.
	States []model.ItemState
}

// SearchHit is one ranked search result: an entity reference plus its display
// fields and BM25 score. Score is the SQLite bm25 value, where a lower (more
// negative) score is a better match; consumers order ascending.
type SearchHit struct {
	PID      model.PID
	Kind     string  // artist|album|track|book|episode
	Title    string  // primary display (track/album/book title, or artist name)
	Subtitle string  // secondary display (artist for a track/album, author for a book; empty for an artist)
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
	Books    []SearchHit
	Episodes []SearchHit
	// Truncated is set when the metadata search did not rank every match: it hit
	// its internal ranked-row scan cap, or it exhausted a
	// SearchOptions.MaxCandidates candidate pool. Either way the groups may omit
	// matches (under a candidate cap, older ones), where "matches" means matches
	// in SearchOptions.States when that is set, since the same narrowing bounds
	// the pool this reports on. The transcript-body rung that
	// tops up Episodes is not covered: it ranks its own bounded pool (the group
	// cap, plus MaxCandidates when set) and never reports exhaustion here, so
	// transcript hits can be partial while Truncated is false, as they always
	// could be under the group cap. A consumer wanting fuller coverage can
	// narrow the query or raise the cap.
	Truncated bool
}

// Empty reports whether the search produced no hits in any group.
func (r *SearchResult) Empty() bool {
	return len(r.Artists) == 0 && len(r.Albums) == 0 && len(r.Tracks) == 0 &&
		len(r.Books) == 0 && len(r.Episodes) == 0
}
