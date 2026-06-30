package organize

import (
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

func render(t *testing.T, tmpl string, fields map[string]fieldVal) string {
	t.Helper()
	out, err := renderTemplate(tmpl, fields)
	if err != nil {
		t.Fatalf("renderTemplate(%q): %v", tmpl, err)
	}
	return out
}

func TestGrammarOptionalField(t *testing.T) {
	f := map[string]fieldVal{"disc": {n: 0, isNum: true}, "track": {n: 3, isNum: true}}
	if got := render(t, "{disc?}{track:02}", f); got != "03" {
		t.Fatalf("empty optional should drop: got %q want 03", got)
	}
	f["disc"] = fieldVal{n: 2, isNum: true}
	if got := render(t, "{disc?}{track:02}", f); got != "203" {
		t.Fatalf("present optional should render: got %q want 203", got)
	}
}

func TestGrammarConditionalGroup(t *testing.T) {
	withYear := map[string]fieldVal{"album": {s: "X"}, "year": {n: 2020, isNum: true}}
	if got := render(t, "{album}< ({year})>", withYear); got != "X (2020)" {
		t.Fatalf("group with value: got %q", got)
	}
	noYear := map[string]fieldVal{"album": {s: "X"}, "year": {n: 0, isNum: true}}
	if got := render(t, "{album}< ({year})>", noYear); got != "X" {
		t.Fatalf("group without value should drop: got %q", got)
	}
}

func TestGrammarGroupKeepsLiteralAroundField(t *testing.T) {
	f := map[string]fieldVal{"disc": {n: 2, isNum: true}, "track": {n: 1, isNum: true}}
	if got := render(t, "<{disc}->{track:02}", f); got != "2-01" {
		t.Fatalf("disc prefix present: got %q want 2-01", got)
	}
	f["disc"] = fieldVal{n: 0, isNum: true}
	if got := render(t, "<{disc}->{track:02}", f); got != "01" {
		t.Fatalf("disc prefix absent should drop separator too: got %q want 01", got)
	}
}

func TestGrammarNestedGroups(t *testing.T) {
	// Outer group survives if any inner field has a value.
	f := map[string]fieldVal{"a": {s: ""}, "b": {s: "B"}}
	if got := render(t, "<x<{a}><{b}>y>", f); got != "xBy" {
		t.Fatalf("nested group: got %q want xBy", got)
	}
	empty := map[string]fieldVal{"a": {s: ""}, "b": {s: ""}}
	if got := render(t, "<x<{a}><{b}>y>", empty); got != "" {
		t.Fatalf("all-empty nested group should drop wholly: got %q", got)
	}
}

func TestGrammarEscapes(t *testing.T) {
	f := map[string]fieldVal{"narrator": {s: "Bob"}}
	if got := render(t, `< \{{narrator}\}>`, f); got != " {Bob}" {
		t.Fatalf("escaped braces: got %q want \" {Bob}\"", got)
	}
	if got := render(t, `a\\b`, nil); got != `a\b` {
		t.Fatalf("escaped backslash: got %q", got)
	}
}

func TestGrammarUnknownFieldErrors(t *testing.T) {
	if _, err := renderTemplate("{nope}", map[string]fieldVal{}); err == nil {
		t.Fatal("unknown field should error")
	}
}

func TestGrammarUnbalancedErrors(t *testing.T) {
	if _, err := renderTemplate("<{a}", map[string]fieldVal{"a": {s: "x"}}); err == nil {
		t.Fatal("unterminated group should error")
	}
	if _, err := renderTemplate("{a", map[string]fieldVal{"a": {s: "x"}}); err == nil {
		t.Fatal("unterminated brace should error")
	}
}

func TestNativeMusicTemplate(t *testing.T) {
	p, _ := ProfileByName("waxbin-native")
	item := &model.ItemView{
		AlbumArtist: "Pink Floyd", Album: "The Wall", Year: 1979,
		DiscNo: 2, TrackNo: 5, Title: "Hey You", DisplayPath: "/in/x.flac",
	}
	rel, err := RenderRelPath(p, item)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("Pink Floyd", "The Wall (1979)", "2-05 - Hey You.flac")
	if rel != want {
		t.Fatalf("native music layout = %q, want %q", rel, want)
	}
}

func TestNativeMusicTemplateCompilation(t *testing.T) {
	p, _ := ProfileByName("waxbin-native")
	item := &model.ItemView{
		Artist: "Some One", AlbumArtist: "Some One", Album: "Hits",
		TrackNo: 1, Title: "Song", Compilation: true, DisplayPath: "/in/x.mp3",
	}
	rel, err := RenderRelPath(p, item)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("Various Artists", "Hits", "01 - Song.mp3")
	if rel != want {
		t.Fatalf("compilation layout = %q, want %q", rel, want)
	}
}

func TestProfileSetCustomOverride(t *testing.T) {
	set, err := NewProfileSet([]Profile{{Name: "flat", Music: "{title}.{ext}"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := set.ByName("flat")
	if err != nil {
		t.Fatal(err)
	}
	// An unspecified field inherits the built-in's template.
	if p.Audiobook == "" {
		t.Fatal("custom profile should inherit built-in audiobook template")
	}
	rel, err := RenderRelPath(p, &model.ItemView{Title: "T", DisplayPath: "x.mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if rel != "T.mp3" {
		t.Fatalf("custom flat layout = %q, want T.mp3", rel)
	}
}

func TestProfileSetRejectsBadTemplate(t *testing.T) {
	if _, err := NewProfileSet([]Profile{{Name: "bad", Music: "{unknownfield}"}}); err == nil {
		t.Fatal("a template with an unknown field should be rejected at load")
	}
	if _, err := NewProfileSet([]Profile{{Name: "bad", Music: "<{title}"}}); err == nil {
		t.Fatal("an unbalanced group should be rejected at load")
	}
}
