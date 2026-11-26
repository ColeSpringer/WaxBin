package waxbin

import (
	"context"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/internal/pathx"
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
	rows, err := l.store.ReplayGainWriteback(ctx)
	if err != nil {
		return rgWriteCounts{}, err
	}
	return l.writeReplayGainRows(ctx, rows)
}

// writeReplayGainRows writes the rows writeReplayGainTags read.
func (l *Library) writeReplayGainRows(ctx context.Context, rows []model.ReplayGainRow) (rgWriteCounts, error) {
	var c rgWriteCounts
	w := meta.NewWriter()
	for _, r := range rows {
		if ctx.Err() != nil {
			return c, ctx.Err()
		}
		// The library may have been flagged read-only since the select; the gain stays
		// owed and the first pass after the flag clears writes it.
		if ro, err := l.store.LibraryReadOnly(ctx, r.LibraryID); err != nil {
			return c, err
		} else if ro {
			l.log.Debug("replaygain tag write skipped in a read-only library", "path", string(r.Path))
			continue
		}
		edits, gain := replayGainEdits(r)
		var opts []meta.ApplyOption
		if gain != nil {
			opts = append(opts, meta.WithOutputGain(*gain))
		}
		res, err := w.Apply(ctx, string(r.Path), edits, opts...)
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

// replayGainEdits builds the format-aware ReplayGain tag edits for one file, plus the
// header output gain where the file has one. Opus carries R128 gains (integer Q7.8,
// referenced to -23 LUFS); every other format uses the REPLAYGAIN_* string tags (dB
// gain, linear peak). Album tags are written only when the file belongs to an album
// aggregate.
//
// In an Ogg file the Opus header brings the album (the track, standalone) to the -18
// LUFS ReplayGain reference, capped where the stored peak would pass full scale since
// every decoder applies it, and each R128 tag is its whole -23 gain less the header.
// Opus elsewhere has a header nothing here reaches, so its tags carry the whole gain.
func replayGainEdits(r model.ReplayGainRow) (edits []meta.TagEdit, headerGain *int) {
	if r.Codec == "opus" {
		header := 0
		if r.Container == "ogg" {
			gain, peak := r.TrackGainDB, r.TrackPeak
			if r.HasAlbum {
				gain, peak = r.AlbumGainDB, r.AlbumPeak
			}
			header = q78(gain)
			if peak > 0 {
				header = min(header, int(math.Floor(-20*math.Log10(peak)*256)))
			}
			headerGain = &header
			// RFC 7845 asks an Opus file not to carry these, and beside the header
			// another tagger's leftovers would have a player apply the gain twice.
			for _, k := range []string{"REPLAYGAIN_TRACK_GAIN", "REPLAYGAIN_TRACK_PEAK", "REPLAYGAIN_ALBUM_GAIN", "REPLAYGAIN_ALBUM_PEAK"} {
				edits = append(edits, meta.TagEdit{Key: k})
			}
		}
		edits = append(edits, meta.TagEdit{Key: "R128_TRACK_GAIN", Values: []string{strconv.Itoa(r128Gain(r.TrackGainDB) - header)}})
		if r.HasAlbum {
			edits = append(edits, meta.TagEdit{Key: "R128_ALBUM_GAIN", Values: []string{strconv.Itoa(r128Gain(r.AlbumGainDB) - header)}})
		} else {
			// Not in an album (any more): clear any stale album gain so the tags mirror
			// the catalog. Clearing an absent tag is a no-op (no rewrite).
			edits = append(edits, meta.TagEdit{Key: "R128_ALBUM_GAIN"})
		}
		return edits, headerGain
	}
	edits = []meta.TagEdit{
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
	return edits, nil
}

// fmtGainDB formats a ReplayGain gain as the conventional "-6.35 dB" string.
func fmtGainDB(db float64) string { return fmt.Sprintf("%.2f dB", db) }

// fmtPeak formats a linear sample peak with ReplayGain's usual precision.
func fmtPeak(peak float64) string { return strconv.FormatFloat(peak, 'f', 6, 64) }

// r128ReferenceLUFS is the EBU R128 reference the Opus R128_*_GAIN tags target.
const r128ReferenceLUFS = -23.0

// q78 converts a gain in dB to the Q7.8 fixed-point integer both the R128 tags and
// the Opus header carry.
func q78(db float64) int { return int(math.Round(db * 256.0)) }

// r128Gain converts a ReplayGain 2.0 gain (dB, referenced to loudness.ReferenceLUFS,
// -18 LUFS) into the Opus R128_*_GAIN integer: Q7.8 fixed point referenced to -23
// LUFS. The reference difference is derived (not hardcoded) so the two stay in step.
func r128Gain(rgDB float64) int {
	offset := loudness.ReferenceLUFS - r128ReferenceLUFS // -18 - (-23) = 5 dB
	return q78(rgDB - offset)
}

// writeEnrichmentTags mirrors what enrichment filled into the backing files: every item
// field with an on-disk tag key (a track's genre, bpm, isrc, composer and year, a book's
// asin, isbn, publisher, genre, year and narrator) plus an album's label and the album and
// release-group ids it matched, fanned across the members. An id the edition tier decided
// is not written, and an id goes on every member file or none (enrichedIDJoins in the
// store). Off by default, and it runs at the end of an enrich pass. It shares
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
// label that the item walk does not open (a file with no enriched field of its own, or a
// virtual track's, which the item select drops) are written afterwards on their own. A
// file several items share is refused wherever it is met, by the item walk when it
// carries an enriched field of its own and by the label pass otherwise, so it is counted
// and settled once either way.
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
	rows, err := l.store.EnrichmentWriteback(ctx, scope)
	if err != nil {
		return enrichWriteCounts{}, err
	}
	// The members still owed an album's label or ids, fetched first so the item walk can
	// strike the ones it opens anyway; what is left is written on its own.
	albums, err := l.store.EnrichedAlbumFiles(ctx, scope)
	if err != nil {
		return enrichWriteCounts{}, err
	}
	// The selects leave out read-only libraries, whose files are counted here instead;
	// the write loop adds any flagged since.
	held, err := l.store.EnrichmentHeldReadOnly(ctx, scope)
	if err != nil {
		return enrichWriteCounts{}, err
	}
	idsHeld, err := l.store.EnrichmentIDsHeld(ctx, scope)
	if err != nil {
		return enrichWriteCounts{}, err
	}
	// An id waits while a file owed it lies outside a scoped pass's reach.
	var owed []model.EnrichedAlbumFile
	if scope != nil {
		if owed, err = l.store.EnrichedAlbumFiles(ctx, nil); err != nil {
			return enrichWriteCounts{}, err
		}
	}
	c, err := l.writeEnrichmentRows(ctx, rows, albums, owed)
	c.readOnly += held
	for _, pid := range idsHeld {
		c.heldIDs[pid] = true
	}
	c.idsHeld = len(c.heldIDs)
	return c, err
}

// writeEnrichmentRows writes the rows and the album leftovers writeEnrichmentTags read.
// owed is every file owed an album's or a group's id, for a pass whose reach may miss
// some, and nil for one that reaches them all (holdEnrichmentIDs).
func (l *Library) writeEnrichmentRows(ctx context.Context, rows []model.EnrichedTagRow, albums []model.EnrichedAlbumFile, owed []model.EnrichedAlbumFile) (enrichWriteCounts, error) {
	c := enrichWriteCounts{heldIDs: map[model.PID]bool{}}
	w := meta.NewWriter()
	held, err := l.holdEnrichmentIDs(ctx, w, rows, albums, owed)
	if err != nil {
		return c, err
	}
	c.heldIDs = held.files
	leftover := make(map[model.PID]model.EnrichedAlbumFile, len(albums))
	for _, f := range albums {
		// First album wins for a file two of them somehow claim, which cannot happen
		// through a track's single album_id but costs nothing to be explicit about.
		if _, seen := leftover[f.FilePID]; !seen {
			leftover[f.FilePID] = f
		}
	}
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
	// One shared file arrives once per item that shares it, and the refusal below is per
	// file: the count and the drift row would otherwise repeat for the same file.
	refused := map[model.PID]bool{}
	readOnly := map[model.PID]bool{}
	for _, r := range rows {
		if ctx.Err() != nil {
			return c, ctx.Err()
		}
		// The library may have been flagged read-only since the select. Nothing is
		// written or settled, so the file and its label stay owed for the first pass
		// after the flag clears.
		if ro, err := l.store.LibraryReadOnly(ctx, r.LibraryID); err != nil {
			return c, err
		} else if ro {
			delete(leftover, r.FilePID)
			if !readOnly[r.FilePID] {
				readOnly[r.FilePID] = true
				c.readOnly++
			}
			continue
		}
		if r.Kind == model.KindBook {
			if mark, ok := abandoned[r.ItemPID]; ok {
				c.skipped++
				if mark.Code == model.DiagTagWriteLost {
					l.noteEnrichmentLoss(ctx, r.FilePID, string(r.Path), mark, r.Newest, idStamps{})
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
		// The album's label and ids ride the row, so the one rewrite carries them and the
		// file is struck from the leftovers. Only a track is ever an album member, so a
		// book row carries none.
		settleAt := r.Newest
		if r.Label != "" {
			settleAt = max(settleAt, r.LabelUpdatedAt)
		}
		r = owedRow(r)
		albumID, groupID, stamps := held.keep(r.FilePID, r.AlbumMBID, r.ReleaseGroupMBID, r.AlbumMBIDAt, r.ReleaseGroupMBIDAt)
		delete(leftover, r.FilePID)
		if r.Newest <= r.SettledAt && r.Label == "" && albumID == "" && groupID == "" {
			// Only an id the pass holds brought the file, so nothing is written or settled.
			continue
		}
		owe := albumOwed(r.Label, albumID, groupID)
		if r.Shared {
			// Its tags belong to every item that shares it, so no per-item rewrite is
			// safe. Refuse it the way the label pass and the edit write-back do, and
			// settle it so the refusal is not repeated every pass. The settle runs per
			// row, so the stamp lands on the newest value any of the sharing items
			// carried rather than on whichever row came first.
			if !refused[r.FilePID] {
				refused[r.FilePID] = true
				c.unrepresented++
				l.noteEnrichmentDrift(ctx, r.FilePID, string(r.Path), sharedFileRefusal)
			}
			l.settleEnrichmentWrite(ctx, r.FilePID, string(r.Path), settleAt, idStamps{})
			if r.Kind == model.KindBook && r.IsPrimary {
				abandoned[r.ItemPID] = model.FileDiagnostic{
					Code: model.DiagTagWriteLost, Severity: model.SeverityWarn,
					Detail: "skipped behind the book's primary part, which could not be written",
				}
			}
			continue
		}
		failure, err := l.writeEnrichmentFile(ctx, w, r, owe, settleAt, stamps, held.refused[r.FilePID], &c)
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
	err = l.writeEnrichmentAlbumFiles(ctx, w, leftover, held, &c)
	c.idsHeld = len(c.heldIDs)
	return c, err
}

// enrichedIDKey names one id a pass writes: a release group's when group is set, else an
// album's.
type enrichedIDKey struct {
	group bool
	value string
}

// idHold is the ids a pass writes to no file (holdEnrichmentIDs), the files in the pass
// one of them waited on, and the refusals of a file's form the check met, which its
// write takes as its own.
type idHold struct {
	ids     map[enrichedIDKey]bool
	files   map[model.PID]bool
	refused map[model.PID]error
}

// owedRow is the row with only what the file still owes of its own: every field when
// one is newer than the file's stamp, none when none is, and the label when it is newer.
func owedRow(r model.EnrichedTagRow) model.EnrichedTagRow {
	if r.Newest <= r.SettledAt {
		r.Fields = nil
	}
	r.Label = owedLabel(r.Label, r.LabelUpdatedAt, r.SettledAt)
	return r
}

// owedLabel is the label when it was written after the file's stamp, else empty.
func owedLabel(label string, at, settledAt int64) string {
	if at <= settledAt {
		return ""
	}
	return label
}

// keep takes the held ids off a file's write: it returns the album's and the group's ids
// the write carries, each empty when held or not owed, and the stamps it settles them at.
func (h idHold) keep(pid model.PID, album, group string, albumAt, groupAt int64) (string, string, idStamps) {
	stamps := idStamps{albumAt, groupAt}
	if album != "" && h.ids[enrichedIDKey{value: album}] {
		album, stamps.album = "", 0
		h.files[pid] = true
	}
	if group != "" && h.ids[enrichedIDKey{group: true, value: group}] {
		group, stamps.group = "", 0
		h.files[pid] = true
	}
	return album, group, stamps
}

// holdEnrichmentIDs works out, before anything is written, the ids a pass writes to no
// file. An id goes on every file owed it in one pass or waits, since a catalog rebuilt
// from members that disagree keys them apart. It waits while a file owed it lies outside
// the pass's reach (owed lists every file owed one, nil when the pass reaches them all),
// sits in a library flagged read-only since the select, or would not take the write the
// id rides, as the tag writer judges a write prepared and not made: a file it never
// writes, such as a WMA or a fragmented MP4, one it cannot reach or replace now, or one
// whose format would not store the id. A refusal of the file's form is recorded, so
// later passes hold the id in the store and open none of the members until the file
// changes.
func (l *Library) holdEnrichmentIDs(ctx context.Context, w *meta.Writer, rows []model.EnrichedTagRow, albums []model.EnrichedAlbumFile, owed []model.EnrichedAlbumFile) (idHold, error) {
	h := idHold{ids: map[enrichedIDKey]bool{}, files: map[model.PID]bool{}, refused: map[model.PID]error{}}
	type write struct {
		libraryID int64
		path      string
		edits     []meta.TagEdit
		ids       []enrichedIDKey
	}
	idKeys := func(album, group string) []enrichedIDKey {
		var out []enrichedIDKey
		if album != "" {
			out = append(out, enrichedIDKey{value: album})
		}
		if group != "" {
			out = append(out, enrichedIDKey{group: true, value: group})
		}
		return out
	}
	writes := map[model.PID]*write{}
	note := func(pid model.PID, libraryID int64, path []byte, edits []meta.TagEdit, album, group string) {
		if ids := idKeys(album, group); len(ids) > 0 && writes[pid] == nil {
			writes[pid] = &write{libraryID: libraryID, path: string(path), edits: edits, ids: ids}
		}
	}
	for _, r := range rows {
		if !r.Shared {
			r = owedRow(r)
			note(r.FilePID, r.LibraryID, r.Path, append(enrichmentEdits(r), albumOwed(r.Label, r.AlbumMBID, r.ReleaseGroupMBID).edits...),
				r.AlbumMBID, r.ReleaseGroupMBID)
		}
	}
	for _, f := range albums {
		if !f.Shared {
			label := owedLabel(f.Label, f.LabelUpdatedAt, f.SettledAt)
			note(f.FilePID, f.LibraryID, f.Path, albumOwed(label, f.AlbumMBID, f.ReleaseGroupMBID).edits, f.AlbumMBID, f.ReleaseGroupMBID)
		}
	}
	for _, f := range owed {
		for _, k := range idKeys(f.AlbumMBID, f.ReleaseGroupMBID) {
			if wr := writes[f.FilePID]; wr == nil || !slices.Contains(wr.ids, k) {
				h.ids[k] = true
			}
		}
	}
	open := func(wr *write) bool {
		return slices.ContainsFunc(wr.ids, func(k enrichedIDKey) bool { return !h.ids[k] })
	}
	holdAll := func(wr *write) {
		for _, k := range wr.ids {
			h.ids[k] = true
		}
	}
	// First what costs no read of a file: a library flagged read-only since the select,
	// and a file that could not be replaced now. A member held that way leaves its album's
	// other members unread.
	order := slices.Sorted(maps.Keys(writes))
	readOnly := map[int64]bool{}
	for _, pid := range order {
		wr := writes[pid]
		if !open(wr) {
			continue
		}
		ro, seen := readOnly[wr.libraryID]
		if !seen {
			var err error
			if ro, err = l.store.LibraryReadOnly(ctx, wr.libraryID); err != nil {
				return h, err
			}
			readOnly[wr.libraryID] = ro
		}
		if ro || meta.Replaceable(wr.path) != nil {
			holdAll(wr)
		}
	}
	for _, pid := range order {
		wr := writes[pid]
		if !open(wr) {
			continue
		}
		lost, err := w.Check(ctx, wr.path, wr.edits)
		if ctx.Err() != nil {
			return h, ctx.Err()
		}
		if meta.FormRefused(err) {
			h.refused[pid] = err
		}
		if heldForGood(err, lost) {
			l.noteTagWriteRefused(ctx, pid, wr.path)
		}
		for _, k := range wr.ids {
			key := albumIDTagKey
			if k.group {
				key = groupIDTagKey
			}
			if err != nil || slices.ContainsFunc(lost, func(lk string) bool { return strings.EqualFold(lk, key) }) {
				h.ids[k] = true
			}
		}
	}
	return h, nil
}

// heldForGood reports whether a check's answer holds a file's ids until its bytes change:
// the writer refuses the file's form, or would not store an id's key in it. Anything else
// holds them for the pass alone, since it may pass or the file still takes the ids.
func heldForGood(err error, lost []string) bool {
	return meta.FormRefused(err) || slices.ContainsFunc(lost, func(k string) bool {
		return strings.EqualFold(k, albumIDTagKey) || strings.EqualFold(k, groupIDTagKey)
	})
}

// idStamps are the stamps a write carrying an album's and a group's ids settles them at,
// each 0 for an id the write does not carry.
type idStamps struct{ album, group int64 }

// albumEdits are the album values a member file's write carries beside its own: the tag
// edits and the owed keys they pay.
type albumEdits struct {
	edits []meta.TagEdit
	owed  []string
}

// albumOwed builds the edits for an album's label and its and its group's ids, each
// skipped when empty.
func albumOwed(label, albumID, groupID string) albumEdits {
	var out albumEdits
	for _, v := range []struct {
		value, key, owed string
	}{
		{label, albumLabelTagKey, albumLabelOwedKey},
		{albumID, albumIDTagKey, model.OwedAlbumMBID},
		{groupID, groupIDTagKey, model.OwedReleaseGroupMBID},
	} {
		if v.value != "" {
			out.edits = append(out.edits, meta.TagEdit{Key: v.key, Values: []string{v.value}})
			out.owed = append(out.owed, v.owed)
		}
	}
	return out
}

// readOnlyLibraryRefusal is why a file in a read-only library is not written.
const readOnlyLibraryRefusal = "on-disk tag write-back is unavailable in a read-only library"

// readOnlyRefusal is the reason a write to a file in library id is refused, or empty
// when it may go ahead. A failed read of the flags refuses too, since the write cannot
// be shown to be allowed.
func readOnlyRefusal(readOnly map[int64]bool, err error, id int64) string {
	switch {
	case err != nil:
		return "checking the library's read-only flag: " + err.Error()
	case readOnly[id]:
		return readOnlyLibraryRefusal
	}
	return ""
}

// sharedFileRefusal is why a file several items back, or one carrying an offset window,
// takes no tag write: its tags belong to the whole file rather than to the item being
// written. The edit write-back refuses the same shape for the same reason.
const sharedFileRefusal = "on-disk tag write-back is unavailable for a file shared by multiple items"

// bookIdentityTagKeys are the on-disk keys whose values feed identity.BookKey, upper
// cased for the lookup. A write that landed without one of them leaves the file outside
// the book its siblings key to, which is why the write-back reads it as a failure rather
// than as a landed write.
var bookIdentityTagKeys = func() map[string]bool {
	out := map[string]bool{}
	for _, field := range []string{"asin", "isbn", "edition"} {
		keys, ok := meta.BookFieldTagKeys(field)
		if !ok {
			panic("no tag keys for the book identity field " + field)
		}
		for _, k := range keys {
			out[strings.ToUpper(k)] = true
		}
	}
	return out
}()

// lostBookIdentity reports a landed write that dropped a value feeding identity.BookKey.
// Only a keyed loss counts: the keyless codes name content the rewrite destroyed around
// the edit rather than a value the edit asked for, and a key outside the three leaves the
// book keying the way its siblings do.
func lostBookIdentity(kind model.Kind, warnings []model.TagWriteWarning) bool {
	if kind != model.KindBook {
		return false
	}
	for _, wn := range warnings {
		if wn.Unrepresented && bookIdentityTagKeys[strings.ToUpper(wn.Key)] {
			return true
		}
	}
	return false
}

// The on-disk keys an album's label and its and its group's ids are written under, from
// the same map the entity edit write-back reads.
var (
	albumLabelTagKey = entityTagKey(model.MergeAlbum, "label")
	albumIDTagKey    = entityTagKey(model.MergeAlbum, "mbid")
	groupIDTagKey    = entityTagKey(model.MergeReleaseGroup, "mbid")
)

func entityTagKey(et model.MergeEntity, field string) string {
	key, ok := meta.EntityFieldTagKey(et, field)
	if !ok {
		panic("no tag key for " + string(et) + " " + field)
	}
	return key
}

// albumLabelOwedKey is the key an album label's owed row is recorded under.
const albumLabelOwedKey = string(model.MergeAlbum) + ".label"

// writeEnrichmentFile applies one row's tag edits, plus any extra edits the caller folded
// in for this same file, and records the outcome in c: the diagnostics, the write count,
// the settle stamp, and the optimistic file-state update. A write that did not land comes
// back as the diagnostic recorded for it, which is what tells a book's caller to abandon
// the remaining parts and how to mark them, and so does a write that landed without a
// book identifier the container could not hold, since that splits the book the same way.
// A clean write, or a row with nothing to write, comes back nil. A cancellation is
// returned rather than recorded, since it says nothing about the file.
//
// settleAt is the newest enrichment value the edits carry, which is what the file is
// settled at once they land or are found unable to, and ids the same for the album and
// group ids. album is how an album's values reach a member file without a second full
// rewrite of that file.
func (l *Library) writeEnrichmentFile(ctx context.Context, w *meta.Writer, r model.EnrichedTagRow, album albumEdits, settleAt int64, ids idStamps, refused error, c *enrichWriteCounts) (*model.FileDiagnostic, error) {
	path := string(r.Path)
	edits := append(enrichmentEdits(r), album.edits...)
	if len(edits) == 0 {
		// Nothing left to write, so nothing is owed: the values the rows named are
		// gone from the catalog, and a drift row about them would be stale.
		if err := l.store.PutFileDiagnostics(ctx, r.FilePID, model.OriginEnrichment, nil); err != nil {
			l.log.Warn("enrichment diagnostics", "path", path, "err", err)
		}
		l.settleEnrichmentWrite(ctx, r.FilePID, path, settleAt, ids)
		return nil, nil
	}
	res, err := (*meta.WriteResult)(nil), refused
	if refused == nil {
		res, err = w.Apply(ctx, path, edits)
	}
	if err != nil {
		if waxerr.Is(err, waxerr.CodeCanceled) || ctx.Err() != nil {
			return nil, err
		}
		if refused == nil && meta.FormRefused(err) {
			l.noteTagWriteRefused(ctx, r.FilePID, path)
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
		// The file's other values settle as lost, but never its ids, which go on every
		// member or none: one that stays owed holds them from the rest at the next pass.
		if d.Code == model.DiagTagWriteLost {
			l.noteEnrichmentLoss(ctx, r.FilePID, path, d, settleAt, idStamps{})
		} else {
			l.noteEnrichmentDrift(ctx, r.FilePID, path, d.Detail)
		}
		return &d, nil
	}
	// The write landed, so what it carried is no longer owed to this file. A value the
	// format could not store is reported below as lost instead, as the edit write-back does.
	if err := l.store.SettleTagWriteOwed(ctx, r.FilePID, append(enrichmentOwedKeys(r), album.owed...)); err != nil {
		l.log.Warn("enrichment owed settle", "path", path, "err", err)
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
	l.settleEnrichmentWrite(ctx, r.FilePID, path, settleAt, ids)
	// The bytes landed, so the settle and the state update below are earned, but a book
	// identifier the container could not represent is still a failure to the caller: asin,
	// isbn and edition feed identity.BookKey, so parts written with one the primary lacks
	// key apart on the next scan and split the book. A keyless loss, or a lost value that
	// is not one of those three, leaves the book whole and reports nothing here.
	var outcome *model.FileDiagnostic
	if lostBookIdentity(r.Kind, res.Warnings) {
		outcome = &model.FileDiagnostic{
			Code: model.DiagTagWriteLost, Severity: model.SeverityWarn,
			Detail: "this file cannot represent the book identifier the write carried",
		}
	}
	if !res.Changed {
		return outcome, nil
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
	return outcome, nil
}

// noteEnrichmentDrift records a write that did not land, beside the rows a landed write
// left, which still hold. The file stays owed unless the caller settles it.
func (l *Library) noteEnrichmentDrift(ctx context.Context, filePID model.PID, path, detail string) {
	d := model.FileDiagnostic{Code: model.DiagTagWriteUnsynced, Severity: model.SeverityWarn, Detail: detail}
	if err := l.store.AddFileDiagnostic(ctx, filePID, model.OriginEnrichment, d); err != nil {
		l.log.Warn("enrichment diagnostics", "path", path, "err", err)
	}
}

// noteTagWriteRefused records that the tag writer refused the file in its current form,
// which holds its album's ids from every member until the file changes. A failure only
// costs the check again on the next pass, so it is logged.
func (l *Library) noteTagWriteRefused(ctx context.Context, filePID model.PID, path string) {
	info, err := os.Stat(pathx.Long(path))
	if err == nil {
		err = l.store.NoteTagWriteRefused(ctx, filePID, info.Size(), info.ModTime().UnixNano())
	}
	if err != nil {
		l.log.Warn("tag write refusal", "path", path, "err", err)
	}
}

// noteEnrichmentLoss records a value that can never land, replacing the writer's set,
// and settles the file: nothing about it is worth retrying, and a file left owed would
// have the next pass write a book's parts without their primary.
func (l *Library) noteEnrichmentLoss(ctx context.Context, filePID model.PID, path string, d model.FileDiagnostic, settleAt int64, ids idStamps) {
	if err := l.store.PutFileDiagnostics(ctx, filePID, model.OriginEnrichment, []model.FileDiagnostic{d}); err != nil {
		l.log.Warn("enrichment diagnostics", "path", path, "err", err)
	}
	l.settleEnrichmentWrite(ctx, filePID, path, settleAt, ids)
}

// settleEnrichmentWrite stamps the file as settled up to settleAt, and its ids up to
// ids. A failure to stamp only costs a reopen on the next pass, so it is logged rather
// than surfaced.
func (l *Library) settleEnrichmentWrite(ctx context.Context, filePID model.PID, path string, settleAt int64, ids idStamps) {
	if err := l.store.SettleEnrichmentWrite(ctx, filePID, settleAt, ids.album, ids.group); err != nil {
		l.log.Warn("enrichment settle", "path", path, "err", err)
	}
}

// writeEnrichmentAlbumFiles writes an album's label and ids still owed to files the item
// walk did not open: a member with no enriched field of its own, or a virtual track's
// file, which the item select drops.
//
// A shared or virtual file is refused rather than opened: its tags belong to every item
// that shares it, so the label is recorded as unsynced drift, counted as unrepresented,
// and settled so the refusal is not repeated every pass. Such a file holds its album's
// ids from every member, so it is never owed one. The rest go through
// writeEnrichmentFile as a row with no fields of its own.
//
// An album's year needs nothing here: it lands as each member's own year provenance and
// rides the item path as DATE.
func (l *Library) writeEnrichmentAlbumFiles(ctx context.Context, w *meta.Writer, leftover map[model.PID]model.EnrichedAlbumFile, held idHold, c *enrichWriteCounts) error {
	// Sorted, so one pass writes the leftovers in a stable order.
	for _, pid := range slices.Sorted(maps.Keys(leftover)) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f := leftover[pid]
		path := string(f.Path)
		if ro, err := l.store.LibraryReadOnly(ctx, f.LibraryID); err != nil {
			return err
		} else if ro {
			c.readOnly++
			continue
		}
		if f.Shared {
			c.unrepresented++
			l.noteEnrichmentDrift(ctx, pid, path, sharedFileRefusal)
			l.settleEnrichmentWrite(ctx, pid, path, f.LabelUpdatedAt, idStamps{})
			continue
		}
		row := model.EnrichedTagRow{
			FilePID: pid, Kind: model.KindTrack, Path: f.Path,
			Size: f.Size, MTimeNS: f.MTimeNS, Newest: f.LabelUpdatedAt,
		}
		label := owedLabel(f.Label, f.LabelUpdatedAt, f.SettledAt)
		albumID, groupID, stamps := held.keep(pid, f.AlbumMBID, f.ReleaseGroupMBID, f.AlbumMBIDAt, f.ReleaseGroupMBIDAt)
		if label == "" && albumID == "" && groupID == "" {
			continue
		}
		owe := albumOwed(label, albumID, groupID)
		if _, err := l.writeEnrichmentFile(ctx, w, row, owe, f.LabelUpdatedAt, stamps, held.refused[pid], c); err != nil {
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
	readOnly      int // files a read-only library holds back, flagged before the select or since
	idsHeld       int // files an album's or a group's id waits on, len(heldIDs)
	// heldIDs is the files within the pass's reach an id waits on, the store's holds
	// (EnrichmentIDsHeld) and the pass's own (holdEnrichmentIDs).
	heldIDs map[model.PID]bool
}

// enrichmentTagFields is the fixed per-kind order enrichmentEdits walks, so one file's
// write is reproducible regardless of Go's map ordering. Every field listed has an
// on-disk tag key the scanner reads back.
var enrichmentTagFields = map[model.Kind][]string{
	model.KindTrack: {"bpm", "composer", "genre", "isrc", "year"},
	model.KindBook:  {"asin", "description", "edition", "genre", "isbn", "narrator", "publisher", "subtitle", "year"},
}

// enrichmentOwedKeys names the catalog fields an enrichment row's edits write, which is
// what an edit surface recorded the file as owed them under.
func enrichmentOwedKeys(r model.EnrichedTagRow) []string {
	var out []string
	for _, field := range enrichmentTagFields[r.Kind] {
		if strings.TrimSpace(r.Fields[field]) != "" {
			out = append(out, field)
		}
	}
	return out
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
