package read

import "testing"

func TestGroupByValidAcceptsTagDimensions(t *testing.T) {
	// The fixed dimensions stay valid.
	for _, g := range GroupBys() {
		if !g.Valid() {
			t.Errorf("fixed dimension %q should be valid", g)
		}
	}
	// A well-formed custom-tag dimension is valid (canonical, non-reserved key).
	for _, g := range []GroupBy{"tag.MOOD", "tag.mood", "tag.MY_KEY"} {
		if !g.Valid() {
			t.Errorf("%q should be a valid tag dimension", g)
		}
	}
	// Reserved or malformed tag dimensions are rejected, mirroring the query barrier.
	for _, g := range []GroupBy{"tag.TITLE", "tag.", "tag.A=B", "tag", "bogus"} {
		if g.Valid() {
			t.Errorf("%q should be rejected", g)
		}
	}
}

func TestTagGroupKeyCanonicalizes(t *testing.T) {
	if k, ok := TagGroupKey("tag.mood"); !ok || k != "MOOD" {
		t.Errorf("TagGroupKey(tag.mood) = (%q,%v), want (MOOD,true)", k, ok)
	}
	if _, ok := TagGroupKey("tag.TITLE"); ok {
		t.Error("TagGroupKey(tag.TITLE) should reject a reserved key")
	}
	if _, ok := TagGroupKey("genre"); ok {
		t.Error("TagGroupKey(genre) should reject a non-tag dimension")
	}
}
