package organize_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
)

// TestRenderRelPathSanitizesSeparatorsInFields ensures a path separator inside a
// metadata field does not create extra nested directories.
func TestRenderRelPathSanitizesSeparatorsInFields(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		"Brandon Sanderson", "Stormlight Archive",
		"1 - 2010 - The Way of Kings - Book One {Kate Reading} [B003ZWFB8C]",
		"The Way of Kings.m4b")
	if rel != want {
		t.Fatalf("audiobook rel =\n  %q\nwant\n  %q", rel, want)
	}
}

// TestRenderRelPathAudiobookSparse drops the optional series/narrator/asin groups
// when those fields are empty.
func TestRenderRelPathAudiobookSparse(t *testing.T) {
	t.Parallel()
	p, _ := organize.ProfileByName("waxbin-native")
	book := &model.ItemView{
		Kind: model.KindBook, Title: "Standalone", Artist: "Solo Author",
		AuthorSort: "solo author", DisplayPath: "/in/x.m4b",
	}
	rel, err := organize.RenderRelPath(p, book)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := filepath.Join("Solo Author", "Standalone", "Standalone.m4b")
	if rel != want {
		t.Fatalf("sparse audiobook rel = %q, want %q", rel, want)
	}
}

// TestBookPartRelPath: a part of a multi-file book is named after the book, its number
// padded to the digits of the book's last (two at least) and led by its disc, padded to the
// last disc's digits, when it has one; a title near the length limit gives way to the
// number.
func TestBookPartRelPath(t *testing.T) {
	t.Parallel()
	rel := filepath.Join("Author", "Book", "Book.mp3")
	p := func(disc, track int) organize.PartNumber { return organize.PartNumber{Disc: disc, Track: track} }
	for _, tc := range []struct {
		part, last organize.PartNumber
		ext        string
		want       string
	}{
		{p(0, 1), p(0, 2), ".mp3", "Book - 01.mp3"},
		{p(0, 7), p(0, 120), ".MP3", "Book - 007.mp3"},
		{p(1, 3), p(2, 3), ".m4b", "Book - 1-03.m4b"},
		{p(2, 12), p(2, 12), ".mp3", "Book - 2-12.mp3"},
		{p(3, 9), p(12, 9), ".mp3", "Book - 03-09.mp3"},
	} {
		if got, want := organize.BookPartRelPath(rel, tc.part, tc.last, tc.ext), filepath.Join("Author", "Book", tc.want); got != want {
			t.Errorf("BookPartRelPath(%+v, %+v, %q) = %q, want %q", tc.part, tc.last, tc.ext, got, want)
		}
	}

	long := filepath.Join("Author", strings.Repeat("x", 251)+".mp3")
	one := filepath.Base(organize.BookPartRelPath(long, p(0, 1), p(0, 2), ".mp3"))
	two := filepath.Base(organize.BookPartRelPath(long, p(0, 2), p(0, 2), ".mp3"))
	if !strings.HasSuffix(one, " - 01.mp3") || !strings.HasSuffix(two, " - 02.mp3") || len(one) > 255 {
		t.Errorf("long title parts = %q, %q, want numbered names within 255 bytes", one, two)
	}
}

// TestNumberParts: a book's parts keep the places their tags or names give them when
// every part has one of its own, padded to the highest place or the tagged part total, and
// are numbered in reading order otherwise.
func TestNumberParts(t *testing.T) {
	t.Parallel()
	p := func(disc, track int) organize.PartNumber { return organize.PartNumber{Disc: disc, Track: track} }
	for _, tc := range []struct {
		name   string
		places []organize.PartNumber
		total  int
		want   []organize.PartNumber
		last   organize.PartNumber
	}{
		{"places kept", []organize.PartNumber{p(0, 2), p(0, 3)}, 0, []organize.PartNumber{p(0, 2), p(0, 3)}, p(0, 3)},
		{"discs kept", []organize.PartNumber{p(1, 1), p(1, 2), p(2, 1)}, 0, []organize.PartNumber{p(1, 1), p(1, 2), p(2, 1)}, p(2, 2)},
		{"a part with no place", []organize.PartNumber{p(0, 0), p(0, 1), p(0, 2)}, 0, []organize.PartNumber{p(0, 1), p(0, 2), p(0, 3)}, p(0, 3)},
		{"two alike", []organize.PartNumber{p(0, 1), p(0, 1)}, 0, []organize.PartNumber{p(0, 1), p(0, 2)}, p(0, 2)},
		{"back matter", []organize.PartNumber{p(1, 1), p(1, 1000)}, 0, []organize.PartNumber{p(0, 1), p(0, 2)}, p(0, 2)},
		{"a high place pads wider", []organize.PartNumber{p(0, 1), p(0, 150)}, 0, []organize.PartNumber{p(0, 1), p(0, 150)}, p(0, 150)},
		{"a tagged total pads wider", []organize.PartNumber{p(0, 1), p(0, 2)}, 120, []organize.PartNumber{p(0, 1), p(0, 2)}, p(0, 120)},
		{"a tagged total leaves an index alone", []organize.PartNumber{p(0, 0), p(0, 2)}, 120, []organize.PartNumber{p(0, 1), p(0, 2)}, p(0, 2)},
	} {
		got, last := organize.NumberParts(tc.places, tc.total)
		if !slices.Equal(got, tc.want) || last != tc.last {
			t.Errorf("%s: NumberParts(%v, %d) = %v, %v; want %v, %v", tc.name, tc.places, tc.total, got, last, tc.want, tc.last)
		}
	}
}

