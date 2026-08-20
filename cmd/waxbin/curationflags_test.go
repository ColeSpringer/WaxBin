package main

import (
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestLockChange pins the mapping from the two lock flags every curation command carries
// onto the store's three-state instruction. Neither flag locks, which is the default
// every one of them has always had.
func TestLockChange(t *testing.T) {
	for _, tc := range []struct {
		noLock, keepLock bool
		want             model.LockChange
	}{
		{false, false, model.LockOn},
		{true, false, model.LockOff},
		{false, true, model.LockUnchanged},
	} {
		if got := lockChange(tc.noLock, tc.keepLock); got != tc.want {
			t.Errorf("lockChange(%v, %v) = %q, want %q", tc.noLock, tc.keepLock, got, tc.want)
		}
	}
}

// TestParseAttribution covers the whole flag-pairing policy: naming nothing is a hand
// edit, each surface takes its own vocabulary, and every refusal names the flag that is
// actually wrong rather than the one that happens to be nearby.
func TestParseAttribution(t *testing.T) {
	art := func(source, provider, url string) (model.Attribution, error) {
		return parseAttribution("art set", source, provider, url, model.Attribution.ValidForArt, artSourceList)
	}
	lyrics := func(source, provider string) (model.Attribution, error) {
		return parseAttribution("lyrics set", source, provider, "", model.Attribution.ValidForLyrics, lyricsSourceList)
	}

	// No flags at all stays empty, so the store applies its own default rather than the
	// CLI deciding for it.
	if got, err := art("", "", ""); err != nil || got != (model.Attribution{}) {
		t.Errorf("no flags = %+v (err %v), want the zero attribution", got, err)
	}
	// A URL alone still records a user set, since something was said.
	if got, err := art("", "", "https://x/y"); err != nil || got.Source != model.SourceUser {
		t.Errorf("url alone = %+v (err %v), want a user source", got, err)
	}
	if got, err := art("enrichment", "itunes", "https://x/y"); err != nil ||
		got.Provider != "itunes" || got.SourceURL != "https://x/y" {
		t.Errorf("stamped = %+v (err %v)", got, err)
	}
	if _, err := art("feed", "", ""); err != nil {
		t.Errorf("feed cover = %v, want accepted", err)
	}

	for _, tc := range []struct {
		what             string
		source, provider string
		wantMsg          string
	}{
		{"unknown source", "nonsense", "", "unknown --source"},
		{"organize on a picture", "organize", "", "unknown --source"},
		{"enrichment with no provider", "enrichment", "", "needs --provider"},
		{"provider on a tag source", "tag", "itunes", "--provider names an enrichment service"},
	} {
		_, err := art(tc.source, tc.provider, "")
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s = %v, want CodeInvalid", tc.what, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantMsg) {
			t.Errorf("%s said %q, want it to name %q", tc.what, err, tc.wantMsg)
		}
	}

	// The lyrics surface is narrower: a feed publishes covers, never words.
	if _, err := lyrics("feed", ""); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("feed lyrics = %v, want CodeInvalid", err)
	}
	if _, err := lyrics("sidecar", ""); err != nil {
		t.Errorf("sidecar lyrics = %v, want accepted", err)
	}
}
