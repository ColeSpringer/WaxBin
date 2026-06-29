package model

import "strings"

// sortKeyNumWidth is the fixed width digit runs are zero-padded to so that
// "Track 2" sorts before "Track 10" under a plain BINARY comparison.
const sortKeyNumWidth = 10

// leadingArticles are stripped from the front of a sort key (case-insensitive).
var leadingArticles = []string{"the ", "a ", "an "}

// SortKey derives a collation-friendly key from a display string so a portable
// BINARY sort matches human expectations: case-folded, leading articles
// stripped, embedded numbers zero-padded, and whitespace collapsed.
//
// This implementation uses ASCII-level folding and digit padding. Unicode
// collation can be added here without changing callers or the stored column.
func SortKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, art := range leadingArticles {
		if strings.HasPrefix(s, art) {
			s = s[len(art):]
			break
		}
	}
	return padNumbers(collapseSpaces(s))
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// padNumbers left-pads each maximal run of ASCII digits to a fixed width.
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
