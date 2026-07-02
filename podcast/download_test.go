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

func TestParseChapterDoc(t *testing.T) {
	// Out-of-order chapters are sorted by start; a negative-start entry is skipped
	// (never stored as a negative offset); Positions are contiguous after filtering.
	body := []byte(`{"chapters":[
		{"startTime":30.5,"title":"Two"},
		{"startTime":-5,"title":"Bad"},
		{"startTime":0,"title":"One"}
	]}`)
	chs, err := parseChapterDoc(body)
	if err != nil {
		t.Fatalf("parseChapterDoc: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("got %d chapters, want 2 (negative-start skipped)", len(chs))
	}
	if chs[0].Position != 0 || chs[0].Title != "One" || chs[0].FileStartMS != 0 {
		t.Errorf("chapter[0] = %+v, want pos0 One @0ms", chs[0])
	}
	if chs[1].Position != 1 || chs[1].Title != "Two" || chs[1].FileStartMS != 30500 {
		t.Errorf("chapter[1] = %+v, want pos1 Two @30500ms", chs[1])
	}

	// A decode error is surfaced (not swallowed).
	if _, err := parseChapterDoc([]byte("{not json")); err == nil {
		t.Error("expected a decode error for malformed JSON")
	}

	// An all-negative document yields no usable chapters.
	only, err := parseChapterDoc([]byte(`{"chapters":[{"startTime":-1,"title":"x"}]}`))
	if err != nil {
		t.Fatalf("parseChapterDoc all-negative: %v", err)
	}
	if len(only) != 0 {
		t.Errorf("all-negative doc yielded %d chapters, want 0", len(only))
	}
}
