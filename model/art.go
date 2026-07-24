package model

// CoverArtNames are the directory cover-image filenames WaxBin recognizes, in
// priority order. It is the single registry shared by the scanner (which discovers
// covers) and organize (which carries them with a relocated album), so the two
// cannot drift. Matching is case-insensitive against the directory listing, so a
// mixed-case name like "Cover.JPG" is recognized on a case-sensitive filesystem.
var CoverArtNames = []string{
	"cover.jpg", "cover.jpeg", "cover.png", "cover.webp",
	"cover.avif", "cover.heic", "cover.heif",
	"folder.jpg", "folder.jpeg", "folder.png",
	"front.jpg", "front.jpeg", "front.png",
	"album.jpg", "albumart.jpg",
}

// ArtEntity is one level of the art fallback chain. The resolver starts at the
// requested level and walks up toward the root (track -> album -> release_group
// -> artist -> genre for music; episode -> podcast for podcasts), returning the
// first level that has art.
type ArtEntity string

const (
	ArtTrack        ArtEntity = "track"
	ArtAlbum        ArtEntity = "album"
	ArtReleaseGroup ArtEntity = "release_group"
	ArtArtist       ArtEntity = "artist"
	ArtGenre        ArtEntity = "genre"
	// ArtEpisode and ArtPodcast form the podcast art chain: an episode falls back to
	// its podcast's feed image.
	ArtEpisode ArtEntity = "episode"
	ArtPodcast ArtEntity = "podcast"
	// ArtPlaylist is a terminal level: a playlist has no ancestry, so its chain is
	// one rung and even a front cover resolves at the playlist's own level or not at
	// all. A playlist is a user-made list, not a catalog entity, so there is nothing
	// above it to inherit a cover from.
	ArtPlaylist ArtEntity = "playlist"
)

// Valid reports whether e is a known art entity level.
func (e ArtEntity) Valid() bool {
	switch e {
	case ArtTrack, ArtAlbum, ArtReleaseGroup, ArtArtist, ArtGenre, ArtEpisode, ArtPodcast, ArtPlaylist:
		return true
	default:
		return false
	}
}

// EntityRef names one entity by its art level and public id, the input to art
// resolution.
type EntityRef struct {
	Type ArtEntity
	PID  PID
}

// ArtRole is one artwork slot on an entity: the front cover plus the auxiliary
// images a release carries. The vocabulary is closed; an entity holds at most one
// image per role. Only the front role participates in the resolution fallback
// chain and in scan/feed ingestion; the other roles are set explicitly and
// resolve at their own level. Artist imagery has no separate portrait role; it
// lands under background.
type ArtRole string

const (
	ArtRoleFront      ArtRole = "front"
	ArtRoleBack       ArtRole = "back"
	ArtRoleDisc       ArtRole = "disc"
	ArtRoleBooklet    ArtRole = "booklet"
	ArtRoleBackground ArtRole = "background"
)

// Valid reports whether r is a known art role.
func (r ArtRole) Valid() bool {
	switch r {
	case ArtRoleFront, ArtRoleBack, ArtRoleDisc, ArtRoleBooklet, ArtRoleBackground:
		return true
	default:
		return false
	}
}

// ParseArtRole parses a role name at an input boundary (a CLI flag, a proxy
// frame). The empty string means the front cover, the pre-role default every
// existing caller relied on; anything outside the closed vocabulary reports ok
// false.
func ParseArtRole(s string) (ArtRole, bool) {
	if s == "" {
		return ArtRoleFront, true
	}
	r := ArtRole(s)
	return r, r.Valid()
}

// ArtRoleInfo describes one artwork slot an entity holds at its own level: the
// role plus the stored source's format, dimensions (0 when the image was not
// decodable), and content hash.
type ArtRoleInfo struct {
	Role       ArtRole
	Format     string
	Width      int
	Height     int
	SourceHash string
}

// ArtImage is a source cover image plus its content hash and decoded dimensions.
// Hash is the content-address key; the ingestor fills it (and the dimensions)
// before the store dedups and persists the image.
type ArtImage struct {
	Data   []byte
	Format string // jpeg|png|webp|gif
	Width  int
	Height int
	Hash   string
}

// ArtBlob is resolved art ready to serve: an original source image or a generated
// thumbnail. SourceHash is the content hash of the originating source; Thumbnail
// is true when Bytes is a derived thumbnail rather than the original. Level names
// the chain level that answered (the requested entity itself, or the ancestor a
// front-cover resolve fell back to), and Derived marks an album answered from a
// member track's cover rather than a durable album row of its own.
type ArtBlob struct {
	Bytes      []byte
	Format     string
	Width      int
	Height     int
	SourceHash string
	Thumbnail  bool
	Level      ArtEntity
	Derived    bool
}
