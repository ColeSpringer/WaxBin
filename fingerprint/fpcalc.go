package fingerprint

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/bits"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxbin/waxerr"
)

// ChromaprintAlgoVersion identifies the Chromaprint (fpcalc) fingerprint. It is
// distinct from the pure-Go AlgoVersion so the two never share a stored
// analysis_version and the candidate query never scores a pure-Go vector against a
// Chromaprint one (their bit layouts and Similar functions differ). When fpcalc
// appears or disappears the effective version changes, forcing re-analysis.
const ChromaprintAlgoVersion = 100

// chromaprintBits is the width of one Chromaprint sub-fingerprint. Chromaprint
// packs a 32-bit value per frame; agreement is measured across all 32 bits.
const chromaprintBits = 32

// fpcalcOutput is the JSON shape fpcalc emits with -json. With -raw the
// fingerprint is an array of integers; without it, a compressed base64 string. The
// duration is the file's as its container states it, not what fpcalc decoded.
type fpcalcOutput struct {
	Duration    float64         `json:"duration"`
	Fingerprint json.RawMessage `json:"fingerprint"`
}

// fpcalcDefaultLength is the span fpcalc fingerprints when it is passed no -length.
const fpcalcDefaultLength = 120 * time.Second

// The default Chromaprint algorithm, which fpcalc runs unless told otherwise, emits one
// value per 1365 samples at 11025 Hz, and its first value has already taken 28666 samples
// of frame and filter context. Together they turn a fingerprint's length back into the
// audio it covers.
const (
	chromaprintItemSeconds  = 1365.0 / 11025
	chromaprintDelaySeconds = 28666.0 / 11025
)

// partialReadSlack is how far short of the span asked for the fingerprint of a read that
// failed may stop and still be kept.
const partialReadSlack = 2 * time.Second

// ChromaprintResult is one fpcalc run: the fingerprint in the form asked for, the file
// duration fpcalc reported, in whole seconds, and how the run ended.
type ChromaprintResult struct {
	Sub         []uint32 // from ChromaprintRawDetail
	Compressed  string   // from ChromaprintCompressedDetail
	DurationSec int
	// Partial says fpcalc exited with ExitCode and the read error in Note after its
	// fingerprint already covered the span asked for, so the fingerprint was kept.
	Partial  bool
	ExitCode int
	Note     string
}

// ChromaprintRaw runs fpcalc to produce a raw Chromaprint sub-fingerprint vector
// for internal grouping, capped at maxDur. The vector is comparable across lossy
// encodings of one recording, like the pure-Go fingerprint, but is Chromaprint's
// own layout, so it is stored under ChromaprintAlgoVersion. Any non-zero exit fails it.
func ChromaprintRaw(ctx context.Context, bin, path string, maxDur time.Duration) ([]uint32, int, error) {
	r, err := ChromaprintRawDetail(ctx, bin, path, maxDur, 0)
	if err != nil {
		return nil, 0, err
	}
	return r.Sub, r.DurationSec, nil
}

// ChromaprintRawDetail is ChromaprintRaw given the file's duration, expected. fpcalc
// prints the fingerprint of what it read before it reports a read error, under an exit
// code that is not documented, so a non-zero exit is judged by that fingerprint alone:
// it is kept, marked Partial, when it covers expected capped at maxDur, and refused
// otherwise, as is every non-zero exit when expected is unknown (0).
func ChromaprintRawDetail(ctx context.Context, bin, path string, maxDur, expected time.Duration) (*ChromaprintResult, error) {
	run, err := runFpcalc(ctx, bin, path, maxDur, true)
	if err != nil {
		return nil, err
	}
	sub, err := decodeRawFingerprint(run.out.Fingerprint)
	if err != nil {
		return nil, run.refuse(err)
	}
	r := &ChromaprintResult{Sub: sub}
	if err := run.settle(r, len(sub), maxDur, expected); err != nil {
		return nil, err
	}
	return r, nil
}

// decodeRawFingerprint parses fpcalc's raw integer-array fingerprint into a
// uint32 sub-fingerprint vector. fpcalc -raw prints signed decimals; each value is
// a 32-bit pattern, so the low 32 bits are kept (two's complement wraps to the
// same bits used for XOR/hash).
func decodeRawFingerprint(raw json.RawMessage) ([]uint32, error) {
	var nums []int64
	if err := json.Unmarshal(raw, &nums); err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeInvalid, "fingerprint.fpcalc", err, "parsing raw fingerprint")
	}
	sub := make([]uint32, len(nums))
	for i, n := range nums {
		sub[i] = uint32(n)
	}
	return sub, nil
}

// ChromaprintCompressed runs fpcalc to produce the compressed base64 fingerprint
// and the duration in whole seconds, the pair the AcoustID API accepts. AcoustID
// is Chromaprint-only, so this is the only fingerprint form it takes. Any non-zero
// exit fails it.
func ChromaprintCompressed(ctx context.Context, bin, path string, maxDur time.Duration) (string, int, error) {
	r, err := ChromaprintCompressedDetail(ctx, bin, path, maxDur, 0)
	if err != nil {
		return "", 0, err
	}
	return r.Compressed, r.DurationSec, nil
}

