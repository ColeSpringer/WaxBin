package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
)

// copyFixture puts one of testaudio's checked-in fixtures into the library root as dst.
func copyFixture(t *testing.T, name, dst string) {
	t.Helper()
	if err := os.WriteFile(dst, testaudio.Fixture(t, name), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileOfItem(t *testing.T, st *sqlite.Store, title string) *model.File {
	t.Helper()
	ctx := context.Background()
	q := query.New(query.EntityItems).Where("title", query.OpIs, title).Build()
	items, err := st.QueryItems(ctx, q, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("find item %q: %v (n=%d)", title, err, len(items))
	}
	f, err := st.FileByPID(ctx, items[0].FilePID)
	if err != nil {
		t.Fatalf("file by pid: %v", err)
	}
	return f
}

// TestScanMusepack: both Musepack extensions scan, the SV7 frame stream and the SV8
// packet stream catalog under the "musepack" labels with the parse's stream
// properties, and an APEv2 write lands on either.
func TestScanMusepack(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	files := []struct {
		fixture, name string
		channels      int
	}{
		{"ref-2s-sv7.mpc", "seven.mpc", 2},
		{"ref-2s-sv8-chapters.mpc", "eight.mp+", 1},
	}
	for _, f := range files {
		p := filepath.Join(root, f.name)
		copyFixture(t, f.fixture, p)
		title := strings.TrimSuffix(f.name, filepath.Ext(f.name))
		if _, err := meta.NewWriter().Apply(ctx, p, []meta.TagEdit{
			{Key: "TITLE", Values: []string{title}}, {Key: "ARTIST", Values: []string{"Wax Test"}},
		}); err != nil {
			t.Fatalf("tag %s: %v", f.name, err)
		}
	}
	scanAll(t, sc, lib, false)
	for _, f := range files {
		file := fileOfItem(t, st, strings.TrimSuffix(f.name, filepath.Ext(f.name)))
		if file.Container != "musepack" || file.Codec != "musepack" {
			t.Errorf("%s: labels = %q/%q, want musepack/musepack", f.name, file.Container, file.Codec)
		}
		if file.SampleRate != 44100 || file.Channels != f.channels {
			t.Errorf("%s: rate/channels = %d/%d, want 44100/%d", f.name, file.SampleRate, file.Channels, f.channels)
		}
		if file.DurationMS < 1900 || file.DurationMS > 2100 {
			t.Errorf("%s: duration = %d ms, want about 2000", f.name, file.DurationMS)
		}
		if file.Bitrate <= 0 {
			t.Errorf("%s: bitrate = %d, want a real kbps figure", f.name, file.Bitrate)
		}
	}
}

// TestScanMusepackBookChapters: the SV8 chapter packets reach the catalog as a
// book's embedded chapters, in start order, each end taken from the next start.
func TestScanMusepackBookChapters(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	p := filepath.Join(root, "book.mpc")
	copyFixture(t, "ref-2s-sv8-chapters.mpc", p)
	if _, err := meta.NewWriter().Apply(context.Background(), p, []meta.TagEdit{
		{Key: "ALBUM", Values: []string{"Chaptered Book"}},
		{Key: "ALBUMARTIST", Values: []string{"An Author"}},
		{Key: "NARRATOR", Values: []string{"A Narrator"}},
	}); err != nil {
		t.Fatalf("tag: %v", err)
	}
	scanAll(t, sc, lib, false)
	assertChapters(t, st, currentItemPID(t, st, "Chaptered Book"), []model.Chapter{
		{Title: "Intro", StartMS: 0, EndMS: 750},
		{Title: "Middle", StartMS: 750, EndMS: 1500},
		{Title: "Coda", StartMS: 1500},
	})
}

// TestScanWMABookChapters: the fixture's ASF markers read as chapters. It carries no
// narrator credit, so the kind is forced the way an import of a known audiobook
// forces it.
func TestScanWMABookChapters(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	p := filepath.Join(root, "book.wma")
	copyFixture(t, "chapters.wma", p)
	if _, err := sc.ScanFileAs(context.Background(), lib, p, model.KindBook); err != nil {
		t.Fatalf("scan: %v", err)
	}
	assertChapters(t, st, currentItemPID(t, st, "Chaptered"), []model.Chapter{
		{Title: "Intro", StartMS: 0, EndMS: 500},
		{Title: "Mïddle", StartMS: 500, EndMS: 1250},
		{Title: "Coda", StartMS: 1250},
	})
}

// assertChapters checks titles and offsets. A zero want.EndMS marks the last
// chapter, whose end is the file's duration.
func assertChapters(t *testing.T, st *sqlite.Store, pid model.PID, want []model.Chapter) {
	t.Helper()
	chs, err := st.Chapters(context.Background(), pid)
	if err != nil {
		t.Fatalf("chapters: %v", err)
	}
	if len(chs) != len(want) {
		t.Fatalf("chapters = %+v, want %d", chs, len(want))
	}
	for i, w := range want {
		got := chs[i]
		if got.Title != w.Title || got.StartMS != w.StartMS {
			t.Errorf("chapter %d = %q at %d ms, want %q at %d ms", i, got.Title, got.StartMS, w.Title, w.StartMS)
		}
		switch {
		case w.EndMS != 0 && got.EndMS != w.EndMS:
			t.Errorf("chapter %d end = %d ms, want %d, the next chapter's start", i, got.EndMS, w.EndMS)
		case w.EndMS == 0 && got.EndMS <= w.StartMS:
			t.Errorf("last chapter end = %d ms, want the file's duration, past %d", got.EndMS, w.StartMS)
		}
	}
}
