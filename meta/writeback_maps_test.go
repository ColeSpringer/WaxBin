package meta

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	waxlabel "github.com/colespringer/waxlabel"
)

// tinyPNG returns a small valid PNG for cover-embed tests.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

// TestBookFieldTagKeys pins the book field-to-tag-key map an audiobook write-back uses.
// A book maps the same catalog fields to DIFFERENT tags than a track (title→ALBUM,
// author→ALBUMARTIST), so the two maps must stay distinct.
func TestBookFieldTagKeys(t *testing.T) {
	want := map[string][]string{
		"title":       {"ALBUM"},
		"author":      {"ALBUMARTIST"},
		"narrator":    {"NARRATOR", "COMPOSER"},
		"genre":       {"GENRE"},
		"year":        {"DATE"},
		"subtitle":    {"SUBTITLE"},
		"edition":     {"EDITION"},
		"description": {"DESCRIPTION"},
	}
	for field, wantKeys := range want {
		got, ok := BookFieldTagKeys(field)
		if !ok {
			t.Errorf("BookFieldTagKeys(%q): want a mapping", field)
			continue
		}
		if len(got) != len(wantKeys) {
			t.Errorf("BookFieldTagKeys(%q) = %v, want %v", field, got, wantKeys)
			continue
		}
		for i := range got {
			if got[i] != wantKeys[i] {
				t.Errorf("BookFieldTagKeys(%q)[%d] = %q, want %q", field, i, got[i], wantKeys[i])
			}
		}
	}
	// A book's title is ALBUM, not the track's TITLE. The two maps must not collude.
	if k, _ := BookFieldTagKeys("title"); k[0] == "TITLE" {
		t.Error("book title must map to ALBUM, not TITLE")
	}
	// series alone has no key here: it packs a name and a sequence into one GROUPING
	// value through BookSeriesTagKey. Every other book field round-trips; subtitle,
	// edition, and description were the last to gain a key applyBookFields reads back.
	if _, ok := BookFieldTagKeys("series"); ok {
		t.Error("BookFieldTagKeys(series): want no mapping (BookSeriesTagKey carries it)")
	}
	// The identifier trio must round-trip: every key written here is one
	// applyBookFields reads back, or a rescan silently clears the value.
	for field, want := range map[string]string{
		"asin": "ASIN", "isbn": "ISBN", "publisher": "LABEL", "mbid": "MUSICBRAINZ_ALBUMID"} {
		got, ok := BookFieldTagKeys(field)
		if !ok || len(got) != 1 || got[0] != want {
			t.Errorf("BookFieldTagKeys(%q) = %v (ok=%v), want [%s]", field, got, ok, want)
		}
	}
}

// TestEntityFieldTagKey pins the entity-curation field fan-out keys, per entity type.
func TestEntityFieldTagKey(t *testing.T) {
	cases := []struct {
		et    model.MergeEntity
		field string
		want  string
		ok    bool
	}{
		{model.MergeAlbum, "sort", "ALBUMSORT", true},
		{model.MergeAlbum, "barcode", "BARCODE", true},
		{model.MergeAlbum, "label", "LABEL", true},
		{model.MergeAlbum, "catalog_number", "CATALOGNUMBER", true},
		// Country fans because every disc of a release shares one and WaxLabel v1.3.0
		// made RELEASECOUNTRY canonical; media does not, being a per-medium tag the
		// fan-out would stamp over a DVD medium's tracks.
		{model.MergeAlbum, "country", "RELEASECOUNTRY", true},
		{model.MergeAlbum, "media", "", false},
		{model.MergeArtist, "sort", "ARTISTSORT", true},
		// An entity MBID (album OR artist) stays DB-only: fanning it to member files would
		// re-key the entity on rescan (its match_key column is not updated by the edit). A
		// release-group field and a release-group type also stay DB-only.
		{model.MergeAlbum, "mbid", "", false},
		{model.MergeArtist, "mbid", "", false},
		{model.MergeReleaseGroup, "sort", "", false},
		{model.MergeReleaseGroup, "type", "", false},
	}
	for _, c := range cases {
		got, ok := EntityFieldTagKey(c.et, c.field)
		if got != c.want || ok != c.ok {
			t.Errorf("EntityFieldTagKey(%s, %q) = %q, %v; want %q, %v", c.et, c.field, got, ok, c.want, c.ok)
		}
	}
}

