package waxbin

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/colespringer/waxbin/loudness"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
)

// writeReplayGainTags mirrors the catalog's computed ReplayGain (track + album, in
// one pass after album aggregation) into files on disk. It is off by default and
// runs at the end of the analyze pass. Disk I/O is kept off any write transaction:
// each file is edited and re-hashed outside a transaction, then a brief optimistic
// update records the new size/mtime/hash only if a concurrent scan/move has not
// touched the file (else it is skipped and the next scan reconciles). Because a tag
// edit preserves audio essence, the item's identity is unchanged and the scanner's
// fast-path recognizes WaxBin's own write instead of re-hashing it.
//
// It returns per-file counts rather than a bare written total: a write-back failure
// is non-fatal (the measurement is in the catalog either way), but a run where every
// write failed must not be indistinguishable from a run with nothing to write.
func (l *Library) writeReplayGainTags(ctx context.Context) (rgWriteCounts, error) {
	var c rgWriteCounts
	rows, err := l.store.ReplayGainWriteback(ctx)
	if err != nil {
		return c, err
	}
	w := meta.NewWriter()
	for _, r := range rows {
		if ctx.Err() != nil {
			return c, ctx.Err()
		}
		edits := replayGainEdits(r)
		res, err := w.Apply(ctx, string(r.Path), edits)
		if err != nil {
			l.log.Warn("replaygain tag write", "path", string(r.Path), "err", err)
			c.failed++
			continue
		}
		// Recorded before the Changed gate: a value the format could not store leaves
		// the bytes unchanged, so it reports as a no-op while being precisely the case
		// the caller needs to hear about. This is also why the diagnostics go through
		// their own entry point, since a no-op never reaches UpdateFileStateIfUnchanged.
		var diags []model.FileDiagnostic
		lost := false
		for _, wn := range res.Warnings {
			if wn.Unrepresented {
				l.log.Warn("replaygain tag unrepresented", "path", string(r.Path), "key", wn.Key, "warning", wn.Message)
				lost = true
				diags = append(diags, model.FileDiagnostic{
					Code: model.DiagTagWriteLost, Severity: model.SeverityWarn,
					TagKey: wn.Key, Detail: wn.Message,
				})
			}
		}
		// Once per FILE, not once per warning, so it means the same thing as the written
		// and failed counters it is printed beside. A warning is fanned out one entry per
		// key, and a single file's edit sets up to four keys (track/album gain and peak),
		// so counting warnings would report one bad file as four.
		if lost {
			c.unrepresented++
		}
		// Always called, even with no diagnostics: this writer replaces its own rows
		// wholesale, so a run that comes back clean clears its own stale ones.
		if err := l.store.PutFileDiagnostics(ctx, r.FilePID, model.OriginReplayGain, diags); err != nil {
			l.log.Warn("replaygain diagnostics", "path", string(r.Path), "err", err)
		}
		if !res.Changed {
			continue
		}
		// The tags are on disk from here on, so this file is written no matter what the
		// catalog bookkeeping below does.
		c.written++
		if _, err := l.store.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
			FilePID:         r.FilePID,
			ExpectedSize:    r.Size,
			ExpectedMTimeNS: r.MTimeNS,
			NewSize:         res.Size,
			NewMTimeNS:      res.MTimeNS,
			NewContentHash:  res.ContentHash,
		}); err != nil {
			// This is not a write failure. The tags landed; only the file row's
			// size/mtime/hash did not follow. Counting it under failed would tell the user
			// their write failed when it succeeded. The stale row heals itself, since the
			// next scan sees the changed bytes and re-hashes.
			l.log.Warn("replaygain file-state update", "path", string(r.Path), "err", err)
		}
	}
	return c, nil
}

// rgWriteCounts tallies one ReplayGain write-back pass.
type rgWriteCounts struct {
	written       int
	failed        int
	unrepresented int
}

// replayGainEdits builds the format-aware ReplayGain tag edits for one file. Opus
// carries R128 gains (integer Q7.8, referenced to -23 LUFS) as its native
// convention; every other format uses the REPLAYGAIN_* string tags (dB gain, linear
// peak) understood by Vorbis comments and ID3 TXXX alike. Album tags are written
// only when the file belongs to an album aggregate.
func replayGainEdits(r model.ReplayGainRow) []meta.TagEdit {
	if r.Codec == "opus" {
		edits := []meta.TagEdit{{Key: "R128_TRACK_GAIN", Values: []string{r128Gain(r.TrackGainDB)}}}
		if r.HasAlbum {
			edits = append(edits, meta.TagEdit{Key: "R128_ALBUM_GAIN", Values: []string{r128Gain(r.AlbumGainDB)}})
		} else {
			// Not in an album (any more): clear any stale album gain so the tags mirror
			// the catalog. Clearing an absent tag is a no-op (no rewrite).
			edits = append(edits, meta.TagEdit{Key: "R128_ALBUM_GAIN"})
		}
		return edits
	}
	edits := []meta.TagEdit{
		{Key: "REPLAYGAIN_TRACK_GAIN", Values: []string{fmtGainDB(r.TrackGainDB)}},
		{Key: "REPLAYGAIN_TRACK_PEAK", Values: []string{fmtPeak(r.TrackPeak)}},
	}
	if r.HasAlbum {
		edits = append(edits,
			meta.TagEdit{Key: "REPLAYGAIN_ALBUM_GAIN", Values: []string{fmtGainDB(r.AlbumGainDB)}},
			meta.TagEdit{Key: "REPLAYGAIN_ALBUM_PEAK", Values: []string{fmtPeak(r.AlbumPeak)}},
		)
	} else {
		// Clear stale album tags left from when this file belonged to an album, so the
		// on-disk tags keep mirroring the catalog. Clearing absent tags is a no-op.
		edits = append(edits,
			meta.TagEdit{Key: "REPLAYGAIN_ALBUM_GAIN"},
			meta.TagEdit{Key: "REPLAYGAIN_ALBUM_PEAK"},
		)
	}
	return edits
}

// fmtGainDB formats a ReplayGain gain as the conventional "-6.35 dB" string.
func fmtGainDB(db float64) string { return fmt.Sprintf("%.2f dB", db) }

// fmtPeak formats a linear sample peak with ReplayGain's usual precision.
func fmtPeak(peak float64) string { return strconv.FormatFloat(peak, 'f', 6, 64) }

// r128ReferenceLUFS is the EBU R128 reference the Opus R128_*_GAIN tags target.
const r128ReferenceLUFS = -23.0

// r128Gain converts a ReplayGain 2.0 gain (dB, referenced to loudness.ReferenceLUFS,
// -18 LUFS) into the Opus R128_*_GAIN integer: Q7.8 fixed point referenced to -23
// LUFS. The reference difference is derived (not hardcoded) so the two stay in step,
// then the value is scaled by 256 and rounded. (The Opus header output_gain remains
// an upstream WaxLabel gap and is intentionally not written.)
func r128Gain(rgDB float64) string {
	offset := loudness.ReferenceLUFS - r128ReferenceLUFS // -18 - (-23) = 5 dB
	q78 := int(math.Round((rgDB - offset) * 256.0))
	return strconv.Itoa(q78)
}
