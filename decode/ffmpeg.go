package decode

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os/exec"
	"time"

	"github.com/colespringer/waxbin/waxerr"
)

// FFmpegDecoder decodes any format ffmpeg understands by piping a normalized
// mono f32le stream out of an ffmpeg subprocess (no CGO). It is registered only
// when ffmpeg is detected on PATH, and only for codecs without a pure-Go
// decoder. The output is resampled to analysisRate mono; the fingerprint
// normalizes rate regardless, so this matches the pure-Go path.
type FFmpegDecoder struct {
	Path string // resolved ffmpeg path (from caps.Detect)
}

// Name reports how doctor labels this decoder's coverage.
func (*FFmpegDecoder) Name() string { return "ffmpeg" }

// analysisRate is the fixed rate ffmpeg resamples to. The fingerprint downsamples
// further; a fixed intermediate keeps the ffmpeg and pure-Go paths comparable.
const analysisRate = 44100

const ffmpegOp = "decode.ffmpeg"

// Decode runs ffmpeg to produce mono float32 PCM, capping the decoded length at
// max (0 == whole file; the analyze pass always passes a cap).
func (d *FFmpegDecoder) Decode(ctx context.Context, path string, max time.Duration) (*PCM, error) {
	bin := d.Path
	if bin == "" {
		bin = "ffmpeg"
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-i", path, "-ac", "1", "-ar", fmt.Sprint(analysisRate)}
	if max > 0 {
		args = append(args, "-t", fmt.Sprintf("%.3f", max.Seconds()))
	}
	args = append(args, "-f", "f32le", "-")

	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, ffmpegOp, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, ffmpegOp, err)
	}

	// Bound the read so a missing -t (or a misbehaving decoder) can't exhaust
	// memory: cap at the requested duration plus headroom, else a fixed ceiling.
	var limit int64 = 512 << 20 // 512 MiB hard ceiling
	if max > 0 {
		limit = int64(max.Seconds()*analysisRate)*4 + 1<<20
	}
	raw, readErr := io.ReadAll(io.LimitReader(stdout, limit))
	// Drain any output beyond the cap: if we stopped reading at the limit, the
	// pipe would fill, ffmpeg would block on write, and Wait would deadlock. We
	// discard the overflow (the cap is intentional) so ffmpeg can finish.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if waitErr != nil {
		return nil, waxerr.Wrapf(waxerr.CodeIO, ffmpegOp, waitErr, "ffmpeg: %s", trimErr(stderr.String()))
	}
	if readErr != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, ffmpegOp, readErr)
	}

	n := len(raw) / 4
	samples := make([]float32, n)
	for i := 0; i < n; i++ {
		samples[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return &PCM{SampleRate: analysisRate, Channels: 1, Samples: samples}, nil
}

// trimErr keeps ffmpeg's stderr short for error messages.
func trimErr(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "..."
	}
	if s == "" {
		return "exited non-zero"
	}
	return s
}
