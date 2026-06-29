package identity

import (
	"reflect"
	"testing"
)

func TestMatchKey(t *testing.T) {
	cases := map[string]string{
		"Hip-Hop":        "hip hop",
		"hip hop":        "hip hop",
		"  R&B  ":        "r b",
		"AC/DC":          "ac dc",
		"The Beatles":    "the beatles", // article-stripping is a sort-key concern, not match
		"Drum & Bass":    "drum bass",
		"":               "",
		"!!!":            "",
		"Multiple   Spc": "multiple spc",
		// Diacritics are stripped to match the FTS tokenizer (remove_diacritics).
		"Sigur Rós":    "sigur ros",
		"Café del Mar": "cafe del mar",
		"Naïve-Remix":  "naive remix", // accent stripped, punctuation folds to space
		// Non-ASCII punctuation/separators fold to spaces while CJK letters stay:
		// fullwidth/CJK punctuation (（ ） ，) and an ideographic space.
		"東京（Live）": "東京 live",
		"夏，秋":      "夏 秋",
		"日本語":      "日本語",
	}
	for in, want := range cases {
		if got := MatchKey(in); got != want {
			t.Errorf("MatchKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchKeyFoldsToSameKey(t *testing.T) {
	// Different display casings/punctuation must collapse to one dedup key.
	for _, v := range []string{"Hip-Hop", "Hip Hop", "hip  hop", "HIP/HOP"} {
		if got := MatchKey(v); got != "hip hop" {
			t.Errorf("MatchKey(%q) = %q, want hip hop", v, got)
		}
	}
}

func TestMatchKeyFoldsDiacritics(t *testing.T) {
	// Accented and unaccented spellings of one name must resolve to one key, both
	// for precomposed and combining-mark forms, so they don't fragment into two
	// entities while the FTS (remove_diacritics) indexes them as one.
	precomposed := "Beyonc\u00e9" // é as a single rune
	combining := "Beyonce\u0301"  // e + combining acute accent
	for _, v := range []string{precomposed, combining, "BEYONCE", "beyonce"} {
		if got := MatchKey(v); got != "beyonce" {
			t.Errorf("MatchKey(%q) = %q, want beyonce", v, got)
		}
	}
}

func TestSplitGenres(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Rock", []string{"Rock"}},
		{"Rock; Pop / Indie", []string{"Rock", "Pop", "Indie"}},
		{"Rock;Rock; rock", []string{"Rock"}}, // dedup by match key, keep first display
		{"  ", nil},
		{"Hip-Hop\\Rap", []string{"Hip-Hop", "Rap"}},
	}
	for _, c := range cases {
		got := SplitGenres(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitGenres(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestArtistKeyMBIDFirst(t *testing.T) {
	withMBID := ArtistKey("ABC-123", "Some Name")
	if withMBID != "mbid:abc-123" {
		t.Errorf("ArtistKey with mbid = %q", withMBID)
	}
	if k := ArtistKey("", "The Beatles"); k != "name:the beatles" {
		t.Errorf("ArtistKey without mbid = %q", k)
	}
}

func TestReleaseGroupKey(t *testing.T) {
	amk := MatchKey("Radiohead")
	a := ReleaseGroupKey("", amk, "OK Computer")
	b := ReleaseGroupKey("", amk, "ok computer") // same RG by normalization
	if a != b {
		t.Errorf("release-group key not stable across casing: %q vs %q", a, b)
	}
	if ReleaseGroupKey("", amk, "") != "" {
		t.Error("a titleless release group should not be keyed (non-album single)")
	}
	if got := ReleaseGroupKey("mb-rg-1", amk, "OK Computer"); got != "mbid:mb-rg-1" {
		t.Errorf("mbid release-group key = %q", got)
	}
}

func TestAlbumKeyDisambiguatesByFolder(t *testing.T) {
	rg := ReleaseGroupKey("", MatchKey("Artist"), "Greatest Hits")
	a := AlbumKey("", rg, 1999, 0, "/music/Artist/GH-1999")
	b := AlbumKey("", rg, 1999, 0, "/music/Artist/GH-remaster")
	if a == b {
		t.Error("same-titled editions in different folders should get distinct album keys")
	}
	if AlbumKey("", "", 1999, 0, "/x") != "" {
		t.Error("an album with no release-group key should not be keyed")
	}
}
