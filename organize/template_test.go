package organize_test

import (
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
)

// TestRenderRelPathSanitizesSeparatorsInFields ensures a path separator inside a
// metadata field does not create extra nested directories.
func TestRenderRelPathSanitizesSeparatorsInFields(t *testing.T) {
	p, err := organize.ProfileByName("waxbin-native")
	if err != nil {
		t.Fatal(err)
	}
	item := &model.ItemView{
		AlbumArtist: "AC/DC",
		Album:       `Live\Dead`,
		Title:       "Back: In/Black",
		TrackNo:     1,
		DisplayPath: "/incoming/orig.mp3",
	}
	rel, err := organize.RenderRelPath(p, item)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := filepath.Join("AC_DC", "Live_Dead", "01 - Back_ In_Black.mp3")
	if rel != want {
		t.Fatalf("rel = %q, want %q", rel, want)
	}
	// Exactly three components: artist / album / file; no separator leaked.
	if got := len(splitAll(rel)); got != 3 {
		t.Fatalf("path has %d components, want 3 (separator leaked): %q", got, rel)
	}
}

func TestRenderRelPathUsesUnknownBuckets(t *testing.T) {
	p, _ := organize.ProfileByName("waxbin-native")
	rel, err := organize.RenderRelPath(p, &model.ItemView{Title: "Solo", TrackNo: 0, DisplayPath: "x.flac"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := filepath.Join("Unknown Artist", "Unknown Album", "00 - Solo.flac")
	if rel != want {
		t.Fatalf("rel = %q, want %q", rel, want)
	}
}

func splitAll(p string) []string {
	var parts []string
	for {
		dir, file := filepath.Split(p)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == "" {
			break
		}
		p = filepath.Clean(dir)
		if p == "." || p == string(filepath.Separator) {
			break
		}
	}
	return parts
}

// TestRenderRelPathAudiobook renders a book through the native audiobook template,
// exercising the author/series/sequence/narrator/asin tokens and their optional
// groups.
func TestRenderRelPathAudiobook(t *testing.T) {
	p, err := organize.ProfileByName("waxbin-native")
	if err != nil {
		t.Fatal(err)
	}
	book := &model.ItemView{
		Kind:        model.KindBook,
		Title:       "The Way of Kings",
		Artist:      "Brandon Sanderson", // author maps onto Artist in the read view
		AuthorSort:  "sanderson, brandon",
		Series:      "Stormlight Archive",
		SeriesSeq:   "1",
		Narrator:    "Kate Reading",
		Subtitle:    "Book One",
		ASIN:        "B003ZWFB8C",
		Year:        2010,
		DisplayPath: "/incoming/kings.m4b",
	}
	rel, err := organize.RenderRelPath(p, book)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := filepath.Join(
		"sanderson, brandon", "Stormlight Archive",
		"1 - 2010 - The Way of Kings - Book One {Kate Reading} [B003ZWFB8C]",
		"The Way of Kings.m4b")
	if rel != want {
		t.Fatalf("audiobook rel =\n  %q\nwant\n  %q", rel, want)
	}
}

// TestRenderRelPathAudiobookSparse drops the optional series/narrator/asin groups
// when those fields are empty.
func TestRenderRelPathAudiobookSparse(t *testing.T) {
	p, _ := organize.ProfileByName("waxbin-native")
	book := &model.ItemView{
		Kind: model.KindBook, Title: "Standalone", Artist: "Solo Author",
		AuthorSort: "solo author", DisplayPath: "/in/x.m4b",
	}
	rel, err := organize.RenderRelPath(p, book)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := filepath.Join("solo author", "Standalone", "Standalone.m4b")
	if rel != want {
		t.Fatalf("sparse audiobook rel = %q, want %q", rel, want)
	}
}
