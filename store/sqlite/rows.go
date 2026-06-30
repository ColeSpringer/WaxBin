package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
)

// queryer is the read surface shared by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

const itemJoins = ` FROM playable_item pi
	JOIN track t ON t.item_id = pi.id
	LEFT JOIN item_file pf ON pf.item_id = pi.id AND pf.role = 'primary'
	LEFT JOIN file f ON f.id = pf.file_id`

// itemViewCols is the column list for an item read-view, shared by the plain
// select and the keyset-paginated select (which prepends pi.sort_key).
const itemViewCols = `pi.pid, pi.kind, pi.state, pi.title,
	t.artist, t.album_artist, t.album, t.track_no, t.disc_no, t.year, t.genre, t.compilation,
	f.pid, f.path, f.display_path, f.duration_ms, f.container, f.codec`

const itemSelect = `SELECT ` + itemViewCols + itemJoins

// pageItemSelect prepends the item's sort key so keyset pagination can build the
// next cursor from the last row without a second lookup.
const pageItemSelect = `SELECT pi.sort_key, ` + itemViewCols + itemJoins

const itemCountSelect = `SELECT COUNT(*)` + itemJoins

const fileSelect = `SELECT id, pid, library_id, path, display_path, rel_path, kind, size,
	mtime_ns, content_hash, essence_hash, analyzed_essence, analysis_version,
	container, codec, duration_ms, bitrate, sample_rate, channels, bit_depth,
	scan_state, first_seen, last_seen FROM file`

// itemViewNulls holds the nullable columns of an item view during a scan.
type itemViewNulls struct {
	trackNo, discNo, year, dur    sql.NullInt64
	compilation                   sql.NullInt64
	fpid, fdisp, container, codec sql.NullString
	fpath                         []byte
}

// itemViewDests returns the scan destinations for an item view, in itemViewCols
// order, so every reader scans the same row shape.
func itemViewDests(v *model.ItemView, n *itemViewNulls) []any {
	return []any{
		&v.PID, &v.Kind, &v.State, &v.Title,
		&v.Artist, &v.AlbumArtist, &v.Album, &n.trackNo, &n.discNo, &n.year, &v.Genre, &n.compilation,
		&n.fpid, &n.fpath, &n.fdisp, &n.dur, &n.container, &n.codec,
	}
}

func (n *itemViewNulls) apply(v *model.ItemView) {
	v.TrackNo = int(n.trackNo.Int64)
	v.DiscNo = int(n.discNo.Int64)
	v.Year = int(n.year.Int64)
	v.Compilation = n.compilation.Int64 != 0
	v.DurationMS = n.dur.Int64
	v.FilePID = model.PID(n.fpid.String)
	v.Path = n.fpath
	v.DisplayPath = n.fdisp.String
	v.Container = n.container.String
	v.Codec = n.codec.String
}

func scanItemView(sc rowScanner) (*model.ItemView, error) {
	var v model.ItemView
	var n itemViewNulls
	if err := sc.Scan(itemViewDests(&v, &n)...); err != nil {
		return nil, err
	}
	n.apply(&v)
	return &v, nil
}

// scanPageItem scans a page row: the leading sort key followed by the item view.
func scanPageItem(sc rowScanner) (*model.ItemView, string, error) {
	var v model.ItemView
	var n itemViewNulls
	var sortKey string
	dests := append([]any{&sortKey}, itemViewDests(&v, &n)...)
	if err := sc.Scan(dests...); err != nil {
		return nil, "", err
	}
	n.apply(&v)
	return &v, sortKey, nil
}

func scanFile(sc rowScanner) (*model.File, error) {
	var f model.File
	var essence, analyzedEssence, container, codec sql.NullString
	var analysisVersion, duration, bitrate, sampleRate, channels, bitDepth sql.NullInt64
	if err := sc.Scan(
		&f.ID, &f.PID, &f.LibraryID, &f.Path, &f.DisplayPath, &f.RelPath, &f.Kind, &f.Size,
		&f.MTimeNS, &f.ContentHash, &essence, &analyzedEssence, &analysisVersion,
		&container, &codec, &duration, &bitrate, &sampleRate, &channels, &bitDepth,
		&f.ScanState, &f.FirstSeen, &f.LastSeen,
	); err != nil {
		return nil, err
	}
	f.EssenceHash = essence.String
	f.AnalyzedEssence = analyzedEssence.String
	f.AnalysisVersion = int(analysisVersion.Int64)
	f.Container = container.String
	f.Codec = codec.String
	f.DurationMS = duration.Int64
	f.Bitrate = int(bitrate.Int64)
	f.SampleRate = int(sampleRate.Int64)
	f.Channels = int(channels.Int64)
	f.BitDepth = int(bitDepth.Int64)
	return &f, nil
}

