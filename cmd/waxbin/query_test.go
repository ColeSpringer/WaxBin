package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
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

// TestBuildQueryKindValidation pins that a mistyped kind is a usage error, not an
// empty result that reads as an empty library. query, browse, facet, and edit all
// route through buildQuery.
func TestBuildQueryKindValidation(t *testing.T) {
	for _, k := range []string{"track", "book", "episode"} {
		if _, err := buildQuery(yearCmd(), "", queryFlags{kind: k}); err != nil {
			t.Errorf("--kind %s: %v", k, err)
		}
	}
	for _, bad := range []string{"tracks", "Track", "music", "podcast"} {
		_, err := buildQuery(yearCmd(), "", queryFlags{kind: bad})
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("--kind %s = %v, want CodeInvalid", bad, err)
		}
		if err != nil && !strings.Contains(err.Error(), "track|book|episode") {
			t.Errorf("--kind %s error does not list the vocabulary: %v", bad, err)
		}
	}
}

// fakeLibraryLister stands in for the catalog so the resolver can be tested here,
// where nothing opens a database.
type fakeLibraryLister struct{ libs []*model.Library }

func (f fakeLibraryLister) Libraries(context.Context) ([]*model.Library, error) {
	return f.libs, nil
}

func testLibs() fakeLibraryLister {
	return fakeLibraryLister{libs: []*model.Library{
		{PID: "lib-music", Root: []byte("/music"), DisplayRoot: "/music"},
		{PID: "lib-books", Root: []byte("/books"), DisplayRoot: "/books"},
	}}
}

func TestBuildQueryLibraryFlagsCompileToIn(t *testing.T) {
	q, err := buildQuery(yearCmd(), "", queryFlags{library: []model.PID{"lib-music", "lib-books"}})
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}
	c := condOf(t, q)
	if c.Field != "library" || c.Op != query.OpIn {
		t.Fatalf("cond = %+v, want library in", c)
	}
	if !reflect.DeepEqual(c.Values, []any{model.PID("lib-music"), model.PID("lib-books")}) {
		t.Errorf("values = %#v, want both pids", c.Values)
	}
}

func TestResolveLibraryRefsAcceptsAPIDOrARootPath(t *testing.T) {
	for _, ref := range []string{"lib-music", "/music", "/music/"} {
		got, err := resolveLibraryRefs(context.Background(), testLibs(), "query", []string{ref})
		if err != nil {
			t.Fatalf("%q: %v", ref, err)
		}
		if len(got) != 1 || got[0] != "lib-music" {
			t.Errorf("%q resolved to %v, want [lib-music]", ref, got)
		}
	}
}

// A pid and its root path name one library, and collapsing them keeps the condition at
// arity 1, which is the only arity compileValueSubCond drives off file_library.
func TestResolveLibraryRefsDeduplicatesSpellingsOfOneLibrary(t *testing.T) {
	got, err := resolveLibraryRefs(context.Background(), testLibs(), "query",
		[]string{"/music", "lib-music", "/books", "/music/"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []model.PID{"lib-music", "lib-books"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolved = %v, want %v (deduplicated, first-seen order)", got, want)
	}
}

// No writer produces a library whose root and display_root differ, so this covers the
// branch real data never reaches: both spellings must still resolve, and a path under
// either must still get the containing-root hint.
func TestResolveLibraryRefsMatchesEitherRootSpelling(t *testing.T) {
	libs := fakeLibraryLister{libs: []*model.Library{
		{PID: "lib-odd", Root: []byte("/raw\xffmusic"), DisplayRoot: "/shown/music"},
	}}
	for _, ref := range []string{"/raw\xffmusic", "/shown/music"} {
		got, err := resolveLibraryRefs(context.Background(), libs, "query", []string{ref})
		if err != nil {
			t.Fatalf("%q: %v", ref, err)
		}
		if len(got) != 1 || got[0] != "lib-odd" {
			t.Errorf("%q resolved to %v, want [lib-odd]", ref, got)
		}
	}
	_, err := resolveLibraryRefs(context.Background(), libs, "query", []string{"/raw\xffmusic/jazz"})
	if !waxerr.Is(err, waxerr.CodeNotFound) || !strings.Contains(err.Error(), "inside root") {
		t.Errorf("err = %v, want the containing-root hint against the raw spelling", err)
	}
}

func TestResolveLibraryRefsRejectsAnUnknownLibrary(t *testing.T) {
	_, err := resolveLibraryRefs(context.Background(), testLibs(), "query", []string{"/nope"})
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("err = %v, want CodeNotFound", err)
	}
	for _, want := range []string{"/nope", "/music", "/books"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	// A path under a root must not resolve to the parent, which would widen the filter.
	_, err = resolveLibraryRefs(context.Background(), testLibs(), "query", []string{"/music/jazz"})
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("err = %v, want CodeNotFound", err)
	}
	if !strings.Contains(err.Error(), "inside root /music") {
		t.Errorf("error %q does not point at the containing root", err)
	}
}

// Roots are stored absolute, so a ref typed relative to the working directory has to
// resolve the same way `library add` resolves the path it registers.
func TestResolveLibraryRefsAcceptsARelativePath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	libs := fakeLibraryLister{libs: []*model.Library{
		{PID: "lib-here", Root: []byte(filepath.Join(wd, "music")), DisplayRoot: filepath.Join(wd, "music")},
	}}
	for _, ref := range []string{"music", "./music", "music/"} {
		got, err := resolveLibraryRefs(context.Background(), libs, "query", []string{ref})
		if err != nil {
			t.Fatalf("%q: %v", ref, err)
		}
		if len(got) != 1 || got[0] != "lib-here" {
			t.Errorf("%q resolved to %v, want [lib-here]", ref, got)
		}
	}
}

