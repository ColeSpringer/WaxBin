package model

import (
	"sort"
	"strings"
	"testing"
	"unicode"
)

func TestSortKeyGolden(t *testing.T) {
	cases := map[string]string{
		"The Beatles":  "beatles",          // leading article stripped
		"A Perfect...": "perfect...",       // "a " stripped
		"An Awesome":   "awesome",          // "an " stripped
		"BEATLES":      "beatles",          // case-folded
		"Foo   Bar":    "foo bar",          // whitespace collapsed
		"  Trim  ":     "trim",             // surrounding space trimmed
		"Track 2":      "track 0000000002", // digit run zero-padded
		"Track 10":     "track 0000000010", // wider run padded to same width
		"Sant ana":     "sant ana",         // no article at a word boundary mid-string
		"Theremin":     "theremin",         // "the" without a following space is not an article

		// Diacritics fold to the base letter.
		"Édith Piaf":       "edith piaf",
		"Motörhead":        "motorhead",
		"Blue Öyster Cult": "blue oyster cult",
		"Beyoncé":          "beyonce",

		// Letters that neither decompose nor lowercase into a plain one.
		"Sigur Rós":  "sigur ros",
		"Bjørn":      "bjorn",
		"Łódź":       "lodz",
		"Æther":      "aether",
		"Œuvre":      "oeuvre",
		"Þunder":     "thunder",
		"Straße":     "strasse",
		"Đevojka":    "devojka",
		"ΟΔΥΣΣΕΥΣ":   "οδυσσευσ", // final sigma folds to plain sigma
		"Ὀδυσσεύς":   "οδυσσευσ",
		"Ħamrun":     "hamrun",
		"Işık":       "isik",
		"Sigurðsson": "sigurdsson",

		// NFKC compatibility folding: fullwidth, ligature, superscript, unit forms.
		"ＹＭＯ":      "ymo",
		"ﬁreworks": "fireworks",
		"№ 5":      "no 0000000005",
		"Ⅻ":        "xii",
		"5 ㎏":      "0000000005 kg",
		"Track²":   "track0000000002", // expands with no separating space

		// Katakana folds to hiragana, and voicing marks drop.
		"ザ・バンド": "さ・はんと",
		"ばんど":   "はんと",
		"ヷ":     "わ", // outside the fold range but decomposes into it
		"ラーメン":  "らーめん",

		// Non-ASCII digits fold to ASCII and pad like any other run.
		"Track ٢": "track 0000000002",
		"Track ۵": "track 0000000005",

		// Marks that carry a sound rather than decorating one survive.
		"สวัสดี":   "สวัสดี",
		"ก้า":      "ก้า",
		"हिंदी":    "हिंदी",
		"עִבְרִית": "עִבְרִית",

		// A diacritics-only name folds away, so the unstripped form stands in.
		"́":   "́",
		" ́ ": "́",
	}
	for in, want := range cases {
		if got := SortKey(in); got != want {
			t.Errorf("SortKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSortKeyASCIIUnchanged bounds the blast radius of folding: every key below is
// byte-identical to what the ASCII-only implementation produced.
// TestSortKeyCollapsesBeforeArticle carries the one deliberate exception.
func TestSortKeyASCIIUnchanged(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"The Beatles":   "beatles",
		"A Perfect...":  "perfect...",
		"An Awesome":    "awesome",
		"BEATLES":       "beatles",
		"Foo   Bar":     "foo bar",
		"  Trim  ":      "trim",
		"Track 2":       "track 0000000002",
		"Track 10":      "track 0000000010",
		"Sant ana":      "sant ana",
		"Theremin":      "theremin",
		"AC/DC":         "ac/dc",
		"2 Unlimited":   "0000000002 unlimited",
		"Andrew W.K.":   "andrew w.k.",
		"the":           "the",
		"A":             "a",
		"An":            "an",
		"!!!":           "!!!",
		"The The":       "the",
		"a b":           "b",
		"Sixteen 16 16": "sixteen 0000000016 0000000016",
	}
	for in, want := range cases {
		if got := SortKey(in); got != want {
			t.Errorf("SortKey(%q) = %q, want %q (ASCII keys must not move)", in, got, want)
		}
	}
}

// TestSortKeyCollapsesBeforeArticle pins the one ASCII key folding moved. Spaces
// collapse before the article is stripped now, so an article separated by a tab is
// recognized where it was not before.
func TestSortKeyCollapsesBeforeArticle(t *testing.T) {
	if got := SortKey("The\tBeatles"); got != "beatles" {
		t.Fatalf("SortKey(%q) = %q, want %q", "The\tBeatles", got, "beatles")
	}
}

// TestFoldASCIIFastPathAgrees keeps the fast path an optimization rather than a
// second behaviour.
func TestFoldASCIIFastPathAgrees(t *testing.T) {
	inputs := []string{
		"", " ", "The Beatles", "AC/DC", "Track 10", "!!!", "\t\n mixed  Space ",
		"ALL CAPS", "already lower", "MiXeD 42 cAsE", "~!@#$%^&*()_+", "the",
	}
	for _, in := range inputs {
		if fast, general := Fold(in), foldGeneral(in); fast != general {
			t.Errorf("Fold(%q) = %q, general path = %q", in, fast, general)
		}
	}
}

// TestAsciiDigitByBlock covers one case per non-ASCII decimal block, zero included:
// r%10 in place of (r-Lo)%10 gets every block's zero wrong while still looking
// right for a nonzero Arabic-Indic digit.
func TestAsciiDigitByBlock(t *testing.T) {
	cases := map[rune]byte{
		'٠': '0', // Arabic-Indic zero
		'٢': '2',
		'٩': '9',
		'۰': '0', // extended Arabic-Indic
		'۵': '5',
		'०': '0', // Devanagari
		'९': '9',
		'০': '0', // Bengali
		'๐': '0', // Thai
		'๗': '7',
		'一': 0, // a letter, not a decimal digit
		// One merged range covering five mathematical digit sets, which is why the
		// modulo is needed after subtracting the range start.
		'\U0001D7CE': '0', // bold zero, the range start
		'\U0001D7D8': '0', // double-struck zero, ten past it
		'\U0001D7D9': '1',
		'\U0001D7FF': '9', // the range end
	}
	for r, want := range cases {
		got, ok := asciiDigit(r)
		if want == 0 {
			if ok {
				t.Errorf("asciiDigit(%q) = %q, want no digit", r, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("asciiDigit(%q) = (%q,%t), want (%q,true)", r, got, ok, want)
		}
	}
}

// TestAsciiDigitCoversEveryUnicodeDigit fails on a Go upgrade that reshapes
// unicode.Nd, rather than letting some script's digits go silently unpadded. It
// pins the two table properties (r-Lo)%10 rests on: stride one, whole 10-digit sets.
func TestAsciiDigitCoversEveryUnicodeDigit(t *testing.T) {
	for _, tab := range []struct {
		name string
		r16  []unicode.Range16
		r32  []unicode.Range32
	}{{"Nd", unicode.Nd.R16, unicode.Nd.R32}} {
		for _, rg := range tab.r16 {
			if rg.Stride != 1 || (rg.Hi-rg.Lo+1)%10 != 0 {
				t.Errorf("%s R16 %#U-%#U: stride %d, %d wide", tab.name, rune(rg.Lo), rune(rg.Hi), rg.Stride, rg.Hi-rg.Lo+1)
			}
		}
		for _, rg := range tab.r32 {
			if rg.Stride != 1 || (rg.Hi-rg.Lo+1)%10 != 0 {
				t.Errorf("%s R32 %#U-%#U: stride %d, %d wide", tab.name, rune(rg.Lo), rune(rg.Hi), rg.Stride, rg.Hi-rg.Lo+1)
			}
		}
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.Is(unicode.Nd, r) {
			continue
		}
		if _, ok := asciiDigit(r); !ok {
			t.Fatalf("%#U is a decimal digit with no ASCII mapping", r)
		}
	}
}

func TestRefoldKeyFoldsWithoutStrippingArticle(t *testing.T) {
	cases := map[string]string{
		"beatles, thé":     "beatles, the",     // an already-stored tag-derived key folds
		"a team":           "a team",           // the article is not re-stripped
		"track ٢":          "track 0000000002", // a non-ASCII digit run pads on the way through
		"track 0000000002": "track 0000000002",
	}
	for in, want := range cases {
		if got := RefoldKey(in); got != want {
			t.Errorf("RefoldKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSortKeyOrderingIsCollationCorrect proves a plain BINARY sort over the
// generated keys matches human collation expectations: numeric-aware, article-
// insensitive, case-insensitive, and accent-insensitive.
func TestSortKeyOrderingIsCollationCorrect(t *testing.T) {
	titles := []string{"Track 10", "the Apple", "Track 2", "Zebra", "a Banana", "Édith Piaf", "Edwards"}
	type kv struct{ title, key string }
	rows := make([]kv, len(titles))
	for i, ti := range titles {
		rows[i] = kv{ti, SortKey(ti)}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })

	var got []string
	for _, r := range rows {
		got = append(got, r.title)
	}
	// Apple/Banana (a-strip), Édith among the E names rather than after Zebra,
	// Track 2 < Track 10 (numeric), Zebra last.
	want := []string{"the Apple", "a Banana", "Édith Piaf", "Edwards", "Track 2", "Track 10", "Zebra"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collation order = %v, want %v", got, want)
		}
	}
}

// FuzzFoldIdempotent guards the property the in-place refold rests on. The input
// that first broke it, invalid UTF-8 that x/text declines to normalize, is in
// testdata; no hand-written case would have found it.
func FuzzFoldIdempotent(f *testing.F) {
	for _, s := range []string{
		"Édith Piaf", "ＹＭＯ", "ザ・バンド", "Straße", "Track ٢", "№ Ⅻ ㎏", "́",
		"ﬁreworks", "The Beatles", "ｱｲｳ", "ẞ", "Ǆ", "ﬀ", "㍿", "Ⅷ", "ǰ",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		once := Fold(s)
		if twice := Fold(once); twice != once {
			t.Fatalf("Fold not idempotent for %q: %q then %q", s, once, twice)
		}
		if strings.TrimSpace(once) == "" && strings.TrimSpace(s) != "" {
			t.Fatalf("Fold(%q) collapsed a non-empty name to %q", s, once)
		}
	})
}

// FuzzRefoldKeyIdempotent covers the same property over already-padded input.
func FuzzRefoldKeyIdempotent(f *testing.F) {
	for _, s := range []string{
		"beatles, thé", "a team", "track ٢", "track 0000000002", "ｔｈｅ ｂｅａｔｌｅｓ",
		"piaf, édith", "0000000002", "́", "ⅻ 2",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		once := RefoldKey(s)
		if twice := RefoldKey(once); twice != once {
			t.Fatalf("RefoldKey not idempotent for %q: %q then %q", s, once, twice)
		}
	})
}
