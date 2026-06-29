package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// FilesNeedingAnalysis returns the next batch of audio files whose fingerprint is
// missing or stale (essence changed, or the algorithm version advanced), ordered
// by (rel_path, id) so album tracks analyze adjacently. It is a keyset page: the
// caller passes the last (rel_path, id) it saw to advance past files that were
// skipped (no decoder) without re-fetching them, so undecodable files in one
// batch can never strand decodable files in a later batch. A file is "stale" when
// its analyzed_essence != essence_hash OR its analysis_version != algoVersion.
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
const needsAnalysisPredicate = `kind = 'audio' AND essence_hash IS NOT NULL AND (
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

// PutFingerprint persists a file's fingerprint and min-hash terms and stamps the
// file's analyzed_essence/analysis_version, all in one transaction, so a crash
// mid-analyze leaves the file unanalyzed rather than half written. A re-analyze
// replaces the prior fingerprint and terms.
func (s *Store) PutFingerprint(ctx context.Context, in model.FingerprintInput) error {
	const op = "store.PutFingerprint"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		fileID, err := fileIDByPID(ctx, tx, in.FilePID, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM fingerprint WHERE file_id = ?", fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM fingerprint_term WHERE file_id = ?", fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO fingerprint(file_id, essence_hash, algo_version, duration_bucket, fp) VALUES (?,?,?,?,?)",
			fileID, in.EssenceHash, in.AlgoVersion, in.DurationBucket, in.FP); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, term := range in.Terms {
			if _, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO fingerprint_term(term, file_id) VALUES (?, ?)", term, fileID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE file SET analyzed_essence = ?, analysis_version = ? WHERE id = ?",
			in.EssenceHash, in.AlgoVersion, fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// The file's analysis state changed; emit a delta so consumers can react.
		return appendChange(ctx, tx, "file", in.FilePID, model.OpUpdate)
	})
}

// FingerprintCandidates returns files that share at least minShared min-hash
// terms with the given file and fall within one duration bucket of it. This is
// the inverted-index lookup that finds alt-encoding candidates without a
// pairwise scan. The +/-1 bucket window tolerates two encodings whose durations straddle a
// bucket boundary (e.g. 119990ms vs 120010ms). The query file is excluded, and
// each candidate carries the item it backs plus its packed fingerprint vector so
// the caller can verify similarity without a second round trip per candidate.
func (s *Store) FingerprintCandidates(ctx context.Context, filePID model.PID, minShared int) ([]model.FingerprintCandidate, error) {
	const op = "store.FingerprintCandidates"
	if minShared < 1 {
		minShared = 1
	}
	stmt := `
SELECT f.pid, COALESCE(pi.pid, ''), cf.fp, COUNT(*) AS shared
FROM fingerprint_term qt
JOIN fingerprint qf       ON qf.file_id = qt.file_id
JOIN fingerprint_term ct  ON ct.term = qt.term AND ct.file_id <> qt.file_id
JOIN fingerprint cf       ON cf.file_id = ct.file_id
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
		if err := rows.Scan(&fpid, &ipid, &c.FP, &c.SharedTerms); err != nil {
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
