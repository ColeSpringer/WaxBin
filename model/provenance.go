package model

import "strings"

// ProvenanceSource records where a value came from. It is shared by two surfaces
// with different value sets. On a scalar field (field_provenance, entity_curation)
// the absence of a row means the value is plain tag-sourced and unlocked, so a row
// is written only for the non-default cases; ValidForField is that surface's gate.
// On an artifact (an art_map cover, a lyrics row) every row carries a source,
// including the ordinary tag one, because the artifact exists whether or not it was
// curated and a consumer wants to mark where the picture came from.
//
// The neighbouring SourceType constants in enums.go (SourceRSS, SourceYouTube,
// SourceManual, SourceLocal) are a different type, describing how an item was acquired
// rather than where a value came from.
type ProvenanceSource string

const (
	// Scalar fields and artifacts both.
	SourceTag        ProvenanceSource = "tag"        // read from the file's own tags (an APIC frame, an embedded USLT)
	SourceUser       ProvenanceSource = "user"       // set through the curation surface
	SourceEnrichment ProvenanceSource = "enrichment" // supplied by a metadata provider, named in Provider
	// Scalar fields only.
	SourceOrganize ProvenanceSource = "organize" // written by an organize tag write-back
	// Artifacts only.
	SourceSidecar ProvenanceSource = "sidecar" // read from a companion file beside the audio (cover.jpg, .lrc)
	SourceFeed    ProvenanceSource = "feed"    // supplied by a podcast feed or its episode metadata
	// SourceGenerated is a picture WaxBin composed from what the catalog already holds,
	// a playlist mosaic being the case that asked for it. Nothing here produces one; the
	// value is for a client composing one through SetEntityArt, which used to store it as
	// a cover a hand chose. Not named derived: ArtProvenance.Derived already means an
	// album answered from a member track's cover, a fact about the resolution chain.
	SourceGenerated ProvenanceSource = "generated"
)

// Valid reports whether s is a known provenance source, across every surface. It is
// the vocabulary half of Attribution.Valid, which adds the provider pairing; a storage
// surface narrows it further through one of the ValidFor* gates.
func (s ProvenanceSource) Valid() bool {
	switch s {
	case SourceTag, SourceUser, SourceEnrichment, SourceOrganize, SourceSidecar, SourceFeed,
		SourceGenerated:
		return true
	default:
		return false
	}
}

// ValidForField reports whether s may be stored on a scalar field row. The artifact
// values (sidecar, feed, generated) are excluded: nothing produces a scalar field from
// a cover.jpg, a feed image, or a composition, so accepting one here would only mint
// the junk row IsMetadataField exists to prevent.
func (s ProvenanceSource) ValidForField() bool {
	switch s {
	case SourceTag, SourceUser, SourceEnrichment, SourceOrganize:
		return true
	default:
		return false
	}
}

// Attribution is where one stored value or artifact came from, as one value, so a
// write path carries the caller's whole answer instead of taking a source and inventing
// the other two.
type Attribution struct {
	Source    ProvenanceSource
	Provider  string
	SourceURL string
}

// OrUser defaults an empty source to SourceUser: a curation write that names no origin
// is a user edit. It is the one place that default lives.
func (a Attribution) OrUser() Attribution {
	if a.Source == "" {
		a.Source = SourceUser
	}
	return a
}

// Valid reports whether a is storable: a known source, paired the way only the whole
// value can express. An enrichment value names the provider that supplied it, and the
// sources with no external provider carry none. A feed is exempt because a feed is its
// own provider and nothing names one for it. SourceURL takes no pairing rule; it is
// empty for the on-disk sources by convention, and a rule there would only refuse
// legitimate writes.
func (a Attribution) Valid() bool {
	if !a.Source.Valid() {
		return false
	}
	switch a.Source {
	case SourceEnrichment:
		return a.Provider != ""
	case SourceFeed:
		return true
	default:
		return a.Provider == ""
	}
}

// The three ValidFor gates narrow Valid to one storage surface's own vocabulary, each
// matching the schema comment on the column it guards. Adding a ProvenanceSource means
// deciding which of them accept it.

