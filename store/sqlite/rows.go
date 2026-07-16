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

// itemJoins LEFT JOINs all three subtypes (track, book, episode) plus the book's
// series and the episode's podcast, so a book or episode item, which has no track
// row, still reads back. The book's author/series/year and the podcast's title
// stand in for the music artist/album/year columns via COALESCE in itemViewCols,
// and the primary file is the representative backing file (the first part of a
// multi-file book, or the downloaded enclosure of an episode; NULL for a not-yet-
// downloaded episode).
const itemJoins = ` FROM playable_item pi
	LEFT JOIN track t ON t.item_id = pi.id
	LEFT JOIN book bk ON bk.item_id = pi.id
	LEFT JOIN series srs ON srs.id = bk.series_id
	LEFT JOIN episode ep ON ep.item_id = pi.id
	LEFT JOIN podcast pod ON pod.id = ep.podcast_id
	LEFT JOIN acquisition acq ON acq.item_id = pi.id
	LEFT JOIN item_file pf ON pf.item_id = pi.id AND pf.role = 'primary'
	LEFT JOIN file f ON f.id = pf.file_id`

// itemEffectiveDurationExpr is the playable duration of one item, in milliseconds,
// given its primary item_file edge (alias pf) and backing file (alias f). A virtual
// track carved from a shared single-file rip by a .cue sheet plays only its window
// [start_ms, end_ms) within the file, so its duration is that window; every other
// item plays the whole file. It is reused verbatim by the item read view, the
// duration_ms query field, the maintained rollups, db verify's rollup-drift checks,
// and the library stats total, so display, filter, and aggregates can never
// disagree. Every one of those sites already binds the pf and f aliases it depends
// on.
//
// The MAX(0, ...) floors the window against a malformed or mismatched cue: if the
// final track's start lies past the file's probed duration (its end is left open, so
// the window falls back to f.duration_ms), the raw subtraction would be negative and
// would corrupt every duration sum. A window of genuinely unknown length (both
// end_ms and f.duration_ms NULL) still yields NULL, since scalar MAX returns NULL
// when any argument is NULL, so it is skipped in a SUM and COALESCEd in the view,
// exactly as before.
const itemEffectiveDurationExpr = `CASE WHEN pf.start_ms IS NOT NULL ` +
	`THEN MAX(0, COALESCE(pf.end_ms, f.duration_ms) - pf.start_ms) ELSE f.duration_ms END`

// itemViewCols is the column list for an item read-view, shared by the plain
// select and the keyset-paginated select (which prepends pi.sort_key). The shared
// artist/album_artist/album/year/genre columns COALESCE the track values with the
// book's author/series/year and the podcast's title so one view shape serves all
// three kinds; the audiobook columns are empty for tracks/episodes and the
// podcast columns (season, pub_date) are empty otherwise. The duration is the
// book's denormalized total_duration_ms (the sum of its parts), then the item's
// effective duration (a virtual track's window, else the primary file's whole
// duration for a downloaded episode or a track), then the feed-declared episode
// duration (an undownloaded episode). The trailing pf.start_ms/pf.end_ms expose a
// virtual track's offset window.
const itemViewCols = `pi.pid, pi.kind, pi.state, pi.title,
	COALESCE(NULLIF(t.artist,''), bk.author, pod.title, ''),
	COALESCE(NULLIF(t.album_artist,''), bk.author, pod.title, ''),
	COALESCE(NULLIF(t.album,''), srs.name, pod.title, ''),
	t.track_no, t.disc_no, COALESCE(t.year, bk.year, ep.year),
	COALESCE(NULLIF(t.genre,''), bk.genre, ''), t.compilation,
	COALESCE(bk.author_sort,''), COALESCE(bk.narrator,''), COALESCE(srs.name,''),
	COALESCE(bk.series_seq,''), COALESCE(bk.subtitle,''), COALESCE(bk.asin,''),
	ep.season, ep.pub_date,
	COALESCE(acq.source_type, pod.source_type, 'local'),
	f.pid, f.path, f.display_path,
	COALESCE(bk.total_duration_ms, ` + itemEffectiveDurationExpr + `, ep.duration_ms),
	f.container, f.codec, pf.start_ms, pf.end_ms`

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
	season, pubDate               sql.NullInt64
	startMS, endMS                sql.NullInt64
	fpid, fdisp, container, codec sql.NullString
	fpath                         []byte
}

