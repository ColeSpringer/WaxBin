package decode

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"time"

	"github.com/colespringer/waxbin/waxerr"
)

// WAVDecoder decodes RIFF/WAVE PCM (integer 8/16/24/32-bit and 32-bit float) in
// pure Go. It is the always-available analysis decoder.
type WAVDecoder struct{}

// Name reports how doctor labels this decoder's coverage.
func (WAVDecoder) Name() string { return "pure-go (wav)" }

const wavOp = "decode.wav"

// Sanity bounds on header fields. The header is untrusted input, so a chunk
// claiming more bytes than the file holds, or an absurd sample rate/channel
// count, is rejected rather than turned into a huge allocation or an int64
// overflow in the frame arithmetic.
const (
	maxSampleRate = 3_000_000 // far above any real rate (~768 kHz)
	maxChannels   = 64
)

// Decode reads a WAV file into PCM, stopping after max of audio (0 == all).
func (WAVDecoder) Decode(ctx context.Context, path string, max time.Duration) (*PCM, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, wavOp, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, wavOp, err)
	}
	fileSize := fi.Size()
	r := bufio.NewReaderSize(f, 1<<16)

	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalid, wavOp, err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, waxerr.New(waxerr.CodeInvalid, wavOp, "not a RIFF/WAVE file")
	}

	var (
		format         uint16
		channels, bits int
		sampleRate     int
		haveFmt        bool
	)
	for {
		if ctx.Err() != nil {
			return nil, waxerr.FromContext(wavOp, ctx.Err(), waxerr.CodeIO)
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil, waxerr.New(waxerr.CodeInvalid, wavOp, "no data chunk before EOF")
			}
			return nil, waxerr.Wrap(waxerr.CodeIO, wavOp, err)
		}
		id := string(hdr[0:4])
		size := int64(binary.LittleEndian.Uint32(hdr[4:8]))

		switch id {
		case "fmt ":
			if size < 16 {
				return nil, waxerr.New(waxerr.CodeInvalid, wavOp, "short fmt chunk")
			}
			// Cap the allocation at the file size so a header claiming a huge fmt
			// chunk cannot force a multi-GB make(); a genuinely short read then errors.
			fmtBuf := make([]byte, minInt64(size, fileSize))
			if len(fmtBuf) < 16 {
				return nil, waxerr.New(waxerr.CodeInvalid, wavOp, "fmt chunk truncated")
			}
			if _, err := io.ReadFull(r, fmtBuf); err != nil {
				return nil, waxerr.Wrap(waxerr.CodeInvalid, wavOp, err)
			}
			format = binary.LittleEndian.Uint16(fmtBuf[0:2])
			channels = int(binary.LittleEndian.Uint16(fmtBuf[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(fmtBuf[4:8]))
			bits = int(binary.LittleEndian.Uint16(fmtBuf[14:16]))
			// WAVE_FORMAT_EXTENSIBLE carries the real format tag in its subformat.
			if format == 0xFFFE && size >= 26 {
				format = binary.LittleEndian.Uint16(fmtBuf[24:26])
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, waxerr.New(waxerr.CodeInvalid, wavOp, "data chunk before fmt")
			}
			return decodeWAVData(r, minInt64(size, fileSize), format, channels, bits, sampleRate, max)
		default:
			if _, err := io.CopyN(io.Discard, r, size); err != nil {
				return nil, waxerr.Wrap(waxerr.CodeIO, wavOp, err)
			}
		}
		if size%2 == 1 { // chunks are word-aligned with a pad byte
			if _, err := io.CopyN(io.Discard, r, 1); err != nil && err != io.EOF {
				return nil, waxerr.Wrap(waxerr.CodeIO, wavOp, err)
			}
		}
	}
}

func decodeWAVData(r io.Reader, size int64, format uint16, channels, bits, sampleRate int, max time.Duration) (*PCM, error) {
	// Validate format params before any size arithmetic so absurd header values
	// (e.g. a ~4e9 sample rate) cannot overflow the frame math below.
	if channels <= 0 || channels > maxChannels {
		return nil, waxerr.New(waxerr.CodeInvalid, wavOp, "invalid channel count in fmt chunk")
	}
	if sampleRate <= 0 || sampleRate > maxSampleRate {
		return nil, waxerr.New(waxerr.CodeInvalid, wavOp, "invalid sample rate in fmt chunk")
	}
	bytesPerSample := bits / 8
	if bytesPerSample <= 0 || bits%8 != 0 || bits > 32 {
		return nil, waxerr.New(waxerr.CodeInvalid, wavOp, "invalid bit depth")
	}
	frameBytes := bytesPerSample * channels

	wantBytes := size
	if max > 0 {
		// sampleRate is bounded above, so maxFrames*frameBytes stays well within int64.
		maxFrames := int64(max.Seconds() * float64(sampleRate))
		if mb := maxFrames * int64(frameBytes); mb < wantBytes {
			wantBytes = mb
		}
	}
	wantBytes -= wantBytes % int64(frameBytes) // whole frames only

	buf := make([]byte, wantBytes)
	// A data chunk header can claim more bytes than the file actually holds
	// (truncated/corrupt WAV). Decode only the bytes really read, snapped to whole
	// frames, so trailing zero bytes are not emitted as phantom silent samples
	// (which would skew the fingerprint).
	nRead, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, waxerr.Wrap(waxerr.CodeIO, wavOp, err)
	}
	nRead -= nRead % frameBytes

	n := nRead / bytesPerSample
	samples := make([]float32, n)
	for i := 0; i < n; i++ {
		samples[i] = sampleToFloat(buf[i*bytesPerSample:], format, bits)
	}
	return &PCM{SampleRate: sampleRate, Channels: channels, Samples: samples}, nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// sampleToFloat converts one little-endian PCM sample to a float32 in [-1, 1].
func sampleToFloat(b []byte, format uint16, bits int) float32 {
	const formatFloat = 3
	switch {
	case format == formatFloat && bits == 32:
		return math.Float32frombits(binary.LittleEndian.Uint32(b))
	case bits == 8: // 8-bit PCM is unsigned, centered at 128
		return (float32(b[0]) - 128) / 128
	case bits == 16:
		return float32(int16(binary.LittleEndian.Uint16(b))) / 32768
	case bits == 24:
		v := int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16
		if v&0x800000 != 0 {
			v |= ^0xFFFFFF // sign-extend 24->32
		}
		return float32(v) / 8388608
	case bits == 32:
		return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
	default:
		return 0
	}
}
