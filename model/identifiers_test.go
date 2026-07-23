package model

import "testing"

func TestNormalizeISRC(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true}, // empty clears
		{"USRC17700001", "USRC17700001", true},
		{"us-rc1-77-00001", "USRC17700001", true},
		{"US RC1 77 00001", "USRC17700001", true},
		{"GBAYE0500001", "GBAYE0500001", true},
		{"1SRC17700001", "", false},  // country must be letters
		{"USRC1770000", "", false},   // eleven characters
		{"USRC177000012", "", false}, // thirteen characters
		{"USRC1770000A", "", false},  // designation must be digits
		{"---", "", false},           // separators alone are not a clear
	}
	for _, tc := range cases {
		got, ok := NormalizeISRC(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeISRC(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeISBN(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"0134685997", "0134685997", true},       // valid ISBN-10
		{"0-13-468599-7", "0134685997", true},    // separators stripped
		{"080442957x", "080442957X", true},       // X check digit, uppercased
		{"9780134685991", "9780134685991", true}, // valid ISBN-13
		{"978-0-13-468599-1", "9780134685991", true},
		{"9790000000001", "9790000000001", true}, // the 979 Bookland prefix is ISBN space too
		{"0134685998", "", false},                // bad mod-11 checksum
		{"9780134685992", "", false},             // bad mod-10 checksum
		{"X134685997", "", false},                // X anywhere but last
		{"12345", "", false},                     // wrong length
		// A checksum-valid EAN-13 outside the 978/979 Bookland prefixes is a
		// product barcode, not an ISBN; without the prefix check every barcode
		// on a CD case would pass.
		{"4006381333931", "", false},
		// A letter in a 13-character value must be rejected even when its ASCII
		// arithmetic happens to satisfy the mod-10 sum ('E' minus '0' is 21 and
		// this string sums to 150).
		{"978013468599E", "", false},
		{"978-01346-8599E", "", false}, // same bypass spelled with separators
	}
	for _, tc := range cases {
		got, ok := NormalizeISBN(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeISBN(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	// No 10-to-13 conversion: the ten-digit form stays ten digits.
	if got, _ := NormalizeISBN("0134685997"); len(got) != 10 {
		t.Errorf("ISBN-10 was converted: %q", got)
	}
}

func TestNormalizeASIN(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"B000FA5KK0", "B000FA5KK0", true},
		{"b000fa5kk0", "B000FA5KK0", true}, // uppercased
		{"B000FA5KK", "", false},           // nine characters
		{"B000-A5KK0", "", false},          // ASINs carry no separators
	}
	for _, tc := range cases {
		got, ok := NormalizeASIN(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeASIN(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeBarcode(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"9780134685991", "9780134685991", true}, // EAN-13
		{"4006381333931", "4006381333931", true}, // EAN-13 a NormalizeISBN rejects: fine as a barcode
		{"036000291452", "036000291452", true},   // UPC-A
		{"96385074", "96385074", true},           // EAN-8
		{"0 36000 29145 2", "036000291452", true},
		{"036000291453", "", false}, // bad check digit
		{"12345678901", "", false},  // eleven digits
		{"03600029145A", "", false}, // non-digit
	}
	for _, tc := range cases {
		got, ok := NormalizeBarcode(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeBarcode(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeIdentifierField(t *testing.T) {
	// Identifier fields dispatch to their normalizer.
	if got, ok := NormalizeIdentifierField("isrc", "us-rc1-77-00001"); got != "USRC17700001" || !ok {
		t.Errorf("isrc = (%q, %v)", got, ok)
	}
	if _, ok := NormalizeIdentifierField("isbn", "not-an-isbn"); ok {
		t.Error("malformed isbn accepted")
	}
	// A non-identifier field passes through untouched, mixed case and all.
	if got, ok := NormalizeIdentifierField("title", "My Song - Live"); got != "My Song - Live" || !ok {
		t.Errorf("title passthrough = (%q, %v)", got, ok)
	}
}
