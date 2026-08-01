package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// LoudnessByItem returns the loudness of an item's primary file, or CodeNotFound
// when no current measurement exists. The essence_hash join hides measurements
// from superseded audio, such as a re-encoded file that has not been analyzed yet.
func (s *Store) LoudnessByItem(ctx context.Context, itemPID model.PID) (*model.Loudness, error) {
	const op = "store.LoudnessByItem"
	var l model.Loudness
	var integrated, trackGain, trackPeak, albumGain, albumPeak sql.NullFloat64
	err := s.read.QueryRowContext(ctx, `SELECT l.integrated_lufs, l.track_gain_db, l.track_peak, l.album_gain_db, l.album_peak
		FROM loudness l
		JOIN file f ON f.id = l.file_id AND f.essence_hash = l.essence_hash
		JOIN item_file pf ON pf.file_id = l.file_id AND pf.role = 'primary'
		JOIN playable_item pi ON pi.id = pf.item_id
		WHERE pi.pid = ?`, string(itemPID)).Scan(&integrated, &trackGain, &trackPeak, &albumGain, &albumPeak)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no loudness for item: "+string(itemPID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	l.IntegratedLUFS, l.TrackGainDB, l.TrackPeak = integrated.Float64, trackGain.Float64, trackPeak.Float64
	l.AlbumGainDB, l.AlbumPeak, l.HasAlbum = albumGain.Float64, albumPeak.Float64, albumGain.Valid
	return &l, nil
}

