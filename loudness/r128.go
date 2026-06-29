package loudness

import (
	"math"

	"github.com/colespringer/waxbin/decode"
)

// R128 measures integrated loudness and sample peak from decoded PCM using
// ITU-R BS.1770. It K-weights each channel, evaluates overlapping 400 ms blocks,
// and applies the absolute (-70 LUFS) and relative (-10 LU) gates. It is the
// pure-Go fallback for hosts without ffmpeg. Channels are summed with unit weight,
// which is appropriate for mono and stereo music; surround weighting is not
// applied.
func R128(pcm *decode.PCM) Result {
	if pcm == nil || pcm.SampleRate <= 0 || pcm.Channels <= 0 || len(pcm.Samples) == 0 {
		return Result{}
	}
	res := Result{SamplePeak: samplePeak(pcm.Samples)}

	// K-weighting coefficients are defined at 48 kHz, so resample each channel to
	// 48 kHz before filtering. Box-average resampling is sufficient here (the
	// gating integrates over 400 ms blocks).
	const filterRate = 48000
	blockLen := filterRate * 4 / 10 // 400 ms
	stepLen := filterRate / 10      // 100 ms (75% overlap)
	if blockLen <= 0 {
		return res
	}

	ch := pcm.Channels
	// Deinterleave, resample, and K-weight each channel.
	weighted := make([][]float64, ch)
	for c := 0; c < ch; c++ {
		mono := deinterleave(pcm.Samples, ch, c)
		rs := resampleTo(mono, pcm.SampleRate, filterRate)
		weighted[c] = kWeight(rs)
	}
	n := len(weighted[0])
	if n < blockLen {
		return res // too short to form a single gating block
	}

	// Per-block summed-channel mean square (the loudness energy z_j).
	var blocks []float64
	for start := 0; start+blockLen <= n; start += stepLen {
		var z float64
		for c := 0; c < ch; c++ {
			var sum float64
			seg := weighted[c][start : start+blockLen]
			for _, s := range seg {
				sum += s * s
			}
			z += sum / float64(blockLen)
		}
		blocks = append(blocks, z)
	}
	if len(blocks) == 0 {
		return res
	}

	// Absolute gate at -70 LUFS, then relative gate at -10 LU below the
	// abs-gated mean loudness.
	const absGate = -70.0
	gated := gateBlocks(blocks, absGate)
	if len(gated) == 0 {
		return res
	}
	relThreshold := blockLoudness(mean(gated)) - 10.0
	// Apply the relative gate to the absolute-gated subset, not the full block set:
	// for a very quiet track relThreshold can fall below the -70 LUFS absolute gate,
	// and re-filtering all blocks would re-admit ones the absolute gate excluded.
	gated = gateBlocks(gated, relThreshold)
	if len(gated) == 0 {
		return res
	}
	res.IntegratedLUFS = blockLoudness(mean(gated))
	res.Valid = !math.IsInf(res.IntegratedLUFS, 0) && !math.IsNaN(res.IntegratedLUFS)
	return res
}

// blockLoudness maps a summed-channel mean square to LUFS.
func blockLoudness(meanSquare float64) float64 {
	if meanSquare <= 0 {
		return math.Inf(-1)
	}
	return -0.691 + 10*math.Log10(meanSquare)
}

// gateBlocks keeps the blocks whose loudness is at or above thresholdLUFS.
func gateBlocks(blocks []float64, thresholdLUFS float64) []float64 {
	var out []float64
	for _, z := range blocks {
		if blockLoudness(z) >= thresholdLUFS {
			out = append(out, z)
		}
	}
	return out
}

func mean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// kWeight applies the two-stage BS.1770 K-weighting filter (a high-shelf "head"
// stage and an RLB high-pass stage) at 48 kHz.
func kWeight(x []float64) []float64 {
	// Stage 1: high-shelf.
	s1 := biquad(x,
		1.53512485958697, -2.69169618940638, 1.19839281085285,
		-1.69065929318241, 0.73248077421585)
	// Stage 2: RLB high-pass.
	return biquad(s1,
		1.0, -2.0, 1.0,
		-1.99004745483398, 0.99007225036621)
}

// biquad applies a direct-form-I biquad with the given coefficients (a0 == 1).
func biquad(x []float64, b0, b1, b2, a1, a2 float64) []float64 {
	y := make([]float64, len(x))
	var x1, x2, y1, y2 float64
	for i, xn := range x {
		yn := b0*xn + b1*x1 + b2*x2 - a1*y1 - a2*y2
		y[i] = yn
		x2, x1 = x1, xn
		y2, y1 = y1, yn
	}
	return y
}

// deinterleave extracts channel c from interleaved samples.
func deinterleave(samples []float32, channels, c int) []float64 {
	n := len(samples) / channels
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = float64(samples[i*channels+c])
	}
	return out
}

// resampleTo converts a mono signal between rates. Downsampling (ratio >= 1)
// box-averages each input window, which low-passes as it decimates. Upsampling
// (ratio < 1, e.g. the common 44.1 kHz -> 48 kHz K-weighting target) linearly
// interpolates instead of duplicating samples: nearest-neighbor upsampling would
// introduce high-frequency imaging that the K-weighting high-shelf then amplifies,
// skewing the loudness measurement.
func resampleTo(in []float64, from, to int) []float64 {
	if from == to || from <= 0 || to <= 0 || len(in) == 0 {
		return in
	}
	ratio := float64(from) / float64(to)
	outLen := int(float64(len(in)) / ratio)
	out := make([]float64, outLen)

	if ratio < 1 { // upsampling: linear interpolation
		for i := range out {
			pos := float64(i) * ratio
			idx := int(pos)
			frac := pos - float64(idx)
			next := idx + 1
			if next >= len(in) {
				next = len(in) - 1
			}
			out[i] = in[idx]*(1-frac) + in[next]*frac
		}
		return out
	}

	// downsampling: average each input window (anti-aliasing)
	for i := range out {
		lo := int(float64(i) * ratio)
		hi := int(float64(i+1) * ratio)
		if hi <= lo {
			hi = lo + 1
		}
		if hi > len(in) {
			hi = len(in)
		}
		var sum float64
		for j := lo; j < hi; j++ {
			sum += in[j]
		}
		out[i] = sum / float64(hi-lo)
	}
	return out
}

// samplePeak returns the maximum absolute sample amplitude.
func samplePeak(samples []float32) float64 {
	var peak float64
	for _, s := range samples {
		a := math.Abs(float64(s))
		if a > peak {
			peak = a
		}
	}
	return peak
}