// itemViewDests returns the scan destinations for an item view, in itemViewCols
// order, so every reader scans the same row shape.
func itemViewDests(v *model.ItemView, n *itemViewNulls) []any {
	return []any{
		&v.PID, &v.Kind, &v.State, &v.Title,
		&v.Artist, &v.AlbumArtist, &v.Album, &n.trackNo, &n.discNo, &n.year, &v.Genre, &n.compilation,
		&v.AuthorSort, &v.Narrator, &v.Series, &v.SeriesSeq, &v.Subtitle, &v.ASIN,
		&n.season, &n.pubDate, &v.Source,
		&n.fpid, &n.fpath, &n.fdisp, &n.dur, &n.container, &n.codec, &n.startMS, &n.endMS,
	}
}

func (n *itemViewNulls) apply(v *model.ItemView) {
	v.TrackNo = int(n.trackNo.Int64)
	v.DiscNo = int(n.discNo.Int64)
	v.Year = int(n.year.Int64)
	v.Compilation = n.compilation.Int64 != 0
	v.Season = int(n.season.Int64)
	v.PubDateNS = n.pubDate.Int64
	v.DurationMS = n.dur.Int64
	v.FilePID = model.PID(n.fpid.String)
	v.Path = n.fpath
	v.DisplayPath = n.fdisp.String
	v.Container = n.container.String
	v.Codec = n.codec.String
	// A non-NULL start offset marks the primary edge as a virtual track's window.
	v.Virtual = n.startMS.Valid
	v.StartMS = n.startMS.Int64
	v.EndMS = n.endMS.Int64
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
	var mode, media string
	if err := sc.Scan(&lib.ID, &lib.PID, &lib.Root, &lib.DisplayRoot, &mode, &media, &lib.Profile, &lib.CreatedAt); err != nil {
		return nil, err
	}
	lib.Mode = model.Mode(mode)
	lib.Media = model.MediaType(media)
	return &lib, nil
}

const librarySelect = "SELECT id, pid, root, display_root, mode, media, profile, created_at FROM library"

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

