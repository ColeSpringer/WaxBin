package model

import (
	"encoding/json"
	"testing"
)

// TestAttributionPairing pins the rule a bare ProvenanceSource cannot express: an
// enrichment value names the provider that supplied it, the sources with no external
// provider carry none, and a feed is exempt because a feed is its own provider.
func TestAttributionPairing(t *testing.T) {
	ok := []Attribution{
		{Source: SourceTag},
		{Source: SourceUser},
		{Source: SourceOrganize},
		{Source: SourceSidecar},
		{Source: SourceFeed},
		{Source: SourceFeed, Provider: "some-feed"},
		{Source: SourceEnrichment, Provider: "musicbrainz"},
		{Source: SourceTag, SourceURL: "https://example/cover.png"},
		{Source: SourceGenerated},
		{Source: SourceGenerated, SourceURL: "https://example/mosaic.png"},
	}
	for _, a := range ok {
		if !a.Valid() {
			t.Errorf("Valid rejected %+v", a)
		}
	}
	bad := []Attribution{
		{},
		{Source: "made-up"},
		{Source: SourceEnrichment},
		{Source: SourceTag, Provider: "musicbrainz"},
		{Source: SourceSidecar, Provider: "musicbrainz"},
		{Source: SourceUser, Provider: "musicbrainz"},
		{Source: SourceOrganize, Provider: "musicbrainz"},
		{Source: SourceGenerated, Provider: "musicbrainz"},
	}
	for _, a := range bad {
		if a.Valid() {
			t.Errorf("Valid accepted %+v", a)
		}
	}
}

// TestAttributionValidForField keeps the scalar gate a strict narrowing of Valid: the
// artifact-only sources are refused there, and the pairing still applies.
func TestAttributionValidForField(t *testing.T) {
	for _, a := range []Attribution{
		{Source: SourceTag},
		{Source: SourceUser},
		{Source: SourceOrganize},
		{Source: SourceEnrichment, Provider: "musicbrainz"},
	} {
		if !a.ValidForField() {
			t.Errorf("ValidForField rejected %+v", a)
		}
	}
	for _, a := range []Attribution{
		{Source: SourceSidecar},
		{Source: SourceFeed},
		{Source: SourceGenerated},
		{Source: SourceEnrichment},
		{},
	} {
		if a.ValidForField() {
			t.Errorf("ValidForField accepted %+v", a)
		}
	}
}

// TestAttributionOrUser pins the one rule governing an unstated source: a curation
// write that names no origin is a user edit, and one that names an origin keeps it.
func TestAttributionOrUser(t *testing.T) {
	if got := (Attribution{}).OrUser(); got.Source != SourceUser {
		t.Errorf("empty source defaulted to %q, want user", got.Source)
	}
	in := Attribution{Source: SourceEnrichment, Provider: "itunes", SourceURL: "https://example/c.png"}
	if got := in.OrUser(); got != in {
		t.Errorf("OrUser rewrote a stated attribution: %+v", got)
	}
}

func TestLockChange(t *testing.T) {
	if LockOf(true) != LockOn || LockOf(false) != LockOff {
		t.Error("LockOf does not map the two-state answer onto the two explicit instructions")
	}
	for _, c := range []LockChange{LockUnchanged, LockOn, LockOff} {
		if !c.Valid() {
			t.Errorf("Valid rejected %q", c)
		}
	}
	for _, s := range []string{"true", "on", "Lock", "locked"} {
		if _, ok := ParseLockChange(s); ok {
			t.Errorf("ParseLockChange accepted %q", s)
		}
	}
	if c, ok := ParseLockChange(""); !ok || c != LockUnchanged {
		t.Errorf("ParseLockChange(\"\") = %q, %v; want the unchanged instruction", c, ok)
	}
	if c, ok := ParseLockChange("unlock"); !ok || c != LockOff {
		t.Errorf("ParseLockChange(\"unlock\") = %q, %v", c, ok)
	}
}

// TestFieldProvenanceJSONStaysFlat is the load-bearing property of embedding
// Attribution rather than adding a sixth copy of the three fields: FieldProvenance
// crosses the proxy wire, and a promoted embedded field marshals at the top level, so
// the shape a client decodes is the one it always decoded.
func TestFieldProvenanceJSONStaysFlat(t *testing.T) {
	b, err := json.Marshal(FieldProvenance{
		ItemPID: "i1", Field: "art", Locked: true, Value: "v", UpdatedAt: 42,
		Attribution: Attribution{Source: SourceEnrichment, Provider: "caa", SourceURL: "https://x/y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var flat map[string]any
	if err := json.Unmarshal(b, &flat); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ItemPID", "Field", "Source", "Provider", "SourceURL", "Locked", "Value", "UpdatedAt"} {
		if _, ok := flat[k]; !ok {
			t.Errorf("key %q is missing from %s", k, b)
		}
	}
	if len(flat) != 8 {
		t.Errorf("json has %d keys, want the 8 flat ones: %s", len(flat), b)
	}
}

// TestGeneratedAcrossTheGates pins which of the four gates the generated source passes.
// It is an artifact value: an art_map row may carry it, and the two scalar-field gates
// and the lyrics gate must not, since nothing composes a scalar value or a set of words
// and accepting one there would only mint the junk row IsMetadataField exists to
// prevent.
func TestGeneratedAcrossTheGates(t *testing.T) {
	if !SourceGenerated.Valid() {
		t.Error("SourceGenerated is not in the vocabulary")
	}
	if SourceGenerated.ValidForField() {
		t.Error("SourceGenerated passed the scalar-field vocabulary gate")
	}
	a := Attribution{Source: SourceGenerated}
	if !a.ValidForArt() {
		t.Error("a generated cover was refused by ValidForArt")
	}
	if a.ValidForField() {
		t.Error("a generated value passed ValidForField")
	}
	if a.ValidForLyrics() {
		t.Error("generated lyrics passed ValidForLyrics")
	}
	// No provider pairing of its own: Valid's default branch already says nobody
	// supplied it, the same rule user carries.
	if (Attribution{Source: SourceGenerated, Provider: "x"}).ValidForArt() {
		t.Error("a generated cover with a provider was accepted")
	}
}

// TestAcquisitionIsLockOnly pins acquisition's place in the two whitelists: it may
// carry a lock row, and it is never scalar-editable, so `edit --set acquisition=x` and
// SetFieldProvenance are refused rather than writing a junk row beside a table that
// holds the real value.
func TestAcquisitionIsLockOnly(t *testing.T) {
	if !IsCuratableField("acquisition") {
		t.Error("acquisition is not curatable; the lock has nowhere to live")
	}
	if IsMetadataField("acquisition") {
		t.Error("acquisition is scalar-editable; it has its own edit API")
	}
}
