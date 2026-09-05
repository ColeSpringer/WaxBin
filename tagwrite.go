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

// writeEnrichmentTags mirrors what enrichment filled into the backing files: every item
// field with an on-disk tag key (a track's genre, bpm, isrc, composer and year, a book's
// asin, isbn, publisher, genre, year and narrator) plus an album's label fanned across
// its members. Off by default, and it runs at the end of an enrich pass. It shares
// writeReplayGainTags' shape (disk I/O outside any transaction, an optimistic file-state
// update that a concurrent scan or move makes a no-op, per-file counts and diagnostics),
// so read that first.
//
// What it writes is what is owed: every file whose enrichment values are newer than the
// file's settle stamp (see enrichedTagSelect), or the scope's share of them for a scoped
// run. A landed write settles the file at the newest value it carried, and so does a
// value that can never land (a file WaxLabel refuses to write, a shared file), which is
// diagnosed as lost or refused and counted as unrepresented rather than chased again. A
// failed write settles nothing and records the writer's drift row, so a read-only mount
// or a transient error costs one pass rather than the value, and a canceled pass or one
// run with write-tags off leaves its files owed for the next pass that writes.
//
// An album's label rides the item rows, so a file carrying both an enriched field and
// an enriched album label is opened once rather than twice. The members still owed the
// label that the item walk does not open (a file with no enriched field of its own, or
// one the item select drops as shared or virtual) are written afterwards on their own.
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
func (l *Library) writeEnrichmentTags(ctx context.Context, scope *model.EnrichScope) (enrichWriteCounts, error) {
	var c enrichWriteCounts
	rows, err := l.store.EnrichmentWriteback(ctx, scope)
	if err != nil {
		return c, err
	}
	// The members still owed a label, fetched first so the item walk can strike the
	// ones it opens anyway; what is left is written on its own.
	labels, err := l.store.EnrichedAlbumLabelFiles(ctx, scope)
	if err != nil {
		return c, err
	}
	leftover := make(map[model.PID]model.EntityFieldFile, len(labels))
	for _, f := range labels {
		// First album wins for a file two of them somehow claim, which cannot happen
		// through a track's single album_id but costs nothing to be explicit about.
		if _, seen := leftover[f.FilePID]; !seen {
			leftover[f.FilePID] = f
		}
	}
	w := meta.NewWriter()
	// Books re-anchor once per item, from the primary part, after every part is written.
	// primaryOf is captured for every book seen rather than only for one that wrote,
	// because the anchor must run even when the primary's own write failed.
	primaryOf := map[model.PID]model.PID{}
	var books []model.PID
	// A book whose primary part could not be written is abandoned mid-item: its
	// remaining parts are skipped, and marked the way the primary was, so the next pass
	// reopens the whole book together or leaves all of it alone. Writing them anyway
	// would leave the parts carrying an identifier the primary lacks, and since
	// identity.BookKey is computed per file the next scan would key them apart and split
	// the book with no way back. Re-anchoring cannot repair that, since it can only
	// follow one file.
	abandoned := map[model.PID]model.FileDiagnostic{}
	for _, r := range rows {
		if ctx.Err() != nil {
			return c, ctx.Err()
		}
		if r.Kind == model.KindBook {
			if mark, ok := abandoned[r.ItemPID]; ok {
				c.skipped++
				if mark.Code == model.DiagTagWriteLost {
					l.noteEnrichmentLoss(ctx, r.FilePID, string(r.Path), mark, r.Newest)
				} else {
					l.noteEnrichmentDrift(ctx, r.FilePID, string(r.Path), mark.Detail)
				}
				continue
			}
			// A book with nothing left to write is settled below without a rewrite, so
			// re-reading its primary would only cost a parse.
			if r.IsPrimary && len(r.Fields) > 0 {
				if _, seen := primaryOf[r.ItemPID]; !seen {
					primaryOf[r.ItemPID] = r.FilePID
					books = append(books, r.ItemPID)
				}
			}
		}
		// The album label rides the row, so the one rewrite carries it and the file is
		// struck from the leftovers. Only a track is ever an album member, so a book row
		// carries none.
		var extra []meta.TagEdit
		settleAt := r.Newest
		if r.Label != "" {
			extra = []meta.TagEdit{{Key: albumLabelTagKey, Values: []string{r.Label}}}
			settleAt = max(settleAt, r.LabelUpdatedAt)
		}
		delete(leftover, r.FilePID)
		failure, err := l.writeEnrichmentFile(ctx, w, r, extra, settleAt, &c)
		if err != nil {
			return c, err
		}
		if failure != nil && r.Kind == model.KindBook && r.IsPrimary {
			abandoned[r.ItemPID] = model.FileDiagnostic{
				Code: failure.Code, Severity: model.SeverityWarn,
				Detail: "skipped behind the book's primary part, which could not be written",
			}
		}
	}
	// Unconditional per book that wrote, like the edit write-back: reanchorBookIdentity
	// re-reads the primary part, so a book whose write did not land recomputes its
	// stored key and the re-key is a no-op.
	for _, itemPID := range books {
		l.reanchorBookIdentity(ctx, itemPID, primaryOf[itemPID])
	}
	return c, l.writeEnrichmentAlbumLabels(ctx, w, leftover, &c)
}

