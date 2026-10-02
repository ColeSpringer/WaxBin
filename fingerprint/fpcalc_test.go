package fingerprint

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxbin/internal/testfpcalc"
)

// rawJSON is fpcalc -raw -json output reporting duration seconds and n values.
func rawJSON(duration float64, n int) string {
	vals := make([]string, n)
	for i := range vals {
		vals[i] = fmt.Sprint(i * 7919)
	}
	return fmt.Sprintf(`{"duration": %.2f, "fingerprint": [%s]}`, duration, strings.Join(vals, ","))
}

const decodeNote = "ERROR: Error decoding audio frame (Invalid data found when processing input)"

// TestChromaprintRawDetailKeepsACoveringPartialRead: fpcalc reports a read error with an
// exit code of its own choosing after printing the fingerprint of what it read, so the
// fingerprint is kept when it covers the span asked for (the file's duration capped at
// the length) and refused when the decode stopped short of it. 948 values span the first
// 120 seconds (948 x 1365/11025 s plus Chromaprint's 2.6 s of context), 221 the first 30,
// and 300 only about 40.
func TestChromaprintRawDetailKeepsACoveringPartialRead(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		stdout   string
		code     int
		expected time.Duration
		keep     bool
	}{
		{"exit 3 after the capped span", rawJSON(300.4, 948), 3, 300 * time.Second, true},
		{"exit 1 after the capped span", rawJSON(300.4, 948), 1, 300 * time.Second, true},
		{"a short file read to its end", rawJSON(30, 221), 3, 30 * time.Second, true},
		{"a read that stopped early", rawJSON(300.4, 300), 3, 300 * time.Second, false},
		{"an empty fingerprint", rawJSON(300.4, 0), 3, 300 * time.Second, false},
		{"no output", "", 2, 300 * time.Second, false},
		{"an unknown file duration", rawJSON(300.4, 948), 3, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := testfpcalc.Write(t, c.stdout, decodeNote, c.code)
			r, err := ChromaprintRawDetail(ctx, bin, "song.flac", MaxAnalyze, c.expected)
			if !c.keep {
				if err == nil || !strings.Contains(err.Error(), "Error decoding audio frame") {
					t.Fatalf("result %+v (err %v), want a refusal naming fpcalc's error", r, err)
				}
				// A read judged short says how short, so it reads apart from one that
				// produced nothing.
				if short := c.name == "a read that stopped early"; short != strings.Contains(err.Error(), "40 s of the 120 s") {
					t.Errorf("refusal = %v, want the covered and asked spans only for the short read", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("partial read refused: %v", err)
			}
			if !r.Partial || r.ExitCode != c.code || r.Note != decodeNote || len(r.Sub) == 0 {
				t.Errorf("result = partial %v, exit %d, note %q, %d values; want the kept fingerprint marked partial",
					r.Partial, r.ExitCode, r.Note, len(r.Sub))
			}
		})
	}

	bin := testfpcalc.Write(t, rawJSON(300.4, 948), "", 0)
	r, err := ChromaprintRawDetail(ctx, bin, "song.flac", MaxAnalyze, 300*time.Second)
	if err != nil || r.Partial || len(r.Sub) != 948 || r.DurationSec != 300 {
		t.Fatalf("clean run = %+v (err %v), want 948 values, 300 s, not partial", r, err)
	}
}

// TestChromaprintCompressedDetailKeepsACoveringPartialRead: the AcoustID form carries its
// value count in its header, so a partial read is judged the same way, against fpcalc's
// own 120-second default when no length is passed.
func TestChromaprintCompressedDetailKeepsACoveringPartialRead(t *testing.T) {
	ctx := context.Background()
	compressed := func(n int) string {
		return base64.RawURLEncoding.EncodeToString([]byte{1, byte(n >> 16), byte(n >> 8), byte(n), 0x5a, 0xc3})
	}
	out := func(n int) string { return `{"duration": 300.4, "fingerprint": "` + compressed(n) + `"}` }

	r, err := ChromaprintCompressedDetail(ctx, testfpcalc.Write(t, out(948), decodeNote, 3), "song.flac", 0, 300*time.Second)
	if err != nil || !r.Partial || r.Compressed != compressed(948) || r.DurationSec != 300 {
		t.Fatalf("covering partial = %+v (err %v), want the fingerprint kept", r, err)
	}
	if r, err := ChromaprintCompressedDetail(ctx, testfpcalc.Write(t, out(300), decodeNote, 3), "song.flac", 0, 300*time.Second); err == nil {
		t.Fatalf("short partial = %+v, want a refusal", r)
	}
}

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

// TestFpcalcNoteIsItsClosingError: a damaged file can make fpcalc's decoder report every
// frame before fpcalc names its error on the last line, so the note a kept read carries and
// the error a refused one returns are that line, short and whole.
func TestFpcalcNoteIsItsClosingError(t *testing.T) {
	ctx := context.Background()
	noisy := strings.Repeat("[mp3float @ 0x5599] Header missing é\n", 20000) + decodeNote + "\n"
	r, err := ChromaprintRawDetail(ctx, testfpcalc.Write(t, rawJSON(300.4, 948), noisy, 3), "song.mp3", MaxAnalyze, 300*time.Second)
	if err != nil {
		t.Fatalf("covering partial read refused: %v", err)
	}
	if r.Note != decodeNote {
		t.Errorf("note = %.80q, want fpcalc's closing error", r.Note)
	}
	_, err = ChromaprintRawDetail(ctx, testfpcalc.Write(t, "", noisy, 2), "song.mp3", MaxAnalyze, 300*time.Second)
	if err == nil || !strings.Contains(err.Error(), decodeNote) || len(err.Error()) > 1024 || !utf8.ValidString(err.Error()) {
		t.Errorf("refusal = %.120q, want a short message naming fpcalc's closing error", fmt.Sprint(err))
	}
	long := strings.Repeat("é", 300)
	if got := fpcalcNote(long); len(got) > 210 || !utf8.ValidString(got) {
		t.Errorf("a long note is %d bytes (valid UTF-8 %v), want it cut short and whole", len(got), utf8.ValidString(got))
	}
}
