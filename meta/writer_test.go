package meta

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/colespringer/waxlabel/tag"
)

// TestTagKeyForField pins the canonical field-to-tag-key map that both the organize
// tag-write and the catalog field-edit write-back share, so the two never drift.
func TestTagKeyForField(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"title": "TITLE", "artist": "ARTIST", "album": "ALBUM", "album_artist": "ALBUMARTIST",
		"composer": "COMPOSER", "comment": "COMMENT", "genre": "GENRE", "year": "DATE",
		"track_no": "TRACKNUMBER", "disc_no": "DISCNUMBER",
		"track_total": "TRACKTOTAL", "disc_total": "DISCTOTAL",
	}
	for field, wantKey := range want {
		if got, ok := TagKeyForField(field); !ok || got != wantKey {
			t.Errorf("TagKeyForField(%q) = %q, %v; want %q, true", field, got, ok, wantKey)
		}
	}
	// A field with no on-disk tag correspondence (a book-only field, or a bogus name).
	for _, f := range []string{"author", "narrator", "series", "subtitle", "nope"} {
		if _, ok := TagKeyForField(f); ok {
			t.Errorf("TagKeyForField(%q): want no mapping", f)
		}
	}
}

// TestWriterRoundTripPreservesEssence writes a tag and confirms it reads back while
// the audio essence hash is unchanged (a tag edit must never alter audio).
func TestWriterRoundTripPreservesEssence(t *testing.T) {
	t.Parallel()
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

	w := NewWriter()
	res, err := w.Apply(ctx, path, []TagEdit{
		{Key: "ALBUMARTIST", Values: []string{"Various Artists"}},
		{Key: "REPLAYGAIN_TRACK_GAIN", Values: []string{"-6.35 dB"}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Changed || res.ContentHash == "" || res.Size == 0 {
		t.Fatalf("write result = %+v, want changed with size+hash", res)
	}

	after, err := r.Read(ctx, path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if after.Tags.AlbumArtist != "Various Artists" {
		t.Errorf("albumArtist = %q, want Various Artists", after.Tags.AlbumArtist)
	}
	if before.EssenceHash == "" {
		t.Fatal("test fixture has no essence hash")
	}
	if after.EssenceHash != before.EssenceHash {
		t.Errorf("essence changed by tag write: before=%s after=%s", before.EssenceHash, after.EssenceHash)
	}

	// A second identical write is a no-op.
	res2, err := w.Apply(ctx, path, []TagEdit{{Key: "ALBUMARTIST", Values: []string{"Various Artists"}}})
	if err != nil {
		t.Fatalf("apply no-op: %v", err)
	}
	if res2.Changed {
		t.Error("identical re-write reported Changed=true")
	}
}

// TestApplyCustomTagGuardsTheKey: a custom tag is written only where it lands as itself. A
// file whose format would write the key onto a modelled field (an ID3 frame id such as
// TPE2, an MP4 name folded onto a MusicBrainz id) refuses it and is left untouched, a key the
// format drops without a word is reported as a lost write, and the same key on a FLAC, whose
// comments are free-form, is an ordinary custom tag.
func TestApplyCustomTagGuardsTheKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mp3 := writeTemp(t, "song.mp3", testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "T", Artist: "A", Album: "Al"}))
	m4a := writeTemp(t, "song.m4a", testaudio.Fixture(t, "sample.m4a"))
	flac := writeTemp(t, "song.flac", testaudio.EncodeAs(t, "flac", "", 8000, testaudio.ReferenceSignal(8000, time.Second)))
	w := NewWriter()
	one := func(path, key string, values []string) (*CustomTagsResult, error) {
		return w.ApplyCustomTags(ctx, path, []TagEdit{{Key: key, Values: values}})
	}

	for _, c := range []struct{ path, key string }{
		{mp3, "TPE2"}, {mp3, "TCOM"}, {m4a, "MUSICBRAINZ TRACK ID"},
	} {
		before, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		res, err := one(c.path, c.key, []string{"x"})
		if refused := res.Refused[c.key]; err != nil || !waxerr.Is(refused, waxerr.CodeUnsupported) || !errors.Is(refused, ErrNotCustomTag) {
			t.Errorf("%s on %s = %+v (err %v), want it refused as a field of the format's own", c.key, filepath.Base(c.path), res, err)
		}
		if after, _ := os.ReadFile(c.path); !bytes.Equal(before, after) {
			t.Errorf("%s on %s rewrote the file", c.key, filepath.Base(c.path))
		}
	}

	res, err := one(mp3, "TPE1", []string{"x"})
	if err != nil || res.Changed || len(res.Refused) != 0 || !slices.ContainsFunc(res.Warnings, func(wn model.TagWriteWarning) bool {
		return wn.Unrepresented && wn.Key == "TPE1"
	}) {
		t.Errorf("TPE1 on mp3 = %+v (err %v), want an unchanged file and the value reported lost", res, err)
	}

	for _, path := range []string{mp3, flac} {
		key := "MOOD"
		if path == flac {
			key = "TPE2"
		}
		res, err := one(path, key, []string{"calm"})
		if err != nil || !res.Changed || len(res.Refused) != 0 || slices.ContainsFunc(res.Warnings, func(wn model.TagWriteWarning) bool { return wn.Unrepresented }) {
			t.Fatalf("%s on %s = %+v (err %v), want a clean write", key, filepath.Base(path), res, err)
		}
		fm, err := NewReader().Read(ctx, path)
		if err != nil || !slices.Equal(fm.Tags.Custom[key], []string{"calm"}) {
			t.Errorf("%s on %s reads back %v (err %v), want [calm]", key, filepath.Base(path), fm.Tags.Custom[key], err)
		}
	}
	if res, err := one(mp3, "ABSENT", nil); err != nil || res.Changed || len(res.Warnings) != 0 || len(res.Refused) != 0 {
		t.Errorf("clearing an absent key = %+v (err %v), want a quiet no-op", res, err)
	}
}

// TestJudgeCustomTagReadsTheChanges: the values landing under another tag key while the
// requested one did not take them is the format's own field (permanent for the file);
// another key changing beside a key that landed is a side effect, refused for itself; a
// picture or chapter delta is no tag key and the write goes ahead.
func TestJudgeCustomTagReadsTheChanges(t *testing.T) {
	t.Parallel()
	x := []string{"x"}
	for _, c := range []struct {
		name       string
		values     []string
		held       []string
		changes    []tag.Change
		refused    bool
		notCustom  bool
		lostKeyRow bool
	}{
		{"landed alone", x, nil, []tag.Change{{Key: "MOOD", Kind: tag.ChangeAdded, New: x}}, false, false, false},
		{"landed beside a picture delta", x, nil, []tag.Change{
			{Key: "MOOD", Kind: tag.ChangeAdded, New: x}, {Key: "pictures", Kind: tag.ChangeChanged}}, false, false, false},
		{"written as another field", x, nil, []tag.Change{{Key: "ALBUMARTIST", Kind: tag.ChangeAdded, New: x}}, true, true, false},
		{"landed while another field changed", x, nil, []tag.Change{
			{Key: "MOOD", Kind: tag.ChangeAdded, New: x}, {Key: "ARTIST", Kind: tag.ChangeRemoved}}, true, false, false},
		{"a clear that would remove another field", nil, nil, []tag.Change{{Key: "ALBUMARTIST", Kind: tag.ChangeRemoved}}, true, false, false},
		{"dropped without a word", x, nil, nil, false, false, true},
		{"already held", x, x, nil, false, false, false},
	} {
		got, err := judgeCustomTag("op", "MOOD", c.values, c.held, c.changes, nil)
		if (err != nil) != c.refused || errors.Is(err, ErrNotCustomTag) != c.notCustom {
			t.Errorf("%s: err = %v, want refused %v, not custom %v", c.name, err, c.refused, c.notCustom)
		}
		lost := slices.ContainsFunc(got, func(wn model.TagWriteWarning) bool { return wn.Unrepresented && wn.Key == "MOOD" })
		if lost != c.lostKeyRow {
			t.Errorf("%s: lost warning %v, want %v", c.name, lost, c.lostKeyRow)
		}
	}
}

// TestApplyCustomTagsWritesThemTogether: keys the format takes as themselves go in with one
// rewrite, a clear among them, and the same keys again rewrite nothing; a file whose format
// writes one key as a field of its own still takes the others in one rewrite, naming the one
// it refused and leaving that field alone.
func TestApplyCustomTagsWritesThemTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mp3 := writeTemp(t, "song.mp3", testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "T", Artist: "A", Album: "Al"}))
	w := NewWriter()
	if _, err := w.ApplyCustomTags(ctx, mp3, []TagEdit{{Key: "OCCASION", Values: []string{"party"}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	read := func() map[string][]string {
		t.Helper()
		fm, err := NewReader().Read(ctx, mp3)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return fm.Tags.Custom
	}

	together := []TagEdit{{Key: "MOOD", Values: []string{"calm"}}, {Key: "STYLE", Values: []string{"lofi"}}, {Key: "OCCASION"}}
	res, err := w.ApplyCustomTags(ctx, mp3, together)
	if err != nil || !res.Changed || len(res.Refused) != 0 {
		t.Fatalf("together = %+v (err %v), want the file rewritten and nothing refused", res, err)
	}
	if c := read(); !slices.Equal(c["MOOD"], []string{"calm"}) || !slices.Equal(c["STYLE"], []string{"lofi"}) || c["OCCASION"] != nil {
		t.Errorf("custom tags = %v, want MOOD and STYLE set and OCCASION cleared", c)
	}
	if res, err := w.ApplyCustomTags(ctx, mp3, together); err != nil || res.Changed {
		t.Errorf("the same keys again = %+v (err %v), want no rewrite", res, err)
	}

	res, err = w.ApplyCustomTags(ctx, mp3, []TagEdit{
		{Key: "TPE2", Values: []string{"x"}}, {Key: "MOOD", Values: []string{"bright"}}, {Key: "GROOVE", Values: []string{"y"}},
	})
	if err != nil || !res.Changed || len(res.Refused) != 1 || !errors.Is(res.Refused["TPE2"], ErrNotCustomTag) {
		t.Fatalf("with a format's own key = %+v (err %v), want the others written and TPE2 refused as not custom", res, err)
	}
	fm, err := NewReader().Read(ctx, mp3)
	if err != nil || fm.Tags.AlbumArtist != "" || !slices.Equal(fm.Tags.Custom["MOOD"], []string{"bright"}) ||
		!slices.Equal(fm.Tags.Custom["GROOVE"], []string{"y"}) {
		t.Errorf("after the fallback: album artist %q, custom %v (err %v), want MOOD and GROOVE alone written",
			fm.Tags.AlbumArtist, fm.Tags.Custom, err)
	}

	if res, err := w.ApplyCustomTags(ctx, mp3, []TagEdit{{Key: "TCOM", Values: []string{"z"}}}); err != nil ||
		res.Changed || !errors.Is(res.Refused["TCOM"], ErrNotCustomTag) {
		t.Errorf("one refused key = %+v (err %v), want it refused with no rewrite", res, err)
	}
}

// TestJudgeCustomTagsReadsTheChanges: several keys landing as asked go ahead; any other
// key changing refuses the combined write, which the caller then makes a key at a time; a
// key the plan dropped without a word is reported lost.
func TestJudgeCustomTagsReadsTheChanges(t *testing.T) {
	t.Parallel()
	x, y := []string{"x"}, []string{"y"}
	edits := []TagEdit{{Key: "MOOD", Values: x}, {Key: "STYLE", Values: y}}
	for _, c := range []struct {
		name    string
		held    map[string][]string
		changes []tag.Change
		refused bool
		lost    []string
	}{
		{"both landed", nil, []tag.Change{{Key: "MOOD", New: x}, {Key: "STYLE", New: y}}, false, nil},
		{"one already held", map[string][]string{"STYLE": y}, []tag.Change{{Key: "MOOD", New: x}}, false, nil},
		{"another field changed", nil, []tag.Change{{Key: "MOOD", New: x}, {Key: "ALBUMARTIST", New: y}}, true, nil},
		{"beside a picture delta", nil, []tag.Change{{Key: "MOOD", New: x}, {Key: "STYLE", New: y}, {Key: "pictures"}}, false, nil},
		{"one dropped", nil, []tag.Change{{Key: "MOOD", New: x}}, false, []string{"STYLE"}},
	} {
		got, err := judgeCustomTags("op", edits, c.held, c.changes, nil)
		if (err != nil) != c.refused {
			t.Errorf("%s: err = %v, want refused %v", c.name, err, c.refused)
		}
		var lost []string
		for _, wn := range got {
			if wn.Unrepresented {
				lost = append(lost, wn.Key)
			}
		}
		if !slices.Equal(lost, c.lost) {
			t.Errorf("%s: lost %v, want %v", c.name, lost, c.lost)
		}
	}
}

// TestApplyCustomTagsIsolatesAValueTheWriterRefuses: a value the tag writer refuses to
// prepare (a NUL byte here) is refused alone while the other keys go in with one rewrite,
// and a file whose every key the writer refuses fails whole, as a lone such key does.
func TestApplyCustomTagsIsolatesAValueTheWriterRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mp3 := writeTemp(t, "song.mp3", testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "T", Artist: "A", Album: "Al"}))
	w := NewWriter()
	res, err := w.ApplyCustomTags(ctx, mp3, []TagEdit{
		{Key: "MOOD", Values: []string{"calm"}}, {Key: "STYLE", Values: []string{"a\x00b"}}, {Key: "GROOVE", Values: []string{"y"}},
	})
	if err != nil || !res.Changed || len(res.Refused) != 1 || res.Refused["STYLE"] == nil {
		t.Fatalf("with one bad value = %+v (err %v), want STYLE refused alone and the others written", res, err)
	}
	fm, err := NewReader().Read(ctx, mp3)
	if err != nil || !slices.Equal(fm.Tags.Custom["MOOD"], []string{"calm"}) || !slices.Equal(fm.Tags.Custom["GROOVE"], []string{"y"}) ||
		fm.Tags.Custom["STYLE"] != nil {
		t.Errorf("custom on disk = %v (err %v), want MOOD and GROOVE alone", fm.Tags.Custom, err)
	}
	for _, edits := range [][]TagEdit{
		{{Key: "STYLE", Values: []string{"a\x00b"}}},
		{{Key: "STYLE", Values: []string{"a\x00b"}}, {Key: "OTHER", Values: []string{"c\x00d"}}},
	} {
		if res, err := w.ApplyCustomTags(ctx, mp3, edits); err == nil {
			t.Errorf("%d bad keys = %+v, want the file to fail whole", len(edits), res)
		}
	}
}
