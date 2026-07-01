package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// FilesNeedingAnalysis returns the next keyset page of audio files whose analysis
// is missing or stale. Ordering by (rel_path, id) keeps album tracks adjacent and
// lets the caller advance past skipped files without fetching them again. A file
// is stale when its analyzed_essence or analysis_version no longer matches.
func (s *Store) FilesNeedingAnalysis(ctx context.Context, algoVersion int, afterRelPath []byte, afterID int64, limit int) ([]*model.File, error) {
	const op = "store.FilesNeedingAnalysis"
	if limit <= 0 {
		limit = 500
	}
	stmt := fileSelect + " WHERE " + needsAnalysisPredicate
	args := []any{algoVersion}
	if afterID > 0 {
		stmt += " AND (rel_path > ? OR (rel_path = ? AND id > ?))"
		args = append(args, afterRelPath, afterRelPath, afterID)
	}
	stmt += " ORDER BY rel_path, id LIMIT ?"
	args = append(args, limit)

	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []*model.File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// needsAnalysisPredicate selects audio files whose fingerprint is missing or
// stale; shared by the list and count queries so they never drift apart.
// Downloaded podcast episodes (in the internal ModePodcast library) are excluded:
// fingerprinting hours of speech is wasteful and, worse, would index episodes into
// the alt-encoding min-hash so dedup could false-match one against a real track.
const needsAnalysisPredicate = `kind = 'audio' AND essence_hash IS NOT NULL AND
	library_id NOT IN (SELECT id FROM library WHERE mode = 'podcast') AND (
	analyzed_essence IS NULL OR analyzed_essence <> essence_hash OR
	analysis_version IS NULL OR analysis_version <> ?)`

// CountFilesNeedingAnalysis returns how many audio files need (re)analysis at the
// given algorithm version. The analyze pass takes this once up front to report a
// real progress ratio.
func (s *Store) CountFilesNeedingAnalysis(ctx context.Context, algoVersion int) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM file WHERE "+needsAnalysisPredicate, algoVersion).Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.CountFilesNeedingAnalysis", err)
	}
	return n, nil
}

// PutAnalysis persists a file's analysis result in one transaction: fingerprint
// and min-hash terms, optional loudness and peaks, and the file's analysis stamp.
// A crash mid-analysis leaves the file stale instead of half updated. A later
// analysis replaces the prior rows.
func (s *Store) PutAnalysis(ctx context.Context, in model.AnalysisInput) error {
	const op = "store.PutAnalysis"
	fp := in.Fingerprint
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		fileID, err := fileIDByPID(ctx, tx, fp.FilePID, op)
		if err != nil {
			return err
		}
		// If the file's essence changed since the last analysis, any prior loudness
		// or peaks that this run cannot re-measure are stale and must be cleared.
		// A version-only re-analysis over the same essence can keep them.
		var prior sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT analyzed_essence FROM file WHERE id = ?", fileID).Scan(&prior); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		essenceChanged := prior.String != fp.EssenceHash

		for _, del := range []string{
			"DELETE FROM fingerprint WHERE file_id = ?",
			"DELETE FROM fingerprint_term WHERE file_id = ?",
		} {
			if _, err := tx.ExecContext(ctx, del, fileID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO fingerprint(file_id, essence_hash, algo_version, duration_bucket, fp) VALUES (?,?,?,?,?)",
			fileID, fp.EssenceHash, fp.AlgoVersion, fp.DurationBucket, fp.FP); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, term := range fp.Terms {
			if _, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO fingerprint_term(term, file_id) VALUES (?, ?)", term, fileID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if err := putLoudnessTx(ctx, tx, fileID, fp.EssenceHash, in.Loudness, essenceChanged); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := putPeaksTx(ctx, tx, fileID, fp.EssenceHash, in.Peaks, essenceChanged); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE file SET analyzed_essence = ?, analysis_version = ? WHERE id = ?",
			fp.EssenceHash, in.AnalysisVersion, fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// The file's analysis state changed; emit a delta so consumers can react.
		return appendChange(ctx, tx, "file", fp.FilePID, model.OpUpdate)
	})
}

