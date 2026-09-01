package scan

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
	waxlabel "github.com/colespringer/waxlabel"
)

// TestAudioExtsTrackTagLibrary pins the scanner's extension set to what the tag
// library claims: every claimed extension is either scanned or named in excludedExts
// with its reason, and nothing is both. A format added upstream fails here until the
// scanner takes a position on it.
func TestAudioExtsTrackTagLibrary(t *testing.T) {
	claimed := map[string]bool{}
	for _, f := range waxlabel.Formats() {
		for _, ext := range waxlabel.ExtensionsFor(f) {
			claimed[ext] = true
			switch {
			case audioExts[ext] && excludedExts[ext]:
				t.Errorf("%q is both scanned and excluded", ext)
			case !audioExts[ext] && !excludedExts[ext]:
				t.Errorf("the tag library claims %q for %v and the scanner neither scans nor excludes it", ext, f)
			}
		}
	}
	for ext := range excludedExts {
		if !claimed[ext] {
			t.Errorf("excludedExts names %q, which the tag library does not claim", ext)
		}
	}
	for ext := range audioExts {
		if !claimed[ext] {
			t.Errorf("the scanner picks up %q, which the tag library does not claim", ext)
		}
	}
}

// unparsedReader stands in for a tag library with no parser for the file: the read
// comes back with the unsupported-format diagnostic and no stream properties, the
// shape that sends the scanner to the decoder's header probe. No such container
// exists today, since WaxLabel reads everything WaxFlow decodes, so the arm is
// exercised through this stand-in.
type unparsedReader struct{}

func (unparsedReader) Read(context.Context, string) (*meta.FileMeta, error) {
	return &meta.FileMeta{
		Tags: model.Tags{Title: "a", Container: "wavpack", Codec: "wavpack"},
		Diagnostics: []model.FileDiagnostic{{
			Code: model.DiagUnsupportedFormat, Severity: model.SeverityInfo, Detail: "no parser",
		}},
	}, nil
}

// TestScanProbesPropertiesForUnparsedContainers: a file no tag parser reads used to
// catalog with a zero duration, sample rate, and bit depth, which blanked the display,
// dropped it from duration rollups, and ranked it below any file with properties in
// the upgrade scan. The scanner asks the decoder for the header instead, which
// decodes no PCM.
func TestScanProbesPropertiesForUnparsedContainers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure lib: %v", err)
	}
	sc := New(st, unparsedReader{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const rate = 44100
	p := filepath.Join(root, "a.wv")
	if err := os.WriteFile(p, testaudio.EncodeAs(t, "wavpack", "", rate,
		testaudio.ReferenceSignal(rate, 2*time.Second)), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("query items: %v (n=%d)", err, len(items))
	}
	f, err := st.FileByPID(ctx, items[0].FilePID)
	if err != nil {
		t.Fatalf("file by pid: %v", err)
	}
	if f.SampleRate != rate || f.Channels != 1 || f.BitDepth != 16 {
		t.Errorf("file = {rate:%d channels:%d depth:%d}, want {%d 1 16} from the container header",
			f.SampleRate, f.Channels, f.BitDepth, rate)
	}
	if f.DurationMS < 1900 || f.DurationMS > 2100 {
		t.Errorf("durationMS = %d, want about 2000", f.DurationMS)
	}
	if f.Bitrate <= 0 {
		t.Errorf("bitrate = %d, want a real kbps figure derived from size over duration", f.Bitrate)
	}
	// The reader's label stands; the probe deliberately reports no codec, so a WaxFlow
	// codec ID can never leak into the catalog vocabulary.
	if f.Codec != "wavpack" {
		t.Errorf("codec = %q, want the reader's wavpack", f.Codec)
	}
}
