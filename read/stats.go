package read

import "github.com/colespringer/waxbin/model"

// Stats is a library summary using the same Facet groupings that browse uses,
// plus maintained rollups and per-user play_state. Catalog-structural figures are
// global; play figures are for one user.
type Stats struct {
	Items         int   // music tracks
	Books         int   // audiobooks
	Artists       int   // distinct artist entities (track artists + book contributors)
	ReleaseGroups int   // distinct release groups
	Albums        int   // distinct albums
	Genres        int   // distinct genre entities
	TotalDuration int64 // summed item-file duration across all parts, ms

	TopGenres  []Bucket // Facet(genre), most items first
	TopArtists []Bucket // Facet(artist), most items first
	ByYear     []Bucket // Facet(year), chronological

	Play PlayStats
}

// PlayStats is the per-user, play-derived half of Stats. These come from indexed
// play_state queries, never the catalog rollups (a scrobble must not trigger a
// rollup write), matching the read-API boundary.
type PlayStats struct {
	User       string
	TotalPlays int          // summed play counts
	Finished   int          // items played to completion
	Starred    int          // starred items
	MostPlayed []PlayedItem // top items by play count
}

// PlayedItem is one entry of the most-played list.
type PlayedItem struct {
	PID       model.PID
	Title     string
	Artist    string
	PlayCount int
}

// YearReview is a per-user listening recap for one calendar year (UTC), derived
// from play_session history. The top lists rank by play count within the year;
// NewInLibrary counts items catalogued that year (catalog-structural). It
// complements the Facet-based catalog Stats with a time-scoped listening view.
type YearReview struct {
	Year          int
	User          string
	Sessions      int
	MinutesPlayed int64
	TracksPlayed  int      // distinct items played that year
	NewInLibrary  int      // tracks/books added to the catalog that year
	TopArtists    []Bucket // Count = plays that year
	TopGenres     []Bucket // Count = plays that year
	TopTracks     []PlayedItem
}
