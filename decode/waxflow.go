package decode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/opus"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/format"
	flowerr "github.com/colespringer/waxflow/waxerr"

	"github.com/colespringer/waxbin/waxerr"
)

// decoderName labels the decoder in doctor's coverage table.
const decoderName = "waxflow"

// ErrUnsupported reports that this build cannot decode an input at all: an
// unrecognized container, or a recognized one whose codec has no decoder (AC-3
// in MP4). The analyze pass skips such a file and retries it on a future run,
// since a later WaxFlow may decode it.
//
// It is set only when opening the input fails, and only when nothing recognized
// the bytes: openErr says which failures those are. A file whose magic matched
// and whose headers are then damaged is corrupt (CodeInvalid), and so is any
// failure once decoding has begun, however it is coded upstream. Burying those as
// a silent skip would retry them forever. Test for it with errors.Is.
var ErrUnsupported = errors.New("decode: unsupported input")

// Measurement is a whole-file loudness measurement in WaxFlow's dB domain.
// TruePeakDB and LoudnessRange are available upstream but deliberately
// unstored: the catalog has no columns for them.
//
// For an Ogg Opus file the values, and the chunks handed to the tap, leave out the
// header output gain the decoder applies, since the ReplayGain write-back sets it:
// kept in, a re-measure would fold the same gain in again, and the waveform, stored
// against an essence hash that masks the header, would go stale on every write.
type Measurement struct {
	IntegratedLUFS float64 // math.Inf(-1) for silence
	SamplePeakDB   float64 // math.Inf(-1) for silence
	// HeaderGainDB is the Ogg Opus header output gain that was removed from the two
	// values above, and 0 everywhere else.
	HeaderGainDB float64
	// InputDamage lists what the tolerant read worked around (a truncated stream, a
	// lost frame sync), complete because Measure reads to the end; nil when clean.
	InputDamage []string
}

// Engine decodes audio. It is safe for concurrent use.
type Engine struct {
	wf  *waxflow.Engine
	log *slog.Logger
}

// New returns an Engine. A nil logger uses slog.Default.
func New(log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{wf: waxflow.New(waxflow.WithLogger(log)), log: log}
}

// Measure decodes path end to end and measures its loudness. tap, when non-nil,
// is called with each decoded chunk's planar channel slices at the source's own
// rate and layout. That is the seam the waveform accumulator rides, so the
// waveform costs no second decode. The tap runs on the decoding goroutine, so
// blocking it pauses the decode, and its slices are valid only for the duration
// of the call.
//
// Memory is O(1) in the track length: the whole file streams through the meter
// and nothing is buffered, which is why Measure has no duration cap where Mono does.
//
// The returned error classifies by phase, which is why Measure opens the input
// itself instead of leaving it to one combined call. ErrUnsupported (the open
// failed, so skip the file) is distinct from any other error (the decode began
// and then failed, so the bytes are bad or the read was). Cancellation surfaces
// as a canceled error, not a partial measurement.
func (e *Engine) Measure(ctx context.Context, path string, tap func(chans [][]float32)) (*Measurement, error) {
	f, med, err := e.open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	defer med.Close()

	hg := headerGainDB(med)
	var wfTap func([][]float32) error
	if tap != nil {
		// WaxBin's taps cannot fail, so the adapter absorbs WaxFlow's error
		// return rather than leaking it to every caller.
		wfTap = func(chans [][]float32) error { tap(chans); return nil }
		if hg != 0 {
			// Scale a copy: the chunk is the meter's too.
			scale := float32(math.Pow(10, -hg/20))
			var scaled [][]float32
			wfTap = func(chans [][]float32) error {
				scaled = scaleChans(scaled, chans, scale)
				tap(scaled)
				return nil
			}
		}
	}
	res, err := e.wf.AnalyzeMedia(ctx, med, waxflow.AnalyzeOptions{Tap: wfTap})
	if err != nil {
		// Stream phase. Never ErrUnsupported, whatever the upstream code says:
		// a mid-decode failure means the bytes are bad, not the format.
		return nil, mapErr("decode.Measure", err)
	}
	// -Inf silence stays -Inf through the subtraction.
	return &Measurement{
		IntegratedLUFS: res.IntegratedLUFS - hg,
		SamplePeakDB:   res.SamplePeakDB - hg,
		HeaderGainDB:   hg,
		InputDamage:    res.InputWarnings,
	}, nil
}

// scaleChans writes chans times scale into dst, reusing its buffers, and returns it.
func scaleChans(dst, chans [][]float32, scale float32) [][]float32 {
	if len(dst) != len(chans) {
		dst = make([][]float32, len(chans))
	}
	for c, src := range chans {
		d := dst[c][:0]
		for _, v := range src {
			d = append(d, v*scale)
		}
		dst[c] = d
	}
	return dst
}

