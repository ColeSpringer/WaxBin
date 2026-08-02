package read

import (
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// DiscoveryList is one of the enumerated browse lists. The vocabulary is fixed
// and individually tested so every consumer (CLI browse, WaxDeck home rows) asks
// for the same canonical list rather than reinventing each ordering. Play-derived
// lists (most-played, recently-played, starred) read indexed play_state for one
// user; the rest are catalog-structural.
type DiscoveryList string

const (
	// by release year, newest first; tracks and books only (see Kinds)
	ListNewest         DiscoveryList = "newest"
	ListRecentlyAdded  DiscoveryList = "recently-added"  // by catalog insertion time
	ListMostPlayed     DiscoveryList = "most-played"     // by play count (per user)
	ListRecentlyPlayed DiscoveryList = "recently-played" // by last played (per user)
	ListRandom         DiscoveryList = "random"          // seeded stable shuffle
	ListStarred        DiscoveryList = "starred"         // starred, most recent first (per user)
	ListByYear         DiscoveryList = "by-year"         // filtered to one year (Options.Year)
	ListByGenre        DiscoveryList = "by-genre"        // filtered to one genre (Options.GenrePID)
	ListAlphabetical   DiscoveryList = "alphabetical"    // by collation sort key
	// Episodes by publication date, newest first, across every feed. Remote and
	// downloaded alike, since finding an undownloaded episode is the point; undated
	// ones are excluded (see the spec). Named off-pattern from recently-* on purpose:
	// it is a kind scope as well as an ordering.
	ListRecentEpisodes DiscoveryList = "recent-episodes"
)

// Valid reports whether d is a known discovery list.
func (d DiscoveryList) Valid() bool {
	switch d {
	case ListNewest, ListRecentlyAdded, ListMostPlayed, ListRecentlyPlayed,
		ListRandom, ListStarred, ListByYear, ListByGenre, ListAlphabetical,
		ListRecentEpisodes:
		return true
	default:
		return false
	}
}

// PerUser reports whether the list itself is derived from per-user play_state, so
// BrowseOptions.UserPID selects whose state defines its membership and order.
//
// It is not a complete answer to "does this browse need a user". Any list can now
// read play_state through BrowseOptions.Query (`starred is 1` on an alphabetical
// browse, say), and that filter is evaluated against the same UserPID. Decide
// whether to pass a user from the options you are about to send, not from this alone.
func (d DiscoveryList) PerUser() bool {
	switch d {
	case ListMostPlayed, ListRecentlyPlayed, ListStarred:
		return true
	default:
		return false
	}
}

// Kinds reports the item kinds a list can contain, nil when it spans all of them.
// Only newest and recent-episodes are scoped, each ordering by something only its
// kinds carry. Consult it before offering a media-type filter over a list; asking for
// a kind outside the scope is an empty page, not an error.
func (d DiscoveryList) Kinds() []model.Kind {
	switch d {
	case ListNewest:
		return []model.Kind{model.KindTrack, model.KindBook}
	case ListRecentEpisodes:
		return []model.Kind{model.KindEpisode}
	default:
		return nil
	}
}

// DiscoveryLists lists the supported browse lists (for help text and tests).
func DiscoveryLists() []DiscoveryList {
	return []DiscoveryList{
		ListNewest, ListRecentlyAdded, ListMostPlayed, ListRecentlyPlayed,
		ListRandom, ListStarred, ListByYear, ListByGenre, ListAlphabetical,
		ListRecentEpisodes,
	}
}

// BrowseOptions parameterizes a discovery-list browse. The zero value is valid
// for the catalog-structural lists; the per-list fields are consulted only by the
// lists that need them.
//
// A cursor is valid only for the identical (list, options) it was issued under. It
// was already sensitive to Seed and Year; Query is one more dimension of that same
// pre-existing contract. Reusing a cursor across a changed query yields a
// coherent-looking wrong window with no error.
type BrowseOptions struct {
	UserPID  model.PID // play-derived lists and per-user Query fields; empty selects the default user
	Year     int       // ListByYear (required)
	GenrePID model.PID // ListByGenre (required)
	Seed     int64     // ListRandom: consumer-supplied seed for a stable shuffle
	Cursor   Cursor    // keyset cursor from a prior page; empty starts at the head
	Limit    int       // page size (0 uses the store default)
	// Query narrows the list to the items it matches, compiled by the same engine
	// QueryPage uses, so a discovery list can be scoped to a facet bucket, a media
	// type, or any whitelisted field. The zero Query means no filter. Its
	// sort/limit/offset/limit-mode are ignored, exactly as QueryPage ignores them:
	// the list owns the order. A per-user field reads UserPID's state, the same user
	// the play-derived lists read.
	//
	// A non-zero Query must name an Entity; a Where with an empty Entity is
	// CodeInvalid rather than defaulting to items, because the entity is what selects
	// the field whitelist. Entity also scopes the kind, so query.EntityTracks
	// excludes books and episodes.
	//
	// The division of labour it creates, so the two surfaces do not drift: Browse
	// owns the ordering vocabulary, QueryPage owns sort_key ordering, and they share
	// one filter engine. by-year and by-genre stay as named lists for vocabulary
	// stability even though a query expresses both, and by-genre keeps resolving its
	// pid up front so an unknown genre is CodeNotFound where a genre_pid filter would
	// silently return an empty page.
	Query query.Query
}
