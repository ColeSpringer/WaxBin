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
)

// Valid reports whether e is a known art entity level.
func (e ArtEntity) Valid() bool {
	switch e {
	case ArtTrack, ArtAlbum, ArtReleaseGroup, ArtArtist, ArtGenre, ArtEpisode, ArtPodcast:
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
// is true when Bytes is a derived thumbnail rather than the original.
type ArtBlob struct {
	Bytes      []byte
	Format     string
	Width      int
	Height     int
	SourceHash string
	Thumbnail  bool
}
