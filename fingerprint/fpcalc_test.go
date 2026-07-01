package fingerprint

import (
	"encoding/json"
	"testing"
)

func TestDecodeRawFingerprint(t *testing.T) {
	// fpcalc -raw -json prints signed decimals; a value beyond int32 range must keep
	// its low 32 bits rather than overflow.
	raw := json.RawMessage(`[0, 1, -1, 2147483648, 4294967295]`)
	sub, err := decodeRawFingerprint(raw)
	if err != nil {
		t.Fatalf("decodeRawFingerprint: %v", err)
	}
	want := []uint32{0, 1, 0xFFFFFFFF, 0x80000000, 0xFFFFFFFF}
	if len(sub) != len(want) {
		t.Fatalf("len = %d, want %d", len(sub), len(want))
	}
	for i := range want {
		if sub[i] != want[i] {
			t.Errorf("sub[%d] = %#x, want %#x", i, sub[i], want[i])
		}
	}
}

func TestChromaprintTermsDeterministicBoundedDistinct(t *testing.T) {
	sub := make([]uint32, 200)
	for i := range sub {
		sub[i] = uint32(i*2654435761) ^ uint32(i<<7) // spread the values
	}
	a := ChromaprintTerms(sub, 64)
	b := ChromaprintTerms(sub, 64)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic term count: %d vs %d", len(a), len(b))
	}
	if len(a) > 64 {
		t.Fatalf("term count %d exceeds cap 64", len(a))
	}
	seen := map[int64]bool{}
	for i, term := range a {
		if term != b[i] {
			t.Fatalf("term %d differs across calls: %d vs %d", i, term, b[i])
		}
		if term < 0 {
			t.Fatalf("term %d is negative (%d); index column requires non-negative", i, term)
		}
		if seen[term] {
			t.Fatalf("duplicate term %d", term)
		}
		seen[term] = true
	}
	// Terms must be sorted ascending (the min-hash keeps the smallest n).
	for i := 1; i < len(a); i++ {
		if a[i] < a[i-1] {
			t.Fatalf("terms not sorted at %d", i)
		}
	}
}

func TestChromaprintTermsTooShort(t *testing.T) {
	if got := ChromaprintTerms([]uint32{42}, 64); got != nil {
		t.Fatalf("single-value fingerprint should yield no terms, got %v", got)
	}
}

func TestSimilarChromaprintIdenticalAndUnrelated(t *testing.T) {
	a := make([]uint32, 300)
	for i := range a {
		a[i] = uint32(i*198491317) ^ uint32(i)
	}
	if s := SimilarChromaprint(a, a); s < 0.999 {
		t.Fatalf("self-similarity = %.3f, want ~1.0", s)
	}

	// A leading-silence shift of one encoding must still score high thanks to the
	// alignment search.
	shifted := append([]uint32{0, 0, 0}, a...)
	if s := SimilarChromaprint(a, shifted); s < altSimilarityFloorTest {
		t.Fatalf("shifted-copy similarity = %.3f, want high (>= %.2f)", s, altSimilarityFloorTest)
	}

	// An unrelated random vector must score near 0.5 and well below the identical
	// case, so grouping does not false-match.
	b := make([]uint32, 300)
	for i := range b {
		b[i] = uint32(i*372036854) ^ uint32(i*7+13)
	}
	if s := SimilarChromaprint(a, b); s > 0.75 {
		t.Fatalf("unrelated similarity = %.3f, want < 0.75", s)
	}
}

// altSimilarityFloorTest mirrors the facade's grouping threshold for the shifted
// case (kept local so the test does not import the facade).
const altSimilarityFloorTest = 0.7

func TestSimilarByAlgoDispatch(t *testing.T) {
	// A 32-bit Chromaprint-style vector compared with the pure-Go Similar would
	// mask to 15 bits; SimilarByAlgo must route by algo so it uses the 32-bit path.
	a := make([]uint32, 100)
	for i := range a {
		a[i] = 0xF000000F | uint32(i) // set high bits the 15-bit pure-Go mask ignores
	}
	chroma := SimilarByAlgo(ChromaprintAlgoVersion, a, a)
	pure := SimilarByAlgo(AlgoVersion, a, a)
	if chroma < 0.999 {
		t.Fatalf("chromaprint self-similarity via dispatch = %.3f, want ~1.0", chroma)
	}
	if pure != Similar(a, a) {
		t.Fatalf("pure-Go dispatch = %.3f, want Similar = %.3f", pure, Similar(a, a))
	}
}