func scanLibrary(sc rowScanner) (*model.Library, error) {
	var lib model.Library
	var mode string
	if err := sc.Scan(&lib.ID, &lib.PID, &lib.Root, &lib.DisplayRoot, &mode, &lib.Profile, &lib.CreatedAt); err != nil {
		return nil, err
	}
	lib.Mode = model.Mode(mode)
	return &lib, nil
}

const librarySelect = "SELECT id, pid, root, display_root, mode, profile, created_at FROM library"

func libraryByRootTx(ctx context.Context, q queryer, root []byte) (*model.Library, error) {
	return libraryByRootDB(ctx, q, root)
}

func libraryByRootDB(ctx context.Context, q queryer, root []byte) (*model.Library, error) {
	lib, err := scanLibrary(q.QueryRowContext(ctx, librarySelect+" WHERE root = ?", root))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return lib, err
}

func fileByPathTx(ctx context.Context, q queryer, path []byte) (*model.File, error) {
	return fileByPathDB(ctx, q, path)
}

func fileByPathDB(ctx context.Context, q queryer, path []byte) (*model.File, error) {
	f, err := scanFile(q.QueryRowContext(ctx, fileSelect+" WHERE path = ?", path))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

// fileByEssenceSingleTx returns the unique file in a library with the given
// essence hash. It returns nil (no error) when there is no match or more than
// one (ambiguous: do not auto re-link).
func fileByEssenceSingleTx(ctx context.Context, q queryer, essence string, libraryID int64) (*model.File, error) {
	rows, err := q.QueryContext(ctx, fileSelect+" WHERE essence_hash = ? AND library_id = ? LIMIT 2", essence, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var matches []*model.File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		matches = append(matches, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(matches) != 1 {
		return nil, nil
	}
	return matches[0], nil
}

func insertFileRow(ctx context.Context, tx *sql.Tx, libraryID int64, pid model.PID, f model.File, now int64) (int64, error) {
	r, err := tx.ExecContext(ctx, `INSERT INTO file
		(pid, library_id, path, display_path, rel_path, kind, size, mtime_ns,
		 content_hash, essence_hash, container, codec, duration_ms, bitrate,
		 sample_rate, channels, bit_depth, scan_state, first_seen, last_seen)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(pid), libraryID, f.Path, f.DisplayPath, f.RelPath, string(f.Kind), f.Size, f.MTimeNS,
		f.ContentHash, nullStr(f.EssenceHash), nullStr(f.Container), nullStr(f.Codec),
		nullInt64(f.DurationMS), nullInt(f.Bitrate), nullInt(f.SampleRate), nullInt(f.Channels),
		nullInt(f.BitDepth), scanStateOr(f.ScanState), now, now)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// updateFileRow refreshes a file's mutable columns (including path, so it serves
// both the rescan/retag and the essence re-link paths). library_id is left
// unchanged.
func updateFileRow(ctx context.Context, tx *sql.Tx, id int64, f model.File, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE file SET
		path=?, display_path=?, rel_path=?, kind=?, size=?, mtime_ns=?,
		content_hash=?, essence_hash=?, container=?, codec=?, duration_ms=?,
		bitrate=?, sample_rate=?, channels=?, bit_depth=?, scan_state=?, last_seen=?
		WHERE id=?`,
		f.Path, f.DisplayPath, f.RelPath, string(f.Kind), f.Size, f.MTimeNS,
		f.ContentHash, nullStr(f.EssenceHash), nullStr(f.Container), nullStr(f.Codec),
		nullInt64(f.DurationMS), nullInt(f.Bitrate), nullInt(f.SampleRate), nullInt(f.Channels),
		nullInt(f.BitDepth), scanStateOr(f.ScanState), now, id)
	return err
}

func upsertItem(ctx context.Context, tx *sql.Tx, item model.PlayableItem, now int64) (int64, model.PID, bool, error) {
	if item.IdentityKey != "" {
		var id int64
		var pid string
		err := tx.QueryRowContext(ctx,
			"SELECT id, pid FROM playable_item WHERE kind = ? AND identity_key = ?",
			string(item.Kind), item.IdentityKey).Scan(&id, &pid)
		switch {
		case err == nil:
			if _, err := tx.ExecContext(ctx,
				"UPDATE playable_item SET title=?, sort_key=?, state=?, updated_at=? WHERE id=?",
				item.Title, item.SortKey, string(item.State), now, id); err != nil {
				return 0, "", false, err
			}
			return id, model.PID(pid), false, nil
		case err != sql.ErrNoRows:
			return 0, "", false, err
		}
	}
	pid := model.NewPID()
	r, err := tx.ExecContext(ctx, `INSERT INTO playable_item
		(pid, kind, state, title, sort_key, identity_key, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		string(pid), string(item.Kind), string(item.State), item.Title, item.SortKey,
		nullStr(item.IdentityKey), now, now)
	if err != nil {
		return 0, "", false, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, "", false, err
	}
	return id, pid, true, nil
}

func upsertTrack(ctx context.Context, tx *sql.Tx, itemID int64, tr model.Track) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO track
		(item_id, artist, artist_sort, album, album_artist, composer, comment,
		 track_no, track_total, disc_no, disc_total, year, genre, compilation, isrc, mbid)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET
			artist=excluded.artist, artist_sort=excluded.artist_sort, album=excluded.album,
			album_artist=excluded.album_artist, composer=excluded.composer, comment=excluded.comment,
			track_no=excluded.track_no, track_total=excluded.track_total, disc_no=excluded.disc_no,
			disc_total=excluded.disc_total, year=excluded.year, genre=excluded.genre,
			compilation=excluded.compilation, isrc=excluded.isrc, mbid=excluded.mbid`,
		itemID, tr.Artist, tr.ArtistSort, tr.Album, tr.AlbumArtist, tr.Composer, tr.Comment,
		nullInt(tr.TrackNo), nullInt(tr.TrackTotal), nullInt(tr.DiscNo), nullInt(tr.DiscTotal),
		nullInt(tr.Year), tr.Genre, boolInt(tr.Compilation), tr.ISRC, nullStr(tr.MBID))
	return err
}

// linkPrimaryFile makes fileID the primary file of itemID and detaches it from
// any other item. It returns the ids of items that previously held this file as
// primary, other than itemID, so the caller can delete items left with no files.
func linkPrimaryFile(ctx context.Context, tx *sql.Tx, itemID, fileID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT item_id FROM item_file WHERE file_id = ? AND role = 'primary' AND item_id <> ?",
		fileID, itemID)
	if err != nil {
		return nil, err
	}
	var prev []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		prev = append(prev, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_file WHERE role = 'primary' AND (file_id = ? OR item_id = ?)",
		fileID, itemID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO item_file(item_id, file_id, role, position) VALUES (?, ?, 'primary', 0)",
		itemID, fileID); err != nil {
		return nil, err
	}
	return prev, nil
}

// itemHasAnyFile reports whether an item still has any backing file.
func itemHasAnyFile(ctx context.Context, tx *sql.Tx, itemID int64) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM item_file WHERE item_id = ?)", itemID).Scan(&exists)
	return exists == 1, err
}

// deleteItemCascade removes an item (and, via FK cascade, its track, edges, and
// item_genre links), returning its pid for the change_log. The FTS row is keyed
// by rowid with no foreign key, so it is removed explicitly to avoid a stale
// search hit pointing at a deleted item.
func deleteItemCascade(ctx context.Context, tx *sql.Tx, itemID int64) (model.PID, error) {
	var pid model.PID
	if err := tx.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE id = ?", itemID).Scan(&pid); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM search_fts WHERE rowid = ?", itemID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM playable_item WHERE id = ?", itemID); err != nil {
		return "", err
	}
	return pid, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullInt64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanStateOr(s model.ScanState) string {
	if s == "" {
		return string(model.ScanIndexed)
	}
	return string(s)
}
