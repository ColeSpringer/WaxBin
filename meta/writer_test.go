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
// TPE2, an MP4 name folded onto a MusicBrainz id) is refused untouched, a key the format
// drops without a word is reported as a lost write, and the same key on a FLAC, whose
// comments are free-form, is an ordinary custom tag.
func TestApplyCustomTagGuardsTheKey(t *testing.T) {
	ctx := context.Background()
	mp3 := writeTemp(t, "song.mp3", testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "T", Artist: "A", Album: "Al"}))
	m4a := writeTemp(t, "song.m4a", testaudio.Fixture(t, "sample.m4a"))
	flac := writeTemp(t, "song.flac", testaudio.EncodeAs(t, "flac", "", 8000, testaudio.ReferenceSignal(8000, time.Second)))
	w := NewWriter()

	for _, c := range []struct{ path, key string }{
		{mp3, "TPE2"}, {mp3, "TCOM"}, {m4a, "MUSICBRAINZ TRACK ID"},
	} {
		before, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.ApplyCustomTag(ctx, c.path, c.key, []string{"x"}); !waxerr.Is(err, waxerr.CodeUnsupported) || !errors.Is(err, ErrNotCustomTag) {
			t.Errorf("%s on %s = %v, want CodeUnsupported for a key the format writes as a field", c.key, filepath.Base(c.path), err)
		}
		if after, _ := os.ReadFile(c.path); !bytes.Equal(before, after) {
			t.Errorf("%s on %s rewrote the file", c.key, filepath.Base(c.path))
		}
	}

	res, err := w.ApplyCustomTag(ctx, mp3, "TPE1", []string{"x"})
	if err != nil || res.Changed || !slices.ContainsFunc(res.Warnings, func(wn model.TagWriteWarning) bool {
		return wn.Unrepresented && wn.Key == "TPE1"
	}) {
		t.Errorf("TPE1 on mp3 = %+v (err %v), want an unchanged file and the value reported lost", res, err)
	}

	for _, path := range []string{mp3, flac} {
		key := "MOOD"
		if path == flac {
			key = "TPE2"
		}
		res, err := w.ApplyCustomTag(ctx, path, key, []string{"calm"})
		if err != nil || !res.Changed || slices.ContainsFunc(res.Warnings, func(wn model.TagWriteWarning) bool { return wn.Unrepresented }) {
			t.Fatalf("%s on %s = %+v (err %v), want a clean write", key, filepath.Base(path), res, err)
		}
		fm, err := NewReader().Read(ctx, path)
		if err != nil || !slices.Equal(fm.Tags.Custom[key], []string{"calm"}) {
			t.Errorf("%s on %s reads back %v (err %v), want [calm]", key, filepath.Base(path), fm.Tags.Custom[key], err)
		}
	}
	if res, err := w.ApplyCustomTag(ctx, mp3, "ABSENT", nil); err != nil || res.Changed || len(res.Warnings) != 0 {
		t.Errorf("clearing an absent key = %+v (err %v), want a quiet no-op", res, err)
	}
}

// TestJudgeCustomTagReadsTheChanges: the values landing under another tag key while the
// requested one did not take them is the format's own field (permanent for the file);
// another key changing beside a key that landed is a side effect, refused for itself; a
// picture or chapter delta is no tag key and the write goes ahead.
func TestJudgeCustomTagReadsTheChanges(t *testing.T) {
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
