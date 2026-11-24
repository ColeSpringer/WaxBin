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
	// libraries: an item counts when any of its files, an alternate copy included,
	// lives in one of them. A fileless item, such as an undownloaded episode, has no
	// library and drops out of a scoped search (its transcript hits included). An
	// unknown library pid is an error, not an empty scope.
	Libraries []model.PID

	// PrimaryLibraries, when non-empty, scopes the search to items placed in these
	// libraries: an item counts when its primary file lives in one of them, the library
	// ItemView.LibraryPID shows, so an item another library holds only a copy of is
	// outside the scope. It is the search-side form of the primary_library query field,
	// for a host that shows each item once under its library or grants a reader one
	// library, where Libraries is the form for a host serving whatever a library can
	// play. With both set, both apply. A fileless item drops out as under Libraries,
	// and an unknown pid is an error.
	PrimaryLibraries []model.PID

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

	// Podcasts, when non-nil, keeps only the episodes of these shows, in the metadata
	// rungs and the transcript rung alike, and narrows inside the candidate pool as the
	// other two do; tracks and books are not affected. An empty non-nil list keeps no
	// episode at all. An unknown show pid is an error, as an unknown library is.
	Podcasts []model.PID
}

// SearchHit is one ranked search result: an entity reference plus its display
// fields and BM25 score. Score is the SQLite bm25 value, where a lower (more
// negative) score is a better match; a hit in an entity group (an artist, an album, an
// author, a narrator, a series or a podcast) carries the score of the best item row that
// brought it in, or, found by its own name alone, the best score the search read. A
// query of symbols alone has no bm25, so its hits score 0.
type SearchHit struct {
	PID model.PID
	// Kind is the group the hit is in: artist|album|track|author|narrator|series|book|
	// podcast|episode. An author or a narrator is an artist entity under the role it was
	// found in, so one person can be a hit in Artists, Authors and Narrators at once,
	// with the same PID in each.
	Kind string
	// Title is the primary display: an item's title, or an entity's own name.
	Title string
	// Subtitle is the secondary display: the artist for a track or an album, the author
	// for a book, the show for an episode and the show's own author for a podcast. A
	// series takes the author of the first of its books the search's narrowing keeps,
	// whichever book found it. Empty for an artist, an author and a narrator.
	Subtitle string
	Score    float64 // bm25; lower is a better match
	// Exact marks an entity hit whose own name is the query: the same match key, the
	// same key once spaces are dropped ("jayz" is Jay-Z), or for a query of symbols
	// alone the same sort key.
	Exact bool
	// ArtHash is the content hash of the front cover the hit resolves to, read under
	// ArtRef as EntityInfo.ArtHash is read for a card: the cover's version, for its URL.
	// Empty when the hit resolves no cover.
	ArtHash string
}

// ArtRef is the reference the hit's cover is asked for under, by its Kind: an artist, an
// author or a narrator as the artist it is, an album and a podcast as themselves, an
// episode in the episode slot, a track or a book in the track slot. ResolveArt takes it.
// A series holds no art and reports false.
func (h SearchHit) ArtRef() (model.EntityRef, bool) {
	switch h.Kind {
	case "artist", "author", "narrator":
		return model.EntityRef{Type: model.ArtArtist, PID: h.PID}, true
	case "album":
		return model.EntityRef{Type: model.ArtAlbum, PID: h.PID}, true
	case "podcast":
		return model.EntityRef{Type: model.ArtPodcast, PID: h.PID}, true
	case "episode":
		return model.EntityRef{Type: model.ArtEpisode, PID: h.PID}, true
	case "series":
		return model.EntityRef{}, false
	}
	return model.EntityRef{Type: model.ArtTrack, PID: h.PID}, true
}

// SearchResult is the grouped answer for one query string, and each group's order is
// authoritative. Tracks, books and episodes come in BM25 order from the metadata FTS,
// whose field weighting makes a title hit outrank an artist or album hit; episodes
// whose transcript matches follow the episodes whose metadata does. A query of symbols
// alone ("$", "!!!") has no words to rank, so its items, found by title or artist, come
// newest first.
//
// The other six groups are the entities the matched items belong to. A matched track
// brings its album and the artists its artist credit names. Its album artist and the
// artists it credits in any other role, a performer or a producer say, come with it only
// when the query names them, each of its words beginning a word of the name, so a title
// match does not list its producers. A matched book brings its series and every author
// and narrator it credits, and a matched episode its show (an episode found by transcript
// alone brings none). Each group also holds the entities whose own name is the query,
// typed with or without the name's inner punctuation, whichever items the ranking read,
// as long as the search's narrowing keeps one of their items. A show needs no episode
// for that while nothing narrows by library or state, so it is found before its first
// one arrives. An Authors or Narrators hit is a person the author or narrator facet
// buckets, under the pid author_pid or narrator_pid matches, and an Artists hit is
// anyone a track credits.
//
// An entity group is ranked on its own names first (exact, then the query beginning a
// word of the name, then every query word inside it, then the rest), and within that by
// the best item row that brought each entity in, so a track titled after an artist
// cannot put its own performer ahead of that artist. Scores are therefore not comparable
// across an entity group's tiers.
type SearchResult struct {
	Query     string
	Artists   []SearchHit
	Albums    []SearchHit
	Tracks    []SearchHit
	Authors   []SearchHit
	Narrators []SearchHit
	Series    []SearchHit
	Books     []SearchHit
	Podcasts  []SearchHit
	Episodes  []SearchHit
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
	for _, hits := range r.Groups() {
		if len(hits) > 0 {
			return false
		}
	}
	return true
}

// Groups returns every group of the result, in the order the fields are declared.
func (r *SearchResult) Groups() [][]SearchHit {
	return [][]SearchHit{r.Artists, r.Albums, r.Tracks, r.Authors, r.Narrators, r.Series, r.Books, r.Podcasts, r.Episodes}
}
