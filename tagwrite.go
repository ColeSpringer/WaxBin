package waxbin

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/loudness"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
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
			if waxerr.Is(err, waxerr.CodeCanceled) || ctx.Err() != nil {
				return c, err
			}
			if waxerr.Is(err, waxerr.CodeUnsupported) {
				// A file WaxLabel refuses to write at all is not a failure to chase.
				// WaxLabel reads WMA but never writes it, and WMA files gained loudness
				// rows as soon as WaxFlow could decode them; ReplayGainWriteback has no
				// format gate, so every run would report the same file as a fresh write
				// failure. The gain has nowhere to go, which is what an unrepresented
				// value means, so it is counted and diagnosed as one. The refusal covers
				// more than missing write support (a fragmented mp4, a chained ogg), so
				// the detail blames the file's form, not the container format.
				l.log.Warn("replaygain tag unwritable file", "path", string(r.Path), "err", err)
				c.unrepresented++
				diags := []model.FileDiagnostic{{
					Code: model.DiagTagWriteLost, Severity: model.SeverityWarn,
					Detail: "this file cannot take tag writes in its current form",
				}}
				if derr := l.store.PutFileDiagnostics(ctx, r.FilePID, model.OriginReplayGain, diags); derr != nil {
					l.log.Warn("replaygain diagnostics", "path", string(r.Path), "err", derr)
				}
			} else {
				// A hard failure landed nothing and proved nothing about the file, so
				// any standing drift row still says what is true: the gain is not on
				// disk. Clearing it here would let one transient lock or permission
				// error hide a real unrepresented mark from audit until the next run.
				l.log.Warn("replaygain tag write", "path", string(r.Path), "err", err)
				c.failed++
			}
			continue
		}
		// Recorded before the Changed gate: a value the format could not store leaves
		// the bytes unchanged, so it reports as a no-op while being precisely the case
		// the caller needs to hear about. This is also why the diagnostics go through
		// their own entry point, since a no-op never reaches UpdateFileStateIfUnchanged.
		var diags []model.FileDiagnostic
		lost := false
		for _, wn := range res.Warnings {
			// The write landed and only a post-commit step failed; log it so the
			// operator hears about the degraded durability, but it is not a loss.
			if wn.Code == meta.PostWriteWarningCode {
				l.log.Warn("replaygain tag post-write", "path", string(r.Path), "warning", wn.Message)
			}
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
		if err := l.store.PutFileDiagnostics(ctx, r.FilePID, model.OriginReplayGain, meta.MergeKeylessDiagnostics(diags)); err != nil {
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

// writeEnrichmentTags mirrors what the enrichment pass filled into the backing files:
// every item field with an on-disk tag key (a track's genre, bpm, isrc, composer and
// year, a book's asin, isbn, publisher, genre, year and narrator) plus an album's label
// fanned across its members. Off by default, and it runs at the end of an enrich pass.
// It shares writeReplayGainTags' shape (disk I/O outside any transaction, an optimistic
// file-state update that a concurrent scan or move makes a no-op, per-file counts and
// diagnostics), so read that first.
//
// An album's label is collected before the item walk and folded into the edits of any
// member file that walk is already rewriting, so a file carrying both an enriched field
// and an enriched album label is opened once rather than twice. The members left over
// (a file with no enriched field of its own, or one the item select drops as shared or
// virtual) are fanned out afterwards through writeBackFiles, which carries the guard
// those files need.
//
// Why it exists rather than the catalog simply keeping the values: the scanner rebuilds
// these columns from the file's tags on every content-changed rescan, so a value living
// only in the catalog is cleared the next time the file is retagged. Writing it to the
// file puts it where the scanner reads, which is also what makes it portable to any
// other tool. The enrichment marker is what made that loss permanent: a later pass
// skips a marked entity, so nothing refilled what the retag cleared. It reaches only
// fields the reader fills; bookFieldTagKeys records which book fields those are.
//
// ASIN, ISBN, and edition feed identity.BookKey, so a book whose parts were written is
// re-anchored from its primary part's post-write tags. Without that the next scan computes a
// different identity key and resolves a different item, orphaning the book's pid, play
// state and locks. reanchorBookIdentity re-reads the file, so it self-corrects when a
// write did not land.
func (l *Library) writeEnrichmentTags(ctx context.Context, sinceNS int64) (enrichWriteCounts, error) {
	var c enrichWriteCounts
	rows, err := l.store.EnrichmentWriteback(ctx, sinceNS)
	if err != nil {
		return c, err
	}
	// Album labels first, as a plan rather than a write: the item walk below folds each
	// one into the file it is already rewriting, and only what is left over is fanned out.
	labels, err := l.enrichmentAlbumLabelEdits(ctx, sinceNS)
	if err != nil {
		return c, err
	}
	w := meta.NewWriter()
	// Books re-anchor once per item, from the primary part, after every part is written.
	// primaryOf is captured for every book seen rather than only for one that wrote,
	// because the anchor must run even when the primary's own write failed.
	primaryOf := map[model.PID]model.PID{}
	var books []model.PID
	// A book whose primary part failed to write is abandoned mid-item: its remaining
	// parts are skipped. Writing them anyway would leave the parts carrying an
	// identifier the primary lacks, and since identity.BookKey is computed per file the
	// next scan would key them apart and split the book with no way back. Re-anchoring
	// cannot repair that, since it can only follow one file.
	abandoned := map[model.PID]bool{}
	for _, r := range rows {
		if ctx.Err() != nil {
			return c, ctx.Err()
		}
		if r.Kind == model.KindBook {
			if abandoned[r.ItemPID] {
				c.skipped++
				continue
			}
			if r.IsPrimary {
				if _, seen := primaryOf[r.ItemPID]; !seen {
					primaryOf[r.ItemPID] = r.FilePID
					books = append(books, r.ItemPID)
				}
			}
		}
		// A file this walk is about to rewrite takes its album's label in the same pass,
		// and is struck from the fan-out below. Only a track is ever an album member, so
		// a book row never finds one here.
		extra := labels[r.FilePID].edits
		delete(labels, r.FilePID)
		if ok := l.writeEnrichmentFile(ctx, w, r, extra, &c); !ok && r.Kind == model.KindBook && r.IsPrimary {
			abandoned[r.ItemPID] = true
		}
	}
	// Unconditional per book, like the edit write-back: reanchorBookIdentity re-reads
	// the primary part, so a book whose write did not land recomputes its stored key
	// and the re-key is a no-op.
	for _, itemPID := range books {
		l.reanchorBookIdentity(ctx, itemPID, primaryOf[itemPID])
	}
	if err := l.writeEnrichmentAlbumLabels(ctx, labels, &c); err != nil {
		return c, err
	}
	return c, nil
}

// writeEnrichmentFile applies one row's tag edits, plus any extra edits the caller folded
// in for this same file, and records the outcome in c: the unrepresented-value
// diagnostics, the write count, and the optimistic file-state update. It reports whether
// the write itself succeeded, which is what tells a book's caller to abandon the
// remaining parts when the primary failed. A row with nothing to write is a success that
// changed nothing.
//
// extra is how an album's label reaches a member file without a second full rewrite of
// that file.
func (l *Library) writeEnrichmentFile(ctx context.Context, w *meta.Writer, r model.EnrichedTagRow, extra []meta.TagEdit, c *enrichWriteCounts) bool {
	edits := append(enrichmentEdits(r), extra...)
	if len(edits) == 0 {
		return true
	}
	res, err := w.Apply(ctx, string(r.Path), edits)
	if err != nil {
		l.log.Warn("enrichment tag write", "path", string(r.Path), "err", err)
		c.failed++
		return false
	}
	var diags []model.FileDiagnostic
	lost := false
	for _, wn := range res.Warnings {
		if wn.Code == meta.PostWriteWarningCode {
			l.log.Warn("enrichment tag post-write", "path", string(r.Path), "warning", wn.Message)
		}
		if wn.Unrepresented {
			l.log.Warn("enrichment tag unrepresented", "path", string(r.Path), "key", wn.Key, "warning", wn.Message)
			lost = true
			diags = append(diags, model.FileDiagnostic{
				Code: model.DiagTagWriteLost, Severity: model.SeverityWarn,
				TagKey: wn.Key, Detail: wn.Message,
			})
		}
	}
	if lost {
		c.unrepresented++
	}
	if err := l.store.PutFileDiagnostics(ctx, r.FilePID, model.OriginEnrichment, meta.MergeKeylessDiagnostics(diags)); err != nil {
		l.log.Warn("enrichment diagnostics", "path", string(r.Path), "err", err)
	}
	if !res.Changed {
		return true
	}
	c.written++
	if _, err := l.store.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID:         r.FilePID,
		ExpectedSize:    r.Size,
		ExpectedMTimeNS: r.MTimeNS,
		NewSize:         res.Size,
		NewMTimeNS:      res.MTimeNS,
		NewContentHash:  res.ContentHash,
	}); err != nil {
		// The tags landed; only the file row's size/mtime/hash did not follow, which
		// the next scan repairs. Not a write failure.
		l.log.Warn("enrichment file-state update", "path", string(r.Path), "err", err)
	}
	return true
}

