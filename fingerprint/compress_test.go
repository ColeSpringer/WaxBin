package fingerprint

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/waxerr"
)

// The cases below are Chromaprint's own, from tests/test_fingerprint_compressor.cpp and
// tests/test_fingerprint_decompressor.cpp in acoustid/chromaprint (read 2026-11-01).

// chromaprintLong is the decompressor test's real fingerprint: 19 values under
// algorithm 1, with the vector it holds.
const chromaprintLong = "AQAAEwkjrUmSJQpUHflR9mjSJMdZpcO_Imdw9dCO9Clu4_wQPvhCB01w6xAtXNcAp5RASgDBhDSCGGIAcwA"

var chromaprintLongVector = func() []uint32 {
	signed := []int32{-587455133, -591649759, -574868448, -576973520, -543396544, 1330439488, 1326360000,
		1326355649, 1191625921, 1192674515, 1194804466, 1195336818, 1165981042, 1165956451, 1157441379,
		1157441299, 1291679571, 1291673457, 1170079601}
	out := make([]uint32, len(signed))
	for i, v := range signed {
		out[i] = uint32(v)
	}
	return out
}()

// TestCompressedKnownVectors: each of Chromaprint's cases encodes to its bytes and decodes
// back to its vector and algorithm.
func TestCompressedKnownVectors(t *testing.T) {
	t.Parallel()
	b64 := base64.RawURLEncoding.EncodeToString
	for _, c := range []struct {
		name      string
		vec       []uint32
		algorithm int
		want      string
	}{
		{"OneItemOneBit", []uint32{1}, 0, b64([]byte{0, 0, 0, 1, 1})},
		{"OneItemThreeBits", []uint32{7}, 0, b64([]byte{0, 0, 0, 1, 73, 0})},
		{"OneItemOneBitExcept", []uint32{1 << 6}, 0, b64([]byte{0, 0, 0, 1, 7, 0})},
		{"OneItemOneBitExcept2", []uint32{1 << 8}, 0, b64([]byte{0, 0, 0, 1, 7, 2})},
		{"TwoItems", []uint32{1, 0}, 0, b64([]byte{0, 0, 0, 2, 65, 0})},
		{"TwoItemsNoChange", []uint32{1, 1}, 0, b64([]byte{0, 0, 0, 2, 1, 0})},
		{"BitAtPosition32", []uint32{0xA0820820}, 0, b64([]byte{0, 0, 0, 1, 182, 109, 1})},
		{"Long", chromaprintLongVector, 1, chromaprintLong},
	} {
		got, err := EncodeCompressed(c.vec, c.algorithm)
		if err != nil || got != c.want {
			t.Errorf("%s: encoded %q (err %v), want %q", c.name, got, err, c.want)
		}
		vec, algorithm, err := DecodeCompressed(c.want)
		if err != nil || !slices.Equal(vec, c.vec) || algorithm != c.algorithm {
			t.Errorf("%s: decoded %v under %d (err %v), want %v under %d", c.name, vec, algorithm, err, c.vec, c.algorithm)
		}
	}
}

// TestDecodeCompressedRefusesWhatChromaprintRefuses: a header claiming more values than
// the data holds, a set bit past position 32 directly or through an exception, and input
// that is not one at all are refused; trailing data past what the header counts is not.
func TestDecodeCompressedRefusesWhatChromaprintRefuses(t *testing.T) {
	t.Parallel()
	b64 := base64.RawURLEncoding.EncodeToString
	for _, c := range []struct {
		name string
		in   string
	}{
		{"Invalid1", b64([]byte{0, 255, 255, 255})},
		{"BitPastPosition32", b64([]byte{0, 0, 0, 1, 182, 109, 3})},
		{"ExceptionPastPosition32", b64([]byte{0, 0, 0, 1, 7, 26})},
		{"SecondItemPastPosition32", b64([]byte{0, 0, 0, 2, 129, 109, 219, 0})},
		{"SecondItemExceptionPastPosition32", b64([]byte{0, 0, 0, 2, 199, 1, 67, 3})},
		{"shorter than its header", b64([]byte{1, 0, 0})},
		{"not base64", "AQAA!!"},
	} {
		if vec, _, err := DecodeCompressed(c.in); !waxerr.Is(err, waxerr.CodeInvalid) || vec != nil {
			t.Errorf("%s: decoded %v (err %v), want CodeInvalid", c.name, vec, err)
		}
	}
	for _, c := range []struct {
		name      string
		in        []byte
		vec       []uint32
		algorithm int
	}{
		{"OneItemOneBitExceptWithTrailingData", []byte{0, 0, 0, 1, 7, 0, 255, 255, 255, 255}, []uint32{1 << 6}, 0},
		{"ZeroItems", []byte{1, 0, 0, 0, 0, 0, 0, 0}, nil, 1},
		{"ZeroItemsWithTrailingOnes", []byte{1, 0, 0, 0, 255, 255, 255, 255}, nil, 1},
		{"ZeroItemsWithTrailingBits", []byte{1, 0, 0, 0, 182, 109, 3, 0}, nil, 1},
	} {
		vec, algorithm, err := DecodeCompressed(b64(c.in))
		if err != nil || !slices.Equal(vec, c.vec) || algorithm != c.algorithm {
			t.Errorf("%s: decoded %v under %d (err %v), want %v under %d", c.name, vec, algorithm, err, c.vec, c.algorithm)
		}
	}
}

