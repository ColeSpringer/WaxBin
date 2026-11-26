package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// folderBookCols reads a book (alias pi) as the folder rule sees one (model.FolderBook).
const folderBookCols = `pi.pid, COALESCE(pi.identity_key, ''), pi.title,
	(SELECT COUNT(*) FROM item_file e WHERE e.item_id = pi.id AND e.role IN ('primary', 'part')),
	(SELECT pf.path FROM item_file pe JOIN file pf ON pf.id = pe.file_id WHERE pe.item_id = pi.id AND pe.role = 'primary' LIMIT 1)`

// AssignedPart returns the book part a host assigned the audio of the file at path to: the
// assignment of essence, else of the audio the file's row held (a file re-encoded in place),
// with its book as the folder rule sees one. It is nil when neither audio is assigned.
func (s *Store) AssignedPart(ctx context.Context, path []byte, essence string) (*model.AssignedPart, error) {
	const op = "store.AssignedPart"
	var ap model.AssignedPart
	var position sql.NullInt64
	err := s.rdb().QueryRowContext(ctx, `SELECT `+folderBookCols+`, ap.position
		FROM assigned_part ap JOIN playable_item pi ON pi.id = ap.item_id AND pi.kind = 'book'
		WHERE ap.essence_hash IN (?, (SELECT essence_hash FROM file WHERE path = ?))
		ORDER BY ap.essence_hash = ? DESC LIMIT 1`, essence, path, essence).Scan(
		&ap.Book.ItemPID, &ap.Book.Key, &ap.Book.Title, &ap.Book.Files, &ap.Book.Primary, &position)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if position.Valid {
		p := int(position.Int64)
		ap.Position = &p
	}
	return &ap, nil
}

// BookStanding returns the book pid names as the folder rule sees one, or nil when no book
// has the pid.
func (s *Store) BookStanding(ctx context.Context, pid model.PID) (*model.FolderBook, error) {
	var b model.FolderBook
	err := s.rdb().QueryRowContext(ctx, `SELECT `+folderBookCols+` FROM playable_item pi
		WHERE pi.pid = ? AND pi.kind = 'book'`, string(pid)).Scan(&b.ItemPID, &b.Key, &b.Title, &b.Files, &b.Primary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.BookStanding", err)
	}
	return &b, nil
}

// recordAssignedTx records a put's assignment of the file's audio to the book, and drops the
// assignment of the audio the file held before (prior) once no file holds that audio.
func recordAssignedTx(ctx context.Context, tx *sql.Tx, essence, prior string, itemID int64, position *int) error {
	var pos sql.NullInt64
	if position != nil {
		pos = sql.NullInt64{Int64: int64(*position), Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assigned_part(essence_hash, item_id, position) VALUES (?, ?, ?)
		ON CONFLICT(essence_hash) DO UPDATE SET item_id = excluded.item_id, position = excluded.position`,
		essence, itemID, pos); err != nil {
		return err
	}
	if prior == "" || prior == essence {
		return nil
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM assigned_part WHERE essence_hash = ?
		AND NOT EXISTS (SELECT 1 FROM file WHERE essence_hash = ?)`, prior, prior)
	return err
}

// dropAssignedTx withdraws the assignment of the audio of the file at path from book, for a
// put that forced the file to a track, when the file is that book's part; a copy's read
// leaves it. It runs before the put moves the file's row or edges.
func dropAssignedTx(ctx context.Context, tx *sql.Tx, book model.PID, path []byte, essence string) error {
	if book == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM assigned_part
		WHERE essence_hash IN (?, (SELECT essence_hash FROM file WHERE path = ?))
		  AND item_id = (SELECT id FROM playable_item WHERE pid = ?)
		  AND EXISTS (SELECT 1 FROM item_file itf JOIN file f ON f.id = itf.file_id
			WHERE f.path = ? AND itf.item_id = assigned_part.item_id AND itf.role IN ('primary', 'part'))`,
		essence, path, string(book), path)
	return err
}

// withdrawAssignedTx deletes the assignment of the audio of the file at path, and of the
// audio its row held, for a read that withdraws it, with an item change for each book it
// named. It runs before the put moves the file's row or edges.
func withdrawAssignedTx(ctx context.Context, tx *sql.Tx, path []byte, essence string) error {
	ids, err := queryInt64sTx(ctx, tx, `DELETE FROM assigned_part
		WHERE essence_hash IN (?, (SELECT essence_hash FROM file WHERE path = ?)) RETURNING item_id`, essence, path)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := appendItemUpdateTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// strandedAssignmentsQ selects the assignments whose item is no book, which no scan applies
// and nothing withdraws.
const strandedAssignmentsQ = `FROM assigned_part WHERE item_id NOT IN (SELECT id FROM playable_item WHERE kind = 'book')`

// GCStrandedAssignments deletes the assignments whose item is no book (strandedAssignmentsQ)
// and returns how many went.
func (s *Store) GCStrandedAssignments(ctx context.Context) (int, error) {
	const op = "store.GCStrandedAssignments"
	var n int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "DELETE "+strandedAssignmentsQ)
		if err != nil {
			return err
		}
		n, err = r.RowsAffected()
		return err
	})
	return int(n), waxerr.Wrap(waxerr.CodeIO, op, err)
}

// PartFile is a file AssignBookParts reads, as it checks one: the file, its kind, audio and
// library, the item it backs through a whole-file edge (a primary or part first) with that
// edge's role and the item's kind and kind lock, how many items it backs, and whether a cue
// rip's window holds it.
type PartFile struct {
	FilePID     model.PID
	Path        []byte
	DisplayPath string
	FileKind    model.FileKind
	Essence     string
	LibraryID   int64
	ItemPID     model.PID
	Kind        model.Kind
	Role        string
	KindLocked  bool
	Items       int
	Windowed    bool
}

// PartFiles returns each file pids names as AssignBookParts checks it, in the order named; a
// pid no file has is CodeNotFound.
func (s *Store) PartFiles(ctx context.Context, pids []model.PID) ([]PartFile, error) {
	const op = "store.PartFiles"
	out := make([]PartFile, 0, len(pids))
	for _, pid := range pids {
		var f PartFile
		err := s.rdb().QueryRowContext(ctx, `SELECT f.pid, f.path, f.display_path, f.kind, COALESCE(f.essence_hash, ''), f.library_id,
				COALESCE(pi.pid, ''), COALESCE(pi.kind, ''), COALESCE(itf.role, ''),
				EXISTS(SELECT 1 FROM field_provenance fp WHERE fp.item_id = pi.id AND fp.field = 'kind' AND fp.locked = 1),
				(SELECT COUNT(DISTINCT e.item_id) FROM item_file e WHERE e.file_id = f.id),
				EXISTS(SELECT 1 FROM item_file e WHERE e.file_id = f.id AND e.start_frames IS NOT NULL)
			FROM file f
			LEFT JOIN item_file itf ON itf.file_id = f.id AND itf.start_frames IS NULL
			LEFT JOIN playable_item pi ON pi.id = itf.item_id
			WHERE f.pid = ? ORDER BY itf.role = 'alternate', itf.item_id LIMIT 1`, string(pid)).Scan(
			&f.FilePID, &f.Path, &f.DisplayPath, &f.FileKind, &f.Essence, &f.LibraryID, &f.ItemPID, &f.Kind, &f.Role, &f.KindLocked, &f.Items, &f.Windowed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, waxerr.New(waxerr.CodeNotFound, op, "no such file: "+string(pid))
		}
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, f)
	}
	return out, nil
}
