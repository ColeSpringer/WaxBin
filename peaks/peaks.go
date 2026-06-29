// Package peaks computes compact max-amplitude waveforms for scrubbers and track
// overviews. It can work from decoded PCM or stream through ffmpeg, so long files
// do not need to be held in memory. Results are stored in the catalog, not as
// sidecars, which keeps in-place libraries untouched.
package peaks

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os/exec"
	"strconv"

	"github.com/colespringer/waxbin/waxerr"
)

// Version identifies the peaks format/algorithm; a bump forces re-analysis.
const Version = 1

// DefaultBuckets is the waveform resolution stored per track.
const DefaultBuckets = 1000

// streamRate is the intermediate coarse bucket rate (buckets/sec) the streaming
// path reduces to before max-pooling down to the requested count. It bounds
// memory to O(duration) rather than O(samples).
const streamRate = 25

// analysisRate is the mono rate ffmpeg resamples to (matches decode.analysisRate).
const analysisRate = 44100

// Peaks is a track waveform: one max-amplitude value per bucket in [0,1].
type Peaks struct {
	Buckets []float32
}

// Compute reduces a mono signal to n max-amplitude buckets.
func Compute(mono []float32, n int) Peaks {
	if n <= 0 {
		n = DefaultBuckets
	}
	if len(mono) == 0 {
		return Peaks{}
	}
	out := make([]float32, n)
	total := len(mono)
	for i := range out {
		// 64-bit arithmetic: i*total overflows int32 for a long track on a 32-bit
		// platform (e.g. 999 * 40M samples), corrupting the bucket bounds.
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
			if a := abs32(mono[j]); a > peak {
				peak = a
			}
		}
		out[i] = peak
	}
	return Peaks{Buckets: out}
}

// StreamFFmpeg streams the whole file through ffmpeg as mono f32le, reducing it
// to coarse per-(1/streamRate)-second buckets on the fly, then max-pools those
// down to n buckets. Memory stays O(duration), not O(samples).
func StreamFFmpeg(ctx context.Context, bin, path string, n int) (Peaks, error) {
	const op = "peaks.StreamFFmpeg"
	if bin == "" {
		bin = "ffmpeg"
	}
	if n <= 0 {
		n = DefaultBuckets
	}
	cmd := exec.CommandContext(ctx, bin,
		"-hide_banner", "-loglevel", "error", "-i", path,
		"-ac", "1", "-ar", strconv.Itoa(analysisRate), "-f", "f32le", "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Peaks{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := cmd.Start(); err != nil {
		return Peaks{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	coarse, readErr := streamCoarse(stdout)
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return Peaks{}, waxerr.Wrapf(waxerr.CodeIO, op, err, "ffmpeg pcm")
	}
	if readErr != nil {
		return Peaks{}, waxerr.Wrap(waxerr.CodeIO, op, readErr)
	}
	return Peaks{Buckets: maxPool(coarse, n)}, nil
}

// streamCoarse reads f32le samples and reduces them to max-amplitude buckets of
// streamRate buckets per second.
func streamCoarse(r io.Reader) ([]float32, error) {
	per := analysisRate / streamRate
	if per <= 0 {
		per = 1
	}
	br := bufio.NewReaderSize(r, 1<<16)
	var coarse []float32
	var cur float32
	count := 0
	var buf [4]byte
	for {
		if _, err := io.ReadFull(br, buf[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		s := abs32(math.Float32frombits(binary.LittleEndian.Uint32(buf[:])))
		if s > cur {
			cur = s
		}
		if count++; count >= per {
			coarse = append(coarse, cur)
			cur, count = 0, 0
		}
	}
	if count > 0 {
		coarse = append(coarse, cur)
	}
	return coarse, nil
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