// CountLoudness returns how many files have a current loudness measurement for
// doctor's ReplayGain coverage. Stale rows awaiting re-analysis are excluded.
func (s *Store) CountLoudness(ctx context.Context) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM loudness l JOIN file f ON f.id = l.file_id AND f.essence_hash = l.essence_hash`).
		Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.CountLoudness", err)
	}
	return n, nil
}

// ReplayGainWriteback returns every track item's current ReplayGain measurement
// paired with its primary file's on-disk state, for the post-analysis tag
// write-back pass. Only essence-current rows are returned (the join filters stale
// measurements), and only track items (ReplayGain is the music use case; a
// standalone track has a NULL album gain, so HasAlbum=false).
func (s *Store) ReplayGainWriteback(ctx context.Context) ([]model.ReplayGainRow, error) {
	const op = "store.ReplayGainWriteback"
	rows, err := s.read.QueryContext(ctx, `SELECT f.pid, f.path, COALESCE(f.container,''), COALESCE(f.codec,''),
			f.size, f.mtime_ns, l.track_gain_db, COALESCE(l.track_peak, 0), l.album_gain_db, COALESCE(l.album_peak, 0)
		FROM loudness l
		JOIN file f ON f.id = l.file_id AND f.essence_hash = l.essence_hash
		JOIN item_file pf ON pf.file_id = l.file_id AND pf.role = 'primary'
		JOIN playable_item pi ON pi.id = pf.item_id
		WHERE pi.kind = 'track' AND l.track_gain_db IS NOT NULL`)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.ReplayGainRow
	for rows.Next() {
		var r model.ReplayGainRow
		var albumGain sql.NullFloat64
		if err := rows.Scan(&r.FilePID, &r.Path, &r.Container, &r.Codec, &r.Size, &r.MTimeNS,
			&r.TrackGainDB, &r.TrackPeak, &albumGain, &r.AlbumPeak); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		r.HasAlbum = albumGain.Valid
		r.AlbumGainDB = albumGain.Float64
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadPeaksForFile returns one file's waveform, or CodeNotFound when the file has
// none or its stored waveform is stale. There is no item_file join at all, so no
// role filter can hide a part: every analyzed file has its own peaks row keyed by
// file_id (needsAnalysisPredicate selects over the file table alone), and this read
// reaches any of them. The essence_hash join is kept, so a re-encoded file whose
// waveform predates the new audio reads as absent rather than wrong.
func (s *Store) LoadPeaksForFile(ctx context.Context, filePID model.PID) (*model.PeaksData, error) {
	const op = "store.LoadPeaksForFile"
	var pk model.PeaksData
	err := s.read.QueryRowContext(ctx, `SELECT p.version, p.bucket_count, p.data
		FROM peaks p
		JOIN file f ON f.id = p.file_id AND f.essence_hash = p.essence_hash
		WHERE f.pid = ?`, string(filePID)).Scan(&pk.Version, &pk.Buckets, &pk.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no peaks for file: "+string(filePID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return &pk, nil
}

// LoadPeaksForItem returns every backing file's waveform for an item, in the same
// reading order ItemFiles and the chapter timeline use. A track yields at most one
// entry; a multi-file audiobook yields one per analyzed part, which is what a
// scrubber over the whole book needs and what LoadPeaks, pinned to the single
// representative primary edge, cannot give it.
//
// An unknown item pid is CodeNotFound. A known item whose parts have no current
// waveform yet returns an empty slice and no error, and a partly analyzed book
// returns only the parts that have one, so the result can be shorter than
// ItemFiles; the Position and FilePID on each entry say which parts those are.
func (s *Store) LoadPeaksForItem(ctx context.Context, itemPID model.PID) ([]model.ItemPeaks, error) {
	const op = "store.LoadPeaksForItem"
	itemID, _, err := s.itemIDKindByPID(ctx, itemPID, op)
	if err != nil {
		return nil, err
	}
	// Reading order comes from bookParts, which sorts numerically-aware in Go
	// ("p2" before "p10"); ordering in SQL here would silently disagree with the
	// chapter timeline. So the peaks are fetched keyed by file and then walked in
	// parts order.
	parts, err := s.bookParts(ctx, itemID)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx, `SELECT p.file_id, p.version, p.bucket_count, p.data
		FROM peaks p
		JOIN file f ON f.id = p.file_id AND f.essence_hash = p.essence_hash
		JOIN item_file itf ON itf.file_id = p.file_id
		WHERE itf.item_id = ?`, itemID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	byFile := make(map[int64]model.PeaksData)
	for rows.Next() {
		var fileID int64
		var pk model.PeaksData
		if err := rows.Scan(&fileID, &pk.Version, &pk.Buckets, &pk.Data); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		byFile[fileID] = pk
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	out := make([]model.ItemPeaks, 0, len(byFile))
	for _, p := range parts {
		pk, ok := byFile[p.fileID]
		if !ok {
			continue
		}
		out = append(out, model.ItemPeaks{FilePID: p.FilePID, Position: p.Position, Peaks: pk})
	}
	return out, nil
}

// LoadPeaks returns an item's current primary-file waveform, or CodeNotFound. The
// essence_hash join mirrors LoudnessByItem and hides waveforms from superseded
// audio. The primary is the representative backing file, not reading-order part
// one: see Library.Peaks for the two rules that pick it. LoadPeaksForItem returns
// every part's waveform.
func (s *Store) LoadPeaks(ctx context.Context, itemPID model.PID) (*model.PeaksData, error) {
	const op = "store.LoadPeaks"
	var pk model.PeaksData
	err := s.read.QueryRowContext(ctx, `SELECT p.version, p.bucket_count, p.data
		FROM peaks p
		JOIN file f ON f.id = p.file_id AND f.essence_hash = p.essence_hash
		JOIN item_file pf ON pf.file_id = p.file_id AND pf.role = 'primary'
		JOIN playable_item pi ON pi.id = pf.item_id
		WHERE pi.pid = ?`, string(itemPID)).Scan(&pk.Version, &pk.Buckets, &pk.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no peaks for item: "+string(itemPID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return &pk, nil
}

// RefreshAlbumGain recomputes album ReplayGain from current per-file loudness and
// writes album_gain_db/album_peak back to each track's loudness row. Album gain
// is -10*log10(duration-weighted mean of 10^(-track_gain/10)); using track gains
// makes the reference loudness cancel out. Album peak is the loudest track peak.
func (s *Store) RefreshAlbumGain(ctx context.Context) error {
	const op = "store.RefreshAlbumGain"

	// No write transaction is needed when the catalog has no loudness rows.
	var n int
	if err := s.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM loudness").Scan(&n); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if n == 0 {
		return nil
	}

	// Read every current loudness row, plus its album membership and existing
	// album gain. Rows with a NULL album_id are included so tracks that left an
	// album get cleared.
	rows, err := s.read.QueryContext(ctx, `SELECT l.file_id, pi.pid, t.album_id,
			l.track_gain_db, COALESCE(l.track_peak,0), COALESCE(f.duration_ms,0),
			l.album_gain_db, l.album_peak
		FROM loudness l
		JOIN file f ON f.id = l.file_id AND f.essence_hash = l.essence_hash
		JOIN item_file pf ON pf.file_id = l.file_id AND pf.role = 'primary'
		JOIN playable_item pi ON pi.id = pf.item_id
		JOIN track t ON t.item_id = pi.id
		WHERE l.track_gain_db IS NOT NULL`)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var all []albumEntry
	byAlbum := map[int64][]albumEntry{}
	// A single-file rip backs N virtual tracks through N primary edges, so the join
	// above returns the file's one loudness row N times. Each row must still drive the
	// per-item write and delta below (all of `all`), but the album-gain WEIGHTING has to
	// count that file (one audio stream, one measurement) exactly once, or the rip is
	// over-weighted N times in a mixed album. Deduplicate the aggregation input by file.
	seenFileInAlbum := map[[2]int64]bool{}
	for rows.Next() {
		var e albumEntry
		if err := rows.Scan(&e.fileID, &e.itemPID, &e.albumID, &e.gainDB, &e.peak, &e.duration,
			&e.curGain, &e.curPeak); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		all = append(all, e)
		if e.albumID.Valid {
			key := [2]int64{e.albumID.Int64, e.fileID}
			if !seenFileInAlbum[key] {
				seenFileInAlbum[key] = true
				byAlbum[e.albumID.Int64] = append(byAlbum[e.albumID.Int64], e)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if len(all) == 0 {
		return nil // only stale rows (essence-mismatched); nothing current to reconcile
	}

	// Precompute each album's target gain/peak once.
	type target struct{ gain, peak float64 }
	targets := make(map[int64]target, len(byAlbum))
	for id, entries := range byAlbum {
		g, p := albumGain(entries)
		targets[id] = target{g, p}
	}

	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, e := range all {
			// Target album gain: the album's aggregate when grouped, else cleared.
			var want sql.NullFloat64
			var wantPeak sql.NullFloat64
			if e.albumID.Valid {
				t := targets[e.albumID.Int64]
				want, wantPeak = sql.NullFloat64{Float64: t.gain, Valid: true}, sql.NullFloat64{Float64: t.peak, Valid: true}
			}
			// Skip rows already at the target so neither a write nor a change delta
			// is emitted for an album whose gain did not move.
			if nullFloatEq(e.curGain, want) && nullFloatEq(e.curPeak, wantPeak) {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE loudness SET album_gain_db = ?, album_peak = ? WHERE file_id = ?",
				want, wantPeak, e.fileID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// Album ReplayGain is consumer-visible. Emit a matching change_log row so
			// data_version tailers can invalidate cached item state.
			if err := appendChange(ctx, tx, "item", e.itemPID, model.OpUpdate); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return nil
	})
}

// nullFloatEq reports whether two nullable floats are equal (both null, or both
// set to the same value).
func nullFloatEq(a, b sql.NullFloat64) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.Float64 == b.Float64
}

type albumEntry struct {
	fileID   int64
	itemPID  model.PID
	albumID  sql.NullInt64
	gainDB   float64
	peak     float64
	duration int64
	curGain  sql.NullFloat64
	curPeak  sql.NullFloat64
}

// albumGain returns the duration-weighted album gain and the loudest peak for a
// set of tracks. A track with unknown duration is weighted as one unit so it
// still contributes.
func albumGain(entries []albumEntry) (float64, float64) {
	var weightedSum, totalWeight, maxPeak float64
	for _, e := range entries {
		w := float64(e.duration)
		if w <= 0 {
			w = 1
		}
		weightedSum += w * math.Pow(10, -e.gainDB/10)
		totalWeight += w
		if e.peak > maxPeak {
			maxPeak = e.peak
		}
	}
	if totalWeight == 0 {
		return 0, maxPeak
	}
	return -10 * math.Log10(weightedSum/totalWeight), maxPeak
}
