package fingerprint

import (
	"testing"

	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/internal/testaudio"
)

// pcm wraps synthesized mono samples (from testaudio, the shared generator) as a
// decode.PCM for the fingerprint under test.
func pcm(rate int, samples []float32) *decode.PCM {
	return &decode.PCM{SampleRate: rate, Channels: 1, Samples: samples}
}

func TestFingerprintSelfConsistent(t *testing.T) {
	orig := pcm(44100, testaudio.RichSignal(44100, 8, testaudio.MusicalPartials, 1))
	transcoded := pcm(44100, testaudio.Reencode(orig.Samples, 0.85, 42)) // quieter copy with noise
	// A different recording: shifted partials and a different modulation pattern.
	different := pcm(44100, testaudio.RichSignal(44100, 8, testaudio.AltPartials, 7))

	a := Compute(orig).Sub
	b := Compute(transcoded).Sub
	c := Compute(different).Sub

	if len(a) == 0 || len(b) == 0 || len(c) == 0 {
		t.Fatal("fingerprints should be non-empty for a 6s signal")
	}

	same := Similar(a, b)
	diff := Similar(a, c)
	t.Logf("similar(orig, transcoded)=%.3f  similar(orig, different)=%.3f", same, diff)

	if same < 0.75 {
		t.Errorf("two encodings of one recording scored %.3f, want >= 0.75", same)
	}
	if diff > 0.65 {
		t.Errorf("a different recording scored %.3f, want < 0.65", diff)
	}
	if same <= diff {
		t.Errorf("same-recording score %.3f must clearly beat different %.3f", same, diff)
	}
}

func TestFingerprintGainInvariant(t *testing.T) {
	orig := pcm(44100, testaudio.RichSignal(44100, 5, testaudio.MusicalPartials, 3))
	louder := &decode.PCM{SampleRate: 44100, Channels: 1, Samples: make([]float32, len(orig.Samples))}
	for i, s := range orig.Samples {
		louder.Samples[i] = s * 0.5 // pure gain change, no other distortion
	}
	if score := Similar(Compute(orig).Sub, Compute(louder).Sub); score < 0.98 {
		t.Errorf("pure gain change scored %.3f, want ~1.0 (the fingerprint is gain-invariant)", score)
	}
}

func TestIndexTermsShareUnderTranscode(t *testing.T) {
	orig := pcm(44100, testaudio.RichSignal(44100, 8, testaudio.MusicalPartials, 1))
	transcoded := pcm(44100, testaudio.Reencode(orig.Samples, 0.85, 42))
	different := pcm(44100, testaudio.RichSignal(44100, 8, testaudio.AltPartials, 7))

	ta := IndexTerms(Compute(orig).Sub, DefaultIndexTerms)
	tb := IndexTerms(Compute(transcoded).Sub, DefaultIndexTerms)
	tc := IndexTerms(Compute(different).Sub, DefaultIndexTerms)

	if shared := overlap(ta, tb); shared < 8 {
		t.Errorf("transcoded copy shares %d min-hash terms, want >= 8", shared)
	}
	if shared := overlap(ta, tc); shared >= overlap(ta, tb) {
		t.Errorf("a different recording should share fewer terms than a transcode (got %d vs %d)",
			shared, overlap(ta, tb))
	}
}

func TestPackRoundTrip(t *testing.T) {
	sub := []uint32{0, 1, 0x7fff, 0x1234, 42}
	if got := Unpack(Pack(sub)); len(got) != len(sub) {
		t.Fatalf("round-trip length %d, want %d", len(got), len(sub))
	} else {
		for i := range sub {
			if got[i] != sub[i] {
				t.Errorf("round-trip[%d] = %d, want %d", i, got[i], sub[i])
			}
		}
	}
}

func overlap(a, b []int64) int {
	set := make(map[int64]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	n := 0
	for _, y := range b {
		if set[y] {
			n++
		}
	}
	return n
}
