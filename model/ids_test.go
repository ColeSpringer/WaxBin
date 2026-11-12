package model

import (
	"strings"
	"testing"
)

// TestPIDValidIsTheCatalogsSpelling: a pid is matched as text, so only the spelling
// NewPID writes is one. A lower-case ULID decodes to the same value and still names no
// entity.
func TestPIDValidIsTheCatalogsSpelling(t *testing.T) {
	t.Parallel()
	minted := NewPID()
	if !minted.Valid() {
		t.Fatalf("a minted pid %q is not valid", minted)
	}
	for name, p := range map[string]PID{
		"lower case":           PID(strings.ToLower(string(minted))),
		"mixed case":           PID(strings.ToLower(string(minted[:13])) + string(minted[13:])),
		"empty":                "",
		"too short":            minted[:25],
		"too long":             minted + "0",
		"a letter ULIDs skip":  PID("0" + strings.Repeat("U", 25)),
		"past the 128-bit top": PID("8" + strings.Repeat("0", 25)),
		"padded":               " " + minted[1:],
	} {
		if p.Valid() {
			t.Errorf("%s: %q is valid, want only the catalog's own spelling", name, p)
		}
	}
	// The smallest and largest ULIDs are spelled the one way.
	for _, p := range []PID{PID(strings.Repeat("0", 26)), PID("7" + strings.Repeat("Z", 25))} {
		if !p.Valid() {
			t.Errorf("%q is not valid", p)
		}
	}
}
