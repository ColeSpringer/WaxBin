// Package fingerprint computes WaxBin's internal acoustic fingerprint and the
// inverted-index terms used for alt-encoding grouping. The fingerprint does not
// try to match Chromaprint; it only needs to be stable against WaxBin's own
// decoder output so different lossy encodings of one recording score similarly.
// AcoustID lookup is a separate Chromaprint concern that requires fpcalc.
//
// The fingerprint is a sequence of 15-bit sub-fingerprints, one per frame
// transition, derived from the sign of a 2-D difference of log-spaced band
// energies. Those signs tend to survive lossy codecs and
// volume changes. Grouping uses a min-hash of overlapping sub-fingerprint
// shingles as the inverted-index key, so candidates are found by shared terms
// (within a duration bucket) rather than pairwise comparison.
package fingerprint

import (
	"encoding/binary"
	"math"
	"math/bits"
	"slices"
	"time"

	"github.com/colespringer/waxbin/decode"
)

// AlgoVersion identifies the fingerprint algorithm. Bumping it forces the
// analyze pass to recompute fingerprints. Analysis is keyed by both essence and
// algorithm version, so a better algorithm supersedes stored fingerprints.
const AlgoVersion = 1

// InternalRate is the mono rate the fingerprint frames at. It is exported
// because the analyze pass decodes straight to it: the decoder's own high-
// quality resampler is a far better path to this rate than resample below, whose
// box-averaging aliases the 5512-22050 Hz band down into the 200-2000 Hz bands
// the fingerprint keys on, which is where lossy codecs differ most. Compute still
// accepts any rate; feeding it audio already at this rate simply makes its
// resample step a no-op.
const InternalRate = 11025

const (
	frameSize     = 4096  // FFT window (power of two)
	hopSize       = 2048  // 50% overlap between frames
	numBands      = 16    // log-spaced energy bands -> numBands-1 = 15 bits/frame
	minFreq       = 200.0 // Hz, low edge of the band range
	maxFreq       = 2000.0
	bucketWidthMS = 2000 // duration bucket granularity for candidate pruning
	maxShift      = 25   // frame shifts tried when aligning two fingerprints
)

// MaxAnalyze caps how much audio the analyze pass decodes per file for
// fingerprinting. A fingerprint of the first couple of minutes is enough to
// group encodings of one recording without decoding a whole audiobook.
const MaxAnalyze = 120 * time.Second

// Fingerprint is a computed acoustic fingerprint.
type Fingerprint struct {
	Version    int
	DurationMS int64    // length of the analyzed PCM (capped at MaxAnalyze)
	Sub        []uint32 // 15-bit sub-fingerprints, one per frame transition
}

// The analysis window and band-edge bins depend only on compile-time constants,
// so they are computed once here rather than on every Compute call, which runs
// once per file across a whole library. Both are read-only after init (Compute
// copies the small edges array and only reads the window), so sharing them is safe.
var (
	fpWindow    = hann(frameSize)
	fpBandEdges = bandEdges()
)

// Compute derives a fingerprint from decoded PCM. It returns a fingerprint with
// no sub-values when the audio is too short to frame (the caller treats that as
// "analyzed, nothing to group on").
func Compute(pcm *decode.PCM) *Fingerprint {
	mono := resample(pcm.Mono(), pcm.SampleRate, InternalRate)
	fp := &Fingerprint{Version: AlgoVersion}
	if pcm.SampleRate > 0 {
		fp.DurationMS = pcm.DurationMS()
	}
	if len(mono) < frameSize {
		return fp
	}

	edges := fpBandEdges
	window := fpWindow

	var prevBands, curBands [numBands]float64
	havePrev := false
	re := make([]float64, frameSize)
	im := make([]float64, frameSize)

	for start := 0; start+frameSize <= len(mono); start += hopSize {
		for i := 0; i < frameSize; i++ {
			re[i] = float64(mono[start+i]) * window[i]
			im[i] = 0
		}
		fft(re, im)
		bandEnergies(re, im, edges, &curBands)

		if havePrev {
			fp.Sub = append(fp.Sub, subFingerprint(&prevBands, &curBands))
		}
		prevBands = curBands
		havePrev = true
	}
	return fp
}

// subFingerprint packs the 2-D difference bits for one frame transition: for
// each adjacent band pair, the sign of how the band-to-band energy gap changed
// between the previous and current frame.
func subFingerprint(prev, cur *[numBands]float64) uint32 {
	var v uint32
	for f := 0; f < numBands-1; f++ {
		d := (cur[f] - cur[f+1]) - (prev[f] - prev[f+1])
		if d > 0 {
			v |= 1 << uint(f)
		}
	}
	return v
}

// bandEnergies sums the magnitude spectrum into log-spaced bands (uses the
// lower half of the spectrum; the upper half mirrors it for real input).
func bandEnergies(re, im []float64, edges [numBands + 1]int, out *[numBands]float64) {
	for b := 0; b < numBands; b++ {
		var sum float64
		for k := edges[b]; k < edges[b+1]; k++ {
			sum += math.Hypot(re[k], im[k])
		}
		out[b] = math.Log1p(sum) // compress; the double-difference sign is the signal
	}
}

