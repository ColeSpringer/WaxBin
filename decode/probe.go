package decode

import (
	"context"
	"log/slog"
	"math"
	"sync"

	"github.com/colespringer/waxflow/audio"
)

// Properties are one file's stream properties as its container header declares
// them, in the units the catalog's file columns use.
//
// Codec and container are deliberately absent. Reporting them here would tie
// catalog codec labels to WaxFlow's codec IDs, the vocabulary sync Coverage
// refuses, and the caller already derives both from the file extension.
type Properties struct {
	DurationMS int64
	Bitrate    int // kbps, averaged over the whole file
	SampleRate int
	Channels   int
	BitDepth   int
}

// probeEngine backs Probe. Probe is a package function rather than a method
// because its caller holds no engine and needs none: a header read has nothing to
// configure and logs nothing.
var probeEngine = sync.OnceValue(func() *Engine { return New(slog.New(slog.DiscardHandler)) })

// Probe reads path's stream properties from its container header. It decodes no
// PCM. Opening parses the headers and wires up a decoder, and Probe reads what the
// headers declared and closes without ever asking for a chunk.
//
// It is here for files no tag parser covers, which otherwise catalog with a zero
// duration, sample rate, and bit depth. Those zeroes blank the display, drop the
// file out of duration rollups, and rank a 24/96 file below a 16/44.1 one in the
// upgrade scan. As of WaxLabel 1.6 every container WaxFlow decodes also parses, so
// this serves the next format the decoder learns first, as WavPack once was.
//
// ErrUnsupported means this build cannot decode the input either, so there is
// nothing to report.
func Probe(ctx context.Context, path string) (*Properties, error) {
	f, med, err := probeEngine().open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	defer med.Close()

	t := med.Info().Default()
	p := &Properties{SampleRate: t.Fmt.Rate, Channels: t.Fmt.Channels}
	// Bit depth is reported only where the file actually holds one. A codec that
	// decodes to float has no stored sample width, and Fmt.BitDepth is then the
	// pipeline's own 32, a number the file does not carry. SourceBitDepth wins where
	// it is set: a WavPack stream that stripped constant zero LSBs codes 20 bits.
	switch {
	case t.SourceBitDepth > 0:
		p.BitDepth = t.SourceBitDepth
	case t.Fmt.Type == audio.Int:
		p.BitDepth = t.Fmt.BitDepth
	}
	p.DurationMS = probeDuration(t.Samples, t.Fmt.Rate)
	// Whole-file size over the duration, so this covers the tags and any embedded
	// art too. It is the same approximation the tag readers make for a format that
	// declares no nominal rate. Bits per millisecond is already kbps.
	if st, serr := f.Stat(); serr == nil && p.DurationMS > 0 {
		p.Bitrate = int(st.Size() * 8 / p.DurationMS)
	}
	return p, nil
}

// maxProbeDurationMS bounds what a header may claim, thirty days. The sample
// count is untrusted input from an unparseable file, and an absurd claim would
// sum into every duration rollup and outlive the file in them.
const maxProbeDurationMS = 30 * 24 * 60 * 60 * 1000

// probeDuration converts a header-declared sample count to milliseconds,
// dropping claims that overflow the multiply or exceed the plausibility bound.
func probeDuration(samples int64, rate int) int64 {
	if samples <= 0 || rate <= 0 || samples > math.MaxInt64/1000 {
		return 0
	}
	d := samples * 1000 / int64(rate)
	if d > maxProbeDurationMS {
		return 0
	}
	return d
}