// TestCorruptedCompressedNeverMiscounts: every truncation and every one-bit flip of a real
// fingerprint either fails or decodes to as many values as its header says.
func TestCorruptedCompressedNeverMiscounts(t *testing.T) {
	t.Parallel()
	data, err := base64.RawURLEncoding.DecodeString(chromaprintLong)
	if err != nil {
		t.Fatal(err)
	}
	check := func(b []byte) {
		t.Helper()
		vec, _, err := DecodeCompressed(base64.RawURLEncoding.EncodeToString(b))
		if err != nil {
			return
		}
		if n := int(b[1])<<16 | int(b[2])<<8 | int(b[3]); len(vec) != n {
			t.Errorf("% x decoded %d values, its header says %d", b, len(vec), n)
		}
	}
	for n := range len(data) {
		check(data[:n])
	}
	for bit := range len(data) * 8 {
		b := slices.Clone(data)
		b[bit/8] ^= 1 << (bit % 8)
		check(b)
	}
}

// TestCompressedLengthAgreesWithTheHeader: the count the header carries is the vector's
// length, for vectors long enough to fill all three of its bytes.
func TestCompressedLengthAgreesWithTheHeader(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 948, 1 << 16, 1<<16 + 300} {
		vec := make([]uint32, n)
		for i := range vec {
			vec[i] = uint32(i) * 2654435761
		}
		s, err := EncodeCompressed(vec, ChromaprintAlgorithmDefault)
		if err != nil {
			t.Fatalf("encode %d: %v", n, err)
		}
		head, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := int(head[1])<<16 | int(head[2])<<8 | int(head[3]); got != n || head[0] != ChromaprintAlgorithmDefault {
			t.Errorf("%d values: header says %d under %d", n, got, head[0])
		}
		if back, _, err := DecodeCompressed(s); err != nil || !slices.Equal(back, vec) {
			t.Errorf("%d values: round trip failed (err %v)", n, err)
		}
	}
}

// TestEncodeNeverWritesTheCatalogsAnalysisID: the catalog's id for a stored Chromaprint
// analysis is not a Chromaprint algorithm, so encoding under it is refused, as is any
// id outside Chromaprint's five; a vector longer than the header can count is refused too.
// (The pure-Go AlgoVersion is 1, Chromaprint's TEST2 as well, so only a caller can keep a
// pure-Go vector out.)
func TestEncodeNeverWritesTheCatalogsAnalysisID(t *testing.T) {
	t.Parallel()
	for _, algorithm := range []int{ChromaprintAlgoVersion, -1, 5, 256} {
		if s, err := EncodeCompressed([]uint32{1, 2, 3}, algorithm); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("algorithm %d: encoded %q (err %v), want CodeInvalid", algorithm, s, err)
		}
	}
	if _, err := EncodeCompressed(make([]uint32, 1<<24), ChromaprintAlgorithmDefault); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("a vector of 2^24 values = %v, want CodeInvalid", err)
	}
}

