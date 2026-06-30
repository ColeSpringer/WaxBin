package read

import "github.com/colespringer/waxbin/model"

// DiscoveryList is one of the enumerated browse lists. The vocabulary is fixed
// and individually tested so every consumer (CLI browse, WaxDeck home rows) asks
// for the same canonical list rather than reinventing each ordering. Play-derived
// lists (most-played, recently-played, starred) read indexed play_state for one
// user; the rest are catalog-structural.
type DiscoveryList string

const (
	ListNewest         DiscoveryList = "newest"          // by release year, newest first
	ListRecentlyAdded  DiscoveryList = "recently-added"  // by catalog insertion time
	ListMostPlayed     DiscoveryList = "most-played"     // by play count (per user)
	ListRecentlyPlayed DiscoveryList = "recently-played" // by last played (per user)
	ListRandom         DiscoveryList = "random"          // seeded stable shuffle
	ListStarred        DiscoveryList = "starred"         // starred, most recent first (per user)
	ListByYear         DiscoveryList = "by-year"         // filtered to one year (Options.Year)
	ListByGenre        DiscoveryList = "by-genre"        // filtered to one genre (Options.GenrePID)
	ListAlphabetical   DiscoveryList = "alphabetical"    // by collation sort key
)

// Valid reports whether d is a known discovery list.
func (d DiscoveryList) Valid() bool {
	switch d {
	case ListNewest, ListRecentlyAdded, ListMostPlayed, ListRecentlyPlayed,
		ListRandom, ListStarred, ListByYear, ListByGenre, ListAlphabetical:
		return true
	default:
		return false
	}
}

// PerUser reports whether d reads per-user play_state (so BrowseOptions.UserPID
// selects whose state to read).
func (d DiscoveryList) PerUser() bool {
	switch d {
	case ListMostPlayed, ListRecentlyPlayed, ListStarred:
		return true
	default:
		return false
	}
}

// DiscoveryLists lists the supported browse lists (for help text and tests).
func DiscoveryLists() []DiscoveryList {
	return []DiscoveryList{
		ListNewest, ListRecentlyAdded, ListMostPlayed, ListRecentlyPlayed,
		ListRandom, ListStarred, ListByYear, ListByGenre, ListAlphabetical,
	}
}

// BrowseOptions parameterizes a discovery-list browse. The zero value is valid
// for the catalog-structural lists; the per-list fields are consulted only by the
// lists that need them.
type BrowseOptions struct {
	UserPID  model.PID // play-derived lists; empty selects the default user
	Year     int       // ListByYear (required)
	GenrePID model.PID // ListByGenre (required)
	Seed     int64     // ListRandom: consumer-supplied seed for a stable shuffle
	Cursor   Cursor    // keyset cursor from a prior page; empty starts at the head
	Limit    int       // page size (0 uses the store default)
}