// ValidForField reports whether a may be stored on a scalar field row (field_provenance,
// entity_curation): no artifact-only source, since nothing produces a scalar value from
// a cover.jpg, a feed image, or a composition.
func (a Attribution) ValidForField() bool {
	return a.Valid() && a.Source.ValidForField()
}

// ValidForArt reports whether a may be stored on an art attachment (art_map). Every
// source but organize can produce a picture; nothing organizes one. A generated cover
// needs no pairing rule of its own: Valid's default branch already requires an empty
// Provider, which is the right rule (no external service supplied it, same as user),
// and SourceURL stays unruled there for the same reason it is for user.
func (a Attribution) ValidForArt() bool {
	switch a.Source {
	case SourceTag, SourceSidecar, SourceUser, SourceEnrichment, SourceFeed, SourceGenerated:
		return a.Valid()
	default:
		return false
	}
}

// ValidForLyrics reports whether a may be stored on a lyrics row. A feed publishes
// covers and never words, nothing organizes lyrics, and nothing composes a set of
// words, so none of the three is accepted.
func (a Attribution) ValidForLyrics() bool {
	switch a.Source {
	case SourceTag, SourceSidecar, SourceUser, SourceEnrichment:
		return a.Valid()
	default:
		return false
	}
}

// LockChange is the lock instruction accompanying a curation write: leave the stored
// lock as it stands, set it, or clear it. A bool cannot express the first, so a caller
// that only meant to change a value had to state a lock intent it never formed, and a
// forced write silently rewrote a lock nobody had read.
type LockChange string

const (
	LockUnchanged LockChange = ""       // leave the stored lock alone
	LockOn        LockChange = "lock"   // lock the field
	LockOff       LockChange = "unlock" // clear the lock
)

// LockOf turns the two-state answer a caller already has into a LockChange, for the
// callers that genuinely decided; one that never formed the intent passes LockUnchanged.
func LockOf(lock bool) LockChange {
	if lock {
		return LockOn
	}
	return LockOff
}

// Valid reports whether c is a known lock instruction. The empty string is one.
func (c LockChange) Valid() bool {
	switch c {
	case LockUnchanged, LockOn, LockOff:
		return true
	default:
		return false
	}
}

// ParseLockChange parses a lock instruction at an input boundary (a CLI flag, a proxy
// frame), mirroring ParseArtRole. The empty string means LockUnchanged, so a caller
// that sends nothing leaves the lock alone.
func ParseLockChange(s string) (LockChange, bool) {
	c := LockChange(s)
	return c, c.Valid()
}

