// Package read holds WaxBin's canonical read/consumer API primitives: faceted
// aggregation, collation-correct keyset pagination, and the canonical "unknown"
// buckets. The types live here; the SQL that produces them lives in
// store/sqlite, which depends on this package. The boundary rule: WaxBin returns
// clean, canonical, counted results; consumers decide pixels/UX.
package read

// GroupBy is a faceting dimension. The set is enumerated and individually
// tested so every consumer (CLI --group-by, OpenSubsonic getGenres/getArtists,
// stats) groups the same way.
type GroupBy string

const (
	GroupGenre  GroupBy = "genre"
	GroupArtist GroupBy = "artist"
	// GroupCreditArtist buckets a track under EVERY artist its credit names, where
	// GroupArtist buckets it once under the primary. A featured artist has a bucket
	// here and none there. It is the second many-per-item dimension after genre, so a
	// track appears in several buckets and the bucket counts sum to more than the
	// item count.
	GroupCreditArtist GroupBy = "creditArtist"
	GroupAlbumArtist  GroupBy = "albumArtist"
	GroupAlbum        GroupBy = "album"
	// GroupReleaseGroup buckets tracks by their album's release group (key = release
	// group pid, display = its title), the dimension above album that collects a
	// record's editions. It is the mirror of the release_group_pid query field. Like
	// album it is track-only and excludes episodes; a track whose album has no release
	// group lands in NoReleaseGroup, which is distinct from NonAlbum because a track
	// can have an album and still no release group.
	GroupReleaseGroup GroupBy = "releaseGroup"
	GroupYear         GroupBy = "year"
	GroupKind         GroupBy = "kind"
	// GroupLibrary buckets items by their primary backing file's library (key =
	// library pid, display = the display root). A fileless item, such as an
	// undownloaded episode, lands in the NoFile unknown bucket.
	GroupLibrary GroupBy = "library"
	// GroupPodcast buckets episodes by their feed (key = podcast pid, display = the
	// feed title). It is the mirror of the podcast_pid query field, so a bucket
	// drills down through it. The dimension is episode-scoped and every episode has
	// a feed, so it has no unknown bucket: the five music dimensions exclude
	// episodes for the same reason this one includes only them.
	GroupPodcast GroupBy = "podcast"
	// GroupPlaylist buckets items by the static playlists holding them (key =
	// playlist pid, display = its name). It is the only dimension whose bucket set
	// depends on the calling user: a playlist gets a bucket when the caller owns it
	// or it is shared, matching what ListPlaylists shows.
	//
	// Smart playlists are absent structurally, since they store no membership rows,
	// and there is no unknown bucket. The complement, "in no playlist", is a query
	// rather than a bucket: `playlist_pid isMissing`, which is catalog-wide where
	// these buckets are owner-scoped. Bucket labels can collide across owners,
	// because a playlist has no sort key and two users may both have a "Favorites";
	// EntityPID is what tells them apart.
	GroupPlaylist GroupBy = "playlist"
)

// Valid reports whether g is a known faceting dimension: one of the fixed dimensions
// or a well-formed custom-tag dimension ("tag.<KEY>" for a canonical, non-reserved key).
func (g GroupBy) Valid() bool {
	switch g {
	case GroupGenre, GroupArtist, GroupCreditArtist, GroupAlbumArtist, GroupAlbum,
		GroupReleaseGroup, GroupYear, GroupKind, GroupLibrary, GroupPodcast, GroupPlaylist:
		return true
	default:
		_, ok := TagGroupKey(g)
		return ok
	}
}

// GroupBys lists the supported faceting dimensions (for help text and tests).
func GroupBys() []GroupBy {
	return []GroupBy{GroupGenre, GroupArtist, GroupCreditArtist, GroupAlbumArtist, GroupAlbum,
		GroupReleaseGroup, GroupYear, GroupKind, GroupLibrary, GroupPodcast, GroupPlaylist}
}

// UserScoped reports whether g's bucket set depends on the calling user. Only
// GroupPlaylist is, so a consumer caching facet results keys this dimension by user
// and may key the rest by (q, g) alone.
func (g GroupBy) UserScoped() bool { return g == GroupPlaylist }

// Canonical "unknown" bucket display labels. A consumer rendering a facet or a
// browse list shows these verbatim for the absent dimension, so every client
// renders the same sentinel rather than inventing its own.
const (
	UnknownArtist = "[Unknown Artist]"
	NoGenre       = "[No Genre]"
	UnknownYear   = "[Unknown Year]"
	NonAlbum      = "[Non-Album]"
	NoFile        = "[No File]"
	// NoReleaseGroup is deliberately not NonAlbum: a track can belong to an album and
	// still have no release group, so folding the two would hide that state.
	NoReleaseGroup = "[No Release Group]"
)
