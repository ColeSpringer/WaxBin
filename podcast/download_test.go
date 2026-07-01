package podcast

import (
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

func TestDownloadFilenameNoCollision(t *testing.T) {
	// Two episodes of one podcast whose enclosures share a basename (differ only by
	// path prefix) must map to distinct on-disk filenames.
	a := &model.Episode{PID: "01AAAA", EnclosureURL: "https://h/1/audio.mp3", EnclosureType: "audio/mpeg"}
	b := &model.Episode{PID: "01BBBB", EnclosureURL: "https://h/2/audio.mp3", EnclosureType: "audio/mpeg"}
	fa, fb := downloadFilename(a), downloadFilename(b)
	if fa == fb {
		t.Fatalf("distinct episodes produced the same filename: %q", fa)
	}
	if !strings.HasPrefix(fa, string(a.PID)+"-") || !strings.HasPrefix(fb, string(b.PID)+"-") {
		t.Fatalf("filenames not pid-prefixed: %q, %q", fa, fb)
	}
	// A query-only difference also stays distinct via the pid prefix, and the audio
	// extension is preserved.
	if !strings.HasSuffix(fa, ".mp3") {
		t.Fatalf("extension not preserved: %q", fa)
	}
}

func TestDownloadFilenameGuaranteesExtension(t *testing.T) {
	e := &model.Episode{PID: "01CCCC", EnclosureURL: "https://h/stream?id=9", EnclosureType: "audio/mp4"}
	name := downloadFilename(e)
	if !strings.HasSuffix(name, ".m4a") {
		t.Fatalf("expected .m4a from audio/mp4 type, got %q", name)
	}
}

func TestCapFilenamePreservesExtension(t *testing.T) {
	long := strings.Repeat("a", 300) + ".mp3"
	got := capFilename(long, 50)
	if len(got) > 50 {
		t.Fatalf("capFilename did not cap: len=%d", len(got))
	}
	if !strings.HasSuffix(got, ".mp3") {
		t.Fatalf("capFilename dropped the extension: %q", got)
	}
}
