package model

import (
	"maps"
	"slices"
)

// queryFieldAliases maps an alternate query field spelling to the canonical one. The
// query engine accepts both members of each pair as the same column, so which one a
// stored rule happens to hold is arbitrary as far as evaluation goes. Any surface that
// reads a field name back out (the .nsp exporter is the first) has to resolve through
// here, or it will refuse a rule it can express perfectly well. Canonical is the
// spelling the rest of WaxBin teaches (see MetadataFields).
//
// It is unexported because two of its three readers snapshot it at init (the engine's
// field map, the exporter's reverse maps) while CanonicalQueryField reads it live, so a
// write would both diverge them and race a long-lived serve.
//
// The organize path templates are a different namespace: {albumartist} in
// organize/profile.go names a filename token, not a query field, and every stored
// profile depends on that spelling. Do not unify the two.
var queryFieldAliases = map[string]string{
	"albumartist": "album_artist",
	"track":       "track_no",
	"disc":        "disc_no",
	"created_at":  "added",
}

// QueryFieldAliases returns the alias-to-canonical table as a fresh map, for a caller
// building its own index over it.
func QueryFieldAliases() map[string]string {
	return maps.Clone(queryFieldAliases)
}

// CanonicalQueryField resolves a query field spelling to its canonical one, leaving
// anything that is not an alias (including an unknown field) exactly as it arrived.
func CanonicalQueryField(field string) string {
	if canon, ok := queryFieldAliases[field]; ok {
		return canon
	}
	return field
}

// QueryFieldSpellings returns every spelling the engine accepts for one field: the
// canonical one first, then its aliases in sorted order. It takes either spelling, so a
// caller holding whichever one a stored rule carries gets the whole set.
func QueryFieldSpellings(field string) []string {
	canon := CanonicalQueryField(field)
	var aliases []string
	for alias, c := range queryFieldAliases {
		if c == canon {
			aliases = append(aliases, alias)
		}
	}
	slices.Sort(aliases)
	return append([]string{canon}, aliases...)
}
