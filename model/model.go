package model

// Library is a registered root and its handling policy. A file belongs to
// exactly one library (roots are validated non-overlapping at config time).
type Library struct {
	ID          int64
	PID         PID
	Root        []byte // raw OS path bytes (non-UTF8 safe)
	DisplayRoot string // fallback UTF-8 rendering for humans/logs
	Mode        Mode
	// Media is the content class a managed root holds (music/audiobook/mixed).
	// Empty is treated as mixed. The internal podcast library ignores it.
	Media     MediaType
	Profile   string // organization profile name
	CreatedAt int64  // unix nanoseconds
}

// MediaType returns the library's media type, defaulting to mixed when unset so an
// older catalog routes as a single content-classified tree.
func (l *Library) MediaType() MediaType {
	if l.Media == "" {
		return MediaMixed
	}
	return l.Media
}

// File is one file on disk: an audio file or a sidecar. Paths are stored as raw
// bytes (BLOB) to survive non-UTF8 filesystems, with a display string alongside.
type File struct {
	ID          int64
	PID         PID
	LibraryID   int64
	Path        []byte // absolute path, raw bytes
	DisplayPath string
	RelPath     []byte // path relative to the library root, raw bytes
	Kind        FileKind
	Size        int64
	MTimeNS     int64 // modification time, nanosecond precision

	ContentHash string // hash of the whole file (changes on any byte change)
	EssenceHash string // hash of decoder-independent audio essence (tag-stable)

	// AnalyzedEssence and AnalysisVersion decide when analysis must rerun: a
	// changed essence or algorithm version invalidates prior analysis.
	AnalyzedEssence string
	AnalysisVersion int

	Container  string
	Codec      string
	DurationMS int64
	Bitrate    int
	SampleRate int
	Channels   int
	BitDepth   int

	ScanState ScanState
	FirstSeen int64 // unix nanoseconds
	LastSeen  int64 // unix nanoseconds
}

// PlayableItem is the logical supertype shared by track/book/episode. Its PK is
// shared with the subtype row.
type PlayableItem struct {
	ID    int64
	PID   PID
	Kind  Kind
	State ItemState
	Title string
	// SortKey is WaxBin-generated (Unicode-fold + "The"-strip + numeric-pad) so
	// a BINARY sort is collation-correct and portable.
	SortKey string
	// IdentityKey is the entity-identity key (see package identity) used to
	// dedup and re-link the item across re-scans. Unique per (Kind, IdentityKey).
	IdentityKey string
	CreatedAt   int64 // unix nanoseconds
	UpdatedAt   int64 // unix nanoseconds
}

// Track is the music subtype of a playable_item (shares its ID). It carries the
// denormalized display columns; normalized artist/album/genre entities are
// resolved and linked alongside during persistence.
type Track struct {
	ItemID      int64
	Artist      string
	ArtistSort  string
	Album       string
	AlbumArtist string
	Composer    string
	Comment     string
	TrackNo     int
	TrackTotal  int
	DiscNo      int
	DiscTotal   int
	Year        int
	Genre       string   // joined display of Genres (the denormalized column)
	Genres      []string // individual genres, resolved into item_genre links
	Compilation bool     // multi-artist release; uses the Various Artists layout
	ISRC        string

	// External identifiers anchor MBID-first entity identity and enrichment
	// lookups. MBID is the recording id (kept for back-compat); the
	// release/release-group/artist ids populate the matching entity rows.
	MBID             string // MusicBrainz recording id
	MBReleaseID      string
	MBReleaseGroupID string
	MBArtistID       string
	MBAlbumArtistID  string
}

// ItemFile is an edge from a logical item to a backing file. Offsets support a
// single file holding multiple items, such as a chapterized audiobook.
type ItemFile struct {
	ItemID   int64
	FileID   int64
	Role     string // "primary" for the main audio file
	Position int
	StartMS  int64
	EndMS    int64
}

// Tags is metadata read from a file's tags, with filename-derived values as a
// fallback. It is the boundary type between metadata readers and the rest of the
// engine. The WaxLabel adapter populates it for every format without decoding
// PCM.
type Tags struct {
	Title       string
	Artist      string   // primary credited artist (display)
	Artists     []string // all credited artists, primary first
	AlbumArtist string
	Album       string
	Composer    string
	Comment     string
	TrackNo     int
	TrackTotal  int
	DiscNo      int
	DiscTotal   int
	Year        int
	Genre       string   // joined display of Genres
	Genres      []string // split genres, normalized into entities downstream
	Compilation bool
	ISRC        string

	// Sort names from tags, used to seed collation sort keys when present.
	ArtistSort      string
	AlbumSort       string
	AlbumArtistSort string

	// External identifiers (MBID-first identity + enrichment fast-path).
	MBID             string // MusicBrainz recording id
	MBReleaseID      string
	MBReleaseGroupID string
	MBArtistID       string
	MBAlbumArtistID  string

	// Audio properties, read from the container without decoding PCM.
	Container  string
	Codec      string
	DurationMS int64
	Bitrate    int
	SampleRate int
	Channels   int
	BitDepth   int

	// Audiobook / spoken-word fields, populated by the WaxLabel adapter from
	// spoken-word tags. IsAudiobook drives the scanner's book-vs-track branch;
	// Edition disambiguates an abridged release from the same title's unabridged
	// one. Abridged is nil when the tags do not say.
	IsAudiobook bool
	Subtitle    string
	Narrators   []string
	Series      string
	SeriesSeq   string
	Publisher   string
	ASIN        string
	ISBN        string
	Edition     string
	Abridged    *bool
	Description string

	// Chapters are the file's embedded navigation chapters (M4B Nero/QuickTime,
	// Matroska, MP3 CHAP), file-relative, in file order. Empty for music.
	Chapters []Chapter
}

// ItemView is the denormalized read shape returned by queries: a playable_item
// joined with its subtype and primary file. It is what consumers render.
type ItemView struct {
	PID         PID
	Kind        Kind
	State       ItemState
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	TrackNo     int
	DiscNo      int
	Year        int
	Genre       string
	Compilation bool // a multi-artist compilation (drives Various Artists layout)
	DurationMS  int64

	// Audiobook fields, populated for book items (empty for tracks). Author maps
	// onto Artist for the shared read/organize paths; these carry the extras the
	// audiobook layout and detail view need.
	AuthorSort string
	Narrator   string
	Series     string
	SeriesSeq  string
	Subtitle   string
	ASIN       string

	// Podcast/episode fields, populated only for episode items. Album and Artist
	// carry the podcast title through the shared view; these fields hold the
	// episode-specific values used by layouts and detail views.
	Season    int
	PubDateNS int64 // publication time, unix nanoseconds (0 when undated)

	// Source is the item's acquisition origin (local/rss/youtube/manual). A locally
	// scanned item has no acquisition row and reads back as "local"; an acquired item
	// carries the source type used at ingest. Queries can filter on this field.
	Source SourceType

	FilePID     PID
	Path        []byte // raw bytes of the primary file path
	DisplayPath string
	Container   string
	Codec       string
}

// Change is one row of the change_log: the single delta vocabulary consumers
// tail to keep their caches current.
type Change struct {
	Seq        int64
	TS         int64 // unix nanoseconds
	EntityType string
	EntityPID  PID
	Op         ChangeOp
}
