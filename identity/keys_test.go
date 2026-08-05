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

func TestSplitPerformerCredit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "Jay-Z", []string{"Jay-Z"}},
		{"feat with a period", "Jay-Z feat. Alicia Keys", []string{"Jay-Z", "Alicia Keys"}},
		{"feat without one", "Jay-Z feat Alicia Keys", []string{"Jay-Z", "Alicia Keys"}},
		{"ft", "Jay-Z ft. Alicia Keys", []string{"Jay-Z", "Alicia Keys"}},
		{"featuring", "Jay-Z Featuring Alicia Keys", []string{"Jay-Z", "Alicia Keys"}},
		{"vs", "Run-D.M.C. vs. Jason Nevins", []string{"Run-D.M.C.", "Jason Nevins"}},
		{"semicolon", "A; B", []string{"A", "B"}},
		{"spaced slash", "A / B", []string{"A", "B"}},
		{"markers compose", "A; B feat. C", []string{"A", "B", "C"}},
		{"dedup keeps the first casing", "Jay-Z feat. JAY-Z", []string{"Jay-Z"}},
		{"blank", "   ", nil},

		// Band names are full of the separators SplitCredits uses for book authors, so
		// none of them splits here. A wrong split creates artist rows nothing removes;
		// a missed one leaves a coarse credit the user can fix with `credit`.
		{"slash inside a name", "AC/DC", []string{"AC/DC"}},
		{"ampersand band", "Hall & Oates", []string{"Hall & Oates"}},
		{"ampersand band 2", "Simon & Garfunkel", []string{"Simon & Garfunkel"}},
		{"ampersand band 3", "Sly & the Family Stone", []string{"Sly & the Family Stone"}},
		{"comma and ampersand", "Earth, Wind & Fire", []string{"Earth, Wind & Fire"}},
		// The cost of the rule above: a real joint credit joined by "&" stays whole.
		{"joint credit joined by an ampersand", "Jay-Z & Alicia Keys", []string{"Jay-Z & Alicia Keys"}},

		{"comma alone", "Crosby, Stills, Nash", []string{"Crosby, Stills, Nash"}},
		{"with", "Tom Petty with the Heartbreakers", []string{"Tom Petty with the Heartbreakers"}},
		{"bare x", "Skrillex x Diplo", []string{"Skrillex x Diplo"}},
		{"marker inside a word", "Ftera Vsevolod", []string{"Ftera Vsevolod"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SplitPerformerCredit(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("SplitPerformerCredit(%q) = %v, want %v", c.in, got, c.want)
			}
		})
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

func TestBookKey(t *testing.T) {
	// ASIN wins over everything; ISBN next; then author+title+edition.
	if got := BookKey("B00X", "978-0-13-468599-1", "Tolkien", "The Hobbit", "Unabridged"); got != "asin:b00x" {
		t.Errorf("ASIN key = %q, want asin:b00x", got)
	}
	if got := BookKey("", "978-0-13-468599-1", "Tolkien", "The Hobbit", ""); got != "isbn:9780134685991" {
		t.Errorf("ISBN key = %q, want isbn:9780134685991 (separators stripped)", got)
	}
	// Edition distinguishes an abridged release from the unabridged one.
	un := BookKey("", "", "Tolkien", "The Hobbit", "Unabridged")
	ab := BookKey("", "", "Tolkien", "The Hobbit", "Abridged")
	if un == ab {
		t.Errorf("abridged and unabridged keyed the same: %q", un)
	}
	// Same author+title with no edition is stable across calls.
	if BookKey("", "", "Tolkien", "The Hobbit", "") != BookKey("", "", "tolkien", "the  hobbit", "") {
		t.Error("author/title key is not normalization-stable")
	}
	// No id and no title: ungrouped.
	if got := BookKey("", "", "Tolkien", "", ""); got != "" {
		t.Errorf("titleless book key = %q, want empty", got)
	}
}

func TestSeriesKey(t *testing.T) {
	if got := SeriesKey("mb-1", "Anything"); got != "mbid:mb-1" {
		t.Errorf("series MBID key = %q, want mbid:mb-1", got)
	}
	if SeriesKey("", "The Stormlight Archive") != SeriesKey("", "the stormlight archive") {
		t.Error("series name key is not normalization-stable")
	}
	if got := SeriesKey("", ""); got != "" {
		t.Errorf("empty series key = %q, want empty", got)
	}
}
