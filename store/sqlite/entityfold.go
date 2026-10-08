package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// genreFoldKey is a genre's fold key: its facet, then its match key. A match key holds no
// colon (identity.MatchKey folds punctuation to spaces), so the two never run together.
func genreFoldKey(facet model.GenreFacet, matchKey string) string {
	return string(facet) + ":" + matchKey
}

// entityFoldKeyTx reads the key an entity's fold is stored under, from its row: the match
// key, and a genre's facet before it.
func entityFoldKeyTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, id int64) (string, error) {
	if et == model.MergeGenre {
		var facet, mk string
		err := tx.QueryRowContext(ctx, "SELECT facet, match_key FROM genre WHERE id = ?", id).Scan(&facet, &mk)
		return genreFoldKey(model.GenreFacet(facet), mk), err
	}
	var key string
	err := tx.QueryRowContext(ctx, "SELECT match_key FROM "+string(et)+" WHERE id = ?", id).Scan(&key)
	return key, err
}

// foldedEntityTx returns the id of the entity a fold of key names, or 0 when none does.
// The resolvers ask it once the key itself has missed, before they mint a row.
func foldedEntityTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, key string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		"SELECT e.id FROM entity_fold f JOIN "+string(et)+" e ON e.pid = f.entity_pid WHERE f.entity_type = ? AND f.key = ?",
		string(et), key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// keyHolderTx returns the pid of the entity other than self that a key resolves to, the
// row holding it or else the one a fold of it names; empty when it resolves to nothing but
// self. A rename or a re-key onto such a key merges into the holder, as a scan of that
// spelling would land there. It serves the types whose fold key is the match key, which
// is every type but genre.
//
// Only an entity rename that locks folds the key it leaves (renameFoldsTx); an unlocked one
// yields to its members' files the way every unlocked edit does, and an item or credit
// edit renaming an entity in place folds nothing.
func keyHolderTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, key string, self int64) (model.PID, error) {
	var pid string
	err := tx.QueryRowContext(ctx,
		"SELECT pid FROM "+string(et)+" WHERE match_key = ? AND id <> ?", key, self).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx,
			"SELECT e.pid FROM entity_fold f JOIN "+string(et)+" e ON e.pid = f.entity_pid"+
				" WHERE f.entity_type = ? AND f.key = ? AND e.id <> ?", string(et), key, self).Scan(&pid)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return model.PID(pid), err
}

// writeFoldTx folds key into the entity pid, replacing whatever it folded into before.
func writeFoldTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, key string, pid model.PID) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO entity_fold(entity_type, key, entity_pid, created_at) VALUES (?,?,?,?)
		ON CONFLICT(entity_type, key) DO UPDATE SET entity_pid = excluded.entity_pid, created_at = excluded.created_at`,
		string(et), key, string(pid), nowNS())
	return err
}

// dropFoldTx forgets a fold of key, for an entity that has just taken the key as its own
// (a rename or a re-key onto it).
func dropFoldTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, key string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM entity_fold WHERE entity_type = ? AND key = ?", string(et), key)
	return err
}

// renameFoldsTx is a rename's fold bookkeeping: the entity takes newKey as its own, so a
// fold of it goes, and with fold (an entity rename that locks) the key it left folds into
// it.
func renameFoldsTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, oldKey, newKey string, pid model.PID, fold bool) error {
	if err := dropFoldTx(ctx, tx, et, newKey); err != nil || !fold {
		return err
	}
	return writeFoldTx(ctx, tx, et, oldKey, pid)
}

// EntityFolds lists an entity type's folds by key, each with the entity it names.
func (s *Store) EntityFolds(ctx context.Context, et model.MergeEntity) ([]model.EntityFold, error) {
	const op = "store.EntityFolds"
	if !et.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown entity type: "+string(et))
	}
	name := "name"
	if et == model.MergeAlbum || et == model.MergeReleaseGroup {
		name = "title"
	}
	rows, err := s.rdb().QueryContext(ctx,
		"SELECT f.key, f.entity_pid, e."+name+", f.created_at FROM entity_fold f JOIN "+string(et)+
			" e ON e.pid = f.entity_pid WHERE f.entity_type = ? ORDER BY f.key", string(et))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EntityFold
	for rows.Next() {
		f := model.EntityFold{EntityType: et}
		var pid string
		if err := rows.Scan(&f.Key, &pid, &f.EntityName, &f.CreatedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		f.EntityPID = model.PID(pid)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// UnfoldEntity forgets folds, so the next scan of a file carrying one of the keys mints
// its own entity again. It is all or nothing: CodeNotFound names the keys no fold holds,
// and then nothing is forgotten.
func (s *Store) UnfoldEntity(ctx context.Context, et model.MergeEntity, keys ...string) error {
	const op = "store.UnfoldEntity"
	if !et.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown entity type: "+string(et))
	}
	if len(keys) == 0 {
		return waxerr.New(waxerr.CodeInvalid, op, "no keys to unfold")
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var missing []string
		seen := make(map[string]bool, len(keys))
		for _, key := range keys {
			// A key named twice is unfolded once.
			if seen[key] {
				continue
			}
			seen[key] = true
			r, err := tx.ExecContext(ctx, "DELETE FROM entity_fold WHERE entity_type = ? AND key = ?", string(et), key)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if n, err := r.RowsAffected(); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			} else if n == 0 {
				missing = append(missing, strconv.Quote(key))
			}
		}
		if len(missing) > 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "no "+string(et)+" fold of "+strings.Join(missing, ", "))
		}
		return nil
	})
}
