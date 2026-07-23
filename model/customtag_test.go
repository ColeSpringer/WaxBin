package model

import "testing"

func TestCanonicalTagKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"mood", "MOOD", true},
		{"BPM", "BPM", true},
		{"  Bpm  ", "BPM", true}, // trimmed + uppercased (bpm/BPM dedup)
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
	free := []string{"MOOD", "BPM", "KEY", "MY TAG", "COPYRIGHT", "ACOUSTID_ID"}
	for _, k := range free {
		if IsReservedTagKey(k) {
			t.Errorf("%q should not be reserved", k)
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