// TestPackSeriesGroupingRoundTrip checks PackSeriesGrouping is the inverse of the
// scanner's parseSeries, so a series+sequence written to GROUPING reads back unchanged.
func TestPackSeriesGroupingRoundTrip(t *testing.T) {
	cases := []struct{ name, seq string }{
		{"Foundation", "2"},
		{"Foundation", "1.5"},
		{"Middle-earth", ""},
		{"Area 51", ""},  // a name ending in a number, no sequence
		{"Area 51", "3"}, // a name ending in a number, with a sequence
	}
	for _, c := range cases {
		packed := PackSeriesGrouping(c.name, c.seq)
		gotName, gotSeq := parseSeries(packed)
		if gotName != c.name || gotSeq != c.seq {
			t.Errorf("PackSeriesGrouping(%q,%q)=%q -> parseSeries=(%q,%q), want (%q,%q)",
				c.name, c.seq, packed, gotName, gotSeq, c.name, c.seq)
		}
	}
	// An empty name clears the tag.
	if got := PackSeriesGrouping("", "5"); got != "" {
		t.Errorf("PackSeriesGrouping(empty name) = %q, want empty", got)
	}
}

// TestApplyPictureRoundTrip embeds a front cover and reads it back, and confirms a
// re-embed of identical bytes is a no-op and the audio essence is preserved.
func TestApplyPictureRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "song.mp3")
	if err := os.WriteFile(path, testaudio.BuildMP3("Song", "Artist", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewReader()
	before, err := r.Read(ctx, path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	cover := tinyPNG(t)
	w := NewWriter()
	res, err := w.ApplyPicture(ctx, path, PictureEdit{Data: cover})
	if err != nil {
		t.Fatalf("apply picture: %v", err)
	}
	if !res.Changed || res.ContentHash == "" {
		t.Fatalf("picture write result = %+v, want changed", res)
	}

	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	pics := doc.Pictures()
	if len(pics) != 1 || pics[0].Type != waxlabel.PicFrontCover || len(pics[0].Data) == 0 {
		t.Fatalf("embedded pictures = %+v, want one front cover", pics)
	}

	// The tag/picture write must not alter the audio essence.
	after, err := r.Read(ctx, path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if before.EssenceHash == "" || after.EssenceHash != before.EssenceHash {
		t.Errorf("essence changed by picture write: before=%s after=%s", before.EssenceHash, after.EssenceHash)
	}

	// Re-embedding the same bytes is a no-op.
	res2, err := w.ApplyPicture(ctx, path, PictureEdit{Data: cover})
	if err != nil {
		t.Fatalf("apply picture no-op: %v", err)
	}
	if res2.Changed {
		t.Error("identical re-embed reported Changed=true")
	}

	// Embed a second, non-front picture directly (file now has a front + a back cover),
	// then clear via ApplyPicture: the clear must remove ONLY the front cover.
	doc3, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		t.Fatalf("reparse for back cover: %v", err)
	}
	plan, err := doc3.Edit().
		AddPicture(waxlabel.Picture{Type: waxlabel.PicBackCover, Data: tinyPNG(t)}).
		Prepare(waxlabel.WithVerifyEssence())
	if err != nil {
		t.Fatalf("prepare back cover: %v", err)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatalf("embed back cover: %v", err)
	}
	if _, err := w.ApplyPicture(ctx, path, PictureEdit{Clear: true}); err != nil {
		t.Fatalf("clear picture: %v", err)
	}
	pics2 := mustPictures(t, ctx, path)
	if len(pics2) != 1 || pics2[0].Type != waxlabel.PicBackCover {
		t.Errorf("pictures after front-cover clear = %+v, want only the back cover to survive", pics2)
	}
}

func mustPictures(t *testing.T, ctx context.Context, path string) []waxlabel.Picture {
	t.Helper()
	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		t.Fatalf("reparse %s: %v", path, err)
	}
	return doc.Pictures()
}
