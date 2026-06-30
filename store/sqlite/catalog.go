package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// Compile-time assertion that Store satisfies the catalog port.
var _ model.Catalog = (*Store)(nil)

// EnsureLibrary upserts a library by root, preserving pid/created_at on an
// existing row and refreshing its mode/profile/display.
func (s *Store) EnsureLibrary(ctx context.Context, lib *model.Library) (*model.Library, error) {
	const op = "store.EnsureLibrary"
	var out *model.Library
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()
		existing, err := libraryByRootTx(ctx, tx, lib.Root)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if existing != nil {
			// No-op when nothing changed, so re-opening a library each session
			// doesn't emit a spurious change_log delta.
			if existing.DisplayRoot == lib.DisplayRoot && existing.Mode == lib.Mode && existing.Profile == lib.Profile {
				out = existing
				return nil
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE library SET display_root=?, mode=?, profile=? WHERE id=?",
				lib.DisplayRoot, string(lib.Mode), lib.Profile, existing.ID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			existing.DisplayRoot, existing.Mode, existing.Profile = lib.DisplayRoot, lib.Mode, lib.Profile
			out = existing
			return appendChange(ctx, tx, "library", existing.PID, model.OpUpdate)
		}
		pid := model.NewPID()
		r, err := tx.ExecContext(ctx,
			"INSERT INTO library(pid, root, display_root, mode, profile, created_at) VALUES (?,?,?,?,?,?)",
			string(pid), lib.Root, lib.DisplayRoot, string(lib.Mode), lib.Profile, now)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		id, _ := r.LastInsertId()
		out = &model.Library{ID: id, PID: pid, Root: lib.Root, DisplayRoot: lib.DisplayRoot,
			Mode: lib.Mode, Profile: lib.Profile, CreatedAt: now}
		return appendChange(ctx, tx, "library", pid, model.OpCreate)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LibraryByRoot looks up a library by its raw root bytes.
func (s *Store) LibraryByRoot(ctx context.Context, root []byte) (*model.Library, error) {
	lib, err := libraryByRootDB(ctx, s.read, root)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.LibraryByRoot", err)
	}
	if lib == nil {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.LibraryByRoot", "no such library root")
	}
	return lib, nil
}

// Libraries lists all registered libraries.
func (s *Store) Libraries(ctx context.Context) ([]*model.Library, error) {
	rows, err := s.read.QueryContext(ctx,
		"SELECT id, pid, root, display_root, mode, profile, created_at FROM library ORDER BY id")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.Libraries", err)
	}
	defer rows.Close()
	var out []*model.Library
	for rows.Next() {
		lib, err := scanLibrary(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.Libraries", err)
		}
		out = append(out, lib)
	}
	return out, rows.Err()
}

