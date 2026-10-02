package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// lockKindTx pins an item's kind for a put whose caller forced it against the rule, and
// reports whether that changed the row.
func lockKindTx(ctx context.Context, tx *sql.Tx, itemID, now int64) (bool, error) {
	r, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, updated_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(item_id, field) DO UPDATE SET locked = 1, updated_at = excluded.updated_at WHERE locked = 0`,
		itemID, model.KindLockField, string(model.SourceUser), now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// standingItemCols reads what a file's standing takes from its item (alias pi).
const standingItemCols = `pi.pid, pi.kind,
	EXISTS(SELECT 1 FROM field_provenance fp WHERE fp.item_id = pi.id AND fp.field = 'kind' AND fp.locked = 1),
	COALESCE(pi.identity_key, ''), pi.title,
	(SELECT COUNT(*) FROM item_file e WHERE e.item_id = pi.id AND e.role IN ('primary', 'part')),
	(SELECT pf.path FROM item_file pe JOIN file pf ON pf.id = pe.file_id WHERE pe.item_id = pi.id AND pe.role = 'primary' LIMIT 1)`

// standingCols reads a file's standing from its whole-file edge (alias itf) and item (pi).
const standingCols = `f.path, itf.role, ` + standingItemCols

const standingFrom = ` FROM file f
	JOIN item_file itf ON itf.file_id = f.id AND itf.start_frames IS NULL
	JOIN playable_item pi ON pi.id = itf.item_id`

func scanStanding(sc rowScanner) (*model.FileStanding, error) {
	var st model.FileStanding
	var book model.FolderBook
	if err := sc.Scan(&st.Path, &st.Role, &st.ItemPID, &st.Kind, &st.KindLocked,
		&book.Key, &book.Title, &book.Files, &book.Primary); err != nil {
		return nil, err
	}
	if st.Kind == model.KindBook {
		book.ItemPID = st.ItemPID
		st.Book = &book
	}
	return &st, nil
}

// trashStandingQ reads the standing of the item a file backed when the trash took it,
// from the journal's newest active entry; %s names the column it matches.
const trashStandingQ = `SELECT t.orig_path, '', ` + standingItemCols + `
	FROM trash t JOIN playable_item pi ON pi.pid = t.item_pid
	WHERE t.restored_at IS NULL AND t.%s = ? ORDER BY t.library_id IS ? DESC, t.trashed_at DESC LIMIT 1`

// FileStanding returns what the catalog holds behind the file at path: the item its own
// row backs, else the item a row with the same audio backs (a moved file not yet relinked,
// or a copy), a row in this library first, else the item the trash journal says the file
// backed when the trash took it, so a file put back by hand is the item it was. A primary
// or part edge outranks an alternate. It is nil when nothing holds the file or its audio.
func (s *Store) FileStanding(ctx context.Context, libraryID int64, path []byte, essence string) (*model.FileStanding, error) {
	const op = "store.FileStanding"
	lookups := []struct {
		q    string
		args []any
	}{
		{`SELECT ` + standingCols + standingFrom + `
			WHERE f.path = ? ORDER BY itf.role = 'alternate', itf.item_id LIMIT 1`, []any{path}},
		{`SELECT ` + standingCols + standingFrom + `
			WHERE f.essence_hash = ? ORDER BY f.library_id = ? DESC, itf.role = 'alternate', f.id, itf.item_id LIMIT 1`,
			[]any{essence, libraryID}},
		{fmt.Sprintf(trashStandingQ, "orig_path"), []any{path, libraryID}},
		{fmt.Sprintf(trashStandingQ, "essence_hash"), []any{essence, libraryID}},
	}
	for i, l := range lookups {
		if essence == "" && i%2 == 1 {
			continue
		}
		st, err := scanStanding(s.read.QueryRowContext(ctx, l.q, l.args...))
		if !errors.Is(err, sql.ErrNoRows) {
			return st, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return nil, nil
}

// folderStandingQ lists the standing of a library's files in a path range. The unary plus
// keeps the planner off file_library, which would walk every file of the library rather
// than seek the range on the path index.
const folderStandingQ = `SELECT ` + standingCols + standingFrom + `
	WHERE +f.library_id = ? AND f.path >= ? AND f.path < ?
	ORDER BY f.path, itf.role = 'alternate', itf.item_id`

// FolderStanding returns the standing of each cataloged file directly in folder or in a
// disc folder under it, in path order, a file's primary or part edge before an alternate.
func (s *Store) FolderStanding(ctx context.Context, libraryID int64, root, folder string) ([]model.FileStanding, error) {
	const op = "store.FolderStanding"
	lo := []byte(folder + string(filepath.Separator))
	rows, err := s.read.QueryContext(ctx, folderStandingQ, libraryID, lo, prefixUpperBound(lo))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.FileStanding
	for rows.Next() {
		st, err := scanStanding(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if n := len(out); n > 0 && string(out[n-1].Path) == string(st.Path) {
			continue
		}
		if pathx.SamePath(identity.AlbumFolder(root, string(st.Path)), folder) {
			out = append(out, *st)
		}
	}
	return out, waxerr.Wrap(waxerr.CodeIO, op, rows.Err())
}

// rekindItemForFileTx turns the item a file is the sole primary of into kind, in place,
// for a put of kind whose key no item holds: the item keeps its pid and everything keyed
// by it (play state, bookmarks, playlist entries, sessions, the queue, acquisition, custom
// tags, cover) and takes key, while what only the old kind had goes: its subtype row (the
// genre and year carry into the new one), contributors, chapters or lyrics, enrichment
// markers, and the provenance and owed rows of any field but genre, year, art, acquisition,
// a custom tag or an art role. A title or an mbid means another thing on each kind (a
// song's title against a book's, a recording against a release), and the kind lock pinned
// the kind that is gone. The entities the item leaves are refreshed here; the put that
// follows writes the new kind's fields and links. A file holding part of a book with other
// parts, or a window of a rip, leaves its item instead. With held set (locks honoured, the kind not forced) a kind lock on the item
// refuses the change with CodeConflict: the scan read the item before the lock was set, and
// its next read follows the lock. It returns the item's id, 0 when it re-kinded nothing.
func rekindItemForFileTx(ctx context.Context, tx *sql.Tx, fileID int64, kind model.Kind, key string, held bool, now int64) (int64, error) {
	var itemID int64
	var from string
	err := tx.QueryRowContext(ctx, `SELECT pi.id, pi.kind FROM item_file itf JOIN playable_item pi ON pi.id = itf.item_id
		WHERE itf.file_id = ? AND itf.role = 'primary' AND itf.start_frames IS NULL
		  AND pi.kind IN ('track', 'book') AND pi.kind <> ?
		  AND (SELECT COUNT(*) FROM item_file e WHERE e.item_id = pi.id AND e.role IN ('primary', 'part')) = 1`,
		fileID, string(kind)).Scan(&itemID, &from)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if held {
		locked, err := fieldLockedTx(ctx, tx, itemID, model.KindLockField)
		if err != nil {
			return 0, err
		}
		if locked {
			return 0, waxerr.New(waxerr.CodeConflict, "store.rekind", "the item's kind is locked as "+from)
		}
	}
	affected := newAffectedRollups()
	if err := affected.collect(ctx, tx, itemID); err != nil {
		return 0, err
	}
	contributors, err := contributorArtistIDs(ctx, tx, itemID)
	if err != nil {
		return 0, err
	}
	for _, id := range contributors {
		affected.artists[id] = true
	}

	var genre string
	var year sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT genre, year FROM "+from+" WHERE item_id = ?", itemID).Scan(&genre, &year); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	keepField := `field IN ('genre', 'year', 'art', 'acquisition') OR field LIKE 'tag.%' OR field LIKE 'art.%'`
	keepOwed := `tag_key IN ('genre', 'year', 'art', 'acquisition') OR tag_key LIKE 'tag.%'`
	// Leaving a book takes its chapters, entering one a track's lyrics.
	shed := "DELETE FROM lyrics WHERE item_id = ?"
	if from == string(model.KindBook) {
		shed = "DELETE FROM chapter WHERE book_item_id = ?"
	}
	stmts := []struct {
		q    string
		args []any
	}{
		{"DELETE FROM " + from + " WHERE item_id = ?", []any{itemID}},
		{"INSERT INTO " + string(kind) + "(item_id, genre, year) VALUES (?, ?, ?)", []any{itemID, genre, year}},
		{"DELETE FROM item_contributor WHERE item_id = ?", []any{itemID}},
		{"DELETE FROM entity_enrichment WHERE entity_type IN ('book', 'lyrics', 'fields') AND entity_id = ?", []any{itemID}},
		{"DELETE FROM field_provenance WHERE item_id = ? AND NOT (" + keepField + ")", []any{itemID}},
		{`DELETE FROM file_diagnostic WHERE origin = 'edit' AND code = 'tag_write_owed' AND NOT (` + keepOwed + `)
			AND file_id IN (SELECT file_id FROM item_file WHERE item_id = ?)`, []any{itemID}},
		{"UPDATE playable_item SET kind = ?, identity_key = ?, updated_at = ? WHERE id = ?", []any{string(kind), key, now, itemID}},
		{shed, []any{itemID}},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return 0, err
		}
	}
	if kind == model.KindBook {
		// A book holds these keys as fields, so a lock cannot keep them as custom tags; the
		// put's tag sync then drops the tags themselves.
		owned := model.BookOwnedTagKeys()
		args := []any{itemID}
		for _, k := range owned {
			args = append(args, "tag."+k)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM field_provenance WHERE item_id = ? AND field IN "+placeholders(len(owned)), args...); err != nil {
			return 0, err
		}
	}
	return itemID, maintainRollupsTx(ctx, tx, affected, now)
}
