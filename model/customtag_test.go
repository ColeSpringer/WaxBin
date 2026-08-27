package model

import "testing"

func TestCanonicalTagKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"mood", "MOOD", true},
		{"KEY", "KEY", true},
		{"  Key  ", "KEY", true}, // trimmed + uppercased (key/KEY dedup)
		{"MY TAG", "MY TAG", true},
		{"", "", false},
		{"  ", "", false},
		{"bad=key", "", false}, // '=' is reserved
		{"héllo", "", false},   // non-ASCII
	}
	for _, c := range cases {
		got, ok := CanonicalTagKey(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("CanonicalTagKey(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestIsReservedTagKey(t *testing.T) {
	reserved := []string{"TITLE", "ARTIST", "ISRC", "BARCODE", "MUSICBRAINZ_TRACKID", "PRODUCER", "NARRATOR", "COMPOSERSORT", "WAXBIN_ITEM_PID", "REPLAYGAIN_TRACK_GAIN"}
	for _, k := range reserved {
		if !IsReservedTagKey(k) {
			t.Errorf("%q should be reserved", k)
		}
	}
	free := []string{"MOOD", "KEY", "MY TAG", "COPYRIGHT", "ACOUSTID_ID"}
	for _, k := range free {
		if IsReservedTagKey(k) {
			t.Errorf("%q should not be reserved", k)
		}
	}
}

// TestFoldedWireSpellingsAreReserved covers the frame spellings WaxLabel folds onto a
// canonical key on every format's read path. Each one now names a value WaxBin already
// owns through another surface, so none may be stored or edited as a custom tag.
func TestFoldedWireSpellingsAreReserved(t *testing.T) {
	folded := []string{
		"PART_NUMBER", "TOTAL_PARTS", "TOTAL_DISCS", "LEAD_PERFORMER",
		"DATE_RECORDED", "DATE_RELEASED", "DATE_RELEASE", "DATE_ORIGINAL",
		"ORIGINAL_DATE", "ENCODED_BY", "CATALOG_NUMBER", "PUBLISHER",
		"REMIXED_BY", "CONTENT_GROUP",
		// BPM is the track column's own key and TBPM the ID3 frame spelling folded onto
		// it. Both retired once the column landed.
		"BPM", "TBPM",
	}
	for _, k := range folded {
		if !IsReservedTagKey(k) {
			t.Errorf("%q should be reserved; it folds onto a key WaxBin owns", k)
		}
		if IsCuratableField(TagLockField(k)) {
			t.Errorf("tag.%s should not be curatable", k)
		}
	}
}

func TestCutTagPrefixAndCuratable(t *testing.T) {
	if key, ok := CutTagPrefix("tag.MOOD"); !ok || key != "MOOD" {
		t.Errorf("CutTagPrefix(tag.MOOD) = (%q,%v)", key, ok)
	}
	if _, ok := CutTagPrefix("tag."); ok {
		t.Error("empty tag key should not parse")
	}
	// A custom-tag lock field is curatable; a reserved one or a bad key is not.
	if !IsCuratableField("tag.MOOD") {
		t.Error("tag.MOOD should be curatable")
	}
	if IsCuratableField("tag.ARTIST") {
		t.Error("tag.ARTIST (reserved key) should not be curatable")
	}
	if IsCuratableField("tag.bad=key") {
		t.Error("tag.bad=key (invalid key) should not be curatable")
	}
}