// ChromaprintCompressedDetail is ChromaprintCompressed judging a non-zero exit the way
// ChromaprintRawDetail does. The compressed form states its length in its header.
func ChromaprintCompressedDetail(ctx context.Context, bin, path string, maxDur, expected time.Duration) (*ChromaprintResult, error) {
	run, err := runFpcalc(ctx, bin, path, maxDur, false)
	if err != nil {
		return nil, err
	}
	var fp string
	if err := json.Unmarshal(run.out.Fingerprint, &fp); err != nil {
		return nil, run.refuse(waxerr.Wrapf(waxerr.CodeInvalid, "fingerprint.fpcalc", err, "parsing compressed fingerprint"))
	}
	r := &ChromaprintResult{Compressed: fp}
	if err := run.settle(r, compressedLength(fp), maxDur, expected); err != nil {
		return nil, err
	}
	return r, nil
}

// compressedLength returns how many values a compressed fingerprint holds: its header is
// an algorithm byte and then the count in three big-endian bytes, in URL-safe base64.
func compressedLength(fp string) int {
	if len(fp) < 8 {
		return 0
	}
	b, err := base64.RawURLEncoding.DecodeString(fp[:8])
	if err != nil {
		return 0
	}
	return int(b[1])<<16 | int(b[2])<<8 | int(b[3])
}

// fpcalcRun is one finished fpcalc process: its output, and when it exited non-zero, the
// exit and what it said on stderr.
type fpcalcRun struct {
	out    *fpcalcOutput
	exit   *exec.ExitError
	stderr string
}

// refuse is the error a run that cannot be used reports: fpcalc's own when it exited
// non-zero, since that names why the output is unusable, else err.
func (r *fpcalcRun) refuse(err error) error {
	if r.exit == nil {
		return err
	}
	return waxerr.Wrapf(waxerr.CodeIO, "fingerprint.fpcalc", r.exit, "fpcalc: %s", trimFpErr(r.stderr))
}

// settle fills res from a run whose fingerprint holds n values, or refuses it: a run that
// exited non-zero is kept only when that fingerprint covers the span asked for. An unknown
// expected keeps nothing, since a read that stopped early cannot then be told from one
// that finished.
func (r *fpcalcRun) settle(res *ChromaprintResult, n int, maxDur, expected time.Duration) error {
	res.DurationSec = int(r.out.Duration + 0.5)
	if r.exit == nil {
		return nil
	}
	if expected <= 0 {
		return r.refuse(nil)
	}
	if maxDur <= 0 {
		maxDur = fpcalcDefaultLength
	}
	asked, covered := min(expected, maxDur), fingerprintSpan(n)
	if covered < asked-partialReadSlack {
		return waxerr.Wrapf(waxerr.CodeIO, "fingerprint.fpcalc", r.exit, "fpcalc: %s; its fingerprint covers %.0f s of the %.0f s asked for",
			trimFpErr(r.stderr), covered.Seconds(), asked.Seconds())
	}
	res.Partial, res.ExitCode, res.Note = true, r.exit.ExitCode(), fpcalcNote(r.stderr)
	return nil
}

// fingerprintSpan is the audio a fingerprint of n values covers.
func fingerprintSpan(n int) time.Duration {
	if n == 0 {
		return 0
	}
	return time.Duration((float64(n)*chromaprintItemSeconds + chromaprintDelaySeconds) * float64(time.Second))
}

// runFpcalc invokes fpcalc with -json (and -raw when raw), bounding the output so a
// misbehaving binary cannot exhaust memory, and parses the JSON envelope. A non-zero exit
// that still printed a fingerprint is returned for the caller to judge, since fpcalc
// prints what it read before it reports a read error. It never passes -ignore-errors,
// which would hide that error behind a clean exit.
func runFpcalc(ctx context.Context, bin, path string, maxDur time.Duration, raw bool) (*fpcalcRun, error) {
	const op = "fingerprint.fpcalc"
	if bin == "" {
		bin = "fpcalc"
	}
	args := []string{"-json"}
	if raw {
		args = append(args, "-raw")
	}
	if maxDur > 0 {
		args = append(args, "-length", strconv.Itoa(int(maxDur.Seconds())))
	}
	args = append(args, path)

	cmd := exec.CommandContext(ctx, bin, args...)
	stderr := &tailWriter{max: 4 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// fpcalc output for one file is a single small JSON object; read at most the cap
	// so a runaway/hostile binary at the configured path cannot exhaust memory (the
	// bound is enforced while reading, not after buffering everything).
	const maxOutput = 8 << 20
	data, readErr := io.ReadAll(io.LimitReader(stdout, maxOutput+1))
	_, _ = io.Copy(io.Discard, stdout) // drain any overflow so fpcalc can exit
	waitErr := cmd.Wait()
	run := &fpcalcRun{stderr: strings.TrimSpace(string(stderr.buf))}
	if waitErr != nil {
		if ctx.Err() != nil {
			return nil, waxerr.FromContext(op, ctx.Err(), waxerr.CodeCanceled)
		}
		if !errors.As(waitErr, &run.exit) {
			return nil, waxerr.Wrapf(waxerr.CodeIO, op, waitErr, "fpcalc: %s", trimFpErr(run.stderr))
		}
	}
	if readErr != nil {
		return nil, run.refuse(waxerr.Wrap(waxerr.CodeIO, op, readErr))
	}
	if len(data) > maxOutput {
		return nil, run.refuse(waxerr.New(waxerr.CodeInvalid, op, "fpcalc output exceeds 8 MiB"))
	}
	var out fpcalcOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, run.refuse(waxerr.Wrapf(waxerr.CodeInvalid, op, err, "parsing fpcalc json"))
	}
	if len(out.Fingerprint) == 0 {
		return nil, run.refuse(waxerr.New(waxerr.CodeInvalid, op, "fpcalc returned no fingerprint"))
	}
	run.out = &out
	return run, nil
}

