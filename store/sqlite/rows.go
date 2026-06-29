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

const itemSelect = `SELECT pi.pid, pi.kind, pi.state, pi.title,
	t.artist, t.album_artist, t.album, t.track_no, t.disc_no, t.year, t.genre,
	f.pid, f.path, f.display_path, f.duration_ms, f.container, f.codec` + itemJoins

const itemCountSelect = `SELECT COUNT(*)` + itemJoins

const fileSelect = `SELECT id, pid, library_id, path, display_path, rel_path, kind, size,
	mtime_ns, content_hash, essence_hash, analyzed_essence, analysis_version,
	container, codec, duration_ms, bitrate, sample_rate, channels, bit_depth,
	scan_state, first_seen, last_seen FROM file`

func scanItemView(sc rowScanner) (*model.ItemView, error) {
	var v model.ItemView
	var trackNo, discNo, year, dur sql.NullInt64
	var fpid, fdisp, container, codec sql.NullString
	var fpath []byte
	if err := sc.Scan(
		&v.PID, &v.Kind, &v.State, &v.Title,
		&v.Artist, &v.AlbumArtist, &v.Album, &trackNo, &discNo, &year, &v.Genre,
		&fpid, &fpath, &fdisp, &dur, &container, &codec,
	); err != nil {
		return nil, err
	}
	v.TrackNo = int(trackNo.Int64)
	v.DiscNo = int(discNo.Int64)
	v.Year = int(year.Int64)
	v.DurationMS = dur.Int64
	v.FilePID = model.PID(fpid.String)
	v.Path = fpath
	v.DisplayPath = fdisp.String
	v.Container = container.String
	v.Codec = codec.String
	return &v, nil
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
		(item_id, artist, artist_sort, album, album_artist, track_no, disc_no, year, genre, mbid)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET
			artist=excluded.artist, artist_sort=excluded.artist_sort, album=excluded.album,
			album_artist=excluded.album_artist, track_no=excluded.track_no, disc_no=excluded.disc_no,
			year=excluded.year, genre=excluded.genre, mbid=excluded.mbid`,
		itemID, tr.Artist, tr.ArtistSort, tr.Album, tr.AlbumArtist,
		nullInt(tr.TrackNo), nullInt(tr.DiscNo), nullInt(tr.Year), tr.Genre, nullStr(tr.MBID))
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

// deleteItemCascade removes an item (and, via FK cascade, its track + edges),
// returning its pid for the change_log.
func deleteItemCascade(ctx context.Context, tx *sql.Tx, itemID int64) (model.PID, error) {
	var pid model.PID
	if err := tx.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE id = ?", itemID).Scan(&pid); err != nil {
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

func scanStateOr(s model.ScanState) string {
	if s == "" {
		return string(model.ScanIndexed)
	}
	return string(s)
}