// FuzzCompressedRoundTrip: any vector encodes and decodes back to itself under its
// algorithm, and any input either fails to decode or decodes to its header's count.
func FuzzCompressedRoundTrip(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 1, 7, 2})
	long, _ := base64.RawURLEncoding.DecodeString(chromaprintLong)
	f.Add(long)
	f.Fuzz(func(t *testing.T, data []byte) {
		vec := make([]uint32, len(data)/4)
		for i := range vec {
			vec[i] = binary.LittleEndian.Uint32(data[i*4:])
		}
		s, err := EncodeCompressed(vec, ChromaprintAlgorithmDefault)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		back, algorithm, err := DecodeCompressed(s)
		if err != nil || algorithm != ChromaprintAlgorithmDefault || !slices.Equal(back, vec) {
			t.Fatalf("round trip of %v = %v under %d (err %v)", vec, back, algorithm, err)
		}
		if got, _, err := DecodeCompressed(base64.RawURLEncoding.EncodeToString(data)); err == nil && len(data) >= 4 {
			if n := int(data[1])<<16 | int(data[2])<<8 | int(data[3]); len(got) != n {
				t.Fatalf("% x decoded %d values, its header says %d", data, len(got), n)
			}
		}
	})
}

// TestEncodedRawMatchesFpcalcsCompressedForm: encoding the vector fpcalc prints with -raw
// reproduces the string it prints without it, for the same file. It needs fpcalc on PATH.
func TestEncodedRawMatchesFpcalcsCompressedForm(t *testing.T) {
	t.Parallel()
	bin, err := exec.LookPath("fpcalc")
	if err != nil {
		t.Skip("fpcalc is not on PATH")
	}
	path := filepath.Join(t.TempDir(), "song.wav")
	if err := os.WriteFile(path, testaudio.EncodeAs(t, "wav", "", 44100, testaudio.ReferenceSignal(44100, 30*time.Second)), 0o644); err != nil {
		t.Fatal(err)
	}
	fpcalc := func(args ...string) json.RawMessage {
		t.Helper()
		out, err := exec.CommandContext(context.Background(), bin, append(args, path)...).Output()
		if err != nil {
			t.Fatalf("fpcalc %v: %v", args, err)
		}
		var o fpcalcOutput
		if err := json.Unmarshal(out, &o); err != nil {
			t.Fatalf("fpcalc %v printed %q: %v", args, out, err)
		}
		return o.Fingerprint
	}
	sub, err := decodeRawFingerprint(fpcalc("-json", "-raw"))
	if err != nil {
		t.Fatal(err)
	}
	var want string
	if err := json.Unmarshal(fpcalc("-json"), &want); err != nil {
		t.Fatal(err)
	}
	if got, err := EncodeCompressed(sub, ChromaprintAlgorithmDefault); err != nil || got != want {
		t.Errorf("encoded raw = %q (err %v), want fpcalc's %q", got, err, want)
	}
}

// TestDecodeCompressedRefusesAHostileCountCheaply: a header counting more values than its
// data could hold is refused, and a large body with no value ending in it is refused
// without collecting what it read. Not parallel: AllocsPerRun reads the process's
// allocation count.
func TestDecodeCompressedRefusesAHostileCountCheaply(t *testing.T) {
	head := []byte{1, 0xff, 0xff, 0xff}
	short := base64.RawURLEncoding.EncodeToString(append(slices.Clone(head), 0, 0, 0))
	if vec, _, err := DecodeCompressed(short); !waxerr.Is(err, waxerr.CodeInvalid) || vec != nil {
		t.Fatalf("a count past the data = %d values (err %v), want CodeInvalid", len(vec), err)
	}
	// 6 MiB holds 16,777,216 three-bit slots, enough for the header's count, so the count
	// is not refused up front and the first pass reads every slot finding no value ends.
	body := make([]byte, 6<<20)
	for i := range body {
		body[i] = 0xff
	}
	hostile := base64.RawURLEncoding.EncodeToString(append(slices.Clone(head), body...))
	allocs := testing.AllocsPerRun(2, func() {
		if _, _, err := DecodeCompressed(hostile); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Fatalf("an all-ones body = %v, want CodeInvalid", err)
		}
	})
	if allocs > 8 {
		t.Errorf("refusing a 6 MiB body with no value in it took %.0f allocations, want a handful", allocs)
	}
}
