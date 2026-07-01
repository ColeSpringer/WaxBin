package fingerprint

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/bits"
	"os/exec"
	"slices"
	"strconv"
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
// fingerprint is an array of integers; without it, a compressed base64 string.
type fpcalcOutput struct {
	Duration    float64         `json:"duration"`
	Fingerprint json.RawMessage `json:"fingerprint"`
}

// ChromaprintRaw runs fpcalc to produce a raw Chromaprint sub-fingerprint vector
// for internal grouping, capped at maxDur. The vector is comparable across lossy
// encodings of one recording, like the pure-Go fingerprint, but is Chromaprint's
// own layout, so it is stored under ChromaprintAlgoVersion.
func ChromaprintRaw(ctx context.Context, bin, path string, maxDur time.Duration) ([]uint32, int, error) {
	out, err := runFpcalc(ctx, bin, path, maxDur, true)
	if err != nil {
		return nil, 0, err
	}
	sub, err := decodeRawFingerprint(out.Fingerprint)
	if err != nil {
		return nil, 0, err
	}
	return sub, int(out.Duration + 0.5), nil
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
// is Chromaprint-only, so this is the only fingerprint form it takes.
func ChromaprintCompressed(ctx context.Context, bin, path string, maxDur time.Duration) (string, int, error) {
	out, err := runFpcalc(ctx, bin, path, maxDur, false)
	if err != nil {
		return "", 0, err
	}
	var fp string
	if err := json.Unmarshal(out.Fingerprint, &fp); err != nil {
		return "", 0, waxerr.Wrapf(waxerr.CodeInvalid, "fingerprint.fpcalc", err, "parsing compressed fingerprint")
	}
	return fp, int(out.Duration + 0.5), nil
}

// runFpcalc invokes fpcalc with -json (and -raw when raw), bounding the output so a
// misbehaving binary cannot exhaust memory, and parses the JSON envelope.
func runFpcalc(ctx context.Context, bin, path string, maxDur time.Duration, raw bool) (*fpcalcOutput, error) {
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
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
	if waitErr != nil {
		if ctx.Err() != nil {
			return nil, waxerr.FromContext(op, ctx.Err(), waxerr.CodeCanceled)
		}
		msg := "fpcalc failed"
		if s := trimFpErr(stderr.String()); s != "" {
			msg = "fpcalc: " + s
		}
		return nil, waxerr.Wrapf(waxerr.CodeIO, op, waitErr, "%s", msg)
	}
	if readErr != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, readErr)
	}
	if len(data) > maxOutput {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "fpcalc output exceeds 8 MiB")
	}
	var out fpcalcOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "parsing fpcalc json")
	}
	if len(out.Fingerprint) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "fpcalc returned no fingerprint")
	}
	return &out, nil
}

func trimFpErr(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "..."
	}
	if s == "" {
		return "exited non-zero"
	}
	return s
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