// TestLonePart: a book's only part is numbered when its place, its disc or the part total
// it is tagged with says more parts are coming.
func TestLonePart(t *testing.T) {
	t.Parallel()
	p := func(disc, track int) organize.PartNumber { return organize.PartNumber{Disc: disc, Track: track} }
	for _, tc := range []struct {
		place organize.PartNumber
		total int
		want  bool
	}{
		{p(0, 1), 0, false}, {p(1, 1), 0, false}, {p(0, 0), 5, false}, {p(0, 1000), 0, false},
		{p(0, 3), 0, true}, {p(0, 1), 2, true}, {p(2, 1), 0, true},
	} {
		if _, got := organize.LonePart(tc.place, tc.total); got != tc.want {
			t.Errorf("LonePart(%+v, %d) = %v, want %v", tc.place, tc.total, got, tc.want)
		}
	}
}

// TestPartAt reads a stored part position back as the disc and place it encodes.
func TestPartAt(t *testing.T) {
	t.Parallel()
	for pos, want := range map[int]organize.PartNumber{
		0: {}, 7: {Track: 7}, 100003: {Disc: 1, Track: 3}, 201000: {Disc: 2, Track: 1000},
	} {
		if got := organize.PartAt(pos); got != want {
			t.Errorf("PartAt(%d) = %+v, want %+v", pos, got, want)
		}
	}
}

// TestAuthorSortStillRendersForACustomLayout: {authorsort} renders the collation key, for
// a custom layout that files books by it.
func TestAuthorSortStillRendersForACustomLayout(t *testing.T) {
	t.Parallel()
	p := organize.Profile{Name: "by-sort", Music: "{artist}/{title}.{ext}", Audiobook: "{authorsort}/{title}.{ext}", Podcast: "{podcast}/{episode}.{ext}"}
	rel, err := organize.RenderRelPath(p, &model.ItemView{Kind: model.KindBook, Title: "Standalone", Artist: "Édith Piaf", DisplayPath: "/in/x.m4b"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := filepath.Join("edith piaf", "Standalone.m4b"); rel != want {
		t.Errorf("custom authorsort rel = %q, want %q", rel, want)
	}
}

// TestOrderBookMoves: a part moves only after the part on its new name has moved off it,
// a part whose new name is its own in another case waits on nothing, and a cycle (two
// parts trading numbers) keeps its order, one move per part, for Execute to resolve.
func TestOrderBookMoves(t *testing.T) {
	t.Parallel()
	p := func(name string) string { return filepath.Join(string(filepath.Separator), "lib", "Book", name) }
	acts := []organize.Action{
		{Src: p("x - 05.mp3"), Dst: p("x - 03.mp3")},
		{Src: p("x - 01.mp3"), Dst: p("x - 02.mp3")},
		{Src: p("x - 02.mp3"), Dst: p("x - 01.mp3")},
		{Src: p("x - 03.mp3"), Dst: p("x - 04.mp3")},
		{Src: p("y - 01.mp3"), Dst: p("Y - 01.mp3")},
	}
	var got []string
	for _, a := range organize.OrderBookMoves(acts) {
		got = append(got, filepath.Base(a.Src)+">"+filepath.Base(a.Dst))
	}
	want := []string{"x - 03.mp3>x - 04.mp3", "y - 01.mp3>Y - 01.mp3", "x - 05.mp3>x - 03.mp3", "x - 01.mp3>x - 02.mp3", "x - 02.mp3>x - 01.mp3"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %q, want %q", got, want)
	}
}
