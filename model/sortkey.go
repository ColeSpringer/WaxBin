package model

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// sortKeyNumWidth is the fixed width digit runs are zero-padded to so that
// "Track 2" sorts before "Track 10" under a plain BINARY comparison.
const sortKeyNumWidth = 10

// leadingArticles are stripped from the front of a sort key (case-insensitive).
var leadingArticles = []string{"the ", "a ", "an "}

// Katakana folds to hiragana by a fixed offset. The range ends before the
// prolonged sound mark (U+30FC), which is left alone.
const (
	katakanaLo    = 0x30A1
	katakanaHi    = 0x30F6
	kanaFoldShift = 0x60
)

// SortKey derives a collation-friendly key from a display string so a portable
// BINARY sort matches human expectations: folded to a plain lowercase form (see
// Fold), leading articles stripped, embedded numbers zero-padded, and whitespace
// collapsed. It is readable folding rather than a DUCET collation key, because
// these keys also drive the letter buckets a client renders, and it does no
// per-locale tailoring.
//
// Changing it restates every stored sort_key, and Store.RefreshSortKeys is the
// only repair: an entity resolved by match_key keeps the key it was created
// with, so no rescan can rewrite one.
func SortKey(s string) string {
	return padNumbers(stripArticle(collapseSpaces(Fold(s))))
}

// RefoldKey re-derives an already-stored sort key, for the columns whose input
// was a sort tag the catalog does not keep (track.artist_sort and friends). It
// folds and pads but does not strip an article, because stripArticle is not
// idempotent: "The A Team" stored as "a team" would re-strip to "team".
func RefoldKey(s string) string {
	return padNumbers(collapseSpaces(Fold(s)))
}

// Fold reduces a display string to a plain lowercase form for ordering: NFKC
// compatibility folding (fullwidth Latin, ligatures, superscripts, unit forms),
// lowercasing, combining diacritics dropped so "é" orders as "e" (see
// isDiacritic for which marks those are), the letters that survive both mapped by
// hand, katakana folded to hiragana so the two syllabaries interleave, and Unicode
// decimal digits mapped to ASCII. It folds characters only, neither collapsing
// whitespace nor padding numbers, and it is idempotent.
//
// Not cases.Fold: it pulls in x/text/language, its Caser cannot be shared across
// goroutines, and full case folding is not lowercase everywhere (Cherokee folds
// upward).
func Fold(s string) string {
	// An all-ASCII string is unaffected by everything foldGeneral does.
	if isASCII(s) {
		return strings.ToLower(s)
	}
	return foldGeneral(s)
}

func foldGeneral(s string) string {
	// x/text leaves a string holding invalid UTF-8 unnormalized while ToLower
	// rewrites the bad bytes to U+FFFD, so without this the second fold of a
	// mojibake tag would normalize what the first could not.
	s = strings.ToValidUTF8(s, string(utf8.RuneError))

	lowered := strings.ToLower(norm.NFKC.String(s))
	folded := dropMarks(lowered)
	var b strings.Builder
	b.Grow(len(folded))
	for _, r := range folded {
		switch {
		case r < utf8.RuneSelf:
			b.WriteByte(byte(r))
		case r >= katakanaLo && r <= katakanaHi:
			// After the mark strip, so voiced "ガ" has already become "カ" and lands on
			// the same "か" that voiced hiragana reaches.
			b.WriteRune(r - kanaFoldShift)
		default:
			if rep, ok := foldSpecial(r); ok {
				b.WriteString(rep)
				continue
			}
			if d, ok := asciiDigit(r); ok {
				b.WriteByte(d)
				continue
			}
			b.WriteRune(r)
		}
	}

	out := b.String()
	// A name of nothing but diacritics folds away entirely. Test the trimmed result,
	// or one that left a space behind pools with every other such name.
	if strings.TrimSpace(out) == "" && strings.TrimSpace(s) != "" {
		return lowered
	}
	return out
}

// foldSpecial maps the letters that neither decompose nor lowercase into a plain
// Latin one. Lowercasing has already run, so only the lowercase forms need a
// case, and the multi-rune expansions are why the caller is a builder loop.
func foldSpecial(r rune) (string, bool) {
	switch r {
	case 'ø':
		return "o", true
	case 'đ', 'ð':
		return "d", true
	case 'ł':
		return "l", true
	case 'ħ':
		return "h", true
	case 'ı':
		return "i", true
	case 'ŋ':
		return "n", true
	case 'ß':
		return "ss", true
	case 'æ':
		return "ae", true
	case 'œ':
		return "oe", true
	case 'þ':
		return "th", true
	case 'ς':
		return "σ", true
	}
	return "", false
}

// dropMarks strips combining diacritics so an accented letter orders as its base.
func dropMarks(s string) string {
	d := norm.NFD.String(s)
	var b strings.Builder
	b.Grow(len(d))
	for _, r := range d {
		if isDiacritic(r) {
			continue
		}
		b.WriteRune(r)
	}
	return norm.NFC.String(b.String())
}

// isDiacritic reports whether r is a generic combining diacritic, plus the kana
// voicing marks. Not every unicode.Mn: a Thai vowel sign, an Indic matra, a Hebrew
// point, and an Arabic harakat carry a sound rather than decorating one, and
// dropping those turns "สวัสดี" into "สวสด".
func isDiacritic(r rune) bool {
	switch {
	case r >= 0x0300 && r <= 0x036F: // combining diacritical marks
		return true
	case r >= 0x1AB0 && r <= 0x1AFF: // ... extended
		return true
	case r >= 0x1DC0 && r <= 0x1DFF: // ... supplement
		return true
	case r >= 0xFE20 && r <= 0xFE2F: // combining half marks
		return true
	case r == 0x3099 || r == 0x309A: // kana voicing
		return true
	}
	return false
}

// asciiDigit maps a Unicode decimal digit to ASCII by numeric value, so
// padNumbers pads "Track ٢" like "Track 2".
//
// The value is (r - Lo) % 10 where Lo starts the containing range, never r % 10:
// Arabic-Indic zero is U+0660, and 1632 % 10 is 2. The modulo is still needed
// after subtracting Lo because Go merges adjacent runs, so U+1D7CE to U+1D7FF is
// one range covering five mathematical digit sets.
func asciiDigit(r rune) (byte, bool) {
	if !unicode.Is(unicode.Nd, r) {
		return 0, false
	}
	for _, rg := range unicode.Nd.R16 {
		if rg.Stride == 1 && r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return byte('0' + (r-rune(rg.Lo))%10), true
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if rg.Stride == 1 && r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return byte('0' + (r-rune(rg.Lo))%10), true
		}
	}
	return 0, false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// stripArticle runs after the spaces are collapsed, so "The\tBeatles" is
// stripped like "The Beatles".
func stripArticle(s string) string {
	for _, art := range leadingArticles {
		if strings.HasPrefix(s, art) {
			return s[len(art):]
		}
	}
	return s
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// padNumbers left-pads each maximal run of ASCII digits to a fixed width. Fold
// has already mapped the other Unicode decimal digits to ASCII, so it sees them
// all.
func padNumbers(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if !isASCIIDigit(rune(s[i])) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && isASCIIDigit(rune(s[j])) {
			j++
		}
		run := s[i:j]
		if pad := sortKeyNumWidth - len(run); pad > 0 {
			b.WriteString(strings.Repeat("0", pad))
		}
		b.WriteString(run)
		i = j
	}
	return b.String()
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }
