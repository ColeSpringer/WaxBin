package meta

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReadProjectsTags verifies the adapter projects WaxLabel's canonical tags
// and stream properties into model.Tags.
func TestReadProjectsTags(t *testing.T) {
	p := writeTemp(t, "song.mp3", testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 3))
	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	tg := fm.Tags
	if tg.Title != "Midnight Drive" || tg.Artist != "The Foobars" || tg.Album != "Night Moves" || tg.TrackNo != 3 {
		t.Fatalf("tag projection wrong: %+v", tg)
	}
	if tg.Codec != "mp3" {
		t.Errorf("codec = %q, want normalized lowercase mp3", tg.Codec)
	}
	if tg.DurationMS <= 0 || tg.SampleRate != 44100 {
		t.Errorf("audio properties not read: dur=%d rate=%d", tg.DurationMS, tg.SampleRate)
	}
	if fm.EssenceHash == "" {
		t.Error("essence hash empty for a valid MP3")
	}
}

// TestEssenceStableAcrossRetag verifies the essence hash is tag-independent: two
// files with identical audio but different tags hash the same, while different
// audio hashes differently.
func TestEssenceStableAcrossRetag(t *testing.T) {
	ctx := context.Background()
	audio := testaudio.DefaultAudio()

	a := writeTemp(t, "a.mp3", testaudio.BuildMP3WithAudio("Title A", "Artist A", "Album A", 1, audio))
	b := writeTemp(t, "b.mp3", testaudio.BuildMP3WithAudio("Totally Different", "Other", "Else", 9, audio))
	c := writeTemp(t, "c.mp3", testaudio.BuildMP3WithAudio("Title A", "Artist A", "Album A", 1, testaudio.AudioWithSeed(0x40)))

	read := func(path string) string {
		fm, err := NewReader().Read(ctx, path)
		if err != nil {
			t.Fatalf("Read %s: %v", path, err)
		}
		if fm.EssenceHash == "" {
			t.Fatalf("empty essence for %s", path)
		}
		return fm.EssenceHash
	}

	if read(a) != read(b) {
		t.Error("essence changed across a pure retag; it must be tag-independent")
	}
	if read(a) == read(c) {
		t.Error("essence matched despite different audio; it must depend on the audio")
	}
}

// TestReadMissingFile surfaces an I/O error rather than panicking.
func TestReadMissingFile(t *testing.T) {
	if _, err := NewReader().Read(context.Background(), filepath.Join(t.TempDir(), "nope.mp3")); err == nil {
		t.Fatal("expected an error reading a missing file")
	}
}

// TestReadToleratesUnsupportedFormat verifies a format WaxLabel cannot parse is
// still cataloged with a filename title and content-hash essence.
func TestReadToleratesUnsupportedFormat(t *testing.T) {
	p := writeTemp(t, "track.wma", []byte("not a recognized audio container, just bytes"))
	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read should tolerate an unsupported format, got %v", err)
	}
	if fm.Tags.Title != "track" {
		t.Errorf("title = %q, want the filename-derived 'track'", fm.Tags.Title)
	}
	if fm.EssenceHash != "" {
		t.Errorf("essence = %q, want empty so the scanner falls back to the content hash", fm.EssenceHash)
	}
	if fm.Tags.Codec != "wma" {
		t.Errorf("codec = %q, want the extension-derived 'wma' for display", fm.Tags.Codec)
	}
}

// TestReadWAVEssence verifies a pure-Go-decodable WAV gets a real essence hash
// and the lowercase pcm codec the analyze registry selects on.
func TestReadWAVEssence(t *testing.T) {
	wav := testaudio.EncodeWAV16(22050, testaudio.RichSignal(22050, 2, testaudio.MusicalPartials, 1))
	p := writeTemp(t, "tone.wav", wav)
	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if fm.Tags.Codec != "pcm" {
		t.Errorf("WAV codec = %q, want pcm", fm.Tags.Codec)
	}
	if fm.EssenceHash == "" {
		t.Error("WAV essence hash empty")
	}
}
