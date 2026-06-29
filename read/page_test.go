package read

import (
	"encoding/base64"
	"testing"
)

func TestCursorRoundTrip(t *testing.T) {
	// Generated sort keys never contain the separator byte, so a realistic key
	// with spaces and padded digits round-trips exactly.
	const sortKey = "beatles 0000000002 abbey road"
	c := EncodeCursor(sortKey, "01HXYZ")
	sk, pid, ok := c.Decode()
	if !ok {
		t.Fatal("decode failed for a freshly encoded cursor")
	}
	if sk != sortKey {
		t.Errorf("sort key round-trip = %q", sk)
	}
	if pid != "01HXYZ" {
		t.Errorf("pid round-trip = %q", pid)
	}
}

func TestCursorRoundTripWithSeparatorInSortKey(t *testing.T) {
	// A (pathological) sort key containing the separator byte must still round-
	// trip: the pid is separator-free, so Decode splits on the last separator.
	const sortKey = "weird\x1ftitle\x1fhere"
	c := EncodeCursor(sortKey, "01HXYZPID")
	sk, pid, ok := c.Decode()
	if !ok || sk != sortKey || pid != "01HXYZPID" {
		t.Fatalf("Decode = (%q, %q, %v), want (%q, 01HXYZPID, true)", sk, pid, ok, sortKey)
	}
}

func TestCursorDecodeRejectsGarbage(t *testing.T) {
	// A well-formed base64 token that lacks the internal separator is still
	// structurally invalid and must be rejected, not silently accepted.
	noSep := Cursor(base64.RawURLEncoding.EncodeToString([]byte("no-separator-here")))
	for _, bad := range []Cursor{"", "!!!not-base64!!!", noSep} {
		if _, _, ok := bad.Decode(); ok {
			t.Errorf("Decode(%q) reported ok for a malformed cursor", bad)
		}
	}
}

func TestGroupByValid(t *testing.T) {
	for _, g := range GroupBys() {
		if !g.Valid() {
			t.Errorf("GroupBys() returned an invalid value %q", g)
		}
	}
	if GroupBy("nonsense").Valid() {
		t.Error("an unknown group-by reported valid")
	}
}
