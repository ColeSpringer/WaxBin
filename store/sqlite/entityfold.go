package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/pathx"
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

// foldSpelling is what a fold keeps of the key it retires, for a person to recognize: the
// name it was read from, and for an album the folder its live members sat in below their
// library's root (nil when none sat under one).
type foldSpelling struct {
	name      string
	folder    []byte
	libraryID sql.NullInt64
}

// foldSpellingTx reads an entity's spelling from its row in table as a fold of its key
// keeps it, before a merge moves its members away.
func foldSpellingTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, table string, id int64) (foldSpelling, error) {
	name := "name"
	if et == model.MergeAlbum || et == model.MergeReleaseGroup {
		name = "title"
	}
	var sp foldSpelling
	if err := tx.QueryRowContext(ctx, "SELECT "+name+" FROM "+table+" WHERE id = ?", id).Scan(&sp.name); err != nil {
		return sp, err
	}
	if et == model.MergeAlbum {
		return albumFoldFolderTx(ctx, tx, id, sp)
	}
	return sp, nil
}

// albumFoldFolderTx fills sp with the folder of an album's first live member's primary
// file, below its library's root.
func albumFoldFolderTx(ctx context.Context, tx *sql.Tx, albumID int64, sp foldSpelling) (foldSpelling, error) {
	var path, root []byte
	var libID int64
	err := tx.QueryRowContext(ctx, `SELECT f.path, l.root, l.id FROM track t `+liveMembers+`
		JOIN item_file itf ON itf.item_id = t.item_id AND itf.role = 'primary'
		JOIN file f ON f.id = itf.file_id JOIN library l ON l.id = f.library_id
		WHERE t.album_id = ? ORDER BY t.disc_no, t.track_no, t.item_id LIMIT 1`, albumID).Scan(&path, &root, &libID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sp, nil
	case err != nil:
		return sp, err
	}
	// The album's folder as its key reads it, a disc folder's parent for a file in one.
	dir := identity.AlbumFolder(string(root), string(path))
	if !pathx.UnderRoot(string(root), dir) || pathx.SamePath(string(root), dir) {
		return sp, nil
	}
	rel, err := filepath.Rel(string(root), dir)
	if err != nil {
		return sp, nil
	}
	sp.folder, sp.libraryID = []byte(rel), sql.NullInt64{Int64: libID, Valid: true}
	return sp, nil
}

