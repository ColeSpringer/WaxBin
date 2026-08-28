package model

// This file defines the vocabulary for entity-level curation: editing a field on a
// shared entity (an artist, a release group, or an album) rather than on one item.
// The two motivating cases are sort-name overrides (a user-chosen collation name) and
// release identifiers (barcode/label/catalog number and the entity MBIDs). Entity
// edits are recorded in the entity_curation table, the entity-scoped analogue of
// field_provenance, and a lock there protects the value from an enrichment overwrite.

// artistEntityEditFields are the fields editable on an artist entity: a sort-name
// override and the artist MBID.
var artistEntityEditFields = map[string]bool{"sort": true, "mbid": true}

// releaseGroupEntityEditFields are the fields editable on a release group: a sort-name
// override, the group MBID, and the release-group type (album|ep|single|…), the one
// field enrichment writes unconditionally, so a user edit must lock it.
var releaseGroupEntityEditFields = map[string]bool{"sort": true, "mbid": true, "type": true}

// albumEntityEditFields are the fields editable on an album: a sort-name override, the
// release MBID, the release identifiers (barcode/label/catalog number), and the two
// descriptive edition columns (media/country).
var albumEntityEditFields = map[string]bool{
	"sort": true, "mbid": true, "barcode": true, "label": true, "catalog_number": true,
	"media": true, "country": true,
}

// entityEditFieldsFor returns the editable-field set for an entity type, or nil for a
// type that supports no entity editing (genre carries no editable identifier).
func entityEditFieldsFor(et MergeEntity) map[string]bool {
	switch et {
	case MergeArtist:
		return artistEntityEditFields
	case MergeReleaseGroup:
		return releaseGroupEntityEditFields
	case MergeAlbum:
		return albumEntityEditFields
	default:
		return nil
	}
}

// EntityEditable reports whether an entity type supports field editing at all.
func EntityEditable(et MergeEntity) bool { return entityEditFieldsFor(et) != nil }

// IsEntityEditField reports whether field is an editable/lockable field on the given
// entity type. It is the entity-scoped analogue of IsMetadataField.
func IsEntityEditField(et MergeEntity, field string) bool {
	fs := entityEditFieldsFor(et)
	return fs != nil && fs[field]
}

// releaseGroupTypes are the accepted release_group.type values (matching enrichment's
// vocabulary). An empty value clears the type.
var releaseGroupTypes = map[string]bool{
	"album": true, "ep": true, "single": true, "compilation": true, "audiobook": true,
}

// ValidReleaseGroupType reports whether s is an accepted release-group type.
func ValidReleaseGroupType(s string) bool { return releaseGroupTypes[s] }

// EntityEditReport records what an entity edit did beyond writing the fields it was
// given. MergedInto names the survivor when clearing an mbid re-keyed the entity onto a
// key a heuristic twin already owned, which is the one case where the entity the caller
// named no longer exists afterwards. It is empty for every other edit.
//
// MovedAlbums names the albums a release-group clear settled onto a group of their own,
// which a differently-titled edition sharing the id takes because the album title lives
// inside a heuristic group key. Their members left the edited group, so a caller fanning
// over that group's members no longer reaches them; write-back reads this to strip their
// files too. It is empty for every other edit.
type EntityEditReport struct {
	MergedInto  PID
	MovedAlbums []PID
}

// EntityRenamable reports whether an entity type can be renamed, which means it has
// keying fields its members carry. Genre is out: its key is its own name, with no member
// field spelling it.
func EntityRenamable(et MergeEntity) bool {
	switch et {
	case MergeAlbum, MergeReleaseGroup, MergeArtist:
		return true
	default:
		return false
	}
}

// EntityRenameOutcome names what a rename did to the entity's identity key. It is a
// string rather than a bool because a bool would say no more than MergedInto == "", and
// because a fourth outcome is conceivable if split is ever allowed back.
type EntityRenameOutcome string

const (
	// EntityRenamed means the key moved and the row stayed, keeping its pid and
	// everything attached to it.
	EntityRenamed EntityRenameOutcome = "renamed"
	// EntityRenameMerged means the new key was already taken, so the row folded into
	// the incumbent and the named entity no longer exists.
	EntityRenameMerged EntityRenameOutcome = "merged"
	// EntityRenameRefreshed means the new key equals the old one, so only the display
	// columns moved. A case-only rename lands here.
	EntityRenameRefreshed EntityRenameOutcome = "refreshed"
)

// EntityRenameReport records what renaming a whole entity did. Members is how many items
// carried the rename, which is every member the entity had: coverage is what makes the
// in-place branch fire instead of the split an under-covered batch falls back to.
//
// MergedInto names the survivor when Outcome is merged, for the same reason
// EntityEditReport carries it: the caller's pid is gone and it needs somewhere to go on
// talking about. MovedAlbums names albums that came out under a different release group
// than they went in under, which an album rename can do when its new anchor or title
// implies a group the album was not in.
type EntityRenameReport struct {
	EntityPID   PID
	Outcome     EntityRenameOutcome
	MergedInto  PID
	MovedAlbums []PID
	Members     int
	// Credits is how many contributor-role credits moved with the rename: the roles that
	// back no item field of their own (producer, composer, narrator, translator, editor),
	// applied on the credit surface inside the same transaction. Members counts the field
	// half, and an item can be in both.
	Credits int
	// MemberEdits is what the rename actually wrote, per member: the item and the field
	// values that landed in its columns. Write-back sends exactly this rather than
	// re-deriving the member list afterwards, which would reach for the surviving entity
	// after a merge and rewrite files that were never part of the rename. It is local to
	// the process that ran the rename and is not carried on the wire; a proxied caller's
	// write-back runs on the server, which has it.
	MemberEdits []ItemFieldEdit
	// CreditEdits is the credit half of the same record: what each contributor role now
	// holds, per item and role. Like MemberEdits it is local to the process that ran the
	// rename and is not carried on the wire, so a proxied caller's write-back runs on the
	// server, which has it. Sending it would silently write back nothing.
	CreditEdits []ItemCreditEdit
}

// DetachReport records a per-member detach: the track pulled off an album keyed on a
// MusicBrainz release id, the album it left, and the album and release group it landed
// on. The two new pids are empty when the member's own tags carry no grouping evidence
// beyond the release id, which leaves it ungrouped exactly as a scan of those tags
// would.
type DetachReport struct {
	ItemPID            PID
	OldAlbumPID        PID
	NewAlbumPID        PID
	NewReleaseGroupPID PID
}

// EntityCuration is one entity_curation row: an entity field's source and the provider
// behind it, its lock state, and the curated value when a user set one. It carries no
// SourceURL, because entity_curation has no column for one, which is why Attribution is
// not embedded here the way it is on FieldProvenance.
type EntityCuration struct {
	EntityType MergeEntity
	EntityPID  PID
	Field      string
	Source     ProvenanceSource
	Provider   string // enrichment provider id (empty for a tag or user edit)
	Locked     bool
	Value      string
	UpdatedAt  int64 // unix nanoseconds
}
