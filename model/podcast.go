package model

// Podcast is a subscribed feed. Its stable identity is IdentityKey, derived from
// the feed URL or published <podcast:guid>, not the title. HTTP validators support
// conditional sync. RetentionKeep is the number of newest downloaded episodes to
// keep; 0 keeps all.
type Podcast struct {
	ID            int64
	PID           PID
	FeedURL       string
	IdentityKey   string
	Title         string
	SortKey       string
	Author        string
	Description   string
	Link          string // show website
	Language      string
	Category      string
	Explicit      bool
	GUID          string // <podcast:guid> when published
	ETag          string
	LastModified  string
	LastFetchedAt int64 // unix nanoseconds; 0 when never fetched
	RetentionKeep int
	AuthUser      string // basic-auth user; the password lives in the secret table
	ImageURL      string // current feed image (ingested into the art store on sync)
	CreatedAt     int64
	UpdatedAt     int64
	// EpisodeCount and DownloadedCount are populated by list/detail reads, not stored.
	EpisodeCount    int
	DownloadedCount int
}

// EpisodeType classifies an episode within its feed.
type EpisodeType string

const (
	EpisodeFull    EpisodeType = "full"
	EpisodeTrailer EpisodeType = "trailer"
	EpisodeBonus   EpisodeType = "bonus"
)

// Episode is the podcast subtype of a playable_item (shares its ID, kind=episode).
// It is cataloged from the feed (State=remote) and gains a file when downloaded
// (State=present). The enclosure pointer survives a download/retention cycle so an
// episode can be re-downloaded after retention reclaims its file.
type Episode struct {
	PID            PID
	PodcastPID     PID
	PodcastTitle   string
	Title          string
	GUID           string
	Description    string
	Link           string
	State          ItemState
	PubDateNS      int64 // unix nanoseconds; 0 when undated
	Year           int
	Season         int
	EpisodeNo      int
	EpisodeType    EpisodeType
	DurationMS     int64
	Explicit       bool
	EnclosureURL   string
	EnclosureType  string
	EnclosureSize  int64
	TranscriptURL  string
	TranscriptType string
	ChaptersURL    string
	ImageURL       string // episode artwork (ingested into the art store)

	// Downloaded reports whether the episode currently has a local file; FilePID and
	// DisplayPath name it when present.
	Downloaded  bool
	FilePID     PID
	DisplayPath string
	CreatedAt   int64
	UpdatedAt   int64
}

// EpisodeDetail is the full read shape for one episode: the episode plus its
// resolved play state and any stored chapters.
type EpisodeDetail struct {
	Episode       *Episode
	HasTranscript bool
	Chapters      []Chapter
}

// FeedEpisode is one episode parsed from a feed, before persistence. It is the
// boundary type between the feed parser and the store: feed-native fields plus the
// derived identity key. PubDateNS is 0 when the item carries no parseable date.
type FeedEpisode struct {
	GUID           string
	Title          string
	Description    string
	Link           string
	PubDateNS      int64
	Year           int
	Season         int
	EpisodeNo      int
	EpisodeType    EpisodeType
	DurationMS     int64
	Explicit       bool
	EnclosureURL   string
	EnclosureType  string
	EnclosureSize  int64
	TranscriptURL  string
	TranscriptType string
	ChaptersURL    string
	ImageURL       string
}

// Feed is a parsed podcast feed: channel metadata plus its episodes. It is what the
// feed parser returns and what an UpsertFeed consumes.
type Feed struct {
	Title       string
	Author      string
	Description string
	Link        string
	Language    string
	Category    string
	Explicit    bool
	GUID        string // <podcast:guid>
	ImageURL    string
	Episodes    []FeedEpisode
}

// OPMLEntry is one subscription line in an OPML document (feed URL plus optional
// display title).
type OPMLEntry struct {
	Title   string
	FeedURL string
}

// UpsertFeedInput carries a parsed feed for atomic persistence: the podcast row is
// created or updated by IdentityKey, then every episode is upserted by its
// per-podcast key. If a feed stops listing older episodes, the store leaves those
// rows in place. Image, when set, is fetched feed artwork ready for the art store.
type UpsertFeedInput struct {
	FeedURL      string
	IdentityKey  string
	Feed         Feed
	ETag         string
	LastModified string
	FetchedAtNS  int64
	Image        *ArtImage // feed cover, or nil
}

// UpsertFeedResult reports what an UpsertFeed changed.
type UpsertFeedResult struct {
	PodcastPID      PID
	Created         bool // the podcast row was newly created
	EpisodesAdded   int
	EpisodesUpdated int
}

// AttachEpisodeFileInput records a downloaded enclosure: the store creates the file
// row in the podcast library, links it to the episode as the primary file, and
// flips the episode to state=present. Image is optional episode artwork.
type AttachEpisodeFileInput struct {
	EpisodePID PID
	LibraryID  int64
	File       File
	Image      *ArtImage
}

// PutTranscriptInput stores an episode's transcript and indexes it in
// transcript_fts so a body hit can rank below a title hit at search time.
type PutTranscriptInput struct {
	EpisodePID PID
	Format     string
	Body       string
	SourceURL  string
}
