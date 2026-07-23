package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// EntityByPID returns the summary info for one shared entity (artist, release
// group, album, genre, or series): identity, parent links, membership counts,
// and the libraries its members' primary files live in. It is the direct read
// behind a facet bucket's or query field's EntityPID, so a consumer holding a
// pid never has to reconstruct the entity from a full facet scan.
//
// Artist, release-group, and genre counts read the maintained rollups (one PK
// seek); album and series have no rollup rows and aggregate live over their
// indexed member columns (track_album_id, book_series), reusing the effective-
// duration expression so a virtual track prices its window here exactly as it
// does in the rollups and the item view. A series sums book.total_duration_ms,
// the maintained sum of each book's parts, because a multi-file book's primary
// edge alone would undercount it. An unknown kind is CodeInvalid; an unknown
// pid is CodeNotFound.
func (s *Store) EntityByPID(ctx context.Context, kind read.EntityKind, pid model.PID) (*read.EntityInfo, error) {
	const op = "store.EntityByPID"
	if !kind.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown entity kind: "+string(kind))
	}
	info := &read.EntityInfo{Kind: kind, PID: pid}
	var id int64
	var artistPID, rgPID string
	var err error
	switch kind {
	case read.EntityArtist:
		err = s.read.QueryRowContext(ctx, `SELECT a.id, a.name, a.sort_key, COALESCE(a.mbid,''),
			COALESCE(ar.track_count,0), COALESCE(ar.release_group_count,0), COALESCE(ar.total_duration_ms,0)
			FROM artist a LEFT JOIN artist_rollup ar ON ar.artist_id = a.id
			WHERE a.pid = ?`, string(pid)).
			Scan(&id, &info.Name, &info.SortKey, &info.MBID, &info.ItemCount, &info.ReleaseGroupCount, &info.TotalDurationMS)
	case read.EntityReleaseGroup:
		err = s.read.QueryRowContext(ctx, `SELECT rg.id, rg.title, rg.sort_key, COALESCE(rg.mbid,''),
			COALESCE(rg.type,''), COALESCE(pa.pid,''),
			COALESCE(rr.track_count,0), COALESCE(rr.total_duration_ms,0)
			FROM release_group rg
			LEFT JOIN artist pa ON pa.id = rg.primary_artist_id
			LEFT JOIN release_group_rollup rr ON rr.release_group_id = rg.id
			WHERE rg.pid = ?`, string(pid)).
			Scan(&id, &info.Name, &info.SortKey, &info.MBID, &info.Type, &artistPID, &info.ItemCount, &info.TotalDurationMS)
	case read.EntityAlbum:
		err = s.read.QueryRowContext(ctx, `SELECT al.id, al.title, al.sort_key, COALESCE(al.mbid,''),
			COALESCE(al.year,0), COALESCE(rg.pid,'')
			FROM album al LEFT JOIN release_group rg ON rg.id = al.release_group_id
			WHERE al.pid = ?`, string(pid)).
			Scan(&id, &info.Name, &info.SortKey, &info.MBID, &info.Year, &rgPID)
	case read.EntityGenre:
		err = s.read.QueryRowContext(ctx, `SELECT g.id, g.name, g.sort_key,
			COALESCE(gr.track_count,0), COALESCE(gr.total_duration_ms,0)
			FROM genre g LEFT JOIN genre_rollup gr ON gr.genre_id = g.id
			WHERE g.pid = ?`, string(pid)).
			Scan(&id, &info.Name, &info.SortKey, &info.ItemCount, &info.TotalDurationMS)
	case read.EntitySeries:
		err = s.read.QueryRowContext(ctx,
			"SELECT srs.id, srs.name, srs.sort_key, COALESCE(srs.mbid,'') FROM series srs WHERE srs.pid = ?",
			string(pid)).
			Scan(&id, &info.Name, &info.SortKey, &info.MBID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such "+string(kind)+": "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	info.ArtistPID, info.ReleaseGroupPID = model.PID(artistPID), model.PID(rgPID)

	switch kind {
	case read.EntityAlbum:
		err = s.read.QueryRowContext(ctx, `SELECT COUNT(DISTINCT t.item_id), COALESCE(SUM(`+itemEffectiveDurationExpr+`),0)
			FROM track t
			LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
			LEFT JOIN file f ON f.id = pf.file_id
			WHERE t.album_id = ?`, id).
			Scan(&info.ItemCount, &info.TotalDurationMS)
	case read.EntitySeries:
		err = s.read.QueryRowContext(ctx,
			"SELECT COUNT(*), COALESCE(SUM(bk.total_duration_ms),0) FROM book bk WHERE bk.series_id = ?", id).
			Scan(&info.ItemCount, &info.TotalDurationMS)
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	libs, err := s.entityLibraryPIDs(ctx, kind, id, op)
	if err != nil {
		return nil, err
	}
	info.LibraryPIDs = libs
	return info, nil
}

// entityLibraryPIDs returns the distinct libraries holding an entity's member
// items' primary backing files, in library order. Artist membership shares
// itemArtistIDExpr with the artist facet and the artist_pid query field, so the
// three surfaces can never disagree about which items an artist backs (a book
// counts under its author). A fileless member contributes nothing (the INNER
// primary-file join).
func (s *Store) entityLibraryPIDs(ctx context.Context, kind read.EntityKind, id int64, op string) ([]model.PID, error) {
	var from, where string
	switch kind {
	case read.EntityArtist:
		from = ` FROM playable_item pi
			LEFT JOIN track t ON t.item_id = pi.id
			LEFT JOIN book bk ON bk.item_id = pi.id
			JOIN item_file pf ON pf.item_id = pi.id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`
		where = itemArtistIDExpr + " = ?"
	case read.EntityReleaseGroup:
		from = ` FROM track t
			JOIN album al ON al.id = t.album_id
			JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`
		where = "al.release_group_id = ?"
	case read.EntityAlbum:
		from = ` FROM track t
			JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`
		where = "t.album_id = ?"
	case read.EntityGenre:
		from = ` FROM item_genre ig
			JOIN item_file pf ON pf.item_id = ig.item_id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`
		where = "ig.genre_id = ?"
	case read.EntitySeries:
		from = ` FROM book bk
			JOIN item_file pf ON pf.item_id = bk.item_id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`
		where = "bk.series_id = ?"
	}
	rows, err := s.read.QueryContext(ctx,
		"SELECT l.pid"+from+" JOIN library l ON l.id = f.library_id WHERE "+where+" GROUP BY l.id ORDER BY l.id", id)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.PID
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, model.PID(pid))
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}
