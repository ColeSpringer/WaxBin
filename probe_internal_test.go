package waxbin

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
)

// TestAuditProbeDecodesEveryContainer: the corrupt-audio probe was a WaxLabel parse,
// with a decode only for a container WaxLabel could not read. WaxLabel 1.6 reads
// WavPack and Monkey's Audio, and a parse reads headers and tag blocks, so a
// bit-rotted WavPack block and a Monkey's Audio file cut short both sail through it.
// Decoding is the only read that fails on the damage, so the probe decodes whatever
// the decoder opens, parsed or not.
//
// The two damage forms are chosen from what WaxFlow's tolerant demuxers actually
// report. Rot inside a WavPack block fails that block's CRC; a truncated Monkey's
// Audio frame runs out of coded data. A WavPack file simply cut short is not here on
// purpose: the parse takes the header's total on trust, and the demuxer notes the
// shortfall, drops the partial block, and decodes the rest clean, so nothing detects
// it and the probe must not be sold as if it did.
func TestAuditProbeDecodesEveryContainer(t *testing.T) {
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

	// The parse on its own passes every one of these. That is the blind spot the
	// decode closes, and it has to stay one for the test to mean anything.
	reader := meta.NewReader()
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		writeRaw(t, p, f.data)
		fm, err := reader.Read(ctx, p)
		if err != nil {
			t.Fatalf("parse of %s: %v", f.name, err)
		}
		if hasDiag(fm.Diagnostics, model.DiagUnsupportedFormat) {
			t.Fatalf("%s did not parse; this test needs a container WaxLabel reads", f.name)
		}
		if hasDiag(fm.Diagnostics, model.DiagCorruptAudio) {
			t.Fatalf("%s: the parse flagged it; this test needs damage only a decode finds", f.name)
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

// TestAuditProbeFailsOnParseTimeTruncation: a FLAC cut short is named by the parse,
// which walks the frame tail against STREAMINFO's declared total, whether or not the
// decoder objects to the partial last frame. The probe reads the parse's verdict as
// well as the decoder's.
func TestAuditProbeFailsOnParseTimeTruncation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const rate = 8000
	flac := testaudio.EncodeAs(t, "flac", "", rate, testaudio.ReferenceSignal(rate, 4*time.Second))
	good := filepath.Join(dir, "good.flac")
	cut := filepath.Join(dir, "cut.flac")
	writeRaw(t, good, flac)
	writeRaw(t, cut, flac[:len(flac)*60/100])

	probe := auditProbe(meta.NewReader(), decode.New(slog.New(slog.DiscardHandler)))
	if err := probe(ctx, good); err != nil {
		t.Errorf("intact flac failed the corrupt-audio probe: %v", err)
	}
	if err := probe(ctx, cut); err == nil {
		t.Error("a flac cut short passed the corrupt-audio probe")
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

// hasDiag reports whether the set carries a diagnostic with this code.
func hasDiag(diags []model.FileDiagnostic, code model.DiagnosticCode) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}