// headerGainDB is the OpusHead output gain of an Ogg Opus file in dB, else 0. The Ogg
// gate matches tagwrite.replayGainEdits: WaxBin writes no other Opus header, so any
// other measurement keeps whatever gain its header applies.
func headerGainDB(med format.Media) float64 {
	info := med.Info()
	if info.Container != "ogg" {
		return 0
	}
	t := info.Default()
	if t.Codec != codec.Opus {
		return 0
	}
	cfg, err := opus.ParseOpusHead(t.CodecConfig)
	if err != nil {
		return 0
	}
	return float64(cfg.Gain) / 256
}

// Mono decodes at most max of path as mono PCM at rate (0 = the source rate).
// A zero or negative max decodes the whole file.
//
// Unlike Measure this buffers, so max is load-bearing and must not be "cleaned
// up" for symmetry: the fingerprint wants a bounded head of the track, which at
// its analysis rate is a few MB, where an unbounded whole-file mono decode of an
// audiobook is not.
//
// The mix is an amplitude average, matching PCM.Mono. WaxFlow's own mix node is
// deliberately not used: it is energy-normalized (0.7071*(L+R), a +3 dB
// difference) and, because its matrix gain exceeds 1, it silently engages the
// chain's true-peak limiter, which would flatten the signal the fingerprint keys
// on. Never set ChainSpec.Channels or ChainSpec.Dynamics here.
func (e *Engine) Mono(ctx context.Context, path string, rate int, max time.Duration) (*PCM, error) {
	f, med, err := e.open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	defer med.Close()

	chain, err := e.chain(med, rate, path)
	if err != nil {
		return nil, err
	}
	defer chain.Release()

	af := chain.Format()
	// A non-positive rate would make the max cap below silently unbounded (limit
	// stays -1), decoding a whole track into memory. A decoded stream with no rate
	// is malformed; reject it rather than risk the OOM.
	if af.Rate <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, "decode.Mono", "decoded stream reports a non-positive sample rate")
	}
	buf := audio.Get(af, audio.StandardChunk)
	defer audio.Put(buf)
	chans := make([][]float32, af.Channels)
	// Chunks never exceed StandardChunk frames, so this scratch never regrows.
	scratch := make([]float32, 0, audio.StandardChunk)

	limit := int64(-1)
	if max > 0 {
		limit = int64(max) * int64(af.Rate) / int64(time.Second)
	}

	out := &PCM{SampleRate: af.Rate, Channels: 1}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := chain.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, mapErr("decode.Mono", err)
		}
		for c := range chans {
			chans[c] = buf.ChanF(c)
		}
		mono := MixMono(scratch, chans)
		if limit >= 0 {
			if room := limit - int64(len(out.Samples)); int64(len(mono)) > room {
				mono = mono[:room]
			}
		}
		out.Samples = append(out.Samples, mono...)
		if limit >= 0 && int64(len(out.Samples)) >= limit {
			break
		}
	}
	return out, nil
}

// chain builds the decode chain, converting to float at rate. It retries at the
// source rate when the resampler refuses the ratio: resample.bankFor rejects a
// conversion needing more taps than it will build, which a pathological coprime
// source rate can provoke. Native samples the caller can resample coarsely beat
// no samples at all.
//
// Note the overload this navigates: CodeUnsupportedFormat from chain
// construction means "cannot resample this ratio", where the same code from the
// open means "cannot decode this input". Which call failed is what separates
// them, not the code, so this never returns ErrUnsupported.
func (e *Engine) chain(med format.Media, rate int, path string) (*dsp.Chain, error) {
	track := med.Info().Default()
	c, err := dsp.NewChain(dsp.NewSource(med, track.Fmt), dsp.ChainSpec{Rate: rate, Float: true})
	if err == nil {
		return c, nil
	}
	if rate == 0 || flowerr.CodeOf(err) != flowerr.CodeUnsupportedFormat {
		return nil, mapErr("decode.Mono: chain", err)
	}
	e.log.Warn("decode: resampler refused the target rate; decoding at the source rate",
		"path", path, "rate", rate, "source_rate", track.Fmt.Rate, "err", err)
	c, err = dsp.NewChain(dsp.NewSource(med, track.Fmt), dsp.ChainSpec{Float: true})
	if err != nil {
		return nil, mapErr("decode.Mono: chain", err)
	}
	return c, nil
}

