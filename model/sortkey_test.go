package model

import (
	"sort"
	"testing"
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
	}
	for in, want := range cases {
		if got := SortKey(in); got != want {
			t.Errorf("SortKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSortKeyOrderingIsCollationCorrect proves a plain BINARY sort over the
// generated keys matches human collation expectations: numeric-aware, article-
// insensitive, case-insensitive.
func TestSortKeyOrderingIsCollationCorrect(t *testing.T) {
	titles := []string{"Track 10", "the Apple", "Track 2", "Zebra", "a Banana"}
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
	// Apple (a-strip), Banana (a-strip), Track 2 < Track 10 (numeric), Zebra last.
	want := []string{"the Apple", "a Banana", "Track 2", "Track 10", "Zebra"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collation order = %v, want %v", got, want)
		}
	}
}
