package fingerprint

import (
	"encoding/base64"
	"strconv"

	"github.com/colespringer/waxbin/waxerr"
)

// ChromaprintAlgorithmDefault is Chromaprint's own id for the algorithm fpcalc runs when
// given no -algorithm (TEST2), the byte a compressed fingerprint opens with. fpcalc's raw
// output names no algorithm, so a vector it printed is taken to be under this one. It is
// not ChromaprintAlgoVersion, the catalog's id for a stored Chromaprint analysis.
const ChromaprintAlgorithmDefault = 1

// chromaprintAlgorithms counts Chromaprint's algorithms, TEST1 to TEST5 as ids 0 to 4.
const chromaprintAlgorithms = 5

// The compressed form spends 3 bits on the gap to each set bit, 7 meaning the gap
// continues in a 5-bit value of its own, and a 0 ending each value.
const (
	normalBits      = 3
	maxNormalValue  = 1<<normalBits - 1
	exceptionalBits = 5
	maxCompressed   = 1<<24 - 1 // the header counts values in three bytes
)

// EncodeCompressed writes vec as Chromaprint's compressed fingerprint under algorithm, the
// URL-safe unpadded base64 string fpcalc prints and AcoustID takes: a header of the
// algorithm byte and the value count in three big-endian bytes, then each value XORed with
// the one before it, written as the gaps between its set bits. An algorithm outside
// Chromaprint's own ids (the catalog's ChromaprintAlgoVersion among them) or a vector
// longer than the header can count is CodeInvalid.
func EncodeCompressed(vec []uint32, algorithm int) (string, error) {
	const op = "fingerprint.EncodeCompressed"
	if algorithm < 0 || algorithm >= chromaprintAlgorithms {
		return "", waxerr.New(waxerr.CodeInvalid, op, "not a Chromaprint algorithm: "+strconv.Itoa(algorithm))
	}
	if len(vec) > maxCompressed {
		return "", waxerr.New(waxerr.CodeInvalid, op, "a compressed fingerprint holds at most 16777215 values")
	}
	normal := make([]byte, 0, len(vec)*8)
	var exceptional []byte
	var prev uint32
	for _, v := range vec {
		x := v ^ prev
		prev = v
		last := 0
		for bit := 1; x != 0; bit, x = bit+1, x>>1 {
			if x&1 == 0 {
				continue
			}
			if gap := bit - last; gap >= maxNormalValue {
				normal = append(normal, maxNormalValue)
				exceptional = append(exceptional, byte(gap-maxNormalValue))
			} else {
				normal = append(normal, byte(gap))
			}
			last = bit
		}
		normal = append(normal, 0)
	}
	out := make([]byte, 4, 4+packedLen(len(normal), normalBits)+packedLen(len(exceptional), exceptionalBits))
	out[0] = byte(algorithm)
	out[1], out[2], out[3] = byte(len(vec)>>16), byte(len(vec)>>8), byte(len(vec))
	out = packBits(out, normal, normalBits)
	out = packBits(out, exceptional, exceptionalBits)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// DecodeCompressed reads a compressed fingerprint back into its vector and algorithm, the
// way Chromaprint's decompressor does: data past what the header counts is ignored, and a
// header counting more values than the data holds, or a value setting a bit past 32, is
// CodeInvalid.
func DecodeCompressed(s string) ([]uint32, int, error) {
	const op = "fingerprint.DecodeCompressed"
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, 0, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "decoding a compressed fingerprint")
	}
	if len(data) < 4 {
		return nil, 0, waxerr.New(waxerr.CodeInvalid, op, "a compressed fingerprint is shorter than its header")
	}
	algorithm := int(data[0])
	n := int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if n == 0 {
		return nil, algorithm, nil
	}
	body := data[4:]
	// A first pass finds where the n-th value ends and counts the long gaps, so a header
	// counting more than the data holds is refused without collecting anything.
	slots := len(body) * 8 / normalBits
	if n > slots {
		return nil, 0, waxerr.New(waxerr.CodeInvalid, op, "a compressed fingerprint holds fewer values than its header counts")
	}
	used, found, exceptions := 0, 0, 0
	for ; used < slots && found < n; used++ {
		switch unpackBits(body, used, normalBits) {
		case 0:
			found++
		case maxNormalValue:
			exceptions++
		}
	}
	if found < n {
		return nil, 0, waxerr.New(waxerr.CodeInvalid, op, "a compressed fingerprint holds fewer values than its header counts")
	}
	rest := body[packedLen(used, normalBits):]
	if len(rest) < packedLen(exceptions, exceptionalBits) {
		return nil, 0, waxerr.New(waxerr.CodeInvalid, op, "a compressed fingerprint is cut short in its long gaps")
	}
	vec := make([]uint32, 0, n)
	var cur uint32
	last, e := 0, 0
	for i := range used {
		v := unpackBits(body, i, normalBits)
		if v == 0 {
			if len(vec) > 0 {
				cur ^= vec[len(vec)-1]
			}
			vec = append(vec, cur)
			cur, last = 0, 0
			continue
		}
		gap := int(v)
		if v == maxNormalValue {
			gap += int(unpackBits(rest, e, exceptionalBits))
			e++
		}
		last += gap
		if last > 32 {
			return nil, 0, waxerr.New(waxerr.CodeInvalid, op, "a compressed fingerprint sets a bit past 32")
		}
		cur |= 1 << (last - 1)
	}
	return vec, algorithm, nil
}

// packedLen is how many bytes n values of width bits take.
func packedLen(n, width int) int { return (n*width + 7) / 8 }

// packBits appends vals, each width bits wide, as one bit stream filling each byte from
// its lowest bit, the way Chromaprint packs them.
func packBits(out, vals []byte, width int) []byte {
	start := len(out)
	out = append(out, make([]byte, packedLen(len(vals), width))...)
	stream := out[start:]
	for i, v := range vals {
		for b := range width {
			if v&(1<<b) != 0 {
				pos := i*width + b
				stream[pos/8] |= 1 << (pos % 8)
			}
		}
	}
	return out
}

// unpackBits reads value i of width bits from a stream packBits wrote.
func unpackBits(stream []byte, i, width int) byte {
	var v byte
	for b := range width {
		pos := i*width + b
		if stream[pos/8]&(1<<(pos%8)) != 0 {
			v |= 1 << b
		}
	}
	return v
}