// open opens path for decoding. It returns the file alongside the media because
// the source borrows it: the caller closes the media, then the file.
//
// This is the phase boundary. An open failure is the only place ErrUnsupported
// is born, because it is the only point at which "this build cannot decode this"
// is what a failure means: format.Open sniffs the container and eagerly builds
// the decoder, so a missing decoder fails here, while damaged frames cannot fail
// until something reads them.
func (e *Engine) open(ctx context.Context, path string) (*os.File, format.Media, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeIO, "decode: open file", err)
	}
	// A path that is not a regular file is refused here, and that says something
	// about the catalog, not the format, so it is an error rather than a skip.
	src, err := container.FileSource(f)
	if err != nil {
		f.Close()
		return nil, nil, mapErr("decode: source", err)
	}
	// The hint only breaks ties; content sniffing is primary. format.resolve
	// lowercases it and strips the dot.
	ext := filepath.Ext(path)
	med, err := e.wf.OpenStream(container.BindContext(ctx, src), ext)
	if err != nil {
		err = openErr(ctx, src, ext, err)
		f.Close()
		return nil, nil, err
	}
	return f, med, nil
}

// openErr classifies a failed OpenStream. The unsupported codes are ErrUnsupported,
// and so is a refusal from a demuxer the extension alone chose: WaxFlow falls back to
// the extension when no magic matches, so a healthy Layer II file named .mp3 is
// refused as malformed. Sniffing again without the extension tells the two apart.
func openErr(ctx context.Context, src container.Source, ext string, err error) error {
	code := flowerr.CodeOf(err)
	unsupported := code == flowerr.CodeUnsupportedFormat || code == flowerr.CodeUnsupportedSource
	if !unsupported && ext != "" {
		_, perr := format.Probe(container.BindContext(ctx, src), "", nil)
		unsupported = flowerr.CodeOf(perr) == flowerr.CodeUnsupportedFormat
	}
	if unsupported {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return mapErr("decode: open stream", err)
}

// MixMono returns an amplitude-average mono mix of the planar chans, using dst's
// capacity as scratch (pass dst[:0] or nil). It is the planar twin of PCM.Mono
// and must stay numerically identical to it: both feed the same waveform code,
// and two mixes that disagree would make a streamed waveform differ from a
// buffered one.
//
// It is emphatically not WaxFlow's mix: that one is energy-preserving and
// engages the chain limiter. See Engine.Mono.
//
// Like PCM.Mono, a single-channel input returns chans[0] itself rather than a
// copy, so the result may alias the caller's chunk and is valid only until that
// chunk is reused. Copy it to keep it.
func MixMono(dst []float32, chans [][]float32) []float32 {
	if len(chans) == 0 {
		return dst[:0]
	}
	if len(chans) == 1 {
		return chans[0]
	}
	n := len(chans[0])
	out := dst[:0]
	if cap(out) < n {
		out = make([]float32, 0, n)
	}
	// Accumulate in float32 and divide by the channel count, in channel order:
	// PCM.Mono's arithmetic exactly, so the two agree bit for bit.
	nch := float32(len(chans))
	for i := 0; i < n; i++ {
		var sum float32
		for c := range chans {
			sum += chans[c][i]
		}
		out = append(out, sum/nch)
	}
	return out
}

// Coverage reports the codecs this build decodes for analysis, for doctor.
//
// It is display-only. Nothing branches on it, and nothing may: a pre-filter
// built from this would be a vocabulary sync point between WaxLabel's codec
// names, WaxBin's catalog labels, and WaxFlow's codec IDs, whose failure mode is
// silently skipping every file of some format. (It would fire immediately, since
// WaxLabel canonicalizes "AAC LC" to "AAC" where WaxFlow's ID is "aac-lc".)
// Deriving it rather than hand-listing it keeps it from drifting, at the cost of
// reporting WaxFlow's IDs rather than WaxBin's catalog labels. A wrong label here
// is visible in doctor, where a wrong gate would be invisible in production.
func Coverage() []FormatSupport {
	ids := format.Decoders()
	out := make([]FormatSupport, 0, len(ids))
	for _, id := range ids {
		out = append(out, FormatSupport{Codec: string(id), Decoder: decoderName, Analysis: true})
	}
	return out
}

// mapErr translates WaxFlow's error vocabulary into WaxBin's, so it stops at
// this package. It never yields ErrUnsupported: only the open call classifies
// that, by phase rather than by code.
// The default arm covers every code it does not name (internal among them), so
// CodeIO from here is not only an unreadable source.
func mapErr(op string, err error) error {
	switch flowerr.CodeOf(err) {
	case flowerr.CodeUnsupportedFormat, flowerr.CodeUnsupportedSource:
		return waxerr.Wrap(waxerr.CodeUnsupported, op, err)
	case flowerr.CodeMalformedInput:
		return waxerr.Wrap(waxerr.CodeInvalid, op, err)
	case flowerr.CodeSourceUnreadable:
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	case flowerr.CodeInvalidRequest:
		return waxerr.Wrap(waxerr.CodeInvalid, op, err)
	case flowerr.CodeCanceled:
		return waxerr.Wrap(waxerr.CodeCanceled, op, err)
	default:
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
}
