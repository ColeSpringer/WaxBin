package model

import "strings"

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

// MetadataFields enumerates the SCALAR, one-value item fields that can be set by the
// field-edit API (and, per kind, provenance-tracked). It is the whitelist behind
// scalar edits and SetFieldProvenance, and works like the query field whitelist: a
// field outside it is rejected rather than stored, so callers cannot create junk
// provenance rows. Which fields actually apply to a given item is kind-specific, and
// the editor enforces that. This is the union across track/book/episode. The
// structured artifacts (lyrics/chapters/art) and credit roles are not here; they are
// only lockable (see IsCuratableField), not scalar-editable.
var MetadataFields = map[string]bool{
	// Shared and track fields.
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
	"isrc":         true,
	"mbid":         true,
	"compilation":  true,
	// Audiobook fields.
	"author":      true,
	"narrator":    true,
	"series":      true,
	"subtitle":    true,
	"asin":        true,
	"isbn":        true,
	"publisher":   true,
	"edition":     true,
	"description": true,
	// Podcast-episode fields (title and description are shared above).
	"pinned":       true,
	"season":       true,
	"episode_no":   true,
	"episode_type": true,
	"explicit":     true,
	"link":         true,
}

// lockOnlyFields are structured artifacts that are lockable but NOT scalar-editable:
// each has its own edit API (SetItemLyrics/SetItemChapters/SetItemArt), and the whole
// artifact is locked as a unit. They belong to IsCuratableField (the lock whitelist)
// but never to IsMetadataField (the scalar-edit gate), so a scalar `--set art=x` or a
// SetFieldProvenance("art", ...) call is rejected instead of writing a junk row.
var lockOnlyFields = map[string]bool{
	"lyrics":   true,
	"chapters": true,
	"art":      true,
}

// IsMetadataField reports whether field is a scalar, one-value curatable/editable
// metadata field. The structured artifacts (lyrics/chapters/art) and namespaced
// credit keys are NOT scalar fields; use IsCuratableField for the lock whitelist.
func IsMetadataField(field string) bool { return MetadataFields[field] }

// IsCuratableField reports whether field may carry a provenance/lock row. It is the
// lock whitelist: the superset of IsMetadataField plus the structured lock-only
// artifacts (lyrics/chapters/art) and a namespaced credit key ("credit.<role>"). The
// scalar edit path stays on IsMetadataField so a credit key or an art lock cannot be
// set as if it were a scalar (those go through their own APIs).
func IsCuratableField(field string) bool {
	if MetadataFields[field] || lockOnlyFields[field] {
		return true
	}
	if role, ok := CutCreditPrefix(field); ok {
		return ContributorRole(role).Valid()
	}
	return false
}

// CutCreditPrefix returns the role portion of a "credit.<role>" field and whether the
// prefix was present with a non-empty role. It is the one place the credit-field
// namespace convention lives, shared by the model whitelist and the store's kind gate.
func CutCreditPrefix(field string) (string, bool) {
	const p = "credit."
	if len(field) > len(p) && field[:len(p)] == p {
		return field[len(p):], true
	}
	return "", false
}

// ParseBoolValue parses a boolean edit value, accepting the common truthy/falsy
// spellings case-insensitively. An empty string is a recognized false (a clear). ok is
// false for a value it does not recognize. It is the single source of the boolean
// vocabulary shared by the store's field validator and the on-disk tag write-back, so
// the two can never disagree about what "yes" means.
func ParseBoolValue(s string) (val, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "no", "off", "n", "f":
		return false, true
	case "1", "true", "yes", "on", "y", "t":
		return true, true
	default:
		return false, false
	}
}

// BatchEditResult reports the outcome of a multi-item field edit: the items whose
// edit applied and, in skip-locked mode, the items skipped because a target field was
// locked. The two lists are disjoint and preserve input order.
type BatchEditResult struct {
	Edited  []PID
	Skipped []PID
}

// FieldProvenance is one provenance row: a field's source, lock state, the curated
// value when a user set one, and the provider that supplied an enrichment value.
type FieldProvenance struct {
	ItemPID   PID
	Field     string
	Source    ProvenanceSource
	Locked    bool
	Value     string
	Provider  string // enrichment provider id (empty for tag/user/organize rows)
	UpdatedAt int64  // unix nanoseconds
}
