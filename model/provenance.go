package model

// ProvenanceSource records where a field's current value came from. The absence
// of a field_provenance row means the value is plain tag-sourced and unlocked;
// a row is written only for the non-default cases below.
type ProvenanceSource string

const (
	SourceTag        ProvenanceSource = "tag"        // read from the file's tags (default; usually no row)
	SourceUser       ProvenanceSource = "user"       // edited by a user
	SourceEnrichment ProvenanceSource = "enrichment" // written by a metadata provider
	SourceOrganize   ProvenanceSource = "organize"   // written by an organize tag write-back
)

// Valid reports whether s is a known provenance source.
func (s ProvenanceSource) Valid() bool {
	switch s {
	case SourceTag, SourceUser, SourceEnrichment, SourceOrganize:
		return true
	default:
		return false
	}
}

// MetadataFields enumerates the curatable, lockable item fields. It is the
// whitelist behind lock/unlock and provenance edits, mirroring the query field
// whitelist: a field outside this set is rejected rather than stored, so callers
// cannot create junk provenance rows.
var MetadataFields = map[string]bool{
	"title":        true,
	"artist":       true,
	"album_artist": true,
	"album":        true,
	"composer":     true,
	"genre":        true,
	"year":         true,
	"track_no":     true,
	"disc_no":      true,
	"comment":      true,
}

// IsMetadataField reports whether field is a curatable/lockable metadata field.
func IsMetadataField(field string) bool { return MetadataFields[field] }

// FieldProvenance is one provenance row: a field's source, lock state, and the
// curated value when a user set one.
type FieldProvenance struct {
	ItemPID   PID
	Field     string
	Source    ProvenanceSource
	Locked    bool
	Value     string
	UpdatedAt int64 // unix nanoseconds
}