// upsertItem finds-or-creates the logical item, returning its id, pid, whether it
// was created, and whether an existing item's state transitioned (e.g. missing ->
// present when a file is restored) so the caller can emit a change_log delta for the
// transition even when the audio content is unchanged.
func upsertItem(ctx context.Context, tx *sql.Tx, item model.PlayableItem, now int64, preferredPID model.PID) (id int64, pid model.PID, created, stateChanged bool, err error) {
	if item.IdentityKey != "" {
		var rid int64
		var rpid, curState string
		qerr := tx.QueryRowContext(ctx,
			"SELECT id, pid, state FROM playable_item WHERE kind = ? AND identity_key = ?",
			string(item.Kind), item.IdentityKey).Scan(&rid, &rpid, &curState)
		switch {
		case qerr == nil:
			if _, uerr := tx.ExecContext(ctx,
				"UPDATE playable_item SET title=?, sort_key=?, state=?, updated_at=? WHERE id=?",
				item.Title, item.SortKey, string(item.State), now, rid); uerr != nil {
				return 0, "", false, false, uerr
			}
			return rid, model.PID(rpid), false, curState != string(item.State), nil
		case qerr != sql.ErrNoRows:
			return 0, "", false, false, qerr
		}
	}
	// A new item mints a fresh PID, unless a rebuild supplied a valid, unclaimed
	// preferred PID (from a WAXBIN_ITEM_PID tag) to restore the original identity.
	// Identity stays essence-first: the tag is only a hint, so any conflict (invalid,
	// or already taken by another item, e.g. a copied file or a wrong-kind or stale tag)
	// falls back to a fresh PID.
	newPID := model.NewPID()
	if preferredPID != "" && preferredPID.Valid() {
		var taken int
		if terr := tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM playable_item WHERE pid = ?)", string(preferredPID)).Scan(&taken); terr != nil {
			return 0, "", false, false, terr
		}
		if taken == 0 {
			newPID = preferredPID
		}
	}
	r, ierr := tx.ExecContext(ctx, `INSERT INTO playable_item
		(pid, kind, state, title, sort_key, identity_key, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		string(newPID), string(item.Kind), string(item.State), item.Title, item.SortKey,
		nullStr(item.IdentityKey), now, now)
	if ierr != nil {
		return 0, "", false, false, ierr
	}
	rid, ierr := r.LastInsertId()
	if ierr != nil {
		return 0, "", false, false, ierr
	}
	return rid, newPID, true, false, nil
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
// any other item, in ANY role. It returns the ids of items that previously held
// this file (other than itemID), so the caller can delete items left with no files
// or promote a new primary. Detaching every role matters because a file can be a
// multi-file book's 'part' edge; re-keying it to a track must not leave that edge
// dangling (the file attached to both the book and the track).
func linkPrimaryFile(ctx context.Context, tx *sql.Tx, itemID, fileID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT DISTINCT item_id FROM item_file WHERE file_id = ? AND item_id <> ?",
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

	// Drop every edge of this file (anywhere it is attached) plus this item's own
	// existing primary, then re-insert it as this item's primary.
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_file WHERE file_id = ? OR (item_id = ? AND role = 'primary')",
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

// linkVirtualTrackFile attaches the shared file to a virtual-track item as its
// primary edge, carrying the [startMS, endMS) offset window, WITHOUT detaching the
// file from its sibling virtual tracks and WITHOUT deleting an item left with no
// files. This is the deliberate departure from linkPrimaryFile's single-owner
// detach: one file backs every virtual track of a single-file rip, so the normal
// path would rip the edge out from under the siblings and delete them. It replaces
// only this item's own primary edge and no-ops when that edge already carries the
// same file and window, so a rescan of an unchanged rip stays silent. It reports
// whether it changed the edge. startMS is stored verbatim (0 for the first track is
// a real value, and its non-NULL presence is what marks the edge virtual); endMS is
// stored NULL when 0 (the final track runs to the end of a file of unknown length).
func linkVirtualTrackFile(ctx context.Context, tx *sql.Tx, itemID, fileID, startMS, endMS int64) (bool, error) {
	var curFile int64
	var curStart, curEnd sql.NullInt64
	err := tx.QueryRowContext(ctx,
		"SELECT file_id, start_ms, end_ms FROM item_file WHERE item_id = ? AND role = 'primary'", itemID).
		Scan(&curFile, &curStart, &curEnd)
	switch {
	case err == nil:
		if curFile == fileID && curStart.Valid && curStart.Int64 == startMS &&
			curEnd.Valid == (endMS != 0) && curEnd.Int64 == endMS {
			return false, nil // unchanged
		}
	case err != sql.ErrNoRows:
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_file WHERE item_id = ? AND role = 'primary'", itemID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO item_file(item_id, file_id, role, position, start_ms, end_ms) VALUES (?,?,'primary',0,?,?)",
		itemID, fileID, startMS, nullInt64(endMS)); err != nil {
		return false, err
	}
	return true, nil
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
	// entity_enrichment is polymorphic (no FK cascade). A book's marker and a track's
	// lyrics marker are both keyed by the item id, so drop them here. Because
	// playable_item.id is not AUTOINCREMENT, a reused rowid could otherwise inherit a
	// stale "already enriched" marker and skip a new book, or skip lyrics for a new
	// track. Artist and release-group markers have no delete path in v1.0; their
	// cleanup rides with the future entity-merge/GC gate.
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM entity_enrichment WHERE entity_type IN ('book','lyrics') AND entity_id = ?", itemID); err != nil {
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
