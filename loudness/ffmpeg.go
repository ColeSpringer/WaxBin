package loudness

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/waxerr"
)

// FFmpeg measures loudness with ffmpeg's ebur128 filter over the whole file. It
// parses integrated loudness and true peak from the final Summary block, keeping
// memory bounded regardless of track length.
func FFmpeg(ctx context.Context, bin, path string) (Result, error) {
	const op = "loudness.FFmpeg"
	if bin == "" {
		bin = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, bin,
		"-hide_banner", "-nostats", "-i", path,
		"-filter_complex", "ebur128=peak=true", "-f", "null", "-")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	res, parseErr := parseEBUR128(stderr)
	_, _ = io.Copy(io.Discard, stderr) // drain any trailing output so Wait never blocks
	if err := cmd.Wait(); err != nil {
		return Result{}, waxerr.Wrapf(waxerr.CodeIO, op, err, "ffmpeg ebur128")
	}
	if parseErr != nil {
		return Result{}, parseErr
	}
	return res, nil
}

// parseEBUR128 reads the ebur128 Summary block. Both the integrated "I:" and the
// true-peak "Peak:" appear as repeated per-frame lines too, so only the Summary
// section (after the "Summary:" marker) is parsed.
func parseEBUR128(r io.Reader) (Result, error) {
	const op = "loudness.FFmpeg"
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	inSummary := false
	var res Result
	var haveI bool
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// ffmpeg prefixes the "Summary:" line with "[Parsed_ebur128_0 @ ...]", so
		// match it anywhere on the line; the I:/Peak: lines it introduces are plain.
		if strings.Contains(line, "Summary:") {
			inSummary = true
			continue
		}
		if !inSummary {
			continue
		}
		switch {
		case strings.HasPrefix(line, "I:"):
			if v, ok := parseFloatField(line, "I:", "LUFS"); ok {
				res.IntegratedLUFS = v
				res.Valid = true
				haveI = true
			}
		case strings.HasPrefix(line, "Peak:"):
			// Current ffmpeg builds emit one aggregate Peak line. Keep the maximum
			// across all Peak lines in case future builds print channel-specific
			// peaks.
			if v, ok := parseFloatField(line, "Peak:", "dBFS"); ok {
				if p := dbToLin(v); p > res.SamplePeak {
					res.SamplePeak = p
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Result{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if !haveI {
		return Result{}, waxerr.New(waxerr.CodeIO, op, "ebur128 produced no integrated loudness")
	}
	return res, nil
}

// parseFloatField extracts the float between a leading label and a trailing unit,
// e.g. parseFloatField("I: -21.1 LUFS", "I:", "LUFS") -> -21.1.
func parseFloatField(line, label, unit string) (float64, bool) {
	s := strings.TrimSpace(strings.TrimPrefix(line, label))
	s = strings.TrimSpace(strings.TrimSuffix(s, unit))
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
