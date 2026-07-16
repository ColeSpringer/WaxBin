// Package peaks computes compact max-amplitude waveforms for scrubbers and track
// overviews. Results are stored in the catalog, not as sidecars, which keeps
// in-place libraries untouched.
//
// Accumulator is the one algorithm: it reduces a mono stream to a fixed bucket
// count in bounded memory without being told the duration up front, so the
// analyze pass can feed it the chunks of a decode it is already paying for and
// never hold a whole track. Compute is a convenience wrapper over it for a
// signal already in memory.
package peaks

import (
	"encoding/binary"
)

// Version identifies the peaks format/algorithm; a bump forces re-analysis.
const Version = 1

// DefaultBuckets is the waveform resolution stored per track.
const DefaultBuckets = 1000

// oversample is how many coarse buckets per output bucket the Accumulator holds
// before halving its resolution. Higher keeps more detail for the final pooling
// at the cost of memory; at the default it is 8000 float32s (32 KB), and the
// final pooling still has 8x the buckets it needs. It must be even, so a merge
// never leaves an unpaired bucket at the cap.
const oversample = 8

// Peaks is a track waveform: one max-amplitude value per bucket in [0,1].
type Peaks struct {
	Buckets []float32
}

// Accumulator reduces a mono stream to n buckets in O(n) memory with no duration
// hint. It keeps up to n*oversample coarse buckets; when full it halves its
// resolution by max-merging adjacent pairs and doubles the samples per bucket, so
// it always holds fewer than n*oversample coarse buckets and (once it has seen
// n*oversample samples) at least n.
//
// The result depends only on the sample sequence: never on how that sequence was
// chunked across Add calls, and never on a length known in advance. Feeding one
// signal a sample at a time and feeding it in one call produce identical buckets.
type Accumulator struct {
	n               int       // target output buckets
	coarse          []float32 // completed coarse buckets
	framesPerBucket int       // samples each coarse bucket covers
	cur             float32   // running peak of the in-progress bucket
	count           int       // samples accumulated into cur
}

// NewAccumulator returns an Accumulator targeting n output buckets; n <= 0 means
// DefaultBuckets.
func NewAccumulator(n int) *Accumulator {
	if n <= 0 {
		n = DefaultBuckets
	}
	return &Accumulator{n: n, framesPerBucket: 1}
}

// Add folds a chunk of mono samples into the waveform. It fully consumes mono
// before returning and never retains it, so the caller is free to reuse the
// slice for the next chunk.
func (a *Accumulator) Add(mono []float32) {
	limit := a.n * oversample
	for _, s := range mono {
		if v := abs32(s); v > a.cur {
			a.cur = v
		}
		a.count++
		if a.count < a.framesPerBucket {
			continue
		}
		a.coarse = append(a.coarse, a.cur)
		a.cur, a.count = 0, 0
		if len(a.coarse) >= limit {
			a.halve()
		}
	}
}

// halve max-merges adjacent coarse buckets in place, doubling the samples each
// covers. An odd bucket count carries its last bucket through unpaired: it holds
// the max of half a bucket's worth of samples, which is still a max over samples
// that belong to it, and the next merge pairs it normally. (The cap is even, so
// this only arises if oversample ever stops being.)
func (a *Accumulator) halve() {
	half := (len(a.coarse) + 1) / 2
	for i := 0; i < half; i++ {
		v := a.coarse[2*i]
		if j := 2*i + 1; j < len(a.coarse) && a.coarse[j] > v {
			v = a.coarse[j]
		}
		a.coarse[i] = v
	}
	a.coarse = a.coarse[:half]
	a.framesPerBucket *= 2
}

// Peaks returns the finished waveform: exactly n buckets, or a zero Peaks with
// nil Buckets when no samples were added at all. It does not consume the
// Accumulator; Add may continue after it.
func (a *Accumulator) Peaks() Peaks {
	coarse := a.coarse
	if a.count > 0 {
		// The partial trailing bucket is real audio and must count. Build the
		// pooling input on a copy so repeated Peaks calls stay identical and a
		// later Add is unaffected.
		coarse = make([]float32, 0, len(a.coarse)+1)
		coarse = append(coarse, a.coarse...)
		coarse = append(coarse, a.cur)
	}
	if len(coarse) == 0 {
		// No samples: no waveform. Never pool here, because maxPool answers an
		// empty input with n zeros, which is a fake all-silent waveform rather
		// than the absence of one, and the caller stores it.
		return Peaks{}
	}
	return Peaks{Buckets: maxPool(coarse, a.n)}
}

// Compute reduces a mono signal to n max-amplitude buckets. It is Accumulator in
// one call, so a signal already in memory and the same signal streamed produce
// byte-identical waveforms.
func Compute(mono []float32, n int) Peaks {
	a := NewAccumulator(n)
	a.Add(mono)
	return a.Peaks()
}

// maxPool reduces coarse buckets to exactly n by max-pooling. When coarse has
// fewer buckets than requested, each source bucket is stretched to at least one
// output bucket.
func maxPool(coarse []float32, n int) []float32 {
	if n <= 0 {
		n = DefaultBuckets
	}
	if len(coarse) == 0 {
		return make([]float32, n)
	}
	out := make([]float32, n)
	total := len(coarse)
	for i := range out {
		// 64-bit arithmetic: i*total overflows int32 for a long track on a 32-bit
		// platform, corrupting the bucket bounds.
		lo := int(int64(i) * int64(total) / int64(n))
		hi := int(int64(i+1) * int64(total) / int64(n))
		if hi <= lo {
			hi = lo + 1
		}
		if hi > total {
			hi = total
		}
		var peak float32
		for j := lo; j < hi; j++ {
			if coarse[j] > peak {
				peak = coarse[j]
			}
		}
		out[i] = peak
	}
	return out
}

// Pack serializes the buckets to a compact little-endian uint16 BLOB (amplitude
// scaled to 0..65535). Unpack reverses it. uint16 is plenty of resolution for a
// waveform overview and halves the storage versus float32.
func Pack(p Peaks) []byte {
	out := make([]byte, len(p.Buckets)*2)
	for i, v := range p.Buckets {
		// Guard the float-to-uint16 cast. Go leaves NaN integer conversion
		// implementation-specific, so normalize invalid buckets to silence first.
		if v != v {
			v = 0
		}
		if v < 0 {
			v = -v
		}
		if v > 1 {
			v = 1
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v*65535))
	}
	return out
}

// Unpack reverses Pack.
func Unpack(b []byte) Peaks {
	out := make([]float32, len(b)/2)
	for i := range out {
		out[i] = float32(binary.LittleEndian.Uint16(b[i*2:])) / 65535
	}
	return Peaks{Buckets: out}
}

func abs32(f float32) float32 {
	if f < 0 {
		return -f
	}
	return f
}