// MetadataFields enumerates the SCALAR, one-value item fields that can be set by the
// field-edit API (and, per kind, provenance-tracked). It is the whitelist behind
// scalar edits and SetFieldProvenance, and works like the query field whitelist: a
// field outside it is rejected rather than stored, so callers cannot create junk
// provenance rows. Which fields actually apply to a given item is kind-specific, and
// the editor enforces that. This is the union across track/book/episode. The
// structured artifacts (lyrics/chapters/art/acquisition) and credit roles are not here;
// they are only lockable (see IsCuratableField), not scalar-editable.
var MetadataFields = map[string]bool{
	// Shared and track fields.
	"title":         true,
	"artist":        true,
	"album_artist":  true,
	"album":         true,
	"composer":      true,
	"composer_sort": true,
	"genre":         true,
	"year":          true,
	"track_no":      true,
	"disc_no":       true,
	"bpm":           true,
	"comment":       true,
	"isrc":          true,
	"mbid":          true,
	"compilation":   true,
	// Audiobook fields.
	"author":      true,
	"author_sort": true,
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
// each has its own edit API (SetItemLyrics/SetItemChapters/SetItemArt/SetAcquisition),
// and the whole artifact is locked as a unit. They belong to IsCuratableField (the lock
// whitelist) but never to IsMetadataField (the scalar-edit gate), so a scalar
// `--set art=x` or a SetFieldProvenance("art", ...) call is rejected instead of writing
// a junk row.
//
// Acquisition is the one whose artifact is a whole row in another table rather than a
// value: the lock says the item's recorded origin is curated, so a scan re-deriving
// SOURCE_URL tags and an import event both leave it alone.
var lockOnlyFields = map[string]bool{
	"lyrics":      true,
	"chapters":    true,
	"art":         true,
	"acquisition": true,
}

// IsMetadataField reports whether field is a scalar, one-value curatable/editable
// metadata field. The structured artifacts (lyrics/chapters/art/acquisition) and
// namespaced credit keys are NOT scalar fields; use IsCuratableField for the lock
// whitelist.
func IsMetadataField(field string) bool { return MetadataFields[field] }

// IsCuratableField reports whether field may carry a provenance/lock row. It is the
// lock whitelist: the superset of IsMetadataField plus the structured lock-only
// artifacts (lyrics/chapters/art/acquisition), a namespaced credit key
// ("credit.<role>"), a namespaced auxiliary-art key ("art.<role>"), and a namespaced
// custom-tag key ("tag.<KEY>"). The scalar edit path stays on IsMetadataField so a
// credit key, an art lock, an acquisition row, or a custom tag cannot be set as if it
// were a scalar (those go through their own APIs).
func IsCuratableField(field string) bool {
	if MetadataFields[field] || lockOnlyFields[field] {
		return true
	}
	if role, ok := CutCreditPrefix(field); ok {
		return ContributorRole(role).Valid()
	}
	if role, ok := CutArtRolePrefix(field); ok {
		// No art.front: the front cover's lock is the plain "art" field.
		r := ArtRole(role)
		return r.Valid() && r != ArtRoleFront
	}
	if key, ok := CutTagPrefix(field); ok {
		// A custom-tag lock names a canonical, non-reserved key.
		canon, valid := CanonicalTagKey(key)
		return valid && canon == key && !IsReservedTagKey(key)
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

// CutArtRolePrefix returns the role portion of an "art.<role>" field and whether the
// prefix was present with a non-empty role, mirroring CutCreditPrefix. It is the one
// place the auxiliary-art lock namespace lives, shared by the model whitelist and the
// store's kind gate.
//
// There is no "art.front": the front cover's lock is the plain "art" field, which is
// also the whole-entity gate on enrichment's aux fills, so giving it a second spelling
// would hide that one row does both jobs. IsCuratableField refuses it for that reason.
func CutArtRolePrefix(field string) (string, bool) {
	const p = "art."
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

// ItemFieldEdit pairs one item with its own field-edit map, for the per-item-map
// batch edit (EditItemsFields): each item in the batch gets different values,
// where EditManyFields applies one map to every item.
type ItemFieldEdit struct {
	ItemPID PID
	Fields  map[string]string
}

// ItemCreditEdit is one entry of a batch credit edit: the item, the contributor role
// being replaced, and the names to credit in it. A batch identifies an entry by the
// (item, role) pair rather than the item alone, since one item legitimately takes an
// author entry and a narrator entry in the same batch.
type ItemCreditEdit struct {
	ItemPID PID
	Role    ContributorRole
	Names   []string
}

// CreditBatchResult reports a batch credit edit's outcome: the entries whose edit
// applied and, in skip-locked mode, the entries skipped because the credit role was
// locked. Both lists name the (item, role) pair, since an item appearing under two roles
// can appear twice in either and a pid alone would not say which slot moved. An applied
// entry carries the names actually stored (trimmed, resolvable, de-duplicated by
// artist), so a caller's tag write-back mirrors what the catalog holds; a skipped entry
// carries no names, since nothing was stored for it.
type CreditBatchResult struct {
	Edited  []ItemCreditEdit
	Skipped []ItemCreditEdit
}

// FieldProvenance is one provenance row: a field's source, lock state, the curated
// value when a user set one, the provider that supplied an enrichment value, and the
// URL it was fetched from where one exists. Only the overlaid artifact rows (art,
// lyrics) ever carry a URL; field_provenance has no column for one.
type FieldProvenance struct {
	ItemPID PID
	Field   string
	Attribution
	Locked    bool
	Value     string
	UpdatedAt int64 // unix nanoseconds
}