// bandEdges returns the FFT bin index boundaries of the log-spaced bands.
func bandEdges() [numBands + 1]int {
	var edges [numBands + 1]int
	ratio := math.Pow(maxFreq/minFreq, 1.0/float64(numBands))
	freq := minFreq
	for i := 0; i <= numBands; i++ {
		bin := int(freq * float64(frameSize) / float64(InternalRate))
		if bin >= frameSize/2 {
			bin = frameSize/2 - 1
		}
		edges[i] = bin
		freq *= ratio
	}
	// Guarantee each band spans at least one bin so no band is empty.
	for i := 1; i <= numBands; i++ {
		if edges[i] <= edges[i-1] {
			edges[i] = edges[i-1] + 1
		}
	}
	return edges
}

func hann(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
	}
	return w
}

// resample converts samples from in to out rate by box-averaging input windows.
// It is a crude resampler, sufficient because two encodings of one recording
// decode to near-identical PCM, so the same averaging yields the same bands.
func resample(in []float32, from, to int) []float32 {
	if from == to || from <= 0 || to <= 0 || len(in) == 0 {
		return in
	}
	ratio := float64(from) / float64(to)
	outLen := int(float64(len(in)) / ratio)
	out := make([]float32, outLen)
	for i := range out {
		lo := int(float64(i) * ratio)
		hi := int(float64(i+1) * ratio)
		if hi <= lo {
			hi = lo + 1
		}
		if hi > len(in) {
			hi = len(in)
		}
		var sum float32
		for j := lo; j < hi; j++ {
			sum += in[j]
		}
		out[i] = sum / float32(hi-lo)
	}
	return out
}

// Similar returns the best bit-agreement in [0,1] between two fingerprints,
// searching a small frame-shift window so leading-silence differences between
// encodings do not defeat the match. 1.0 is identical; ~0.5 is unrelated.
func Similar(a, b []uint32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	best := 0.0
	for shift := -maxShift; shift <= maxShift; shift++ {
		if agree := bitAgreement(a, b, shift); agree > best {
			best = agree
		}
	}
	return best
}

// bitAgreement compares a[i] against b[i+shift] over their overlap and returns
// the fraction of agreeing bits (0 when the overlap is too small to be meaningful).
func bitAgreement(a, b []uint32, shift int) float64 {
	var matches, total int
	for i := range a {
		j := i + shift
		if j < 0 || j >= len(b) {
			continue
		}
		diff := bits.OnesCount32((a[i] ^ b[j]) & subMask)
		matches += bitsPerSub - diff
		total += bitsPerSub
	}
	if total < bitsPerSub*minOverlapFrames {
		return 0
	}
	return float64(matches) / float64(total)
}

const (
	bitsPerSub       = numBands - 1
	subMask          = (1 << bitsPerSub) - 1
	minOverlapFrames = 8 // require a few seconds of overlap before trusting a score
)

// Pack serializes sub-fingerprints to a compact little-endian BLOB for storage.
func Pack(sub []uint32) []byte {
	out := make([]byte, len(sub)*4)
	for i, s := range sub {
		binary.LittleEndian.PutUint32(out[i*4:], s)
	}
	return out
}

// Unpack reverses Pack.
func Unpack(b []byte) []uint32 {
	sub := make([]uint32, len(b)/4)
	for i := range sub {
		sub[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return sub
}

// IndexTerms returns up to n min-hash terms for the inverted index: the smallest
// distinct 2-frame shingles of the fingerprint. Shingling widens the term space
// (so a shared term implies locally identical audio, not a chance 15-bit
// collision), and taking the smallest n is a min-hash that approximates set
// similarity with a bounded number of rows per file.
func IndexTerms(sub []uint32, n int) []int64 {
	if len(sub) < 2 || n <= 0 {
		return nil
	}
	seen := make(map[int64]bool, len(sub))
	terms := make([]int64, 0, len(sub))
	for i := 0; i+1 < len(sub); i++ {
		shingle := int64(sub[i])<<bitsPerSub | int64(sub[i+1]&subMask)
		if !seen[shingle] {
			seen[shingle] = true
			terms = append(terms, shingle)
		}
	}
	slices.Sort(terms) // min-hash: keep the n smallest distinct shingles
	if len(terms) > n {
		terms = terms[:n]
	}
	return terms
}

// DefaultIndexTerms is the number of min-hash terms stored per file.
const DefaultIndexTerms = 64

// TermsForAlgo returns the DefaultIndexTerms min-hash terms for a fingerprint under
// the given algorithm: the Chromaprint backend hashes its wider 32-bit sub-values,
// the pure-Go backend bit-packs its 15-bit ones. It is the single source of truth for
// the algo->terms dispatch, called both by the analyze write path (via indexTerms) and
// by the cross-catalog resolve probe, so the write side and the probe side can never
// derive terms differently for the same fingerprint.
func TermsForAlgo(algo int, sub []uint32) []int64 {
	if algo == ChromaprintAlgoVersion {
		return ChromaprintTerms(sub, DefaultIndexTerms)
	}
	return IndexTerms(sub, DefaultIndexTerms)
}

// DurationBucket maps a track duration to its pruning bucket; only files in the
// same bucket are compared, so grouping never scans the whole catalog.
func DurationBucket(durationMS int64) int64 {
	if durationMS <= 0 {
		return 0
	}
	return durationMS / bucketWidthMS
}
