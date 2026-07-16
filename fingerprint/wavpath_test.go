package fingerprint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/internal/testaudio"
)

// TestFingerprintThroughWAVPath locks in the match through the exact path the
// analyze pass uses (WAV -> Engine.Mono at InternalRate -> Compute): a transcode
// of one recording both scores high and shares index terms, while an unrelated
// track does neither. This is the regression guard for the end-to-end grouping,
// so it must track the route the pass actually takes — decoding straight to
// InternalRate, which makes Compute's own resample a no-op.
func TestFingerprintThroughWAVPath(t *testing.T) {
	const rate = 22050
	dir := t.TempDir()
	orig := testaudio.RichSignal(rate, 20, testaudio.MusicalPartials, 1)
	trans := testaudio.Reencode(orig, 0.85, 42)
	other := testaudio.RichSignal(rate, 20, testaudio.AltPartials, 7)

	fpOf := func(name string, samples []float32) *Fingerprint {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, testaudio.EncodeWAV16(rate, samples), 0o644); err != nil {
			t.Fatal(err)
		}
		pcm, err := decode.New(nil).Mono(context.Background(), p, InternalRate, MaxAnalyze)
		if err != nil {
			t.Fatal(err)
		}
		if pcm.SampleRate != InternalRate || pcm.Channels != 1 {
			t.Fatalf("decoded %d Hz / %d ch, want %d / 1 (Compute's resample must be a no-op here)",
				pcm.SampleRate, pcm.Channels, InternalRate)
		}
		return Compute(pcm)
	}

	a := fpOf("a.wav", orig)
	b := fpOf("b.wav", trans)
	c := fpOf("c.wav", other)

	ta := IndexTerms(a.Sub, DefaultIndexTerms)
	tb := IndexTerms(b.Sub, DefaultIndexTerms)
	tc := IndexTerms(c.Sub, DefaultIndexTerms)
	simAB, simAC := Similar(a.Sub, b.Sub), Similar(a.Sub, c.Sub)
	sharedAB, sharedAC := overlap(ta, tb), overlap(ta, tc)
	t.Logf("similar(a,b)=%.3f similar(a,c)=%.3f shared(a,b)=%d shared(a,c)=%d",
		simAB, simAC, sharedAB, sharedAC)

	if simAB < 0.8 {
		t.Errorf("transcode similarity %.3f, want >= 0.8", simAB)
	}
	if simAC > 0.65 {
		t.Errorf("unrelated similarity %.3f, want < 0.65", simAC)
	}
	if sharedAB < 2 {
		t.Errorf("transcode shares %d index terms, want >= 2 (candidate threshold)", sharedAB)
	}
	if sharedAC >= sharedAB {
		t.Errorf("unrelated shares %d terms, should be fewer than the transcode's %d", sharedAC, sharedAB)
	}
}