// putLoudnessTx upserts per-file loudness while preserving album_gain/album_peak,
// which are maintained by RefreshAlbumGain. A nil measurement keeps a prior row
// for the same essence, but clears it after an essence change.
func putLoudnessTx(ctx context.Context, tx *sql.Tx, fileID int64, essence string, ld *model.LoudnessData, essenceChanged bool) error {
	if ld == nil {
		if essenceChanged {
			_, err := tx.ExecContext(ctx, "DELETE FROM loudness WHERE file_id = ?", fileID)
			return err
		}
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO loudness(file_id, essence_hash, integrated_lufs, track_gain_db, track_peak, updated_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(file_id) DO UPDATE SET
		   essence_hash=excluded.essence_hash, integrated_lufs=excluded.integrated_lufs,
		   track_gain_db=excluded.track_gain_db, track_peak=excluded.track_peak, updated_at=excluded.updated_at`,
		fileID, essence, ld.IntegratedLUFS, ld.TrackGainDB, ld.TrackPeak, nowNS())
	return err
}

// putPeaksTx upserts a file's waveform, stamped with the essence it was computed
// from. A nil result keeps a prior waveform when the essence is unchanged, but
// clears it on an essence change (the old waveform is for different audio).
func putPeaksTx(ctx context.Context, tx *sql.Tx, fileID int64, essence string, pk *model.PeaksData, essenceChanged bool) error {
	if pk == nil {
		if essenceChanged {
			_, err := tx.ExecContext(ctx, "DELETE FROM peaks WHERE file_id = ?", fileID)
			return err
		}
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO peaks(file_id, version, bucket_count, data, essence_hash, updated_at) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(file_id) DO UPDATE SET
		   version=excluded.version, bucket_count=excluded.bucket_count, data=excluded.data,
		   essence_hash=excluded.essence_hash, updated_at=excluded.updated_at`,
		fileID, pk.Version, pk.Buckets, pk.Data, essence, nowNS())
	return err
}

// FingerprintCandidates returns files that share enough min-hash terms with the
// query file and fall within one duration bucket. This inverted-index lookup
// finds alternate-encoding candidates without a pairwise scan. The +/-1 bucket
// window handles encodings that straddle a bucket boundary, and each candidate
// carries its packed fingerprint so the caller can verify it without another
// query.
func (s *Store) FingerprintCandidates(ctx context.Context, filePID model.PID, minShared int) ([]model.FingerprintCandidate, error) {
	const op = "store.FingerprintCandidates"
	if minShared < 1 {
		minShared = 1
	}
	// Only compare fingerprints computed by the same algorithm. The pure-Go
	// fingerprint and Chromaprint (fpcalc) produce incomparable vectors, and a
	// catalog can hold both mid-migration when fpcalc appears or disappears; the
	// algo_version match keeps a pure-Go vector from ever scoring against a
	// Chromaprint one (their bit layouts and Similar functions differ).
	stmt := `
SELECT f.pid, COALESCE(pi.pid, ''), cf.fp, cf.algo_version, COUNT(*) AS shared
FROM fingerprint_term qt
JOIN fingerprint qf       ON qf.file_id = qt.file_id
JOIN fingerprint_term ct  ON ct.term = qt.term AND ct.file_id <> qt.file_id
JOIN fingerprint cf       ON cf.file_id = ct.file_id
                         AND cf.algo_version = qf.algo_version
                         AND cf.duration_bucket BETWEEN qf.duration_bucket - 1 AND qf.duration_bucket + 1
JOIN file f               ON f.id = ct.file_id
LEFT JOIN item_file pf    ON pf.file_id = f.id AND pf.role = 'primary'
LEFT JOIN playable_item pi ON pi.id = pf.item_id
WHERE qt.file_id = (SELECT id FROM file WHERE pid = ?)
GROUP BY ct.file_id
HAVING shared >= ?
ORDER BY shared DESC`
	rows, err := s.read.QueryContext(ctx, stmt, string(filePID), minShared)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.FingerprintCandidate
	for rows.Next() {
		var c model.FingerprintCandidate
		var fpid, ipid string
		if err := rows.Scan(&fpid, &ipid, &c.FP, &c.AlgoVersion, &c.SharedTerms); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		c.FilePID, c.ItemPID = model.PID(fpid), model.PID(ipid)
		out = append(out, c)
	}
	return out, rows.Err()
}

// LoadFingerprint returns a file's packed sub-fingerprint vector for full
// verification, or CodeNotFound when the file has not been analyzed.
func (s *Store) LoadFingerprint(ctx context.Context, filePID model.PID) ([]byte, error) {
	const op = "store.LoadFingerprint"
	var fp []byte
	err := s.read.QueryRowContext(ctx,
		"SELECT fp FROM fingerprint WHERE file_id = (SELECT id FROM file WHERE pid = ?)", string(filePID)).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "file not analyzed: "+string(filePID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return fp, nil
}

// CountFingerprints returns how many files have a stored fingerprint (for doctor
// and the consistency check).
func (s *Store) CountFingerprints(ctx context.Context) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM fingerprint").Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.CountFingerprints", err)
	}
	return n, nil
}
