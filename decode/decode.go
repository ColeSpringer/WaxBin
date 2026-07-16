// Package decode is WaxBin's PCM-decoding layer, and the only package that
// knows WaxFlow exists. Scanning never imports it; cataloging stays pure Go and
// reads only tags and essence hashes. The separate analyze pass is the only path
// that decodes PCM, for loudness, waveforms, and fingerprinting.
//
// Decoding is unconditional and universal: every container WaxBin can tag-read,
// it can decode, on every host, with no external binaries and no CGO. There is
// therefore no decoder registry and no per-codec capability gate — the questions
// those answered ("is ffmpeg here", "which codecs does this build cover") no
// longer have interesting answers. What remains is an adapter: PCM, the two
// decode entry points the analyze pass needs, and Coverage for doctor.
//
// Nothing here pre-filters by codec. To learn whether an input can be decoded,
// attempt it and read the error: ErrUnsupported means this build cannot decode
// that input, and anything else means the input is damaged. See Engine.Measure.
package decode

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
// For single-channel audio it returns Samples itself rather than a copy.
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

// FormatSupport describes how this build decodes a codec for analysis. doctor
// renders it so users can see current coverage.
type FormatSupport struct {
	Codec    string
	Decoder  string
	Analysis bool
}
