package model

// AuditCheck names a category of audit finding. Consumers filter and group by it.
type AuditCheck string

const (
	CheckDuplicateArtist AuditCheck = "duplicate_artist"
	CheckDuplicateGenre  AuditCheck = "duplicate_genre"
	CheckDuplicateAlbum  AuditCheck = "duplicate_album"
	// CheckDuplicateReleaseGroup reports release groups sharing one MusicBrainz id,
	// the group-rung sibling of duplicate_album.
	CheckDuplicateReleaseGroup AuditCheck = "duplicate_release_group"
	CheckSplitAlbum            AuditCheck = "split_album"
	CheckInconsistentMeta      AuditCheck = "inconsistent_metadata"
	CheckMissingArt            AuditCheck = "missing_art"
	CheckMissingReplayGain     AuditCheck = "missing_replaygain"
	CheckBadFilename           AuditCheck = "bad_filename"
	CheckOrphanSidecar         AuditCheck = "orphan_sidecar"
	CheckPathConflict          AuditCheck = "path_conflict"
	CheckInvalidFeed           AuditCheck = "invalid_feed"
	CheckDerivedData           AuditCheck = "derived_data"
	CheckIntegrity             AuditCheck = "integrity"
	CheckCorruptAudio          AuditCheck = "corrupt_audio"
	// CheckFileDiagnostic reports the diagnostics the scan and tag writers persisted
	// (unsupported containers, legacy-only tag fallbacks, partial lyrics, lost tag
	// writes). Corrupt-audio diagnostics belong to CheckCorruptAudio instead, so that
	// one concept keeps one --check name.
	CheckFileDiagnostic AuditCheck = "file_diagnostic"
	// CheckMissingMBID reports items nothing can resolve to a MusicBrainz recording,
	// release, or release group, which is what a Cover Art Archive lookup or a rich
	// presence card needs.
	CheckMissingMBID AuditCheck = "missing_mbid"
	// CheckLibraryConflict reports library roots that differ only by case, which name
	// one tree on a case-insensitive filesystem. The store now folds when matching a
	// root, so new ones cannot collide; this is for a catalog that already holds both
	// spellings from before it did.
	CheckLibraryConflict AuditCheck = "library_conflict"
	// CheckDurationMismatch reports audio files whose header states a length the
	// decoded audio does not have, off by more than two seconds and two percent. It
	// reads the span the analyze pass stored with each waveform, so it covers analyzed
	// files only.
	CheckDurationMismatch AuditCheck = "duration_mismatch"
	// CheckDuplicateCopy lists the alternate files items hold beside their primaries: a
	// copy of the same audio, or another encoding of the recording. It is informational;
	// a copy can be deleted on its own with rm --file.
	CheckDuplicateCopy AuditCheck = "duplicate_copy"
)

// AuditChecks returns every known audit check, for validation and help text.
func AuditChecks() []AuditCheck {
	return []AuditCheck{
		CheckDuplicateArtist, CheckDuplicateGenre, CheckDuplicateAlbum,
		CheckDuplicateReleaseGroup, CheckSplitAlbum,
		CheckInconsistentMeta, CheckMissingArt, CheckMissingReplayGain, CheckBadFilename,
		CheckOrphanSidecar, CheckPathConflict, CheckInvalidFeed, CheckDerivedData,
		CheckIntegrity, CheckCorruptAudio, CheckFileDiagnostic, CheckMissingMBID,
		CheckLibraryConflict, CheckDurationMismatch, CheckDuplicateCopy,
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
	FilePID   PID         // the file a file-level finding names, when the check had one
	MergeType MergeEntity // set on duplicate findings, "" otherwise
	// HeaderMS and DecodedMS are a duration_mismatch finding's two lengths.
	HeaderMS  int64
	DecodedMS int64
}

// DuplicateMember is one entity in a duplicate set.
type DuplicateMember struct {
	PID        PID
	Name       string
	TrackCount int
}

// DuplicateSet is a group of entities that should probably be one: they share an
// MBID, or normalize to the same collation key, or (albums) carry one title under one
// album artist. The audit turns each set into a merge-candidate finding (survivor = the
// member backing the most tracks).
type DuplicateSet struct {
	EntityType MergeEntity
	Reason     string
	Members    []DuplicateMember
}

// ReasonSameAlbumName is the reason of a duplicate album set found by name, the albums
// a folder keeps apart.
const ReasonSameAlbumName = "same title and album artist"

// SplitAlbum reports one album title by one artist spread across multiple album
// entities (its tracks split by folder/tags into separate rows).
type SplitAlbum struct {
	Artist string
	Title  string
	Albums []DuplicateMember
}

// AlbumIssue reports metadata inconsistency within one album entity. RepeatedPositions
// counts the track numbers (within a disc) two or more of its members claim, which is how
// two same-titled releases sharing a folder, and so an album, show.
type AlbumIssue struct {
	AlbumPID          PID
	Title             string
	Problem           string
	RepeatedPositions int
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

// FileDurationMismatch is one audio file whose header duration disagrees with the
// length its current waveform was decoded from.
type FileDurationMismatch struct {
	FilePID     PID
	DisplayPath string
	HeaderMS    int64
	DecodedMS   int64
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
	BookISBNKeyDrift        int
	AlbumYearDrift          int
}

// Consistent reports whether the derived data is drift-free.
func (d DerivedDrift) Consistent() bool {
	return d.ItemsMissingFTS == 0 && d.OrphanFTSRows == 0 &&
		d.ArtistRollupDrift == 0 && d.GenreRollupDrift == 0 &&
		d.ReleaseGroupRollupDrift == 0 && d.SortKeyDrift == 0 &&
		d.BookDurationDrift == 0 && d.BookISBNKeyDrift == 0 && d.AlbumYearDrift == 0
}

// CopyReason says why a file is an alternate of its item.
type CopyReason string

const (
	// CopySameAudio is a file holding the same audio as one of its item's parts.
	CopySameAudio CopyReason = "same audio"
	// CopyOtherEncoding is another encoding of the item's recording. It ranked below the
	// primary when it was attached, a writable library first and then quality, so a
	// better encoding in a read-only library is one too.
	CopyOtherEncoding CopyReason = "other encoding"
)

// ItemCopies is an item holding alternate files, with every file it has: its parts in
// reading order, then its alternates, each with the reason it is one.
type ItemCopies struct {
	ItemPID PID
	Kind    Kind
	Title   string
	Artist  string
	Files   []CopyFile
}

// CopyFile is one file of an item with copies. Reason is empty for a part.
type CopyFile struct {
	FilePID     PID
	LibraryPID  PID
	DisplayPath string
	Size        int64
	Role        string
	Reason      CopyReason
	Codec       string
	Bitrate     int
	SampleRate  int
	BitDepth    int
}
