package waxbin

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// TestAuditProbeDecodesUnparsedContainers: the corrupt-audio probe was a WaxLabel
// parse, and WaxLabel reports success for a container it has no parser for, so a
// bit-rotted WavPack sailed through the one check meant to catch it. Decoding is
// the only read of such a file that can fail on damage.
//
// The two damage forms are chosen from what WaxFlow's tolerant demuxers actually
// report. Rot inside a WavPack block fails that block's CRC; a truncated Monkey's
// Audio frame runs out of coded data. A WavPack file simply cut short is not here
// on purpose: its last partial block is discarded and the rest decodes clean, so
// nothing detects it and the probe must not be sold as if it did.
func TestAuditProbeDecodesUnparsedContainers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const rate = 8000
	sig := testaudio.ReferenceSignal(rate, 4*time.Second)

	wv := testaudio.EncodeAs(t, "wavpack", "", rate, sig)
	rotted := append([]byte(nil), wv...)
	for i := len(rotted) / 2; i < len(rotted)/2+64; i++ {
		rotted[i] ^= 0xFF
	}
	ape := testaudio.EncodeAs(t, "ape", "", rate, sig)

	files := []struct {
		name    string
		data    []byte
		corrupt bool
	}{
		{"good.wv", wv, false},
		{"rotted.wv", rotted, true},
		{"good.ape", ape, false},
		{"cut.ape", ape[:len(ape)*60/100], true},
	}

	// The old probe was the WaxLabel read on its own, and it passes every one of
	// these. That is the blind spot: it cannot judge a container it has no parser for.
	reader := meta.NewReader()
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		writeRaw(t, p, f.data)
		fm, err := reader.Read(ctx, p)
		if err != nil {
			t.Fatalf("parse of %s: %v", f.name, err)
		}
		if !hasDiag(fm.Diagnostics, model.DiagUnsupportedFormat) {
			t.Fatalf("%s parsed after all; this test needs a container WaxLabel does not read", f.name)
		}
	}

	probe := auditProbe(reader, decode.New(slog.New(slog.DiscardHandler)))
	for _, f := range files {
		err := probe(ctx, filepath.Join(dir, f.name))
		if f.corrupt && err == nil {
			t.Errorf("%s passed the corrupt-audio probe", f.name)
		}
		if !f.corrupt && err != nil {
			t.Errorf("intact %s failed the corrupt-audio probe: %v", f.name, err)
		}
	}
}

// TestAuditProbeIgnoresUndecodableInput: a container neither the parser nor the
// decoder covers says nothing about the bytes, so the probe must not call it
// corrupt. Only a file that opens and then fails is damaged.
func TestAuditProbeIgnoresUndecodableInput(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "mystery.wv")
	writeRaw(t, p, []byte("no container here, just bytes nothing can open"))

	probe := auditProbe(meta.NewReader(), decode.New(slog.New(slog.DiscardHandler)))
	if err := probe(ctx, p); err != nil {
		t.Errorf("an unreadable container is not evidence of corruption: %v", err)
	}
}

// TestScanFillsPropertiesForUnparsedContainers: a .wv used to catalog with a zero
// duration, sample rate, and bit depth, because no tag parser reads the container.
// Those zeroes blank the display, drop the file out of duration rollups, and rank a
// 24/96 lossless file below a 16/44.1 one in the upgrade scan. The scanner asks the
// decoder for the header instead, which decodes no PCM.
func TestScanFillsPropertiesForUnparsedContainers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := Open(ctx, Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer lib.Close()

	const rate = 44100
	writeRaw(t, filepath.Join(root, "a.wv"),
		testaudio.EncodeAs(t, "wavpack", "", rate, testaudio.ReferenceSignal(rate, 2*time.Second)))
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("query items: %v (n=%d)", err, len(items))
	}
	f, err := lib.store.FileByPID(ctx, items[0].FilePID)
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
	// The extension-derived codec label still stands; the probe deliberately does not
	// report one, so a WaxFlow codec ID can never leak into the catalog vocabulary.
	if f.Codec != "wavpack" {
		t.Errorf("codec = %q, want the extension-derived wavpack", f.Codec)
	}
}