// albumLabelTagKey is the on-disk key an album label is written under, from the same
// map the entity edit write-back reads.
var albumLabelTagKey = func() string {
	key, ok := meta.EntityFieldTagKey(model.MergeAlbum, "label")
	if !ok {
		panic("no tag key for an album label")
	}
	return key
}()

// writeEnrichmentFile applies one row's tag edits, plus any extra edits the caller folded
// in for this same file, and records the outcome in c: the diagnostics, the write count,
// the settle stamp, and the optimistic file-state update. A write that did not land comes
// back as the diagnostic recorded for it, which is what tells a book's caller to abandon
// the remaining parts and how to mark them; a landed write, or a row with nothing to
// write, comes back nil. A cancellation is returned rather than recorded, since it says
// nothing about the file.
//
// settleAt is the newest enrichment value the edits carry, which is what the file is
// settled at once they land or are found unable to. extra is how an album's label
// reaches a member file without a second full rewrite of that file.
func (l *Library) writeEnrichmentFile(ctx context.Context, w *meta.Writer, r model.EnrichedTagRow, extra []meta.TagEdit, settleAt int64, c *enrichWriteCounts) (*model.FileDiagnostic, error) {
	path := string(r.Path)
	edits := append(enrichmentEdits(r), extra...)
	if len(edits) == 0 {
		// Nothing left to write, so nothing is owed: the values a rescan cleared are
		// gone from the catalog too, and a drift row about them would be stale.
		if err := l.store.PutFileDiagnostics(ctx, r.FilePID, model.OriginEnrichment, nil); err != nil {
			l.log.Warn("enrichment diagnostics", "path", path, "err", err)
		}
		l.settleEnrichmentWrite(ctx, r.FilePID, path, settleAt)
		return nil, nil
	}
	res, err := w.Apply(ctx, path, edits)
	if err != nil {
		if waxerr.Is(err, waxerr.CodeCanceled) || ctx.Err() != nil {
			return nil, err
		}
		var d model.FileDiagnostic
		if waxerr.Is(err, waxerr.CodeUnsupported) {
			// WaxLabel refuses the file in its current form (it reads WMA but never
			// writes it; a fragmented mp4, a chained ogg), so no retry would change the
			// outcome and the detail blames the file's form, not the container format.
			l.log.Warn("enrichment tag unwritable file", "path", path, "err", err)
			c.unrepresented++
			d = model.FileDiagnostic{
				Code: model.DiagTagWriteLost, Severity: model.SeverityWarn,
				Detail: "this file cannot take tag writes in its current form",
			}
		} else {
			l.log.Warn("enrichment tag write", "path", path, "err", err)
			c.failed++
			d = model.FileDiagnostic{Code: model.DiagTagWriteUnsynced, Severity: model.SeverityWarn, Detail: err.Error()}
		}
		if d.Code == model.DiagTagWriteLost {
			l.noteEnrichmentLoss(ctx, r.FilePID, path, d, settleAt)
		} else {
			l.noteEnrichmentDrift(ctx, r.FilePID, path, d.Detail)
		}
		return &d, nil
	}
	var diags []model.FileDiagnostic
	lost := false
	for _, wn := range res.Warnings {
		if wn.Code == meta.PostWriteWarningCode {
			l.log.Warn("enrichment tag post-write", "path", path, "warning", wn.Message)
		}
		if wn.Unrepresented {
			l.log.Warn("enrichment tag unrepresented", "path", path, "key", wn.Key, "warning", wn.Message)
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
	// Always called, even with nothing to record: the write landed, so the drift row a
	// failed pass left is cleared along with the writer's other stale rows.
	if err := l.store.PutFileDiagnostics(ctx, r.FilePID, model.OriginEnrichment, meta.MergeKeylessDiagnostics(diags)); err != nil {
		l.log.Warn("enrichment diagnostics", "path", path, "err", err)
	}
	l.settleEnrichmentWrite(ctx, r.FilePID, path, settleAt)
	if !res.Changed {
		return nil, nil
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
		l.log.Warn("enrichment file-state update", "path", path, "err", err)
	}
	return nil, nil
}

// noteEnrichmentDrift records a write that did not land, beside the rows a landed write
// left, which still hold. The file stays owed unless the caller settles it.
func (l *Library) noteEnrichmentDrift(ctx context.Context, filePID model.PID, path, detail string) {
	d := model.FileDiagnostic{Code: model.DiagTagWriteUnsynced, Severity: model.SeverityWarn, Detail: detail}
	if err := l.store.AddFileDiagnostic(ctx, filePID, model.OriginEnrichment, d); err != nil {
		l.log.Warn("enrichment diagnostics", "path", path, "err", err)
	}
}

// noteEnrichmentLoss records a value that can never land, replacing the writer's set,
// and settles the file: nothing about it is worth retrying, and a file left owed would
// have the next pass write a book's parts without their primary.
func (l *Library) noteEnrichmentLoss(ctx context.Context, filePID model.PID, path string, d model.FileDiagnostic, settleAt int64) {
	if err := l.store.PutFileDiagnostics(ctx, filePID, model.OriginEnrichment, []model.FileDiagnostic{d}); err != nil {
		l.log.Warn("enrichment diagnostics", "path", path, "err", err)
	}
	l.settleEnrichmentWrite(ctx, filePID, path, settleAt)
}

// settleEnrichmentWrite stamps the file as settled up to settleAt. A failure to stamp
// only costs a reopen on the next pass, so it is logged rather than surfaced.
func (l *Library) settleEnrichmentWrite(ctx context.Context, filePID model.PID, path string, settleAt int64) {
	if err := l.store.SettleEnrichmentWrite(ctx, filePID, settleAt); err != nil {
		l.log.Warn("enrichment settle", "path", path, "err", err)
	}
}

// writeEnrichmentAlbumLabels writes the album labels still owed to files the item walk
// did not open: a member with no enriched field of its own, or one the item select drops
// because it is shared or carries an offset window.
//
// A shared or virtual file is refused rather than opened: its tags belong to every item
// that shares it, so the label is recorded as unsynced drift, counted as unrepresented,
// and settled so the refusal is not repeated every pass. The rest go through
// writeEnrichmentFile as a row with no fields of its own.
//
// An album's year needs nothing here: it lands as each member's own year provenance and
// rides the item path as DATE.
func (l *Library) writeEnrichmentAlbumLabels(ctx context.Context, w *meta.Writer, leftover map[model.PID]model.EntityFieldFile, c *enrichWriteCounts) error {
	// Sorted, so one pass writes the leftovers in a stable order.
	pids := make([]model.PID, 0, len(leftover))
	for pid := range leftover {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	for _, pid := range pids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f := leftover[pid]
		path := string(f.Path)
		if f.Shared {
			c.unrepresented++
			l.noteEnrichmentDrift(ctx, pid, path, "on-disk tag write-back is unavailable for a file shared by multiple items")
			l.settleEnrichmentWrite(ctx, pid, path, f.UpdatedAt)
			continue
		}
		key, ok := meta.EntityFieldTagKey(f.EntityType, f.Field)
		if !ok {
			continue
		}
		row := model.EnrichedTagRow{
			FilePID: pid, Kind: model.KindTrack, Path: f.Path,
			Size: f.Size, MTimeNS: f.MTimeNS, Newest: f.UpdatedAt,
		}
		edits := []meta.TagEdit{{Key: key, Values: []string{f.Value}}}
		if _, err := l.writeEnrichmentFile(ctx, w, row, edits, f.UpdatedAt, c); err != nil {
			return err
		}
	}
	return nil
}

// enrichWriteCounts tallies one enrichment write-back pass. unrepresented counts files
// whose value could not land and will not be retried: a lossy write, a file WaxLabel
// refuses to write, or a shared file refused for the label. failed counts files a later
// pass retries. skipped counts parts left unwritten because their book's primary part
// could not be written.
type enrichWriteCounts struct {
	written       int
	failed        int
	unrepresented int
	skipped       int
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
