package decode

import "testing"

// TestAIFFRoutingNotWAV verifies AIFF's container-keyed "aiff" codec never routes
// to the RIFF/WAVE-only pure-Go decoder: with no ffmpeg it has no decoder (so the
// analyze pass skips it cleanly rather than erroring), and ffmpeg covers it.
func TestAIFFRoutingNotWAV(t *testing.T) {
	// A pure-Go-only registry (the no-ffmpeg baseline): "pcm" is the WAV decoder,
	// "aiff" has none (skip, not the WAV decoder).
	pure := NewRegistry()
	pure.Register("pcm", &WAVDecoder{})
	if _, ok := pure.For("aiff"); ok {
		t.Error("aiff should have no pure-Go decoder (must not route to the WAV decoder)")
	}
	if d, ok := pure.For("pcm"); !ok || d.Name() != "pure-go (wav)" {
		t.Errorf("pcm should map to the pure-Go WAV decoder, got %v/%v", d, ok)
	}

	// aiff is in the ffmpeg-covered set, so a host with ffmpeg can decode it.
	found := false
	for _, c := range ffmpegCodecs {
		if c == "aiff" {
			found = true
		}
	}
	if !found {
		t.Error("aiff missing from ffmpegCodecs; AIFF would never decode")
	}

	// doctor reports coverage for aiff.
	var covered bool
	for _, c := range knownCodecs {
		if c == "aiff" {
			covered = true
		}
	}
	if !covered {
		t.Error("aiff missing from knownCodecs; doctor would not report its coverage")
	}
}
