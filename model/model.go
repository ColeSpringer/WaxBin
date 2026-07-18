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

	// Album-level release identifiers carried from the file's tags into album
	// resolution (they land on the album entity, not the denormalized track row).
	Barcode       string
	Label         string
	CatalogNumber string
}

// ItemFile is an edge from a logical item to a backing file. The offsets support a
// single file holding multiple items, as a single-file album rip carved into virtual
// tracks does; they are CD frames (75/sec), the unit the .cue that carved them is
// written in, and 0 for a whole-file edge.
type ItemFile struct {
	ItemID      int64
	FileID      int64
	Role        string // "primary" for the main audio file
	Position    int
	StartFrames int64
	EndFrames   int64
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

	// Release identifiers (album-level), carried into album resolution. They are
	// stored on the album entity, not the track row.
	Barcode       string
	Label         string
	CatalogNumber string

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

	// Custom holds the tag frames WaxBin's typed model does not map (keyed by canonical
	// uppercase tag key), so they are preserved on scan and searchable rather than
	// dropped. The reserved keys WaxBin owns elsewhere are excluded (see IsReservedTagKey).
	Custom map[string][]string

	// Acquisition is origin provenance carried by the file's own tags, when it has
	// any. It does not feed Year; see TagAcquisition for why.
	Acquisition TagAcquisition
}

// TagAcquisition is origin provenance read from a file's own tags, meaning the
// acquisition keys a downloader stamps into what it writes. It is evidence of
// external origin carried by the file itself, as distinct from an acquisition event
// WaxBin performed and recorded.
//
// AcquiredAt does not feed Tags.Year, and must not. Year is the release year of the
// work: it drives the year: query field, the year facet, sort keys, and organize's
// {year} path token. Folding an acquisition date in would catalog a 1975 album
// downloaded in 2019 as year=2019, corrupting the facet and the on-disk layout with it.
type TagAcquisition struct {
	SourceURL  string
	SourceID   string
	AcquiredAt int64 // unix nanoseconds; 0 when absent or unparseable
}

// Present reports whether the tags claim an external origin. It requires a URL or an
// ID. A bare ACQUISITION_DATE is not such a claim, since a local rip can carry one,
// so on its own it must never flip an item off source:local.
func (a TagAcquisition) Present() bool {
	return a.SourceURL != "" || a.SourceID != ""
}

// TagWriteWarning is one non-fatal condition reported by an on-disk tag write,
// projected out of the tag library's own warning vocabulary so the model stays
// independent of it. Key is the canonical tag key the warning concerns, or "" for a
// warning that names no specific key. Message is pre-sanitized for display.
//
// Unrepresented marks the subset that means the value did not land: the write itself
// succeeded, but the key does not hold what the caller asked for. It is the only
// field a consumer should branch on. An advisory warning, such as a benign format
// note, carries Unrepresented=false and must not gate anything.
type TagWriteWarning struct {
	Key           string
	Code          string
	Message       string
	Unrepresented bool
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

	// Virtual reports that this item is a virtual track carved out of a shared
	// single-file album rip by a .cue sheet: it plays only a window within the backing
	// file rather than the whole file.
	//
	// The window carries two coordinate systems with different jobs. StartFrames/
	// EndFrames are CD frames (75/sec), the cue sheet's own unit and what is stored:
	// they are the track's content identity, exact to the sample at every rate a
	// player serves. StartMS/EndMS are the same window in milliseconds, derived
	// through FramesToMS for display and for a player's seek. Read FramesToMS before
	// combining the millisecond pair with anything: each field rounds independently,
	// so the arithmetic across them does not close.
	//
	// All four are 0 for a whole-file item (Virtual is false). EndFrames (and so
	// EndMS) is 0 when the window runs to the end of the file.
	Virtual     bool
	StartFrames int64
	EndFrames   int64
	StartMS     int64
	EndMS       int64

	FilePID PID
	Path    []byte // raw bytes of the primary file path
	// SampleRate is the backing file's sample rate in Hz, 0 when the header did not
	// declare one. A virtual track's consumer needs it to convert the frame window to
	// sample offsets.
	SampleRate  int
	DisplayPath string
	Container   string
	Codec       string
}

// FramesPerSecond is the CD frame rate: a frame is 1/75 s, the disc's own addressing
// quantum and the unit every MM:SS:FF in a .cue sheet is written in. It is a fixed
// property of the CD format, not a tunable.
//
// It is the reason a window is stored in frames at all: every sample rate a player
// serves divides 75 exactly (44100/75 = 588, 48000/75 = 640, and likewise the rest of
// the CD and hi-res families), so a frame converts to a sample with no rounding,
// while a millisecond quantizes 50 of every 75 frames away.
const FramesPerSecond = 75

// FramesToMS converts CD frames to milliseconds for display and for a player's seek.
// It is lossy by construction: only frames divisible by 3 land on a whole
// millisecond. Never convert back. A span is content identity and a millisecond is
// not precise enough to carry one.
//
// Every millisecond field derived through it is rounded independently, so arithmetic
// across them does not close: a 1-frame to 3-frame window reports StartMS 13, EndMS
// 40, and DurationMS 26, and 13 + 26 is not 40. Read them; don't combine them.
func FramesToMS(frames int64) int64 { return frames * 1000 / FramesPerSecond }

// Change is one row of the change_log: the single delta vocabulary consumers
// tail to keep their caches current.
type Change struct {
	Seq        int64
	TS         int64 // unix nanoseconds
	EntityType string
	EntityPID  PID
	Op         ChangeOp
}
