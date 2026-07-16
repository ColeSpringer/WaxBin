package testaudio

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
)

// ReferenceSignal returns a fixed, deterministic signal reused by every encoded
// fixture. A pure 440 Hz tone is too spectrally thin for a fingerprint, so this is
// a multi-tone signal with gentle movement (RichSignal's musical partials at a
// fixed seed): one signal serves loudness, peaks, and the cross-codec
// fingerprint-grouping tests alike.
func ReferenceSignal(rate int, dur time.Duration) []float32 {
	return RichSignal(rate, dur.Seconds(), MusicalPartials, 1)
}

// EncodeAs re-encodes samples (as a 16-bit WAV) into format via WaxFlow, returning
// the encoded container bytes. format is one of WaxFlow's output names: "wav",
// "aiff", "flac", "alac", "mp3", "aac", "opus", or "vorbis"; container overrides
// the format default where one exists ("adts" for a raw AAC elementary stream)
// and is empty for the default.
//
// It sets no GainDB, Channels, or Dynamics, so the transcode has no mix stage, no
// matrix, and no true-peak limiter, the same ChainSpec{Channels:1} trap the decode
// path avoids. Keep those zero; a nonzero one would silently reshape the fixture
// the loudness/peaks tests key on.
//
// The output goes to a temp file rather than an in-memory buffer because the AIFF
// and MP4 (aac/alac) muxers backpatch header sizes and so require a seekable
// destination; the bytes are read back and returned.
func EncodeAs(tb testing.TB, format, container_ string, rate int, samples []float32) []byte {
	tb.Helper()
	f, err := os.CreateTemp(tb.TempDir(), "wf-*")
	if err != nil {
		tb.Fatalf("EncodeAs temp: %v", err)
	}
	defer f.Close()
	src := container.BytesSource(EncodeWAV16(rate, samples))
	eng := waxflow.New(waxflow.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if _, err := eng.Transcode(context.Background(), src, "wav", f,
		waxflow.TranscodeOptions{Format: format, Container: container_}); err != nil {
		tb.Fatalf("EncodeAs(%q, %q): %v", format, container_, err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		tb.Fatalf("EncodeAs readback: %v", err)
	}
	return data
}
