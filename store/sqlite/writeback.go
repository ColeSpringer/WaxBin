package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file holds the read helpers that back the facade's on-disk write-back fan-outs:
// re-anchoring a book's identity after a title/author edit reaches its files, and
// enumerating the member files an entity-level edit fans across.

// RekeyBook updates a book item's stored identity key to newKey, so a later scan --force
// of its edited, written-back file resolves to the same item and keeps its pid and its
// locks, rather than re-keying to the new on-disk title and author. It is deliberately
// conservative. It is a no-op when newKey is empty, when it already matches the item's
// key, or when another book already holds it (the next scan resolves that collision the
// normal way). It only ever touches a book, never a track, whose identity is
// essence-anchored and must not be rewritten. It reports whether the key changed.
func (s *Store) RekeyBook(ctx context.Context, itemPID model.PID, newKey string) (bool, error) {
	const op = "store.RekeyBook"
	if newKey == "" {
		return false, nil
	}
	changed := false
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if kind != string(model.KindBook) {
			return nil
		}
		var cur sql.NullString
		if err := tx.QueryRowContext(ctx,
			"SELECT identity_key FROM playable_item WHERE id = ?", itemID).Scan(&cur); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if cur.Valid && cur.String == newKey {
			return nil
		}
		// A key already owned by another book of the same kind would violate the
		// (kind, identity_key) unique index. Leave it to the next scan, which merges the two
		// onto the shared key the normal way, rather than forcing the collision here.
		var other int64
		switch err := tx.QueryRowContext(ctx,
			"SELECT id FROM playable_item WHERE kind = ? AND identity_key = ? AND id <> ?",
			kind, newKey, itemID).Scan(&other); {
		case err == nil:
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE playable_item SET identity_key = ? WHERE id = ?", newKey, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		changed = true
		return nil
	})
	return changed, err
}

// EntityMemberFiles returns the primary backing file of every item an entity-level edit
// fans onto. For an album or release group that is its member tracks. For an artist it is
// only the tracks the artist is the primary artist of, and that restriction matters: the
// one artist field that fans out is its sort, and ARTISTSORT is the primary artist's tag.
// Writing it to a track where the artist is merely the album-artist or a featured
// contributor would put the wrong artist's sort into that track's ARTISTSORT and its
// artist_sort column. A file that carries an offset window or is shared by several items
// is still returned; the caller's write-back guard refuses it. A file backing more than
// one member appears once.
func (s *Store) EntityMemberFiles(ctx context.Context, et model.MergeEntity, entityPID model.PID) ([]model.ItemFileRef, error) {
	const op = "store.EntityMemberFiles"
	table, ok := entityTableFor(et)
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupported, op, "no member-file fan-out for a "+string(et)+" entity")
	}
	var id int64
	err := s.read.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE pid = ?", string(entityPID)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such "+string(et)+": "+string(entityPID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// The member selection mirrors affectedItemPIDs (merge.go): an album/release-group
	// gathers its tracks, an artist gathers everything it is credited on. DISTINCT
	// collapses a file that backs several members to one row.
	var q string
	var args []any
	switch et {
	case model.MergeAlbum:
		q = `SELECT DISTINCT f.pid, f.path, f.display_path, itf.position
			FROM track t
			JOIN item_file itf ON itf.item_id = t.item_id AND itf.role = 'primary'
			JOIN file f ON f.id = itf.file_id
			WHERE t.album_id = ?`
		args = []any{id}
	case model.MergeReleaseGroup:
		q = `SELECT DISTINCT f.pid, f.path, f.display_path, itf.position
			FROM track t
			JOIN album al ON al.id = t.album_id
			JOIN item_file itf ON itf.item_id = t.item_id AND itf.role = 'primary'
			JOIN file f ON f.id = itf.file_id
			WHERE al.release_group_id = ?`
		args = []any{id}
	case model.MergeArtist:
		// Primary-artist tracks only (see the doc comment): ARTISTSORT is the primary
		// artist's tag, so an album-artist or contributor track must not receive it.
		q = `SELECT DISTINCT f.pid, f.path, f.display_path, itf.position
			FROM track t
			JOIN item_file itf ON itf.item_id = t.item_id AND itf.role = 'primary'
			JOIN file f ON f.id = itf.file_id
			WHERE t.artist_id = ?`
		args = []any{id}
	}

	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.ItemFileRef
	for rows.Next() {
		var ref model.ItemFileRef
		if err := rows.Scan(&ref.FilePID, &ref.Path, &ref.DisplayPath, &ref.Position); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}
