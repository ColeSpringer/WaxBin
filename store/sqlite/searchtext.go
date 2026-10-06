package sqlite

import (
	"slices"
	"strings"
	"unicode"

	"github.com/colespringer/waxbin/model"
	"golang.org/x/text/unicode/norm"
)

// SearchAltForms returns the words the search index keeps beside a text so that what
// people type finds it: each word run together across its inner punctuation (with a
// "$" beside a letter read as "s", so "Ke$ha" gives "kesha"), its compatibility fold
// where that differs (fullwidth and halfwidth forms), katakana folded to hiragana,
// and the overlapping pairs of every run of a script written without spaces (CJK,
// Thai and its neighbours), which the tokenizer otherwise keeps as one word. Plain
// words give nothing.
func SearchAltForms(text string) string {
	return altForms(norm.NFC.String(text), false)
}

// altForms is SearchAltForms over NFC text. In prose a run of digits joined across its
// punctuation is a number or a time ("3.5", "19:45") rather than a word, so it gives
// nothing, where a title such as "4:44" keeps it.
func altForms(text string, prose bool) string {
	var alts []string
	for _, word := range strings.Fields(text) {
		alts = appendWordAlts(alts, word, prose)
	}
	return strings.Join(alts, " ")
}

// searchColumnText is a search_fts column as stored: the text in NFC, then its
// alternate forms. Only bm25 reads the table, so alternates in the column rank as
// the column's own words would.
func searchColumnText(text string) string {
	return indexText(text, false)
}

// proseIndexText is searchColumnText for prose: an episode's show notes, and a
// transcript body as transcript_fts holds it.
func proseIndexText(text string) string {
	return indexText(text, true)
}

func indexText(text string, prose bool) string {
	text = norm.NFC.String(text)
	if alts := altForms(text, prose); alts != "" {
		return text + " " + alts
	}
	return text
}

func appendWordAlts(alts []string, word string, prose bool) []string {
	f := searchWordForms(word)
	if f.compat != nil {
		alts = append(alts, f.compat...)
	}
	if f.joined != "" && (!prose || strings.IndexFunc(f.joined, unicode.IsLetter) >= 0) {
		alts = append(alts, f.joined)
		if k := foldKana(f.joined); k != f.joined {
			alts = append(alts, k)
		}
	}
	for _, tok := range f.base() {
		k := foldKana(tok)
		if k != tok {
			alts = append(alts, k)
		}
		alts = appendRunAlts(alts, k)
	}
	return alts
}

// wordForms is how one whitespace-separated word reads to the search.
type wordForms struct {
	tokens []string // as the tokenizer cuts the word, lowercased
	compat []string // the tokens of the word's NFKC form, nil when they are the same
	joined string   // the word run together, "" when that is one of its tokens already
}

func (f wordForms) base() []string {
	if f.compat != nil {
		return f.compat
	}
	return f.tokens
}

func searchWordForms(word string) wordForms {
	f := wordForms{tokens: searchTokens(word)}
	compat := norm.NFKC.String(word)
	if compat != word {
		if ct := searchTokens(compat); !slices.Equal(ct, f.tokens) {
			f.compat = ct
		}
	}
	if j := joinedWord(compat); j != "" {
		if base := f.base(); len(base) != 1 || base[0] != j {
			f.joined = j
		}
	}
	return f
}

// isBaseRune reports whether r is one of the token characters search_fts declares (L*
// N* Co M*) other than a mark.
func isBaseRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r)
}

// searchTokens cuts a word as the tokenizer does, lowercased, except that a mark
// extends a token and never starts one: the tokenizer folds a leading diacritic away,
// so a token of marks alone would be an empty prefix matching every row.
func searchTokens(word string) []string {
	var out []string
	start := -1
	for i, r := range word {
		switch {
		case isBaseRune(r):
			if start < 0 {
				start = i
			}
		case start >= 0 && unicode.IsMark(r):
		case start >= 0:
			out = append(out, strings.ToLower(word[start:i]))
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, strings.ToLower(word[start:]))
	}
	return out
}