// pathx.UnderRoot is the repo's one containment check. A hand-rolled
// !HasPrefix(rel, "..") calls a sibling like /musicarchive a child of /music, which
// would hand back the wrong root in the hint.
func TestResolveLibraryRefsDoesNotTreatASiblingAsContained(t *testing.T) {
	for _, ref := range []string{"/musicarchive", "/music.old"} {
		_, err := resolveLibraryRefs(context.Background(), testLibs(), "query", []string{ref})
		if !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Fatalf("%q: err = %v, want CodeNotFound", ref, err)
		}
		if strings.Contains(err.Error(), "inside root") {
			t.Errorf("%q is a sibling of /music, not a child, but got the containment hint: %v", ref, err)
		}
	}
}

// The op names the command the error came from, so `search --library bogus` does not
// report itself as a query error.
func TestResolveLibraryRefsCarriesTheCallersOp(t *testing.T) {
	for _, op := range []string{"query", "facet", "search", "diagnostics"} {
		_, err := resolveLibraryRefs(context.Background(), testLibs(), op, []string{"/nope"})
		if err == nil || !strings.HasPrefix(err.Error(), op+": ") {
			t.Errorf("op %q produced %v, want the message to lead with its own op", op, err)
		}
	}
}

// scopedQuery is the seam that keeps resolve and apply together. Dropping the apply
// would leave a --library that validates its input and then filters nothing.
func TestScopedQueryAppliesTheResolvedLibraries(t *testing.T) {
	q, err := scopedQuery(context.Background(), testLibs(), yearCmd(), "query", "",
		queryFlags{}, []string{"/music"})
	if err != nil {
		t.Fatalf("scopedQuery: %v", err)
	}
	c := condOf(t, q)
	if c.Field != "library" || c.Op != query.OpIn ||
		!reflect.DeepEqual(c.Values, []any{model.PID("lib-music")}) {
		t.Errorf("cond = %+v, want the resolved pid applied as `library in`", c)
	}
}

// A rule document owns the whole where-clause, so layering --library on top is refused
// rather than dropped. Silently ignoring it would answer a wider question than asked.
func TestScopedQueryRefusesLibraryWithARule(t *testing.T) {
	_, err := scopedQuery(context.Background(), testLibs(), yearCmd(), "query", "rule.json",
		queryFlags{}, []string{"/music"})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("err = %v, want CodeInvalid", err)
	}
	if !strings.Contains(err.Error(), "--rule") {
		t.Errorf("error %q does not name the conflicting flag", err)
	}
}

// TestBrowseKindDoesNotInheritYearFlag pins the collision that made
// `browse by-year --year 2000 --kind track` return an empty page. buildQuery keys its
// year filter off Changed("year"), right for query and facet where --year is a
// filter, wrong for browse where it is the by-year list's parameter. browse builds
// its kind filter directly instead.
func TestBrowseKindDoesNotInheritYearFlag(t *testing.T) {
	cmd := newBrowseCmd(&globals{})
	if err := cmd.Flags().Set("year", "2000"); err != nil {
		t.Fatalf("set --year: %v", err)
	}
	if err := cmd.Flags().Set("kind", "track"); err != nil {
		t.Fatalf("set --kind: %v", err)
	}
	// The shape browse must not use: with --year changed, buildQuery folds in a year
	// predicate from the zero queryFlags.
	q, err := buildQuery(cmd, "", queryFlags{kind: "track"})
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}
	rule, err := query.MarshalRule(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(rule), `"year"`) {
		t.Fatalf("buildQuery no longer folds in the year flag; browse may be able to "+
			"share it again, so re-check browse.go's comment: %s", rule)
	}

	// What browse compiles instead: the kind alone.
	direct, err := query.MarshalRule(query.New(query.EntityItems).
		Where("kind", query.OpIs, "track").Build())
	if err != nil {
		t.Fatalf("marshal direct: %v", err)
	}
	if strings.Contains(string(direct), `"year"`) {
		t.Errorf("browse's kind filter carries a year predicate: %s", direct)
	}
}
