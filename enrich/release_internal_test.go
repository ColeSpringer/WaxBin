package enrich

import (
	"reflect"
	"testing"

	"github.com/colespringer/waxbin/model"
)

const testRG = "11111111-2222-3333-4444-555555555555"

// rel builds a search document for the matcher: in the album's own release group and
// scoring high enough, so a test that wants a rejection has to say which rule rejects.
func rel(id, barcode string, catNos ...string) mbRelease {
	r := mbRelease{
		ID: id, Barcode: barcode, Score: 100,
		ReleaseGroup: &mbReleaseGroup{ID: testRG},
	}
	for _, c := range catNos {
		r.LabelInfo = append(r.LabelInfo, mbLabelInfo{CatalogNumber: c})
	}
	return r
}

func target(barcode, catNo string) model.EnrichTarget {
	return model.EnrichTarget{
		Type: model.EnrichAlbumType, ReleaseGroupMBID: testRG,
		Barcode: barcode, CatalogNumber: catNo,
	}
}

func TestMatchReleaseByBarcode(t *testing.T) {
	// 0075992739429 is a valid EAN-13; 075992739429 is the same barcode as a UPC-A.
	cases := []struct {
		name           string
		album, release string
	}{
		{"identical", "0075992739429", "0075992739429"},
		{"tagged with spaces", "0075 9927 39429", "0075992739429"},
		{"upc against a padded ean", "075992739429", "0075992739429"},
		{"ean against an unpadded upc", "0075992739429", "075992739429"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := matchRelease(target(tc.album, ""), []mbRelease{rel("rel-a", tc.release)}, nil)
			if got != "rel-a" || reason != "barcode" {
				t.Errorf("matchRelease = (%q, %q), want (rel-a, barcode)", got, reason)
			}
		})
	}

	// A barcode that fails normalization is not a search key at all, so nothing is
	// compared and nothing is written.
	if got, _ := matchRelease(target("12345", ""), []mbRelease{rel("rel-a", "12345")}, nil); got != "" {
		t.Errorf("malformed barcode matched %q, want no match", got)
	}
}

func TestMatchReleaseByCatalogNumber(t *testing.T) {
	hits := []mbRelease{rel("rel-a", "", "SHVL 804")}
	for _, tagged := range []string{"SHVL 804", "shvl-804", "shvl804"} {
		got, reason := matchRelease(target("", tagged), nil, hits)
		if got != "rel-a" || reason != "catalog number" {
			t.Errorf("catalog number %q = (%q, %q), want (rel-a, catalog number)", tagged, got, reason)
		}
	}

	// Each tier is judged only against its own search results: a release the barcode
	// query surfaced is not adopted on a catalog number that query never constrained.
	if got, _ := matchRelease(target("", "SHVL 804"), hits, nil); got != "" {
		t.Errorf("catalog number matched a barcode-query hit (%q), want no match", got)
	}
}

// TestMatchReleaseRefusesAmbiguity is the assertion that must never be relaxed. Every
// entity-MBID writer fills only when empty, so a best-of-a-bad-lot winner would be
// permanent and unappealable; a tie writes nothing instead.
func TestMatchReleaseRefusesAmbiguity(t *testing.T) {
	two := []mbRelease{rel("rel-a", "0075992739429"), rel("rel-b", "0075992739429")}
	if got, _ := matchRelease(target("0075992739429", ""), two, nil); got != "" {
		t.Errorf("two releases on one barcode matched %q, want no match", got)
	}
	twoCat := []mbRelease{rel("rel-a", "", "SHVL 804"), rel("rel-b", "", "SHVL-804")}
	if got, _ := matchRelease(target("", "SHVL 804"), nil, twoCat); got != "" {
		t.Errorf("two releases on one catalog number matched %q, want no match", got)
	}
	// The same release returned twice is still one release.
	dup := []mbRelease{rel("rel-a", "0075992739429"), rel("rel-a", "0075992739429")}
	if got, _ := matchRelease(target("0075992739429", ""), dup, nil); got != "rel-a" {
		t.Errorf("duplicate documents for one release = %q, want rel-a", got)
	}
}

// TestMatchReleaseIgnoresYearAndCounts pins that the deferred half was not smuggled
// in: a release matching on title, year, or track count but on no identifier is not a
// match, because those are the signals a group's reissue variants share.
func TestMatchReleaseIgnoresYearAndCounts(t *testing.T) {
	near := rel("rel-a", "0000000000000")
	near.Title = "Wish You Were Here"
	if got, _ := matchRelease(target("0075992739429", ""), []mbRelease{near}, nil); got != "" {
		t.Errorf("a non-identifier resemblance matched %q, want no match", got)
	}
	// With no local identifier at all there is nothing to compare, whatever came back.
	if got, _ := matchRelease(target("", ""), []mbRelease{rel("rel-a", "0075992739429")}, nil); got != "" {
		t.Errorf("an album with no identifier matched %q, want no match", got)
	}
}

// TestMatchReleaseRejectsAForeignReleaseGroup checks the belt-and-braces re-check: a
// hit whose release group is not the album's is discarded even though the identifier
// matched, and so is one carrying no release group to verify against.
func TestMatchReleaseRejectsAForeignReleaseGroup(t *testing.T) {
	foreign := rel("rel-a", "0075992739429")
	foreign.ReleaseGroup = &mbReleaseGroup{ID: "99999999-8888-7777-6666-555555555555"}
	if got, _ := matchRelease(target("0075992739429", ""), []mbRelease{foreign}, nil); got != "" {
		t.Errorf("a foreign release group matched %q, want no match", got)
	}

	unverifiable := rel("rel-a", "0075992739429")
	unverifiable.ReleaseGroup = nil
	if got, _ := matchRelease(target("0075992739429", ""), []mbRelease{unverifiable}, nil); got != "" {
		t.Errorf("an unverifiable document matched %q, want no match", got)
	}

	weak := rel("rel-a", "0075992739429")
	weak.Score = minMatchScore - 1
	if got, _ := matchRelease(target("0075992739429", ""), []mbRelease{weak}, nil); got != "" {
		t.Errorf("a below-threshold hit matched %q, want no match", got)
	}
}

// TestBarcodeSpellingsAsksForBothStoredForms pins that the query reaches every
// spelling the comparison accepts. A UPC-A and its zero-padded EAN-13 are one
// barcode, and MusicBrainz holds whichever was entered, so asking for only one form
// would leave matchRelease recognizing hits it never retrieved.
func TestBarcodeSpellingsAsksForBothStoredForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"075992739429", []string{"075992739429", "0075992739429"}},  // UPC-A
		{"0075992739429", []string{"0075992739429", "075992739429"}}, // the same, as EAN-13
		{"5099902154251", []string{"5099902154251"}},                 // a real EAN-13 has no UPC form
		{"96385074", []string{"96385074"}},                           // EAN-8
		{"nonsense", nil},
	} {
		got := barcodeSpellings(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("barcodeSpellings(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// Every spelling asked for must fold to the value the matcher compares on.
	want := normBarcode("075992739429")
	for _, s := range barcodeSpellings("075992739429") {
		if normBarcode(s) != want {
			t.Errorf("spelling %q folds to %q, want %q", s, normBarcode(s), want)
		}
	}
}