// PutScannedTrack persists one scanned track atomically: resolve/insert the
// file (preserving pid on a path or essence match), resolve/insert the logical
// item by (kind, identity_key), upsert the track subtype, and link them, writing
// the matching change_log rows. The store owns all pid assignment.
func (s *Store) PutScannedTrack(ctx context.Context, in model.PutScannedTrackInput) (*model.ScanItemResult, error) {
	const op = "store.PutScannedTrack"
	res := &model.ScanItemResult{}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()

		fileID, filePID, priorEssence, err := s.resolveFile(ctx, tx, in, now, res)
		if err != nil {
			return err
		}
		res.FilePID = filePID

		// Unchanged bytes with a different essence hash mean the essence algorithm
		// changed. A real re-encode would change content_hash too. Re-key the item
		// in place so its pid, play_state, and provenance survive the upgrade.
		if !res.ContentChanged && priorEssence != "" && priorEssence != in.File.EssenceHash {
			if err := preserveItemIdentityForFile(ctx, tx, fileID, in.Item.Kind, in.Item.IdentityKey); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		itemID, itemPID, created, err := upsertItem(ctx, tx, in.Item, now)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		res.ItemPID, res.ItemCreated = itemPID, created

		if err := upsertTrack(ctx, tx, itemID, in.Track); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Track the entities whose maintained rollups this write touches, then
		// recompute only those rows inside this transaction.
		affected := newAffectedRollups()

		// Resolve normalized entities and refresh the item's FTS row when the scan
		// actually changed catalog inputs. A byte-identical rescan skips this work
		// and emits no entity-side deltas.
		if created || res.FileCreated || res.ContentChanged || res.Relinked {
			// The entities the item leaves (on a retag) lose the track, so collect
			// them before relinking, and the entities it joins after.
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := resolveAndLinkEntities(ctx, tx, itemID, in.Track, in.File.Path); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// Lyrics and cover art are re-evaluated on every scan, outside the
		// audio-change gate, so an added or edited .lrc sidecar or directory cover
		// image is picked up even when the audio bytes are unchanged. Both writes are
		// idempotent (they compare against the stored value and do nothing when it is
		// unchanged), so a no-op rescan stays silent. They run after entity resolution
		// so a freshly resolved album_id is available to map art onto; an unchanged
		// rescan reuses the album_id persisted by a prior scan.
		if err := putLyricsTx(ctx, tx, itemID, in.Lyrics); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := attachArtTx(ctx, tx, itemID, in.CoverArt); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Re-home the file onto this item, detaching it from any prior item (the
		// case when an in-place essence change re-keys the file to a new identity).
		orphans, err := linkPrimaryFile(ctx, tx, itemID, fileID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, oid := range orphans {
			has, err := itemHasAnyFile(ctx, tx, oid)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if has {
				// A surviving item (a multi-file book) that just lost a file must keep a
				// primary (or it reads back headless) AND have its rollups recomputed,
				// since its summed duration/genre rollup shrank with the detached part.
				if err := affected.collect(ctx, tx, oid); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if err := ensurePrimary(ctx, tx, oid); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if err := refreshBookDuration(ctx, tx, oid); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				continue
			}
			// The orphaned item's entities lose it; collect them before the delete.
			if err := affected.collect(ctx, tx, oid); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			opid, err := deleteItemCascade(ctx, tx, oid)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "item", opid, model.OpDelete); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// Recompute touched rollups from the final base tables, keeping browse
		// counts current without a whole-catalog rebuild.
		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// Emit change_log rows only for real changes, so a no-op rescan is silent
		// (essence-first change detection) and delta consumers don't re-process.
		if res.FileCreated || res.ContentChanged || res.Relinked {
			if err := appendChange(ctx, tx, "file", filePID, opFor(res.FileCreated)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if created || res.ContentChanged {
			if err := appendChange(ctx, tx, "item", itemPID, opFor(created)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// resolveFile finds-or-creates the file row, preserving the pid on a path match
// (rescan/retag) or an essence match (re-link after a move). For a path match it
// also returns the file's prior essence hash, so the caller can detect an
// essence-algorithm change over unchanged bytes and preserve item identity.
func (s *Store) resolveFile(ctx context.Context, tx *sql.Tx, in model.PutScannedTrackInput, now int64, res *model.ScanItemResult) (int64, model.PID, string, error) {
	if existing, err := fileByPathTx(ctx, tx, in.File.Path); err != nil {
		return 0, "", "", err
	} else if existing != nil {
		res.ContentChanged = existing.ContentHash != in.File.ContentHash
		if err := updateFileRow(ctx, tx, existing.ID, in.File, now); err != nil {
			return 0, "", "", err
		}
		return existing.ID, existing.PID, existing.EssenceHash, nil
	}

	// No path match: re-link an existing row with identical essence only when
	// that row's file is gone from disk (a genuine move). If the old path still
	// exists, this is a duplicate copy, not a relocation. Give it its own file
	// row so both copies stay cataloged.
	if in.File.EssenceHash != "" {
		relink, err := fileByEssenceSingleTx(ctx, tx, in.File.EssenceHash, in.LibraryID)
		if err != nil {
			return 0, "", "", err
		}
		if relink != nil && !pathExists(relink.Path) {
			res.Relinked = true
			res.ContentChanged = relink.ContentHash != in.File.ContentHash
			if err := updateFileRow(ctx, tx, relink.ID, in.File, now); err != nil {
				return 0, "", "", err
			}
			return relink.ID, relink.PID, relink.EssenceHash, nil
		}
	}

	res.FileCreated = true
	pid := model.NewPID()
	id, err := insertFileRow(ctx, tx, in.LibraryID, pid, in.File, now)
	if err != nil {
		return 0, "", "", err
	}
	return id, pid, "", nil
}

// preserveItemIdentityForFile re-keys the item backing fileID to newKey. It is
// used only when a file's bytes are unchanged but a new essence algorithm
// produced a different digest, letting the same audio keep its item identity. It
// is a no-op when there is no backing item, the key is already current, or another
// item already owns newKey.
func preserveItemIdentityForFile(ctx context.Context, tx *sql.Tx, fileID int64, kind model.Kind, newKey string) error {
	if newKey == "" {
		return nil
	}
	var itemID int64
	var curKey sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT pi.id, pi.identity_key FROM item_file itf
		 JOIN playable_item pi ON pi.id = itf.item_id
		 WHERE itf.file_id = ? AND itf.role = 'primary'`, fileID).Scan(&itemID, &curKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if curKey.String == newKey {
		return nil
	}
	// Do not collide with a different item that already owns newKey; the normal
	// upsert/orphan path handles that real dedup case.
	var other int64
	switch err := tx.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE kind = ? AND identity_key = ? AND id <> ?",
		string(kind), newKey, itemID).Scan(&other); {
	case err == nil:
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE playable_item SET identity_key = ? WHERE id = ?", newKey, itemID)
	return err
}

// QueryItems compiles q against the item field whitelist and returns the
// matching item views.
func (s *Store) QueryItems(ctx context.Context, q query.Query) ([]*model.ItemView, error) {
	const op = "store.QueryItems"
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	c, err := query.Compile(q, fm)
	if err != nil {
		return nil, err
	}

	var sb strings.Builder
	sb.WriteString(itemSelect)
	args := append([]any(nil), c.Args...)
	where := andWhere(c.Where, entityPredicate(q.Entity))
	if where != "" {
		sb.WriteString(" WHERE ")
		sb.WriteString(where)
	}
	if c.OrderBy != "" {
		sb.WriteString(" ORDER BY " + c.OrderBy + ", pi.pid")
	} else {
		sb.WriteString(" ORDER BY pi.sort_key, pi.pid")
	}
	if c.Limit > 0 {
		sb.WriteString(" LIMIT ?")
		args = append(args, c.Limit)
	} else if c.Offset > 0 {
		sb.WriteString(" LIMIT -1") // SQLite requires a LIMIT before OFFSET
	}
	if c.Offset > 0 {
		sb.WriteString(" OFFSET ?")
		args = append(args, c.Offset)
	}

	rows, err := s.read.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []*model.ItemView
	for rows.Next() {
		v, err := scanItemView(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CountItems returns the number of items matching q (ignoring limit/offset).
func (s *Store) CountItems(ctx context.Context, q query.Query) (int, error) {
	const op = "store.CountItems"
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	c, err := query.Compile(q, fm)
	if err != nil {
		return 0, err
	}
	stmt := itemCountSelect
	if where := andWhere(c.Where, entityPredicate(q.Entity)); where != "" {
		stmt += " WHERE " + where
	}
	var n int
	if err := s.read.QueryRowContext(ctx, stmt, c.Args...).Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return n, nil
}

// ItemByPID returns a single item view by public id.
func (s *Store) ItemByPID(ctx context.Context, pid model.PID) (*model.ItemView, error) {
	const op = "store.ItemByPID"
	row := s.read.QueryRowContext(ctx, itemSelect+" WHERE pi.pid = ?", string(pid))
	v, err := scanItemView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return v, nil
}

// FileByPath returns the file at the given raw path, or CodeNotFound.
func (s *Store) FileByPath(ctx context.Context, path []byte) (*model.File, error) {
	f, err := fileByPathDB(ctx, s.read, path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.FileByPath", err)
	}
	if f == nil {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.FileByPath", "no such file")
	}
	return f, nil
}

// FileByEssence returns a file by essence hash (first match), or CodeNotFound.
func (s *Store) FileByEssence(ctx context.Context, essence string) (*model.File, error) {
	row := s.read.QueryRowContext(ctx, fileSelect+" WHERE essence_hash = ? LIMIT 1", essence)
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.FileByEssence", "no file with that essence")
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.FileByEssence", err)
	}
	return f, nil
}

// PlanMove records a 'planned' organize_journal row before the on-disk move,
// returning its journal pid.
func (s *Store) PlanMove(ctx context.Context, in model.RelocateInput) (model.PID, error) {
	const op = "store.PlanMove"
	jpid := model.NewPID()
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		fileID, err := fileIDByPID(ctx, tx, in.FilePID, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO organize_journal(pid, job_pid, file_id, src, dst, state, created_at) VALUES (?,?,?,?,?,'planned',?)",
			string(jpid), string(in.JobPID), fileID, in.SrcPath, in.NewPath, nowNS()); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return jpid, nil
}

// CommitMove updates the file's path columns, marks the journal row
// 'committed', and logs the change in one transaction.
func (s *Store) CommitMove(ctx context.Context, journalPID model.PID, in model.RelocateInput) error {
	const op = "store.CommitMove"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		fileID, err := fileIDByPID(ctx, tx, in.FilePID, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE file SET path=?, display_path=?, rel_path=?, last_seen=? WHERE id=?",
			in.NewPath, in.NewDisplayPath, in.NewRelPath, nowNS(), fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE organize_journal SET state='committed' WHERE pid=?", string(journalPID)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "file", in.FilePID, model.OpUpdate)
	})
}

// AbortMove marks a planned move 'rolled_back' after an on-disk move failed.
func (s *Store) AbortMove(ctx context.Context, journalPID model.PID) error {
	const op = "store.AbortMove"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE organize_journal SET state='rolled_back' WHERE pid=?", string(journalPID)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
}

func fileIDByPID(ctx context.Context, tx *sql.Tx, pid model.PID, op string) (int64, error) {
	var fileID int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM file WHERE pid = ?", string(pid)).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such file: "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return fileID, nil
}

// ChangesSince returns change_log rows after seq (capped per call).
func (s *Store) ChangesSince(ctx context.Context, seq int64) ([]model.Change, error) {
	const op = "store.ChangesSince"
	rows, err := s.read.QueryContext(ctx,
		"SELECT seq, ts, entity_type, entity_pid, op FROM change_log WHERE seq > ? ORDER BY seq LIMIT 1000", seq)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.Change
	for rows.Next() {
		var c model.Change
		var pid string
		if err := rows.Scan(&c.Seq, &c.TS, &c.EntityType, &pid, &c.Op); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		c.EntityPID = model.PID(pid)
		out = append(out, c)
	}
	return out, rows.Err()
}

// LatestChangeSeq returns the highest change_log seq (0 if empty).
func (s *Store) LatestChangeSeq(ctx context.Context) (int64, error) {
	var seq int64
	if err := s.read.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) FROM change_log").Scan(&seq); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.LatestChangeSeq", err)
	}
	return seq, nil
}

func appendChange(ctx context.Context, tx *sql.Tx, entityType string, pid model.PID, op model.ChangeOp) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO change_log(ts, entity_type, entity_pid, op) VALUES (?,?,?,?)",
		nowNS(), entityType, string(pid), string(op))
	return err
}

func opFor(created bool) model.ChangeOp {
	if created {
		return model.OpCreate
	}
	return model.OpUpdate
}

// pathExists reports whether the file at the given raw path is still present on
// disk. It distinguishes a move (old path gone) from a duplicate copy (old path
// still present) when deciding whether to re-link by essence, and backs
// organize-journal recovery, so a Windows long path must be probed with the
// extended-length prefix or a present file would read as absent (mis-classifying a
// move, or rolling back a completed move during recovery).
func pathExists(path []byte) bool {
	_, err := os.Stat(pathx.Long(string(path)))
	return err == nil
}
