// Package decode is WaxBin's PCM-decoding layer. Scanning never imports this
// package; cataloging stays pure Go and reads only tags and essence hashes. The
// separate analyze pass is the only path that decodes PCM for fingerprinting and
// other audio-derived data.
//
// The pure-Go baseline decodes WAV. ffmpeg (a subprocess, not CGO) covers the
// formats without a pure-Go decoder yet (AAC/ALAC/Opus, and MP3/FLAC/Vorbis
// until their pure-Go decoders land). Decoders are selected by codec, pure-Go
// preferred, and capability-detected so a build without ffmpeg still catalogs
// every supported format, even when it cannot analyze all of them.
package decode

import (
	"context"
	"time"

	"github.com/colespringer/waxbin/internal/caps"
)

// PCM is decoded linear audio. Samples are interleaved float32 in [-1, 1] with
// length Frames()*Channels.
type PCM struct {
	SampleRate int
	Channels   int
	Samples    []float32
}

// Frames returns the number of multi-channel sample frames.
func (p *PCM) Frames() int {
	if p.Channels <= 0 {
		return 0
	}
	return len(p.Samples) / p.Channels
}

// DurationMS is the decoded length in milliseconds.
func (p *PCM) DurationMS() int64 {
	if p.SampleRate <= 0 {
		return 0
	}
	return int64(p.Frames()) * 1000 / int64(p.SampleRate)
}

// Mono returns a mono mixdown (channel average), the input most analysis wants.
func (p *PCM) Mono() []float32 {
	if p.Channels <= 1 {
		return p.Samples
	}
	out := make([]float32, p.Frames())
	for i := range out {
		var sum float32
		base := i * p.Channels
		for c := 0; c < p.Channels; c++ {
			sum += p.Samples[base+c]
		}
		out[i] = sum / float32(p.Channels)
	}
	return out
}

// Decoder decodes a file to PCM. max caps the decoded duration; 0 means the
// whole file. Name is the label shown in doctor's coverage table.
type Decoder interface {
	Decode(ctx context.Context, path string, max time.Duration) (*PCM, error)
	Name() string
}

// Registry resolves a Decoder by codec, preferring a pure-Go decoder over the
// ffmpeg fallback. It is immutable after construction (safe for concurrent use).
type Registry struct {
	byCodec map[string]Decoder
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byCodec: map[string]Decoder{}} }

// Register binds a codec to a decoder, replacing any prior binding.
func (r *Registry) Register(codec string, d Decoder) { r.byCodec[codec] = d }

// For returns the decoder for a codec, or false when none can decode it.
func (r *Registry) For(codec string) (Decoder, bool) {
	d, ok := r.byCodec[codec]
	return d, ok
}

// Codecs decoded by the built-in pure-Go layer.
var pureGoCodecs = map[string]bool{"pcm": true}

// ffmpegCodecs are the codecs ffmpeg covers when present (its WAV output feeds
// the same fingerprint as the pure-Go path). "aiff" is the container-keyed form
// of PCM-in-AIFF, which the pure-Go RIFF/WAVE decoder cannot read.
var ffmpegCodecs = []string{"mp3", "flac", "vorbis", "opus", "aac", "alac", "pcm", "aiff"}

// Default builds the registry for this host: the pure-Go WAV decoder always,
// plus the ffmpeg decoder for codecs lacking a pure-Go decoder when ffmpeg is on
// PATH. Pure-Go bindings win, so a codec with both uses pure-Go.
func Default() *Registry {
	r := NewRegistry()
	r.Register("pcm", &WAVDecoder{})
	if c := caps.Detect(); c.FFmpeg {
		ff := &FFmpegDecoder{Path: c.FFmpegPath}
		for _, codec := range ffmpegCodecs {
			if !pureGoCodecs[codec] { // never shadow a pure-Go decoder
				r.Register(codec, ff)
			}
		}
	}
	return r
}

// FormatSupport describes how this build decodes a codec for analysis. doctor
// renders it so users can see current coverage.
type FormatSupport struct {
	Codec    string
	Decoder  string // the registered decoder's Name(), or "none"
	Analysis bool
}

// knownCodecs is the set of audio codecs doctor reports coverage for (the
// universe WaxBin recognizes), independent of which are decodable in this build.
// "aiff" is the container-keyed PCM-in-AIFF case (ffmpeg-only).
var knownCodecs = []string{"pcm", "mp3", "flac", "vorbis", "opus", "aac", "alac", "aiff"}

// Coverage reports analysis decode support across the known codecs. The label
// comes from the registered decoder's Name(), so a future pure-Go MP3/FLAC
// decoder is reported correctly without a type switch here.
func (r *Registry) Coverage() []FormatSupport {
	out := make([]FormatSupport, 0, len(knownCodecs))
	for _, c := range knownCodecs {
		fs := FormatSupport{Codec: c, Decoder: "none"}
		if d, ok := r.For(c); ok {
			fs.Analysis = true
			fs.Decoder = d.Name()
		}
		out = append(out, fs)
	}
	return out
}
