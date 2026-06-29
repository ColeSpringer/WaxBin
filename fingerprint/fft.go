package fingerprint

import "math"

// fft performs an in-place iterative radix-2 Cooley-Tukey FFT on the complex
// signal held in re/im. len(re) must be a power of two and equal to len(im).
// This is a small self-contained transform so the analyze pass stays pure-Go
// with no external DSP dependency.
func fft(re, im []float64) {
	n := len(re)

	// Bit-reversal permutation.
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}

	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wr, wi := math.Cos(ang), math.Sin(ang)
		half := length >> 1
		for i := 0; i < n; i += length {
			cr, ci := 1.0, 0.0
			for k := 0; k < half; k++ {
				jr := re[i+k+half]*cr - im[i+k+half]*ci
				ji := re[i+k+half]*ci + im[i+k+half]*cr
				re[i+k+half] = re[i+k] - jr
				im[i+k+half] = im[i+k] - ji
				re[i+k] += jr
				im[i+k] += ji
				cr, ci = cr*wr-ci*wi, cr*wi+ci*wr
			}
		}
	}
}