// enrichmentAlbumLabelEdits plans the album-label fan-out: the tag edit each member file
// should receive, keyed by file pid. The label lives on the album row, so it carries no
// field_provenance row and never appears in the per-item write-back select.
//
// It is a plan rather than a write so the item walk can fold a file's label into the one
// rewrite it was already going to do. Every enrichment label is collected, not just this
// pass's, for the reason the item select emits every enrichment field: a file being
// opened anyway costs nothing more to stamp fully, and stamping fully heals an earlier
// pass whose write failed. sinceNS marks which are recent enough to be worth opening a
// file for on their own; see albumLabelPlan.
func (l *Library) enrichmentAlbumLabelEdits(ctx context.Context, sinceNS int64) (map[model.PID]albumLabelPlan, error) {
	labels, err := l.store.EnrichedAlbumLabels(ctx, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[model.PID]albumLabelPlan, len(labels))
	for _, v := range labels {
		key, ok := meta.EntityFieldTagKey(v.EntityType, v.Field)
		if !ok {
			continue
		}
		files, err := l.store.EntityMemberFiles(ctx, v.EntityType, v.PID)
		if err != nil {
			l.log.Warn("enrichment album write-back members", "album", v.PID, "err", err)
			continue
		}
		for _, f := range files {
			// First album wins for a file two of them somehow claim, which cannot happen
			// through a track's single album_id but costs nothing to be explicit about.
			if _, seen := out[f.FilePID]; !seen {
				out[f.FilePID] = albumLabelPlan{
					edits:  []meta.TagEdit{{Key: key, Values: []string{v.Value}}},
					recent: v.UpdatedAt >= sinceNS,
				}
			}
		}
	}
	return out, nil
}

