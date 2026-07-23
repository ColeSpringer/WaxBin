package model

// Podcast is a subscribed feed. Its stable identity is IdentityKey, derived from
// the feed URL or published <podcast:guid>, not the title. HTTP validators support
// conditional sync. RetentionKeep is the number of newest downloaded episodes to
// keep; 0 keeps all.
type Podcast struct {
	ID          int64
	PID         PID
	FeedURL     string
	IdentityKey string
	Title       string
	SortKey     string
	Author      string
	Description string
	Link        string // show website
	Language    string
	Category    string
	Explicit    bool
	// Podcasting 2.0 channel extras: the show's funding link with its label text,
	// and its declared medium (lowercased; podcast|music|audiobook|...), stored as
	// published with no whitelist.
	FundingURL     string
	FundingMessage string
	Medium         string
	GUID           string // <podcast:guid> when published
	ETag           string
	LastModified   string
	LastFetchedAt  int64 // unix nanoseconds; 0 when never fetched
	RetentionKeep  int
	AuthUser       string // basic-auth user; the password lives in the secret table
	ImageURL       string // current feed image (ingested into the art store on sync)
	// SourceType selects the acquisition provider and sync behavior: rss (an HTTP
	// feed), youtube (an injected provider, feed_url is a channel/playlist URL), or
	// manual (no feed to sync; episodes arrive via UpsertEpisode). Empty reads as
	// rss for older subscriptions.
	SourceType SourceType
	CreatedAt  int64
	UpdatedAt  int64
	// EpisodeCount and DownloadedCount are populated by list/detail reads, not stored.
	EpisodeCount    int
	DownloadedCount int
	// Persons are the show-level <podcast:person> credits. Populated by the detail
	// read (PodcastByPID), not the list.
	Persons []FeedPerson
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
	// Pinned marks an explicitly kept episode that retention never reclaims, so its
	// download outlives a keep-newest-N sweep.
	Pinned bool

	// Downloaded reports whether the episode currently has a local file; FilePID and
	// DisplayPath name it when present.
	Downloaded  bool
	FilePID     PID
	DisplayPath string
	CreatedAt   int64
	UpdatedAt   int64
}

// EpisodeDetail is the full read shape for one episode: the episode plus its
// resolved play state, any stored chapters, and its Podcasting 2.0 extras.
// Persons and Soundbites are detail-only so list reads stay one query.
type EpisodeDetail struct {
	Episode       *Episode
	HasTranscript bool
	Chapters      []Chapter
	Persons       []FeedPerson
	Soundbites    []FeedSoundbite
}

// FeedPerson is one <podcast:person> credit, at the channel (show) or item
// (episode) level. Role and Group are lowercased on parse; an empty Role means
// the feed left it unspecified (the spec default reads as "host").
type FeedPerson struct {
	Name  string
	Role  string
	Group string
	Img   string // portrait URL
	Href  string // profile/info URL
}

// FeedSoundbite is one <podcast:soundbite> clip of a feed item: a highlight
// window into the episode audio.
type FeedSoundbite struct {
	StartMS    int64
	DurationMS int64
	Title      string // "" = use the episode title, per spec
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
	Persons        []FeedPerson
	Soundbites     []FeedSoundbite
}

// Feed is a parsed podcast feed: channel metadata plus its episodes. It is what the
// feed parser returns and what an UpsertFeed consumes.
type Feed struct {
	Title          string
	Author         string
	Description    string
	Link           string
	Language       string
	Category       string
	Explicit       bool
	FundingURL     string // <podcast:funding> url (first tag carrying one)
	FundingMessage string
	Medium         string // <podcast:medium>, lowercased
	GUID           string // <podcast:guid>
	ImageURL       string
	Persons        []FeedPerson // channel-level <podcast:person> credits
	Episodes       []FeedEpisode
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
	// SourceType selects the show's provider (rss|youtube|manual); empty defaults to
	// rss. A youtube channel/playlist id lives in the identity key and feed_url, not a
	// separate column.
	SourceType SourceType
}

// UpsertShowInput creates or updates a show that has no feed sync in this call:
// a manual show, or a youtube channel added before its first enumeration. Episodes
// are added separately via UpsertEpisode. FeedURL is empty for a manual show and the
// channel/playlist URL for a youtube show.
type UpsertShowInput struct {
	IdentityKey string
	FeedURL     string
	SourceType  SourceType
	Title       string
	Author      string
	Description string
	Link        string
	ImageURL    string
	Image       *ArtImage
}

// UpsertEpisodeInput adds or updates a single episode under an existing show,
// bypassing a feed sync. Pinned keeps the episode out of retention pruning. It
// returns the episode's pid and whether it was newly created.
type UpsertEpisodeInput struct {
	PodcastPID PID
	Episode    FeedEpisode
	Pinned     bool
}

// UpsertEpisodeResult reports a single-episode upsert.
type UpsertEpisodeResult struct {
	EpisodePID PID
	Created    bool
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

// Transcript is a stored episode transcript: the reduced searchable text plus
// its provenance. Body is what the reducer kept (words, not timecodes), not the
// original document; a client that renders cues keeps its own copy.
type Transcript struct {
	EpisodePID PID
	Format     string // srt|vtt|json|text (the source document's format)
	Body       string
	SourceURL  string // where it was fetched from; "" for a caller-supplied body
	CreatedAt  int64  // unix nanoseconds
}