// joinedWord runs a word together, reading a "$" as "s" when the run of dollar signs
// it sits in touches a letter ("Ke$ha", "Bada$$"). A mark is kept only where it
// extends a token, as searchTokens keeps it.
func joinedWord(word string) string {
	rs := []rune(word)
	var b strings.Builder
	inToken := false
	for i, r := range rs {
		switch {
		case isBaseRune(r):
			b.WriteRune(r)
			inToken = true
		case unicode.IsMark(r):
			if inToken {
				b.WriteRune(r)
			}
		case r == '$' && dollarTouchesLetter(rs, i):
			b.WriteByte('s')
			inToken = true
		default:
			inToken = false
		}
	}
	return strings.ToLower(b.String())
}

func dollarTouchesLetter(rs []rune, i int) bool {
	l, r := i, i
	for l > 0 && rs[l-1] == '$' {
		l--
	}
	for r+1 < len(rs) && rs[r+1] == '$' {
		r++
	}
	return l > 0 && unicode.IsLetter(rs[l-1]) || r+1 < len(rs) && unicode.IsLetter(rs[r+1])
}

// foldKana maps katakana to hiragana (model.HiraganaOf).
func foldKana(s string) string {
	return strings.Map(model.HiraganaOf, s)
}

// unspaced reports whether r belongs to a script whose words run together (Han,
// kana, Thai and its neighbours), or whose words carry particles a search should see
// past (Hangul).
func unspaced(r rune) bool {
	return r == 'ー' || unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul,
		unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar)
}

// tokenSegment is a stretch of a token: a run of unspaced units (a character with the
// marks that follow it), or the other characters between runs.
type tokenSegment struct {
	units []string // nil for a stretch of other characters
	text  string
}

func tokenSegments(tok string) []tokenSegment {
	rs := []rune(tok)
	var out []tokenSegment
	for i := 0; i < len(rs); {
		start := i
		if !unspaced(rs[i]) {
			for i < len(rs) && !unspaced(rs[i]) {
				i++
			}
			out = append(out, tokenSegment{text: string(rs[start:i])})
			continue
		}
		var units []string
		for i < len(rs) && unspaced(rs[i]) {
			j := i + 1
			for j < len(rs) && unicode.IsMark(rs[j]) {
				j++
			}
			units = append(units, string(rs[i:j]))
			i = j
		}
		out = append(out, tokenSegment{units: units, text: string(rs[start:i])})
	}
	return out
}

// appendRunAlts adds what a token needs for every character in it to begin a word the
// index holds: the overlapping pairs of each unspaced run (a run of two units that is
// the whole token is its own pair already), the run's last unit alone, and any other
// stretch the token does not begin with, such as Latin glued to a run.
func appendRunAlts(alts []string, tok string) []string {
	segs := tokenSegments(tok)
	for i, sg := range segs {
		n := len(sg.units)
		switch {
		case n == 0:
			if i > 0 {
				alts = append(alts, sg.text)
			}
		case n > 2 || n == 2 && len(segs) > 1:
			for k := 0; k+1 < n; k++ {
				alts = append(alts, sg.units[k]+sg.units[k+1])
			}
			alts = append(alts, sg.units[n-1])
		case n == 2 || i > 0:
			alts = append(alts, sg.units[n-1])
		}
	}
	return alts
}

// runPairs returns the overlapping pairs of a token that is one unspaced run of three
// or more units, the phrase a query matches anywhere inside a longer run, and nil for
// any other token.
func runPairs(tok string) []string {
	segs := tokenSegments(tok)
	if len(segs) != 1 || len(segs[0].units) < 3 {
		return nil
	}
	u := segs[0].units
	pairs := make([]string, 0, len(u)-1)
	for k := 0; k+1 < len(u); k++ {
		pairs = append(pairs, u[k]+u[k+1])
	}
	return pairs
}
