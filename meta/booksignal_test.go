package meta

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
)

// TestBookSignal: the reader reports what a file's own tags say about it being a book,
// from the strongest evidence it holds: an .m4b name, an audiobook media type or a
// narrator credit, else an audiobook genre, which ID3's numeric genre 183 names too.
// Speech and Spoken Word name a skit or a comedy record as often as a book, so they
// signal nothing.
func TestBookSignal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, file string
		spec       testaudio.MP3Spec
		want       model.BookSignal
	}{
		{"plain", "a.mp3", testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Record", Genre: "Fiction"}, model.NoBookSignal},
		{"audiobook genre", "a.mp3", testaudio.MP3Spec{Title: "Song", Genre: "Audiobook"}, model.BookGenreSignal},
		{"numeric genre", "a.mp3", testaudio.MP3Spec{Title: "Song", Genre: "(183)"}, model.BookGenreSignal},
		{"spoken word", "a.mp3", testaudio.MP3Spec{Title: "Song", Genre: "Spoken-Word"}, model.NoBookSignal},
		{"speech", "a.mp3", testaudio.MP3Spec{Title: "Song", Genre: "speech"}, model.NoBookSignal},
		{"numeric speech", "a.mp3", testaudio.MP3Spec{Title: "Song", Genre: "(101)"}, model.NoBookSignal},
		{"two words", "a.mp3", testaudio.MP3Spec{Title: "Song", Genre: "Audio Book"}, model.BookGenreSignal},
		{"one genre of two", "a.mp3", testaudio.MP3Spec{Title: "Song", Genre: "Comedy; Audiobook"}, model.BookGenreSignal},
		{"narrator", "a.mp3", testaudio.MP3Spec{Title: "Song", TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}}, model.BookTagSignal},
		{"media type", "a.mp3", testaudio.MP3Spec{Title: "Song", TXXX: []testaudio.TXXXFrame{{Desc: "MEDIATYPE", Value: "2"}}}, model.BookTagSignal},
		{"m4b", "a.m4b", testaudio.MP3Spec{Title: "Song", Genre: "Speech"}, model.BookTagSignal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm, err := NewReader().Read(context.Background(), writeTemp(t, c.file, testaudio.BuildMP3FromSpec(c.spec)))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if fm.Tags.BookSignal != c.want {
				t.Errorf("BookSignal = %v, want %v", fm.Tags.BookSignal, c.want)
			}
		})
	}
}

// TestLegacyGenreByteIsNoBookSignal: an ID3v1 trailer's genre byte fills the genre, but a
// legacy container never changes a file's kind, so ID3v1's Audiobook (183) signals nothing.
func TestLegacyGenreByteIsNoBookSignal(t *testing.T) {
	t.Parallel()
	raw := testaudio.AppendID3v1(testaudio.DefaultAudio(), "V1 Title", "V1 Artist", "V1 Album")
	raw[len(raw)-1] = 183
	fm, err := NewReader().Read(context.Background(), writeTemp(t, "v1.mp3", raw))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if fm.Tags.Genre != "Audiobook" {
		t.Fatalf("genre = %q, want the ID3v1 byte read as Audiobook (fixture check)", fm.Tags.Genre)
	}
	if fm.Tags.BookSignal != model.NoBookSignal {
		t.Errorf("BookSignal = %v from a legacy genre byte, want none", fm.Tags.BookSignal)
	}
}

// TestPromoteBookFieldsOnAnUntaggedBook: the reader promotes nothing, so a file with no
// book signal keeps its book-owned keys as custom tags until a caller decides it is a book
// (a forced kind, an audiobook library), and the promotion then fills a narrator from the
// composer, the series from the grouping and the identifiers from their custom keys.
func TestPromoteBookFieldsOnAnUntaggedBook(t *testing.T) {
	t.Parallel()
	p := writeTemp(t, "part.mp3", testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Chapter One", Artist: "Author", Album: "Tome", Composer: "Reader", Label: "House",
		TXXX: []testaudio.TXXXFrame{{Desc: "ASIN", Value: "B00TOME"}, {Desc: "SUBTITLE", Value: "A Tale"}},
	}))
	ctx := context.Background()
	if _, err := NewWriter().Apply(ctx, p, []TagEdit{{Key: "GROUPING", Values: []string{"Saga #2"}}}); err != nil {
		t.Fatalf("stage grouping: %v", err)
	}
	fm, err := NewReader().Read(ctx, p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	tags := fm.Tags
	if tags.BookSignal != model.NoBookSignal {
		t.Fatalf("BookSignal = %v, want none (fixture check)", tags.BookSignal)
	}
	if len(tags.Narrators) != 0 || tags.ASIN != "" || tags.Series != "" || tags.Publisher != "" {
		t.Errorf("read promoted book fields: narrators %v, asin %q, series %q, publisher %q",
			tags.Narrators, tags.ASIN, tags.Series, tags.Publisher)
	}
	if got := tags.Custom["ASIN"]; len(got) != 1 || got[0] != "B00TOME" {
		t.Errorf("custom[ASIN] = %v, want it kept until the kind is known", got)
	}
	PromoteBookFields(&tags)
	if len(tags.Narrators) != 1 || tags.Narrators[0] != "Reader" {
		t.Errorf("narrators = %v, want the composer", tags.Narrators)
	}
	if tags.Series != "Saga" || tags.SeriesSeq != "2" {
		t.Errorf("series = %q #%q, want Saga #2 from the grouping", tags.Series, tags.SeriesSeq)
	}
	if tags.ASIN != "B00TOME" || tags.Subtitle != "A Tale" || tags.Publisher != "House" {
		t.Errorf("asin/subtitle/publisher = %q/%q/%q", tags.ASIN, tags.Subtitle, tags.Publisher)
	}
	for _, k := range []string{"ASIN", "SUBTITLE"} {
		if v, ok := tags.Custom[k]; ok {
			t.Errorf("custom[%s] = %v after promotion, want it moved to the typed field", k, v)
		}
	}
}

// TestPromoteBookFieldsTwice: a second promotion finds the custom keys the first moved and
// keeps the fields they filled, and the promotion leaves the map the caller read alone.
func TestPromoteBookFieldsTwice(t *testing.T) {
	t.Parallel()
	read := map[string][]string{"EDITION": {"Collector's"}, "SUBTITLE": {"A Tale"}, "ASIN": {"B00TOME"},
		"ISBN": {"9780306406157"}, "MOOD": {"calm"}}
	tags := model.Tags{Album: "Tome (Abridged)", Custom: read}
	PromoteBookFields(&tags)
	PromoteBookFields(&tags)
	if tags.Edition != "Collector's" || tags.Subtitle != "A Tale" || tags.ASIN != "B00TOME" || tags.ISBN != "9780306406157" {
		t.Errorf("edition/subtitle/asin/isbn = %q/%q/%q/%q after two promotions", tags.Edition, tags.Subtitle, tags.ASIN, tags.ISBN)
	}
	if tags.Abridged == nil || !*tags.Abridged {
		t.Errorf("abridged = %v, want the album's marker kept", tags.Abridged)
	}
	if len(read) != 5 {
		t.Errorf("caller's map = %v, want it untouched", read)
	}
	if _, ok := tags.Custom["ASIN"]; ok || len(tags.Custom["MOOD"]) != 1 {
		t.Errorf("custom = %v, want the book keys moved out and the rest kept", tags.Custom)
	}
}
