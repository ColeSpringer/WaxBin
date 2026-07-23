package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// currentDiagVersion is the diagnostic rule-set version. Bump it when the rules
// change what would be derived from the same bytes: the audit's coverage finding
// then reports the affected files as not yet derived, and the user can choose to run
// `scan --force`. A mismatch never triggers a re-derive on its own.
const currentDiagVersion = 1

// replaceFileDiagnosticsTx makes one writer's diagnostics for a file exactly ds,
// deleting that origin's existing rows and inserting the current set. It touches
// only the given origin's rows, so writers cannot clear each other's findings and a
// retry that comes back clean clears its own stale rows.
func replaceFileDiagnosticsTx(ctx context.Context, tx *sql.Tx, fileID int64, origin model.DiagnosticOrigin, ds []model.FileDiagnostic) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM file_diagnostic WHERE file_id = ? AND origin = ?", fileID, string(origin)); err != nil {
		return err
	}
	now := nowNS()
	for _, d := range ds {
		sev := d.Severity
		if sev == "" {
			sev = model.SeverityWarn
		}
		seen := d.SeenAt
		if seen == 0 {
			seen = now
		}
		// The primary key is (file_id, origin, code, tag_key), so one writer reporting
		// the same code for the same key twice collapses to the last one rather than
		// failing the whole scan transaction.
		if _, err := tx.ExecContext(ctx, `INSERT INTO file_diagnostic
			(file_id, origin, code, severity, tag_key, detail, seen_at)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(file_id, origin, code, tag_key) DO UPDATE SET
				severity=excluded.severity, detail=excluded.detail, seen_at=excluded.seen_at`,
			fileID, string(origin), string(d.Code), string(sev), d.TagKey, d.Detail, seen); err != nil {
			return err
		}
	}
	return nil
}

// stampDiagVersionTx records that a file's diagnostics were derived under the
// current rule set. Only the scan path calls it, and it stays out of
// replaceFileDiagnosticsTx on purpose: that helper also backs the writer-origin
// entry point, so a stamp inside it would let an organize run mark a never-scanned
// file as derived, and the scan pass would skip the file from then on.
func stampDiagVersionTx(ctx context.Context, tx *sql.Tx, fileID int64) error {
	_, err := tx.ExecContext(ctx, "UPDATE file SET diag_version = ? WHERE id = ?", currentDiagVersion, fileID)
	return err
}