// writeFoldTx folds key into the entity pid, replacing whatever it folded into before,
// and keeps sp as the key's spelling.
func writeFoldTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, key string, pid model.PID, sp foldSpelling) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO entity_fold(entity_type, key, entity_pid, created_at, name, folder, library_id)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(entity_type, key) DO UPDATE SET entity_pid = excluded.entity_pid, created_at = excluded.created_at,
			name = excluded.name, folder = excluded.folder, library_id = excluded.library_id`,
		string(et), key, string(pid), nowNS(), sp.name, sp.folder, sp.libraryID)
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
// it, keeping old, the spelling it left.
func renameFoldsTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, oldKey, newKey string, pid model.PID, fold bool, old foldSpelling) error {
	if err := dropFoldTx(ctx, tx, et, newKey); err != nil || !fold {
		return err
	}
	return writeFoldTx(ctx, tx, et, oldKey, pid, old)
}

// EntityFolds pages an entity type's folds in key order, each with the spelling it keeps
// and the entity it names. afterKey is the previous page's Next, empty for the first; a
// non-positive limit takes the default page size. The primary key serves it.
func (s *Store) EntityFolds(ctx context.Context, et model.MergeEntity, afterKey string, limit int) (model.EntityFoldPage, error) {
	const op = "store.EntityFolds"
	if !et.Valid() {
		return model.EntityFoldPage{}, waxerr.New(waxerr.CodeInvalid, op, "unknown entity type: "+string(et))
	}
	if limit <= 0 {
		limit = defaultPageSize
	}
	name := "name"
	if et == model.MergeAlbum || et == model.MergeReleaseGroup {
		name = "title"
	}
	rows, err := s.rdb().QueryContext(ctx,
		"SELECT f.key, f.entity_pid, e."+name+", f.created_at, f.name, COALESCE(f.folder, ''), COALESCE(l.pid, '')"+
			" FROM entity_fold f JOIN "+string(et)+" e ON e.pid = f.entity_pid LEFT JOIN library l ON l.id = f.library_id"+
			" WHERE f.entity_type = ? AND f.key > ? ORDER BY f.key LIMIT ?", string(et), afterKey, limit+1)
	if err != nil {
		return model.EntityFoldPage{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var page model.EntityFoldPage
	for rows.Next() {
		f := model.EntityFold{EntityType: et}
		var pid, lib string
		var folder []byte
		if err := rows.Scan(&f.Key, &pid, &f.EntityName, &f.CreatedAt, &f.Name, &folder, &lib); err != nil {
			return model.EntityFoldPage{}, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		f.EntityPID, f.Folder, f.LibraryPID = model.PID(pid), string(folder), model.PID(lib)
		page.Folds = append(page.Folds, f)
	}
	if err := rows.Err(); err != nil {
		return model.EntityFoldPage{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if len(page.Folds) > limit {
		page.Folds, page.HasMore, page.Next = page.Folds[:limit], true, page.Folds[limit-1].Key
	}
	return page, nil
}

// UnfoldEntity forgets folds, so the next scan of a file carrying one of the keys mints
// its own entity again, and an artist forgets the alias a key was read from (one whose
// name folds to it). It is all or nothing: CodeNotFound names the keys no fold holds, and
// then nothing is forgotten.
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
			if et == model.MergeArtist {
				if err := dropFoldAliasTx(ctx, tx, key); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
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
		return noFoldOf(op, et, missing)
	})
}

// CheckFolds refuses keys no fold of et holds as UnfoldEntity would, forgetting nothing, so
// a caller can refuse them before it starts the work an unfold leads.
func (s *Store) CheckFolds(ctx context.Context, et model.MergeEntity, keys ...string) error {
	const op = "store.CheckFolds"
	if !et.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown entity type: "+string(et))
	}
	if len(keys) == 0 {
		return waxerr.New(waxerr.CodeInvalid, op, "no keys to unfold")
	}
	var missing []string
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		var one int
		err := s.rdb().QueryRowContext(ctx, "SELECT 1 FROM entity_fold WHERE entity_type = ? AND key = ?", string(et), key).Scan(&one)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			missing = append(missing, strconv.Quote(key))
		case err != nil:
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return noFoldOf(op, et, missing)
}

// noFoldOf is the CodeNotFound naming the quoted keys no fold holds, nil when there are none.
func noFoldOf(op string, et model.MergeEntity, missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return waxerr.New(waxerr.CodeNotFound, op, "no "+string(et)+" fold of "+strings.Join(missing, ", "))
}

// dropFoldAliasTx forgets the aliases of the artist an artist fold of key names whose
// names fold to key.
func dropFoldAliasTx(ctx context.Context, tx *sql.Tx, key string) error {
	rows, err := tx.QueryContext(ctx, `SELECT al.id, al.name FROM entity_fold f
		JOIN artist a ON a.pid = f.entity_pid JOIN artist_alias al ON al.artist_id = a.id
		WHERE f.entity_type = 'artist' AND f.key = ? AND al.is_primary = 0`, key)
	if err != nil {
		return err
	}
	var drop []int64
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if identity.MatchKey(name) == key {
			drop = append(drop, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range drop {
		if _, err := tx.ExecContext(ctx, "DELETE FROM artist_alias WHERE id = ?", id); err != nil {
			return err
		}
	}
	return nil
}

// ItemsByEntityKeyedSpelling returns the items an unfold of key reads again, those linked
// to the entity a fold of key names: a genre's whose genre list names the key, and every
// other entity's every member, since no stored text replays how a file spelled them (an
// artist sits in joint credits, second credits and other roles under a split of the text,
// and an album's key holds its folder and release group). Archived items, which hold no
// file, are left out, and a key no fold holds names nothing. The pids come in pid order.
func (s *Store) ItemsByEntityKeyedSpelling(ctx context.Context, et model.MergeEntity, key string) ([]model.PID, error) {
	const op = "store.ItemsByEntityKeyedSpelling"
	if !et.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown entity type: "+string(et))
	}
	const fold = `entity_fold f JOIN %s e ON e.pid = f.entity_pid`
	const live = ` JOIN playable_item pi ON pi.id = m.item_id AND pi.state <> 'archived'
		WHERE f.entity_type = ? AND f.key = ?`
	var q string
	match := func(spelling string) bool { return true }
	switch et {
	case model.MergeArtist:
		q = `SELECT pi.pid, '' FROM ` + fmt.Sprintf(fold, "artist") + ` JOIN track m ON m.artist_id = e.id` + live + `
			UNION ALL SELECT pi.pid, '' FROM ` + fmt.Sprintf(fold, "artist") + ` JOIN track m ON m.album_artist_id = e.id` + live + `
			UNION ALL SELECT pi.pid, '' FROM ` + fmt.Sprintf(fold, "artist") + ` JOIN book m ON m.author_id = e.id` + live + `
			UNION ALL SELECT pi.pid, '' FROM ` + fmt.Sprintf(fold, "artist") + ` JOIN item_contributor m ON m.artist_id = e.id` + live
	case model.MergeGenre:
		q = `SELECT pi.pid, COALESCE(t.genre, b.genre, '') FROM ` + fmt.Sprintf(fold, "genre") + `
			JOIN item_genre m ON m.genre_id = e.id LEFT JOIN track t ON t.item_id = m.item_id
			LEFT JOIN book b ON b.item_id = m.item_id` + live
		if facet, mk, ok := strings.Cut(key, ":"); ok && model.GenreFacet(facet) == model.FacetGenre {
			match = func(spelling string) bool {
				return slices.ContainsFunc(identity.SplitGenres(spelling), func(g string) bool { return identity.MatchKey(g) == mk })
			}
		}
	case model.MergeAlbum:
		q = `SELECT pi.pid, '' FROM ` + fmt.Sprintf(fold, "album") + ` JOIN track m ON m.album_id = e.id` + live
	case model.MergeReleaseGroup:
		q = `SELECT pi.pid, '' FROM ` + fmt.Sprintf(fold, "release_group") + ` JOIN album al ON al.release_group_id = e.id
			JOIN track m ON m.album_id = al.id` + live
	case model.MergeSeries:
		q = `SELECT pi.pid, '' FROM ` + fmt.Sprintf(fold, "series") + ` JOIN book m ON m.series_id = e.id` + live
	}
	args := []any{string(et), key}
	if et == model.MergeArtist {
		args = []any{string(et), key, string(et), key, string(et), key, string(et), key}
	}
	rows, err := s.rdb().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	seen := map[model.PID]bool{}
	var out []model.PID
	for rows.Next() {
		var pid, spelling string
		if err := rows.Scan(&pid, &spelling); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !seen[model.PID(pid)] && match(spelling) {
			seen[model.PID(pid)] = true
			out = append(out, model.PID(pid))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	slices.Sort(out)
	return out, nil
}

// PrimaryFiles returns the primary file of each item in items that has one, by item pid,
// reading a batch of items at a time; an item with none, or a pid naming no item, is left
// out.
func (s *Store) PrimaryFiles(ctx context.Context, items []model.PID) (map[model.PID]model.ItemFileRef, error) {
	const op = "store.PrimaryFiles"
	out := make(map[model.PID]model.ItemFileRef, len(items))
	err := chunkSlice(uniquePIDs(items), idBatchSize, func(chunk []model.PID) error {
		rows, err := s.rdb().QueryContext(ctx, `SELECT pi.pid, f.pid, f.path, f.display_path, itf.position, l.pid
			FROM playable_item pi JOIN item_file itf ON itf.item_id = pi.id AND itf.role = 'primary'
			JOIN file f ON f.id = itf.file_id JOIN library l ON l.id = f.library_id
			WHERE pi.pid IN `+inArray, pidArray(chunk))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var item model.PID
			ref := model.ItemFileRef{Role: primaryRole}
			if err := rows.Scan(&item, &ref.FilePID, &ref.Path, &ref.DisplayPath, &ref.Position, &ref.LibraryPID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			out[item] = ref
		}
		if err := rows.Err(); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
