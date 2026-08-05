package enrich

import (
	"strings"

	"github.com/colespringer/waxbin/model"
)

// This file holds the album release matcher: given an album's own identifiers and
// the releases MusicBrainz returned for its release group, decide which release the
// local album is, or decide nothing.
//
// There is no score, no threshold, and no tie-break anywhere in it, and that is the
// design rather than an omission. Every writer of an entity MBID fills only when
// empty, so a best-of-a-bad-lot winner would be permanent and unappealable, and a
// float invites a later maintainer to lower the bar when coverage disappoints. What
// decides a match is a uniqueness gate, the shape pickEssenceMatch already uses
// ("only when that closest item stands alone").
//
// Track and disc counts stay out: they are the only signals an untagged library has,
// and they are also the signals a release group's reissue variants share, so the
// population where counts are the only evidence is the one where they do not
// discriminate.

// gtinWidth is the GTIN-14 width both sides of a barcode comparison are padded to.
// model.NormalizeBarcode accepts 8, 12, and 13 digits and returns them unchanged, so
// a CD tagged with a 12-digit UPC and a MusicBrainz release storing the same number
// as a zero-padded 13-digit EAN are two spellings of one barcode. The target
// population is CD rips, so that is the common case rather than an edge.
const gtinWidth = 14

// normBarcode returns a barcode's GTIN-14 form, or "" when it is absent or fails
// normalization (length and check digit). Normalizing both sides matters because a
// scan stores identifiers verbatim, so album.barcode can carry the spaces it was
// tagged with.
func normBarcode(s string) string {
	v, ok := model.NormalizeBarcode(strings.TrimSpace(s))
	if !ok || v == "" {
		return ""
	}
	return strings.Repeat("0", gtinWidth-len(v)) + v
}

// foldCatNo folds a label catalog number for comparison: uppercased with every
// non-alphanumeric dropped, so "SHVL 804" and "shvl-804" are one number.
//
// It widens the COMPARISON, not the search. The catalog-number query goes out as the
// tagged string, unlike a barcode's two spellings, so a release stored under different
// punctuation is not retrieved in the first place. What this buys is the uniqueness
// gate: among the releases that did come back, two spellings of one number count as
// one release rather than as a tie that writes nothing.
func foldCatNo(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(s) {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// barcodeSpellings returns the forms of a barcode to search for, or nothing when it
// is absent or malformed. MusicBrainz stores it as entered, and a UPC-A is routinely
// held as the zero-padded EAN-13 and vice versa, so both are asked for. Not the
// GTIN-14 normBarcode compares on: no release carries a shipping-container code, so
// querying it would only recognize hits it never retrieved.
func barcodeSpellings(barcode string) []string {
	v, ok := model.NormalizeBarcode(strings.TrimSpace(barcode))
	if !ok || v == "" {
		return nil
	}
	switch {
	case len(v) == 12:
		return []string{v, "0" + v}
	case len(v) == 13 && strings.HasPrefix(v, "0"):
		return []string{v, v[1:]}
	}
	return []string{v}
}

// matchRelease picks the release that t names, or returns ("", "") to write nothing.
// byBarcode and byCatNo are what the two identifier searches returned; each tier is
// judged only against its own results, so a release surfaced by a barcode query can
// never be adopted on a catalog number that query never constrained.
//
// A release is accepted when the album's barcode equals its barcode and exactly one
// returned release satisfies that, or, failing that, when the album's catalog number
// equals one of its label-info catalog numbers and exactly one returned release
// satisfies that. A tie at either tier writes nothing.
//
// The identifier is re-checked against the returned documents rather than the search
// hit count being taken as the answer. The query already constrains by identifier, so
// this is redundant on a well-behaved server, and that is the point: it is the same
// belt-and-braces searchReleaseGroup applies when it re-checks the title and artist
// it just searched on. Uniqueness is then a property of the compared set rather than
// of the search engine's ranking.
func matchRelease(t model.EnrichTarget, byBarcode, byCatNo []mbRelease) (mbid, reason string) {
	if want := normBarcode(t.Barcode); want != "" {
		if id, ok := soleRelease(t.ReleaseGroupMBID, byBarcode, func(r *mbRelease) bool {
			return normBarcode(r.Barcode) == want
		}); ok {
			return id, "barcode"
		}
	}
	if want := foldCatNo(t.CatalogNumber); want != "" {
		if id, ok := soleRelease(t.ReleaseGroupMBID, byCatNo, func(r *mbRelease) bool {
			for _, li := range r.LabelInfo {
				if foldCatNo(li.CatalogNumber) == want {
					return true
				}
			}
			return false
		}); ok {
			return id, "catalog number"
		}
	}
	return "", ""
}

// soleRelease returns the one release satisfying pred, and false when none or more
// than one distinct release does. A release must also clear minMatchScore and belong
// to rgMBID; a document carrying no release group cannot be verified and so is
// discarded rather than trusted, which fails closed on a write that would be
// permanent.
func soleRelease(rgMBID string, releases []mbRelease, pred func(*mbRelease) bool) (string, bool) {
	var found string
	for i := range releases {
		r := &releases[i]
		if r.ID == "" || r.Score < minMatchScore {
			continue
		}
		if r.ReleaseGroup == nil || !strings.EqualFold(r.ReleaseGroup.ID, rgMBID) {
			continue
		}
		if !pred(r) {
			continue
		}
		if found != "" && !strings.EqualFold(found, r.ID) {
			return "", false
		}
		found = r.ID
	}
	return found, found != ""
}
