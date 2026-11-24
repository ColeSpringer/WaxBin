package read

import (
	"testing"

	"github.com/colespringer/waxbin/model"
)

func TestGroupByValidAcceptsTagDimensions(t *testing.T) {
	// The fixed dimensions stay valid.
	for _, g := range GroupBys() {
		if !g.Valid() {
			t.Errorf("fixed dimension %q should be valid", g)
		}
	}
	// A well-formed custom-tag dimension is valid (canonical, non-reserved key).
	for _, g := range []GroupBy{"tag.MOOD", "tag.mood", "tag.MY_KEY"} {
		if !g.Valid() {
			t.Errorf("%q should be a valid tag dimension", g)
		}
	}
	// Reserved or malformed tag dimensions are rejected, mirroring the query barrier.
	for _, g := range []GroupBy{"tag.TITLE", "tag.", "tag.A=B", "tag", "bogus"} {
		if g.Valid() {
			t.Errorf("%q should be rejected", g)
		}
	}
}

// TestGroupByUserScopedIsPlaylistOnly is the consumer-facing half of the caching
// contract: every other dimension may be keyed by (query, dimension) alone. The
// store-side half, that the flag matches the specs, is
// TestFacetSpecUserScopedMatchesFlag.
func TestGroupByUserScopedIsPlaylistOnly(t *testing.T) {
	for _, g := range GroupBys() {
		if want := g == GroupPlaylist; g.UserScoped() != want {
			t.Errorf("%q.UserScoped() = %v, want %v", g, g.UserScoped(), want)
		}
	}
	if GroupBy("tag.MOOD").UserScoped() {
		t.Error("a custom-tag dimension should not be user-scoped")
	}
}

func TestTagGroupKeyCanonicalizes(t *testing.T) {
	if k, ok := TagGroupKey("tag.mood"); !ok || k != "MOOD" {
		t.Errorf("TagGroupKey(tag.mood) = (%q,%v), want (MOOD,true)", k, ok)
	}
	if _, ok := TagGroupKey("tag.TITLE"); ok {
		t.Error("TagGroupKey(tag.TITLE) should reject a reserved key")
	}
	if _, ok := TagGroupKey("genre"); ok {
		t.Error("TagGroupKey(genre) should reject a non-tag dimension")
	}
}

// TestArtEntityClassifiesEveryDimensionAndKind lists what each fixed dimension's bucket
// pids and each entity kind are asked for art under. A dimension or kind added without
// a line here fails, so its buckets cannot go without covers by oversight.
func TestArtEntityClassifiesEveryDimensionAndKind(t *testing.T) {
	dims := map[GroupBy]model.ArtEntity{
		GroupGenre: model.ArtGenre, GroupArtist: model.ArtArtist, GroupCreditArtist: model.ArtArtist,
		GroupAlbumArtist: model.ArtArtist, GroupAlbum: model.ArtAlbum, GroupReleaseGroup: model.ArtReleaseGroup,
		GroupYear: "", GroupKind: "", GroupLibrary: "", GroupPrimaryLibrary: "",
		GroupPodcast: model.ArtPodcast, GroupPlaylist: model.ArtPlaylist, GroupSeries: "",
		GroupAuthor: model.ArtArtist, GroupNarrator: model.ArtArtist,
	}
	for _, g := range append(GroupBys(), "tag.MOOD") {
		want, listed := dims[g]
		if !listed && g != "tag.MOOD" {
			t.Errorf("dimension %q is not classified", g)
		}
		if got, ok := g.ArtEntity(); got != want || ok != (want != "") || ok && !got.Valid() {
			t.Errorf("%q.ArtEntity() = %q, %v, want %q", g, got, ok, want)
		}
	}
	kinds := map[EntityKind]model.ArtEntity{
		EntityArtist: model.ArtArtist, EntityReleaseGroup: model.ArtReleaseGroup,
		EntityAlbum: model.ArtAlbum, EntityGenre: model.ArtGenre, EntitySeries: "",
	}
	for _, k := range EntityKinds() {
		want, listed := kinds[k]
		if !listed {
			t.Errorf("kind %q is not classified", k)
		}
		if got, ok := k.ArtEntity(); got != want || ok != (want != "") || ok && !got.Valid() {
			t.Errorf("%q.ArtEntity() = %q, %v, want %q", k, got, ok, want)
		}
	}
}

// TestSearchHitArtRefFollowsItsKind: a hit is asked for art as the kind it is, a book
// sharing the track slot and an author or a narrator the artist's, and a series for none.
func TestSearchHitArtRefFollowsItsKind(t *testing.T) {
	for kind, want := range map[string]model.ArtEntity{
		"artist": model.ArtArtist, "album": model.ArtAlbum, "track": model.ArtTrack,
		"author": model.ArtArtist, "narrator": model.ArtArtist,
		"book": model.ArtTrack, "podcast": model.ArtPodcast, "episode": model.ArtEpisode,
	} {
		if got, ok := (SearchHit{PID: "p", Kind: kind}).ArtRef(); !ok || got != (model.EntityRef{Type: want, PID: "p"}) {
			t.Errorf("a %s hit is asked for under %+v, want %s", kind, got, want)
		}
	}
	if got, ok := (SearchHit{PID: "p", Kind: "series"}).ArtRef(); ok || got != (model.EntityRef{}) {
		t.Errorf("a series hit is asked for art under %+v, want no reference", got)
	}
}
