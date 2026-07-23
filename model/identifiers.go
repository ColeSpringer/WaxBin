package model

import "strings"

// This file holds the external-identifier normalizers (ISRC, ISBN, ASIN,
// barcode) behind the edit surfaces. Validation is an edit-surface contract:
// the normalizers run where a user or a provider hands WaxBin an identifier
// (item field edits, entity edits, enrichment apply), never at scan time. A
// scan stays faithful to what a file's tags say, malformed or not, because
// rewriting scanned values would put the catalog out of step with the file it
// claims to mirror.
//
// Each normalizer follows the ParseBoolValue shape: (normalized, ok). An empty
// input is a clear and always normalizes to ("", true). A non-empty input
// either normalizes to the canonical stored form or is rejected; nothing
// malformed is ever stored as a "best effort".
//
// One deliberate non-feature: NormalizeISBN performs no ISBN-10 to ISBN-13
// conversion. The edit surface stays faithful to the form the user supplied,
// because the stored value is also what write-back puts in the file's tags.
// The identity key layer has its own ISBN folding for matching.

// NormalizeISRC normalizes an International Standard Recording Code: separators
// (hyphens, spaces) are stripped and the rest uppercased, then validated as the
// 12-character CC-XXX-YY-NNNNN shape (country letters, alphanumeric registrant,
// seven digits for year and designation).
func NormalizeISRC(value string) (string, bool) {
	s, ok := stripIdentifier(value)
	if !ok || s == "" {
		return s, ok
	}
	s = strings.ToUpper(s)
	if len(s) != 12 {
		return "", false
	}
	for i, r := range s {
		switch {
		case i < 2:
			if r < 'A' || r > 'Z' {
				return "", false
			}
		case i < 5:
			if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return "", false
			}
		default:
			if r < '0' || r > '9' {
				return "", false
			}
		}
	}
	return s, true
}

// NormalizeISBN normalizes an ISBN: separators are stripped, a check character
// x is uppercased, and the result is validated as either an ISBN-10 (mod-11
// checksum, X allowed only as the check digit) or an ISBN-13 (a 978/979
// Bookland EAN with a valid mod-10 checksum). The prefix requirement is what
// keeps a plain product barcode out of an isbn field: every EAN-13 on a CD
// case is checksum-valid, and only the Bookland prefixes mark the number as an
// ISBN at all. The 10 and 13 digit forms are both kept as given; see the file
// header for why there is no 10-to-13 conversion.
func NormalizeISBN(value string) (string, bool) {
	s, ok := stripIdentifier(value)
	if !ok || s == "" {
		return s, ok
	}
	s = strings.ToUpper(s)
	switch len(s) {
	case 10:
		sum := 0
		for i, r := range s {
			var d int
			switch {
			case r >= '0' && r <= '9':
				d = int(r - '0')
			case r == 'X' && i == 9:
				d = 10
			default:
				return "", false
			}
			sum += d * (10 - i)
		}
		if sum%11 != 0 {
			return "", false
		}
	case 13:
		if !strings.HasPrefix(s, "978") && !strings.HasPrefix(s, "979") {
			return "", false
		}
		if !gtinChecksumOK(s) {
			return "", false
		}
	default:
		return "", false
	}
	return s, true
}

// NormalizeASIN normalizes an Amazon Standard Identification Number: uppercased
// and validated as exactly ten alphanumeric characters. ASINs carry no
// separators, so none are stripped; a hyphenated value is malformed.
func NormalizeASIN(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	s := strings.ToUpper(value)
	if len(s) != 10 {
		return "", false
	}
	for _, r := range s {
		if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return "", false
		}
	}
	return s, true
}

// NormalizeBarcode normalizes a release barcode: separators are stripped and
// the digits validated as an EAN-8, UPC-A (12), or EAN-13 code with a correct
// GTIN mod-10 check digit.
func NormalizeBarcode(value string) (string, bool) {
	s, ok := stripIdentifier(value)
	if !ok || s == "" {
		return s, ok
	}
	switch len(s) {
	case 8, 12, 13:
	default:
		return "", false
	}
	if !gtinChecksumOK(s) {
		return "", false
	}
	return s, true
}

// NormalizeIdentifierField dispatches a field edit's value to the matching
// identifier normalizer. A field without an identifier format passes its value
// through unchanged (ok is always true for it), so callers can run every edited
// field through one call. mbid is not here; it has its own UUID validation at
// the edit sites.
func NormalizeIdentifierField(field, value string) (string, bool) {
	switch field {
	case "isrc":
		return NormalizeISRC(value)
	case "isbn":
		return NormalizeISBN(value)
	case "asin":
		return NormalizeASIN(value)
	case "barcode":
		return NormalizeBarcode(value)
	default:
		return value, true
	}
}

// stripIdentifier removes the separators (hyphens and spaces) an identifier is
// commonly written with. An empty input is a clear, ("", true); a non-empty
// input that is nothing but separators is malformed rather than a silent clear.
func stripIdentifier(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	var b strings.Builder
	for _, r := range value {
		if r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

// gtinChecksumOK validates a GTIN-family digit string (EAN-8, UPC-A, EAN-13,
// and the ISBN-13 that is an EAN-13): every character must be an ASCII digit,
// and the mod-10 check digit must hold (counting from the right, the check
// digit weighs 1 and the remaining digits alternate 3, 1). The digit check
// lives here, not with the callers, because checksum arithmetic over an
// ASCII-converted letter can pass by accident ('E' minus '0' is 21, and
// "978013468599E" sums to a clean 150), so a caller that skips its own
// pre-validation must still get a refusal rather than a lucky acceptance.
func gtinChecksumOK(digits string) bool {
	sum := 0
	triple := false
	for i := len(digits) - 1; i >= 0; i-- {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
		d := int(digits[i] - '0')
		if triple {
			d *= 3
		}
		sum += d
		triple = !triple
	}
	return sum%10 == 0
}
