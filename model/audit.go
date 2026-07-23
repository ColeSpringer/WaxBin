package model

// AuditCheck names a category of audit finding. Consumers filter and group by it.
type AuditCheck string

const (
	CheckDuplicateArtist   AuditCheck = "duplicate_artist"
	CheckDuplicateGenre    AuditCheck = "duplicate_genre"
	CheckDuplicateAlbum    AuditCheck = "duplicate_album"
	CheckSplitAlbum        AuditCheck = "split_album"
	CheckInconsistentMeta  AuditCheck = "inconsistent_metadata"
	CheckMissingArt        AuditCheck = "missing_art"
	CheckMissingReplayGain AuditCheck = "missing_replaygain"
	CheckBadFilename       AuditCheck = "bad_filename"
	CheckOrphanSidecar     AuditCheck = "orphan_sidecar"
	CheckPathConflict      AuditCheck = "path_conflict"
	CheckInvalidFeed       AuditCheck = "invalid_feed"
	CheckDerivedData       AuditCheck = "derived_data"
	CheckIntegrity         AuditCheck = "integrity"
	CheckCorruptAudio      AuditCheck = "corrupt_audio"
	// CheckFileDiagnostic reports the diagnostics the scan and tag writers persisted
	// (unsupported containers, legacy-only tag fallbacks, partial lyrics, lost tag
	// writes). Corrupt-audio diagnostics belong to CheckCorruptAudio instead, so that
	// one concept keeps one --check name.
	CheckFileDiagnostic AuditCheck = "file_diagnostic"
)

// AuditChecks returns every known audit check, for validation and help text.
func AuditChecks() []AuditCheck {
	return []AuditCheck{
		CheckDuplicateArtist, CheckDuplicateGenre, CheckDuplicateAlbum, CheckSplitAlbum,
		CheckInconsistentMeta, CheckMissingArt, CheckMissingReplayGain, CheckBadFilename,
		CheckOrphanSidecar, CheckPathConflict, CheckInvalidFeed, CheckDerivedData,
		CheckIntegrity, CheckCorruptAudio, CheckFileDiagnostic,
	}
}

// Valid reports whether c is a known audit check.
func (c AuditCheck) Valid() bool {
	for _, k := range AuditChecks() {
		if c == k {
			return true
		}
	}
	return false
}

// AuditSeverity ranks a finding. error = broken/data loss; warn = should fix;
// info = expected-but-worth-surfacing (e.g. no ReplayGain because analysis has
// not run).
type AuditSeverity string

const (
	SeverityInfo  AuditSeverity = "info"
	SeverityWarn  AuditSeverity = "warn"
	SeverityError AuditSeverity = "error"
)

// Valid reports whether s is a known severity.
func (s AuditSeverity) Valid() bool {
	switch s {
	case SeverityInfo, SeverityWarn, SeverityError:
		return true
	default:
		return false
	}
}

// AuditFinding is one issue the audit reports. For duplicate/split findings,
// MergeType + Entities describe a repair the `merge` primitive can apply
// (Entities[0] is the suggested survivor).
type AuditFinding struct {
	Check     AuditCheck
	Severity  AuditSeverity
	Message   string
	Entities  []PID       // involved entity/item PIDs (survivor first for merges)
	Path      string      // involved on-disk path, for file-level findings
	MergeType MergeEntity // set on duplicate findings, "" otherwise
}

// DuplicateMember is one entity in a duplicate set.
type DuplicateMember struct {
	PID        PID
	Name       string
	TrackCount int
}

// DuplicateSet is a group of entities that should probably be one: they share an
// MBID, or normalize to the same collation key. The audit turns each set into a
// merge-candidate finding (survivor = the member backing the most tracks).
type DuplicateSet struct {
	EntityType MergeEntity
	Reason     string
	Members    []DuplicateMember
}

// SplitAlbum reports one album title by one artist spread across multiple album
// entities (its tracks split by folder/tags into separate rows).
type SplitAlbum struct {
	Artist string
	Title  string
	Albums []DuplicateMember
}

// AlbumIssue reports metadata inconsistency within one album entity.
type AlbumIssue struct {
	AlbumPID PID
	Title    string
	Problem  string
}

// AuditFileInfo is the file-row projection the filesystem-level checks inspect
// (bad filenames, orphan sidecars, path conflicts, integrity/corrupt audio).
type AuditFileInfo struct {
	PID         PID
	Path        []byte
	DisplayPath string
	Kind        FileKind
	ContentHash string
	ItemPID     PID // owning item, if any
}

// ItemRef is a minimal item reference for list-style findings.
type ItemRef struct {
	PID   PID
	Title string
	Kind  Kind
}

// DerivedDrift mirrors the derived-data consistency counts (FTS/rollups/sort
// keys) for the audit report, so audit can fold `db verify`'s result in without
// depending on the store's report type.
type DerivedDrift struct {
	ItemsMissingFTS         int
	OrphanFTSRows           int
	ArtistRollupDrift       int
	GenreRollupDrift        int
	ReleaseGroupRollupDrift int
	SortKeyDrift            int
	BookDurationDrift       int
}

// Consistent reports whether the derived data is drift-free.
func (d DerivedDrift) Consistent() bool {
	return d.ItemsMissingFTS == 0 && d.OrphanFTSRows == 0 &&
		d.ArtistRollupDrift == 0 && d.GenreRollupDrift == 0 &&
		d.ReleaseGroupRollupDrift == 0 && d.SortKeyDrift == 0 &&
		d.BookDurationDrift == 0
}