// PutFileDiagnostics replaces one writer's diagnostics for a file, keyed by FilePID.
//
// The PID key matters. Both callers already hold one, and UpdateFileStateIfUnchanged
// takes a FilePID too. A path key would be wrong for organize, which retags at the
// source path and then moves the file, with CommitMove rewriting the path row in
// between; a path-keyed call would be order-sensitive against that window for no
// benefit. file_diagnostic is keyed by file_id, which follows a move on its own.
//
// The scan path cannot serve as the only entry point, because a no-op write that
// carries a warning never reaches UpdateFileStateIfUnchanged.
func (s *Store) PutFileDiagnostics(ctx context.Context, filePID model.PID, origin model.DiagnosticOrigin, ds []model.FileDiagnostic) error {
	const op = "store.PutFileDiagnostics"
	// Callers hand over their whole set on every run so that a clean run clears what a
	// prior one left, which makes the common call "nothing to record for a file that
	// has nothing recorded". One indexed read answers that, and costs far less than a
	// write transaction and its fsync for every organized or retagged file.
	if len(ds) == 0 {
		has, err := s.hasFileDiagnostics(ctx, filePID, origin)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !has {
			return nil
		}
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		fileID, err := idByPIDTx(ctx, tx, "file", filePID, op)
		if err != nil {
			return err
		}
		if err := replaceFileDiagnosticsTx(ctx, tx, fileID, origin, ds); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
}

// diagnosticFilterSQL lowers a DiagnosticFilter into a WHERE fragment and its
// bind args (both empty for the zero filter, so that path runs today's full-dump
// SQL). The enum dimensions are validated against their vocabularies first, so
// a typo is CodeInvalid rather than a silently empty result, the same
// fail-closed treatment the facet group-by and entity-kind enums get. A library
// scope resolves the pid to its rowid, so an unknown library is CodeNotFound.
// The origin/code filters ride the file_diagnostic primary key and
// file_diagnostic_code index, the library filter rides file_library; no new
// index is needed at this table's grain.
func (s *Store) diagnosticFilterSQL(ctx context.Context, filter model.DiagnosticFilter, op string) (string, []any, error) {
	var conds []string
	var args []any
	if filter.Origin != "" {
		if !filter.Origin.Valid() {
			return "", nil, waxerr.New(waxerr.CodeInvalid, op, "unknown diagnostic origin: "+string(filter.Origin))
		}
		conds = append(conds, "d.origin = ?")
		args = append(args, string(filter.Origin))
	}
	if filter.Code != "" {
		if !filter.Code.Valid() {
			return "", nil, waxerr.New(waxerr.CodeInvalid, op, "unknown diagnostic code: "+string(filter.Code))
		}
		conds = append(conds, "d.code = ?")
		args = append(args, string(filter.Code))
	}
	if filter.Severity != "" {
		if !filter.Severity.Valid() {
			return "", nil, waxerr.New(waxerr.CodeInvalid, op, "unknown severity: "+string(filter.Severity))
		}
		conds = append(conds, "d.severity = ?")
		args = append(args, string(filter.Severity))
	}
	if filter.LibraryPID != "" {
		ids, err := s.libraryIDsByPIDs(ctx, []model.PID{filter.LibraryPID}, op)
		if err != nil {
			return "", nil, err
		}
		conds = append(conds, "f.library_id = ?")
		args = append(args, ids[0])
	}
	if len(conds) == 0 {
		return "", nil, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

// FileDiagnostics returns the persisted diagnostics matching the filter, each
// joined to its file's display path. Ordering is deterministic (path, origin,
// code, key) so capped and offset-paged output is stable. The zero filter is the
// full dump the audit reads.
func (s *Store) FileDiagnostics(ctx context.Context, filter model.DiagnosticFilter) ([]model.FileDiagnostic, error) {
	const op = "store.FileDiagnostics"
	where, args, err := s.diagnosticFilterSQL(ctx, filter, op)
	if err != nil {
		return nil, err
	}
	stmt := `SELECT f.pid, f.display_path, d.origin, d.code,
		d.severity, d.tag_key, d.detail, d.seen_at
		FROM file_diagnostic d JOIN file f ON f.id = d.file_id` + where + `
		ORDER BY f.display_path, d.origin, d.code, d.tag_key`
	switch {
	case filter.Limit > 0:
		stmt += " LIMIT ?"
		args = append(args, filter.Limit)
		if filter.Offset > 0 {
			stmt += " OFFSET ?"
			args = append(args, filter.Offset)
		}
	case filter.Offset > 0:
		stmt += " LIMIT -1 OFFSET ?" // SQLite requires a LIMIT before OFFSET
		args = append(args, filter.Offset)
	}
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.FileDiagnostic
	for rows.Next() {
		var d model.FileDiagnostic
		var origin, code, sev string
		if err := rows.Scan(&d.FilePID, &d.DisplayPath, &origin, &code, &sev, &d.TagKey, &d.Detail, &d.SeenAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		d.Origin, d.Code, d.Severity = model.DiagnosticOrigin(origin), model.DiagnosticCode(code), model.AuditSeverity(sev)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// DiagnosticSummary returns the matching diagnostics grouped by writer, code,
// and severity, most severe band first (then origin and code for a stable
// order). It answers "what is wrong, roughly how much" without materializing
// per-file rows. The filter's dimensions apply; Limit and Offset do not, since
// a summary is an aggregation over the whole match and its bucket count is
// bounded by the curated code vocabulary. The file join always rides along so
// the one statement serves the library scope too; it is a PK probe per row on a
// table sized by real findings, not by the catalog.
func (s *Store) DiagnosticSummary(ctx context.Context, filter model.DiagnosticFilter) ([]model.DiagnosticCount, error) {
	const op = "store.DiagnosticSummary"
	where, args, err := s.diagnosticFilterSQL(ctx, filter, op)
	if err != nil {
		return nil, err
	}
	stmt := `SELECT d.origin, d.code, d.severity, COUNT(*)
		FROM file_diagnostic d JOIN file f ON f.id = d.file_id` + where + `
		GROUP BY d.origin, d.code, d.severity
		ORDER BY CASE d.severity WHEN 'error' THEN 0 WHEN 'warn' THEN 1 ELSE 2 END, d.origin, d.code`
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.DiagnosticCount
	for rows.Next() {
		var c model.DiagnosticCount
		var origin, code, sev string
		if err := rows.Scan(&origin, &code, &sev, &c.Count); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		c.Origin, c.Code, c.Severity = model.DiagnosticOrigin(origin), model.DiagnosticCode(code), model.AuditSeverity(sev)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// hasFileDiagnostics reports whether a file already carries rows for one writer. It
// is the guard that lets a no-op replace skip its write transaction. file.pid is
// unique-indexed and file_diagnostic's primary key leads with file_id, so the check
// costs two index lookups.
func (s *Store) hasFileDiagnostics(ctx context.Context, filePID model.PID, origin model.DiagnosticOrigin) (bool, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM file_diagnostic d JOIN file f ON f.id = d.file_id
		WHERE f.pid = ? AND d.origin = ?)`, string(filePID), string(origin)).Scan(&n)
	if err != nil {
		return false, err
	}
	return n != 0, nil
}

// CountFileDiagnostics returns how many diagnostics are recorded, without building
// them. Doctor wants only the number, and materializing every row (a join to file
// plus an ORDER BY) just to take its length would scale the cost with how many
// problems a library has.
func (s *Store) CountFileDiagnostics(ctx context.Context) (int, error) {
	const op = "store.CountFileDiagnostics"
	var n int
	if err := s.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_diagnostic").Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return n, nil
}

// DiagnosticCoverage reports how many audio files have not had diagnostics derived
// under the current rule set, and the total. It is what lets the audit say that no
// rows means clean, and state its coverage alongside. Without it, no rows could mean
// clean, not yet derived, or derived under an older rule set, differently per file.
//
// It runs one aggregate query over the file table. That is a table scan: no index
// covers diag_version, and adding one would tax every scan's writes to speed up a
// count that runs once per audit. The cost worth comparing against is the one it
// replaces, re-reading and re-hashing every file on disk.
func (s *Store) DiagnosticCoverage(ctx context.Context) (stale, total int, err error) {
	const op = "store.DiagnosticCoverage"
	err = s.read.QueryRowContext(ctx, `SELECT
		COUNT(*) FILTER (WHERE diag_version < ?), COUNT(*)
		FROM file WHERE kind = ?`, currentDiagVersion, string(model.FileAudio)).Scan(&stale, &total)
	if err != nil {
		return 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return stale, total, nil
}
