package organize

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	f := map[string]fieldVal{"narrator": {s: "Bob"}}
	if got := render(t, `< \{{narrator}\}>`, f); got != " {Bob}" {
		t.Fatalf("escaped braces: got %q want \" {Bob}\"", got)
	}
	if got := render(t, `a\\b`, nil); got != `a\b` {
		t.Fatalf("escaped backslash: got %q", got)
	}
}

func TestGrammarUnknownFieldErrors(t *testing.T) {
	t.Parallel()
	if _, err := renderTemplate("{nope}", map[string]fieldVal{}); err == nil {
		t.Fatal("unknown field should error")
	}
}

func TestGrammarUnbalancedErrors(t *testing.T) {
	t.Parallel()
	if _, err := renderTemplate("<{a}", map[string]fieldVal{"a": {s: "x"}}); err == nil {
		t.Fatal("unterminated group should error")
	}
	if _, err := renderTemplate("{a", map[string]fieldVal{"a": {s: "x"}}); err == nil {
		t.Fatal("unterminated brace should error")
	}
}

func TestNativeMusicTemplate(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	for folder, want := range map[string]string{"": "Some One", "Compilations": "Compilations"} {
		p.CompilationFolder = &folder
		if rel, err := RenderRelPath(p, item); err != nil || rel != filepath.Join(want, "Hits", "01 - Song.mp3") {
			t.Errorf("compilation folder %q: layout = %q (err %v), want it under %s", folder, rel, err, want)
		}
	}
	// Set empty, a compilation tagged with no album artist stays together under Various
	// Artists rather than scattering across its performers.
	empty := ""
	p.CompilationFolder = &empty
	untagged := *item
	untagged.AlbumArtist = ""
	if rel, err := RenderRelPath(p, &untagged); err != nil || rel != filepath.Join("Various Artists", "Hits", "01 - Song.mp3") {
		t.Errorf("a compilation with no album artist = %q (err %v), want it under Various Artists", rel, err)
	}
}

// TestProfileRefusesACompilationFolderThatNamesNoFolder: a compilation folder that folds to
// nothing would file compilations under Unknown Artist while the tag write stored the value
// itself, so it is refused; a blank one reads as empty, and spaces around a name are
// dropped from the folder and the tag alike.
func TestProfileRefusesACompilationFolderThatNamesNoFolder(t *testing.T) {
	t.Parallel()
	for _, folder := range []string{"..", " . "} {
		p := nativeProfile
		p.Name, p.CompilationFolder = "x", &folder
		if err := p.Validate(); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("compilation folder %q: Validate = %v, want CodeInvalid", folder, err)
		}
	}
	item := &model.ItemView{AlbumArtist: "Some One", Album: "Hits", TrackNo: 1, Title: "Song", Compilation: true, DisplayPath: "/in/x.mp3"}
	for folder, want := range map[string]string{"   ": "Some One", " Comps ": "Comps"} {
		p := nativeProfile
		p.CompilationFolder = &folder
		if err := p.Validate(); err != nil {
			t.Errorf("compilation folder %q: Validate = %v", folder, err)
		}
		if rel, err := RenderRelPath(p, item); err != nil || rel != filepath.Join(want, "Hits", "01 - Song.mp3") || p.Compilations() != strings.TrimSpace(folder) {
			t.Errorf("compilation folder %q: layout = %q (err %v), Compilations %q; want it under %s", folder, rel, err, p.Compilations(), want)
		}
	}
}

// TestProfileSetKeepsACompilationFolder: a custom profile's compilation folder, the empty
// one included, survives the merge, and one that names none inherits the built-in's.
func TestProfileSetKeepsACompilationFolder(t *testing.T) {
	t.Parallel()
	empty := ""
	set, err := NewProfileSet([]Profile{{Name: "by-artist", CompilationFolder: &empty}, {Name: "waxbin-native", TagWrite: true}})
	if err != nil {
		t.Fatal(err)
	}
	item := &model.ItemView{AlbumArtist: "Some One", Album: "Hits", TrackNo: 1, Title: "Song", Compilation: true, DisplayPath: "/in/x.mp3"}
	for name, want := range map[string]string{"by-artist": "Some One", "waxbin-native": "Various Artists"} {
		p, err := set.ByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if rel, err := RenderRelPath(p, item); err != nil || !strings.HasPrefix(rel, want+string(filepath.Separator)) {
			t.Errorf("%s: layout = %q (err %v), want it under %s", name, rel, err, want)
		}
	}
	empty = "changed"
	p, _ := set.ByName("by-artist")
	if p.CompilationFolder == nil || *p.CompilationFolder != "" {
		t.Errorf("the set shares the caller's folder: %v", p.CompilationFolder)
	}
	*p.CompilationFolder = "written through ByName"
	for _, q := range set.All() {
		if q.Name == "by-artist" {
			*q.CompilationFolder = "written through All"
		}
	}
	if p, _ := set.ByName("by-artist"); *p.CompilationFolder != "" {
		t.Errorf("a profile handed out shares the set's folder: %q", *p.CompilationFolder)
	}
}

func TestProfileSetCustomOverride(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	if _, err := NewProfileSet([]Profile{{Name: "bad", Music: "{unknownfield}"}}); err == nil {
		t.Fatal("a template with an unknown field should be rejected at load")
	}
	if _, err := NewProfileSet([]Profile{{Name: "bad", Music: "<{title}"}}); err == nil {
		t.Fatal("an unbalanced group should be rejected at load")
	}
}

// TestProfileValidate: a profile handed in whole (the facade's ad-hoc organize) needs
// a name and three clean templates, since nothing is inherited for it.
func TestProfileValidate(t *testing.T) {
	t.Parallel()
	good := nativeProfile
	good.Name = "adhoc"
	if err := good.Validate(); err != nil {
		t.Fatalf("a complete profile: %v", err)
	}
	for name, p := range map[string]Profile{
		"no name":          {Music: good.Music, Audiobook: good.Audiobook, Podcast: good.Podcast},
		"no podcast":       {Name: "x", Music: good.Music, Audiobook: good.Audiobook},
		"unknown field":    {Name: "x", Music: "{nope}", Audiobook: good.Audiobook, Podcast: good.Podcast},
		"unbalanced group": {Name: "x", Music: good.Music, Audiobook: "<{title}", Podcast: good.Podcast},
	} {
		if err := p.Validate(); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s: Validate = %v, want CodeInvalid", name, err)
		}
	}
}

// TestProfileSetAll: every profile the set resolves, merged as ByName returns it, in
// name order.
func TestProfileSetAll(t *testing.T) {
	t.Parallel()
	set, err := NewProfileSet([]Profile{{Name: "zeta", Music: "{title}.{ext}"}, {Name: "alpha", Podcast: "{episode}.{ext}"}})
	if err != nil {
		t.Fatal(err)
	}
	all := set.All()
	var names []string
	for _, p := range all {
		names = append(names, p.Name)
	}
	if want := []string{"alpha", "waxbin-native", "zeta"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if all[2].Music != "{title}.{ext}" || all[2].Audiobook != nativeProfile.Audiobook {
		t.Errorf("zeta = %+v, want its music template and the inherited rest", all[2])
	}
}
