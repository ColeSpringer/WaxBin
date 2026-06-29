package peaks

import (
	"math"
	"testing"
)

func TestComputeBucketsPeak(t *testing.T) {
	// A ramp from 0 to 1: each later bucket should hold a larger max than the prior.
	n := 4096
	mono := make([]float32, n)
	for i := range mono {
		mono[i] = float32(i) / float32(n)
	}
	p := Compute(mono, 8)
	if len(p.Buckets) != 8 {
		t.Fatalf("bucket count = %d, want 8", len(p.Buckets))
	}
	for i := 1; i < len(p.Buckets); i++ {
		if p.Buckets[i] <= p.Buckets[i-1] {
			t.Errorf("bucket %d (%.3f) not greater than %d (%.3f) for a ramp", i, p.Buckets[i], i-1, p.Buckets[i-1])
		}
	}
	if p.Buckets[7] < 0.9 {
		t.Errorf("last bucket of a 0..1 ramp = %.3f, want near 1", p.Buckets[7])
	}
}

func TestComputeTakesMaxAbs(t *testing.T) {
	// A negative spike must register as a positive peak.
	mono := []float32{0, 0, -0.8, 0, 0, 0.2, 0, 0}
	p := Compute(mono, 2)
	if math.Abs(float64(p.Buckets[0])-0.8) > 1e-6 {
		t.Errorf("bucket 0 = %.3f, want 0.8 (abs of the negative spike)", p.Buckets[0])
	}
}

func TestPackRoundTrip(t *testing.T) {
	in := Peaks{Buckets: []float32{0, 0.25, 0.5, 1.0, 0.123}}
	out := Unpack(Pack(in))
	if len(out.Buckets) != len(in.Buckets) {
		t.Fatalf("length changed: %d -> %d", len(in.Buckets), len(out.Buckets))
	}
	for i := range in.Buckets {
		if math.Abs(float64(in.Buckets[i]-out.Buckets[i])) > 1.0/65535 {
			t.Errorf("bucket %d: %.5f -> %.5f beyond uint16 quantization", i, in.Buckets[i], out.Buckets[i])
		}
	}
}

func TestPackHandlesNaN(t *testing.T) {
	// A NaN bucket (which the max logic never selects, but a future caller might
	// pass) must serialize deterministically to silence, not an arch-dependent
	// uint16 from casting NaN.
	in := Peaks{Buckets: []float32{float32(math.NaN()), 0.5}}
	out := Unpack(Pack(in))
	if out.Buckets[0] != 0 {
		t.Errorf("NaN bucket serialized to %.3f, want 0", out.Buckets[0])
	}
	if math.Abs(float64(out.Buckets[1])-0.5) > 1.0/65535 {
		t.Errorf("adjacent real bucket corrupted: %.3f", out.Buckets[1])
	}
}

func TestMaxPoolDownsamples(t *testing.T) {
	coarse := []float32{0.1, 0.9, 0.2, 0.3, 0.8, 0.4}
	out := maxPool(coarse, 3)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	// Each output is the max of its source pair.
	want := []float32{0.9, 0.3, 0.8}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("pool[%d] = %.2f, want %.2f", i, out[i], want[i])
		}
	}
}