// albumLabelPlan is one member file's pending album label. recent says the label was
// written by the pass now finishing, which is what decides whether it is worth opening a
// file for on its own: an older label still rides along free when the item walk is
// opening that file anyway, and is otherwise left for the pass that refills it.
type albumLabelPlan struct {
	edits  []meta.TagEdit
	recent bool
}

// writeEnrichmentAlbumLabels writes the album labels the item walk did not already fold
// into a file it was rewriting: a member with no enriched field of its own, or one the
// item select drops because it is shared or carries an offset window. Only the labels
// this pass wrote are written here, since these open a file for the label alone and an
// unbounded fan-out would reopen every enriched album's members every run.
//
// It goes through writeBackFiles rather than writeEnrichmentFile precisely for those
// dropped files: the item select excludes them with its start_frames join, whereas
// EntityMemberFiles returns them on purpose and expects the caller's guard, which
// writeBackFiles carries along with the drift rows and the per-file failure accounting.
// It passes the enrichment origin, since a clean write clears that origin's whole set for
// the file and borrowing the edit origin would wipe drift a user's own edit recorded.
//
// An album's year needs nothing here: it lands as each member's own year provenance and
// rides the item path as DATE.
func (l *Library) writeEnrichmentAlbumLabels(ctx context.Context, plan map[model.PID]albumLabelPlan, c *enrichWriteCounts) error {
	const op = "waxbin.writeEnrichmentAlbumLabels"
	// Sorted, so one pass writes the leftovers in a stable order.
	pids := make([]model.PID, 0, len(plan))
	for pid := range plan {
		if plan[pid].recent {
			pids = append(pids, pid)
		}
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	for _, pid := range pids {
		edits := plan[pid].edits
		wbErr := &WriteBackError{}
		err := l.writeBackFiles(ctx, op, model.OriginEnrichment,
			[]model.ItemFileRef{{FilePID: pid}}, wbErr, nil,
			func(w *meta.Writer, path string) (*meta.WriteResult, error) {
				res, err := w.Apply(ctx, path, edits)
				if err == nil && res.Changed {
					c.written++
				}
				return res, err
			})
		if err != nil {
			return err
		}
		c.failed += len(wbErr.Failures)
	}
	return nil
}

// enrichWriteCounts tallies one enrichment write-back pass.
type enrichWriteCounts struct {
	written       int
	failed        int
	unrepresented int
	// skipped counts parts left unwritten because their book's primary part failed.
	skipped int
}

// enrichmentTagFields is the fixed per-kind order enrichmentEdits walks, so one file's
// write is reproducible regardless of Go's map ordering. Every field listed has an
// on-disk tag key the scanner reads back.
var enrichmentTagFields = map[model.Kind][]string{
	model.KindTrack: {"bpm", "composer", "genre", "isrc", "year"},
	model.KindBook:  {"asin", "description", "edition", "genre", "isbn", "narrator", "publisher", "subtitle", "year"},
}

// enrichmentEdits builds the tag edits for one file. Every key comes from
// meta.BookFieldTagKeys or meta.TagKeyForField, so a written key is one the reader
// reads back. An empty value produces no edit at all rather than a clear: this pass
// mirrors what enrichment supplied, and it is not the authority on what a file should
// not contain.
func enrichmentEdits(r model.EnrichedTagRow) []meta.TagEdit {
	var out []meta.TagEdit
	add := func(keys []string, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		for _, k := range keys {
			out = append(out, meta.TagEdit{Key: k, Values: []string{value}})
		}
	}
	for _, field := range enrichmentTagFields[r.Kind] {
		value := r.Fields[field]
		if r.Kind == model.KindBook {
			if keys, ok := meta.BookFieldTagKeys(field); ok {
				add(keys, value)
			}
			continue
		}
		if key, ok := meta.TagKeyForField(field); ok {
			add([]string{key}, value)
		}
	}
	return out
}