func trimFpErr(s string) string {
	if s == "" {
		return "exited non-zero"
	}
	return fpcalcNote(s)
}

// fpcalcNote is what fpcalc said, shortened: the line that names its error, which it
// prints last, else its last line, cut to 200 bytes.
func fpcalcNote(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	note := lines[len(lines)-1]
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "ERROR:") {
			note = lines[i]
			break
		}
	}
	note = strings.TrimSpace(note)
	if len(note) > 200 {
		note = note[:200] + "..."
	}
	return strings.ToValidUTF8(note, "")
}

// tailWriter keeps the last max bytes written to it, since a damaged file can make
// fpcalc's decoder report every frame before fpcalc names its error.
type tailWriter struct {
	buf []byte
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// ChromaprintTerms returns up to n min-hash terms for the inverted index over a
// Chromaprint vector: the smallest distinct hashes of consecutive 2-frame
// shingles. Chromaprint sub-values are 32-bit, so a shingle is 64 bits; hashing it
// to an int64 both fits the index column and spreads the terms so a shared term
// implies locally identical audio rather than a chance collision. Mirrors the
// pure-Go IndexTerms but hashes instead of bit-packing the wider values.
func ChromaprintTerms(sub []uint32, n int) []int64 {
	if len(sub) < 2 || n <= 0 {
		return nil
	}
	seen := make(map[int64]bool, len(sub))
	terms := make([]int64, 0, len(sub))
	var buf [8]byte
	for i := 0; i+1 < len(sub); i++ {
		binary.LittleEndian.PutUint32(buf[0:], sub[i])
		binary.LittleEndian.PutUint32(buf[4:], sub[i+1])
		term := int64(hash64(buf[:]) >> 1) // >>1 keeps it non-negative for the index column
		if !seen[term] {
			seen[term] = true
			terms = append(terms, term)
		}
	}
	slices.Sort(terms) // min-hash: keep the n smallest distinct shingle hashes
	if len(terms) > n {
		terms = terms[:n]
	}
	return terms
}

// hash64 is FNV-1a over the shingle bytes: cheap, well-distributed, deterministic.
func hash64(b []byte) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, c := range b {
		h ^= uint64(c)
		h *= prime
	}
	return h
}

// SimilarChromaprint returns the best bit-agreement in [0,1] between two
// Chromaprint vectors, searching a small frame-shift window so leading-silence
// differences do not defeat the match. 1.0 is identical; ~0.5 is unrelated.
func SimilarChromaprint(a, b []uint32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	best := 0.0
	for shift := -maxShift; shift <= maxShift; shift++ {
		if agree := chromaprintAgreement(a, b, shift); agree > best {
			best = agree
		}
	}
	return best
}

// chromaprintAgreement compares a[i] against b[i+shift] over their overlap and
// returns the fraction of agreeing bits across all 32 bits of each value.
func chromaprintAgreement(a, b []uint32, shift int) float64 {
	var matches, total int
	for i := range a {
		j := i + shift
		if j < 0 || j >= len(b) {
			continue
		}
		diff := bits.OnesCount32(a[i] ^ b[j])
		matches += chromaprintBits - diff
		total += chromaprintBits
	}
	if total < chromaprintBits*minOverlapFrames {
		return 0
	}
	return float64(matches) / float64(total)
}

// SimilarByAlgo dispatches the full-vector comparison to the function matching the
// stored fingerprint algorithm. The candidate query guarantees both vectors share
// one algorithm, so grouping never mixes the incomparable layouts.
func SimilarByAlgo(algoVersion int, a, b []uint32) float64 {
	if algoVersion == ChromaprintAlgoVersion {
		return SimilarChromaprint(a, b)
	}
	return Similar(a, b)
}
