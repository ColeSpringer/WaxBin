package testaudio

import (
	"encoding/binary"
	"math"
	"math/rand"
)

// MusicalPartials and AltPartials are two distinct sets of frequencies spread
// across the fingerprint's band range, for generating "same recording" vs
// "different recording" test signals.
var (
	MusicalPartials = []float64{262, 330, 392, 523, 660, 784, 988, 1175, 1480, 1760}
	AltPartials     = []float64{220, 277, 349, 440, 587, 698, 880, 1046, 1318, 1568}
)

// RichSignal synthesizes a broadband, music-like mono signal: several partials,
// each amplitude-modulated by its own slow LFO, so every fingerprint band
// carries time-varying energy (the realistic case the grouping fingerprint
// targets, unlike a single tone or chirp).
func RichSignal(rate int, durSec float64, partials []float64, seed int64) []float32 {
	n := int(durSec * float64(rate))
	out := make([]float32, n)
	rng := rand.New(rand.NewSource(seed))
	lfoFreq := make([]float64, len(partials))
	lfoPhase := make([]float64, len(partials))
	for i := range partials {
		lfoFreq[i] = 0.3 + rng.Float64()*1.5
		lfoPhase[i] = rng.Float64() * 2 * math.Pi
	}
	for i := range out {
		t := float64(i) / float64(rate)
		var v float64
		for p, f := range partials {
			amp := 0.5 * (1 + math.Sin(2*math.Pi*lfoFreq[p]*t+lfoPhase[p]))
			v += amp * math.Sin(2*math.Pi*f*t)
		}
		out[i] = float32(0.3 * v / float64(len(partials)))
	}
	return out
}

// Reencode simulates a lossy transcode of one recording: a gain change plus
// low-level noise. The resulting samples differ byte-for-byte from the input but
// represent the same recording, so their fingerprints should still match.
func Reencode(samples []float32, gain float64, seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]float32, len(samples))
	for i, s := range samples {
		out[i] = float32(float64(s)*gain + (rng.Float64()-0.5)*0.02)
	}
	return out
}

// EncodeWAV16Multi wraps interleaved float32 samples as a minimal RIFF/WAVE 16-bit
// PCM file with the given channel count (samples are frame-interleaved, length a
// multiple of channels). It is the multichannel companion to EncodeWAV16, used to
// price the analyze pass's per-channel resample on wide layouts (5.1, 7.1).
func EncodeWAV16Multi(rate, channels int, interleaved []float32) []byte {
	if channels < 1 {
		channels = 1
	}
	dataLen := len(interleaved) * 2
	blockAlign := channels * 2
	buf := make([]byte, 0, 44+dataLen)
	buf = append(buf, "RIFF"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(36+dataLen))
	buf = append(buf, "WAVE"...)
	buf = append(buf, "fmt "...)
	buf = binary.LittleEndian.AppendUint32(buf, 16)
	buf = binary.LittleEndian.AppendUint16(buf, 1) // PCM
	buf = binary.LittleEndian.AppendUint16(buf, uint16(channels))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(rate))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(rate*blockAlign))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(blockAlign))
	buf = binary.LittleEndian.AppendUint16(buf, 16)
	buf = append(buf, "data"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(dataLen))
	for _, s := range interleaved {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		buf = binary.LittleEndian.AppendUint16(buf, uint16(int16(math.Round(float64(s)*32767))))
	}
	return buf
}

// EncodeWAV16 wraps mono float32 samples as a minimal RIFF/WAVE 16-bit PCM file. It
// delegates to EncodeWAV16Multi at one channel. The output stays byte-identical to
// the standalone mono encoder its many callers key on, and the two copies of the
// encoding cannot drift apart.
func EncodeWAV16(rate int, samples []float32) []byte {
	return EncodeWAV16Multi(rate, 1, samples)
}
