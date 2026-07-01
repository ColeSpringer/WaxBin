package enrich

import (
	"sort"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// artistAliasNames collects an artist's alternate names for storage, dropping
// empties and the canonical display name (which is already the artist row's name).
// Duplicates are removed by normalized match key.
func artistAliasNames(a *mbArtist) []string {
	seen := map[string]bool{identity.MatchKey(a.Name): true}
	var out []string
	for _, al := range a.Aliases {
		name := strings.TrimSpace(al.Name)
		mk := identity.MatchKey(name)
		if name == "" || mk == "" || seen[mk] {
			continue
		}
		seen[mk] = true
		out = append(out, name)
	}
	return out
}

// artistRelations maps MusicBrainz relations to the directed relations WaxBin
// stores, keeping only the kinds it models and that target another artist. The
// store links each target by its MBID and skips targets not in the catalog.
func artistRelations(a *mbArtist) []model.ArtistRelationInput {
	var out []model.ArtistRelationInput
	seen := map[string]bool{}
	for _, r := range a.Relations {
		if r.Artist == nil || r.Artist.ID == "" {
			continue
		}
		kind := mapRelationKind(r.Type)
		if kind == "" {
			continue
		}
		key := kind + "\x1f" + r.Artist.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		// MusicBrainz reports a directed relation (like "member of band") from both
		// ends: on the person it points forward to the band, on the band it points
		// backward to the person. Reverse the edge on the backward direction so the
		// stored relation is always oriented the same way (member -> band) no matter
		// which artist was enriched first.
		inbound := strings.EqualFold(strings.TrimSpace(r.Direction), "backward")
		out = append(out, model.ArtistRelationInput{TargetMBID: r.Artist.ID, Kind: kind, Inbound: inbound})
	}
	return out
}

// mapRelationKind folds a MusicBrainz relation type into one of WaxBin's relation
// kinds, or "" to drop it. Only the relations WaxBin models are kept.
func mapRelationKind(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "member of band":
		return model.RelationMemberOf
	case "collaboration", "supporting musician", "founder":
		return model.RelationSimilar
	default:
		return ""
	}
}

// mapReleaseGroupType folds a MusicBrainz primary/secondary type into WaxBin's
// release_group.type vocabulary (album|ep|single|compilation). Compilation and
// audiobook secondary types take precedence over the primary type.
func mapReleaseGroupType(primary string, secondary []string) string {
	for _, s := range secondary {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "compilation":
			return "compilation"
		case "audiobook", "audio drama", "spokenword":
			return "audiobook"
		}
	}
	switch strings.ToLower(strings.TrimSpace(primary)) {
	case "album":
		return "album"
	case "ep":
		return "ep"
	case "single":
		return "single"
	case "":
		return ""
	default:
		// Broadcast, Other, etc. group under album for browse.
		return "album"
	}
}

// genreNames extracts distinct, non-empty genre display names from MusicBrainz
// genres, ordered by descending vote count so the most-agreed genres come first
// (the denormalized track.genre stores the first, so the ordering is user-visible).
func genreNames(gs []mbGenre) []string {
	// Sort a copy by descending count (stable, so equal counts keep MB's order),
	// then take distinct non-empty names.
	sorted := append([]mbGenre(nil), gs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Count > sorted[j].Count })
	seen := map[string]bool{}
	var out []string
	for _, g := range sorted {
		name := strings.TrimSpace(g.Name)
		mk := identity.MatchKey(name)
		if name == "" || mk == "" || seen[mk] {
			continue
		}
		seen[mk] = true
		out = append(out, name)
	}
	return out
}
