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
// release MBID, and the release identifiers (barcode/label/catalog number).
var albumEntityEditFields = map[string]bool{
	"sort": true, "mbid": true, "barcode": true, "label": true, "catalog_number": true,
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

// EntityCuration is one entity_curation row: an entity field's source, lock state, and
// the curated value when a user set one.
type EntityCuration struct {
	EntityType MergeEntity
	EntityPID  PID
	Field      string
	Source     ProvenanceSource
	Locked     bool
	Value      string
	UpdatedAt  int64 // unix nanoseconds
}
