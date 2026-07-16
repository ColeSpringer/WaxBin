package peaks

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

// signal builds a deterministic pseudo-random signal in [-1, 1].
func signal(n int, seed int64) []float32 {
	r := rand.New(rand.NewSource(seed))
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(r.Float64()*2 - 1)
	}
	return out
}

// addChunked feeds mono to a fresh Accumulator in fixed-size chunks.
func addChunked(mono []float32, n, chunk int) Peaks {
	a := NewAccumulator(n)
	for i := 0; i < len(mono); i += chunk {
		end := i + chunk
		if end > len(mono) {
			end = len(mono)
		}
		a.Add(mono[i:end])
	}
	return a.Peaks()
}

// TestAccumulatorChunkIndependence is the contract that lets the analyze pass
// feed the accumulator whatever chunk size the decoder happens to emit.
func TestAccumulatorChunkIndependence(t *testing.T) {
	mono := signal(200_000, 1)
	want := Pack(Compute(mono, DefaultBuckets))
	for _, chunk := range []int{1, 7, 4096, 65536, len(mono)} {
		got := Pack(addChunked(mono, DefaultBuckets, chunk))
		if !bytes.Equal(got, want) {
			t.Errorf("chunk size %d produced a different waveform than a single-shot Compute", chunk)
		}
	}
}

// TestAccumulatorBucketCount pins the exact output width across the lengths that
// exercise different merge states: shorter than the bucket count, non-power-of-two
// with a partial trailing bucket, and long enough for many merge generations.
func TestAccumulatorBucketCount(t *testing.T) {
	for _, n := range []int{1, 7, 999, 1000, 1001, 4096, 8000, 8001, 123_457, 1_000_000} {
		p := Compute(signal(n, int64(n)), DefaultBuckets)
		if len(p.Buckets) != DefaultBuckets {
			t.Errorf("%d samples: %d buckets, want exactly %d", n, len(p.Buckets), DefaultBuckets)
		}
	}
}

// TestAccumulatorShortStreamHasNoTrailingZeros guards against index drift padding
// the tail: a stream shorter than the bucket count stretches across every bucket,
// so with an all-nonzero signal no bucket may read as silence.
func TestAccumulatorShortStreamHasNoTrailingZeros(t *testing.T) {
	mono := make([]float32, 17)
	for i := range mono {
		mono[i] = 0.5
	}
	p := Compute(mono, DefaultBuckets)
	if len(p.Buckets) != DefaultBuckets {
		t.Fatalf("%d buckets, want %d", len(p.Buckets), DefaultBuckets)
	}
	for i, v := range p.Buckets {
		if v != 0.5 {
			t.Fatalf("bucket %d = %v, want 0.5 everywhere (no drift, no padding)", i, v)
		}
	}
}

// TestAccumulatorEmptyIsNilNotZeros is the divergence the accumulator exists to
// kill: maxPool answers empty input with n zeros, which stores as a fake
// all-silent waveform instead of no waveform at all.
func TestAccumulatorEmptyIsNilNotZeros(t *testing.T) {
	if p := NewAccumulator(DefaultBuckets).Peaks(); p.Buckets != nil {
		t.Errorf("empty accumulator: Buckets = %v, want nil", p.Buckets)
	}
	if p := Compute(nil, DefaultBuckets); p.Buckets != nil {
		t.Errorf("Compute(nil): Buckets = %v, want nil", p.Buckets)
	}
	// Adding nothing is still nothing.
	a := NewAccumulator(DefaultBuckets)
	a.Add([]float32{})
	if p := a.Peaks(); p.Buckets != nil {
		t.Errorf("Add(empty): Buckets = %v, want nil", p.Buckets)
	}
}

// TestAccumulatorPeaksIsRepeatable checks Peaks does not consume the accumulator:
// the partial trailing bucket must not be folded into its state.
func TestAccumulatorPeaksIsRepeatable(t *testing.T) {
	a := NewAccumulator(64)
	a.Add(signal(1234, 9))
	first := Pack(a.Peaks())
	second := Pack(a.Peaks())
	if !bytes.Equal(first, second) {
		t.Error("Peaks() is not repeatable; it mutated accumulator state")
	}
	// And a continued Add still agrees with the equivalent single-shot signal.
	mono := signal(1234, 9)
	more := signal(500, 10)
	a.Add(more)
	got := Pack(a.Peaks())
	want := Pack(Compute(append(append([]float32{}, mono...), more...), 64))
	if !bytes.Equal(got, want) {
		t.Error("Add after Peaks diverged from the single-shot waveform")
	}
}

// TestAccumulatorPreservesPeak checks a merge never loses a spike: max-merging is
// what makes the reduction lossless in the one dimension a waveform reports.
func TestAccumulatorPreservesPeak(t *testing.T) {
	// Long enough to force many merge generations, with one full-scale spike
	// buried mid-stream.
	mono := make([]float32, 500_000)
	for i := range mono {
		mono[i] = 0.01
	}
	mono[321_987] = -1.0 // negative: the waveform reports magnitude

	p := Compute(mono, DefaultBuckets)
	var peak float32
	for _, v := range p.Buckets {
		if v > peak {
			peak = v
		}
	}
	if math.Abs(float64(peak)-1.0) > 1e-6 {
		t.Errorf("max bucket = %v, want the 1.0 spike to survive every merge", peak)
	}
}

// TestAccumulatorLongSignal drives enough samples for many halvings, checking the
// bucket count holds and memory stays bounded (a 24h-scale stream at a coarse
// rate; the point is the merge generations, not the wall clock).
func TestAccumulatorLongSignal(t *testing.T) {
	a := NewAccumulator(DefaultBuckets)
	chunk := signal(65536, 3)
	// ~100M samples: ~17 halvings past the first fill.
	for i := 0; i < 1600; i++ {
		a.Add(chunk)
	}
	p := a.Peaks()
	if len(p.Buckets) != DefaultBuckets {
		t.Fatalf("%d buckets, want %d", len(p.Buckets), DefaultBuckets)
	}
	if got := len(a.coarse); got >= DefaultBuckets*oversample {
		t.Errorf("coarse buckets = %d, want bounded below %d", got, DefaultBuckets*oversample)
	}
	if got := len(a.coarse); got < DefaultBuckets {
		t.Errorf("coarse buckets = %d, want at least %d to pool from", got, DefaultBuckets)
	}
}

// TestHalveOddCount covers the unpaired trailing bucket. The cap is even so Add
// cannot reach this state today; halve handles it anyway, and this pins that.
func TestHalveOddCount(t *testing.T) {
	a := &Accumulator{n: 2, framesPerBucket: 1, coarse: []float32{0.1, 0.9, 0.2, 0.3, 0.7}}
	a.halve()
	want := []float32{0.9, 0.3, 0.7} // pairs max-merged; the odd last carries through
	if len(a.coarse) != len(want) {
		t.Fatalf("len = %d, want %d", len(a.coarse), len(want))
	}
	for i := range want {
		if a.coarse[i] != want[i] {
			t.Errorf("coarse[%d] = %v, want %v", i, a.coarse[i], want[i])
		}
	}
	if a.framesPerBucket != 2 {
		t.Errorf("framesPerBucket = %d, want 2", a.framesPerBucket)
	}
}
