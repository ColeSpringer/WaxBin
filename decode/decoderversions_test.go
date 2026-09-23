package decode

import (
	"testing"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/format"
)

// decoderVersions pins WaxFlow's per-codec decoder versions as of the pinned module.
// When a WaxFlow bump moves one, TestWaxFlowDecoderVersionsPinned fails so the question
// analyze.effectiveVersion's comment asks (does loudness.AnalysisVersion move?) is asked
// rather than skipped; update the table once it is answered.
//
// The seven the 05f3032 bump added are new here, and none of the twelve already here
// moved, so no measurement went stale for a decoder change; loudness.AnalysisVersion
// moves in that bump for other reasons.
var decoderVersions = map[string]string{
	"pcm":         "pcm-1",
	"flac":        "flac-dec-1",
	"mp3":         "mp3-dec-1",
	"alac":        "alac-dec-1",
	"aac-lc":      "aac-dec-3",
	"he-aac":      "aac-hedec-2",
	"wavpack":     "wavpack-dec-1",
	"ape":         "ape-dec-1",
	"wma":         "wma-dec-2",
	"wmalossless": "wmalossless-dec-1",
	"wmapro":      "wmapro-dec-2",
	"wmavoice":    "wmavoice-dec-1",
	"musepack":    "musepack-dec-1",
	"vorbis":      "vorbis-dec-2",
	"opus":        "opus-dec-2",
	"alaw":        "g711-alaw-dec-1",
	"mulaw":       "g711-mulaw-dec-1",
	"ima-adpcm":   "ima-adpcm-dec-1",
	"ms-adpcm":    "ms-adpcm-dec-1",
}

func TestWaxFlowDecoderVersionsPinned(t *testing.T) {
	for _, id := range format.Decoders() {
		want, ok := decoderVersions[string(id)]
		got := format.DecoderVersion(id)
		switch {
		case !ok:
			t.Errorf("WaxFlow decodes %q, which is not pinned here; decide whether loudness.AnalysisVersion moves, then pin it", id)
		case got != want:
			t.Errorf("WaxFlow's %q decoder moved from %q to %q; decide whether loudness.AnalysisVersion moves, then update the pin", id, want, got)
		}
	}
	for id := range decoderVersions {
		if format.DecoderVersion(codec.ID(id)) == "" {
			t.Errorf("%q is pinned here but WaxFlow no longer decodes it", id)
		}
	}
}
