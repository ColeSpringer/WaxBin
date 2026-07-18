package main

import (
	"strings"
	"testing"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// yearCmd is a bare command carrying just the "year" flag buildQuery probes via
// Changed("year"), so a test can exercise buildQuery without opening a catalog.
func yearCmd() *cobra.Command {
	cmd := &cobra.Command{}
	var year int
	cmd.Flags().IntVar(&year, "year", 0, "")
	return cmd
}

func condOf(t *testing.T, q query.Query) query.Cond {
	t.Helper()
	c, ok := q.Where.(query.Cond)
	if !ok {
		t.Fatalf("where = %T, want a single Cond", q.Where)
	}
	return c
}

func TestBuildQueryTagEqualityPreservesEqualsInValue(t *testing.T) {
	// KEY=VALUE splits on the FIRST '=', so a value that itself contains '=' survives.
	q, err := buildQuery(yearCmd(), "", queryFlags{tagEq: []string{"DISCOGS_RELEASE=id=12345"}})
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}
	c := condOf(t, q)
	if c.Field != "tag.DISCOGS_RELEASE" || c.Op != query.OpIs || c.Value != "id=12345" {
		t.Fatalf("cond = %+v, want tag.DISCOGS_RELEASE is \"id=12345\"", c)
	}
}

func TestBuildQueryTagWithoutEqualsIsUsageError(t *testing.T) {
	_, err := buildQuery(yearCmd(), "", queryFlags{tagEq: []string{"NOEQUALS"}})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("--tag with no '=' should be CodeInvalid, got %v", err)
	}
}

func TestBuildQueryTagEmptyKeyIsUsageError(t *testing.T) {
	// An empty key on any tag flag gets a clear message at the point of use, not the
	// resolver's generic "unknown field" error.
	for _, qf := range []queryFlags{
		{tagEq: []string{"=value"}},
		{tagContains: []string{"=sub"}},
		{tagPresent: []string{""}},
		{tagMissing: []string{"   "}},
	} {
		_, err := buildQuery(yearCmd(), "", qf)
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%+v: empty tag key should be CodeInvalid, got %v", qf, err)
		}
	}
}

func TestBuildQueryTagReservedKeyIsClearError(t *testing.T) {
	// A reserved key (owned by a modeled/edit surface) is rejected at the point of use
	// with a message naming it as reserved, not the resolver's generic "unknown field".
	for _, qf := range []queryFlags{
		{tagEq: []string{"ISRC=X"}},
		{tagPresent: []string{"TITLE"}},
	} {
		_, err := buildQuery(yearCmd(), "", qf)
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Fatalf("%+v: reserved tag key should be CodeInvalid, got %v", qf, err)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%+v: error should name the key as reserved, got %q", qf, err)
		}
	}
}

func TestBuildQueryTagPresenceAndContains(t *testing.T) {
	q, err := buildQuery(yearCmd(), "", queryFlags{tagPresent: []string{"MYKEY"}})
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}
	if c := condOf(t, q); c.Field != "tag.MYKEY" || c.Op != query.OpIsPresent {
		t.Fatalf("present cond = %+v, want tag.MYKEY isPresent", c)
	}

	q, err = buildQuery(yearCmd(), "", queryFlags{tagContains: []string{"MOOD=hap"}})
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}
	if c := condOf(t, q); c.Field != "tag.MOOD" || c.Op != query.OpContains || c.Value != "hap" {
		t.Fatalf("contains cond = %+v, want tag.MOOD contains \"hap\"", c)
	}
}
