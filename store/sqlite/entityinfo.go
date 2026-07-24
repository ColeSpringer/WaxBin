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

// entityMemberSource returns the FROM/JOIN clause reaching an entity's member
// items' primary backing files, and the expression selecting each member's entity
// id. It is shared by the single and batched library-pid lookups so the two can
// never diverge about which items back an entity. Artist membership uses
// itemArtistIDExpr, the same expression the artist facet and the artist_pid query
// field consume, so a book counts under its author consistently across all of them.
// A fileless member contributes nothing (the INNER primary-file join).
func entityMemberSource(kind read.EntityKind) (from, idExpr string) {
	switch kind {
	case read.EntityArtist:
		return ` FROM playable_item pi
			LEFT JOIN track t ON t.item_id = pi.id
			LEFT JOIN book bk ON bk.item_id = pi.id
			JOIN item_file pf ON pf.item_id = pi.id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`, itemArtistIDExpr
	case read.EntityReleaseGroup:
		return ` FROM track t
			JOIN album al ON al.id = t.album_id
			JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`, "al.release_group_id"
	case read.EntityAlbum:
		return ` FROM track t
			JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`, "t.album_id"
	case read.EntityGenre:
		return ` FROM item_genre ig
			JOIN item_file pf ON pf.item_id = ig.item_id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`, "ig.genre_id"
	case read.EntitySeries:
		return ` FROM book bk
			JOIN item_file pf ON pf.item_id = bk.item_id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id`, "bk.series_id"
	}
	return "", ""
}

// entityLibraryPIDs returns the distinct libraries holding an entity's member
// items' primary backing files, in library order.
func (s *Store) entityLibraryPIDs(ctx context.Context, kind read.EntityKind, id int64, op string) ([]model.PID, error) {
	from, idExpr := entityMemberSource(kind)
	rows, err := s.read.QueryContext(ctx,
		"SELECT l.pid"+from+" JOIN library l ON l.id = f.library_id WHERE "+idExpr+" = ? GROUP BY l.id ORDER BY l.id", id)
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

// EntityByPIDs is the batched form of EntityByPID: it returns summary info for many
// entities of a single kind, keyed by pid. It retires the one-EntityByPID-per-hit
// cost a consumer pays hydrating a page of entity pids, such as a restricted-user
// entity search that scopes each hit to the user's libraries through
// EntityInfo.LibraryPIDs. An unknown or repeated pid is dropped (a repeat collapses
// to one entry). The caller batches within a single kind. Like ItemsByPIDs it chunks
// to stay under the bound-parameter limit, so a set larger than a chunk spans several
// statements and is NOT an atomic snapshot. It runs the same three reads EntityByPID
// does, each batched: the base identity/rollup row, the live album/series aggregate,
// and the member libraries. Field-for-field it matches EntityByPID (an
// entities-by-pids parity test pins that).
func (s *Store) EntityByPIDs(ctx context.Context, kind read.EntityKind, pids []model.PID) (map[model.PID]*read.EntityInfo, error) {
	const op = "store.EntityByPIDs"
	if !kind.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown entity kind: "+string(kind))
	}
	if len(pids) == 0 {
		return nil, nil
	}
	unique := uniquePIDs(pids)
	out := make(map[model.PID]*read.EntityInfo, len(unique))
	err := chunkSlice(unique, idBatchSize, func(chunk []model.PID) error {
		byID, ids, err := s.entityBaseBatch(ctx, kind, chunk, out, op)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		// Album and series carry no rollup rows, so their counts aggregate live,
		// keyed by entity id (the other kinds read counts from the base rollup query).
		if kind == read.EntityAlbum || kind == read.EntitySeries {
			counts, err := s.entityLiveCountsBatch(ctx, kind, ids, op)
			if err != nil {
				return err
			}
			for id, c := range counts {
				if info := byID[id]; info != nil {
					info.ItemCount, info.TotalDurationMS = c.items, c.duration
				}
			}
		}
		libs, err := s.entityLibraryPIDsBatch(ctx, kind, ids, op)
		if err != nil {
			return err
		}
		for id, pids := range libs {
			if info := byID[id]; info != nil {
				info.LibraryPIDs = pids
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// entityBaseBatch runs the per-kind base identity/rollup query for one chunk of
// pids, keying each found entity into out by pid and returning an id->info map plus
// the found ids (both driving the live-count and library follow-ups). It mirrors
// EntityByPID's base switch column for column, adding the entity's own pid so the
// result can be keyed without a second lookup.
func (s *Store) entityBaseBatch(ctx context.Context, kind read.EntityKind, chunk []model.PID, out map[model.PID]*read.EntityInfo, op string) (map[int64]*read.EntityInfo, []int64, error) {
	args := make([]any, len(chunk))
	for i, pid := range chunk {
		args[i] = string(pid)
	}
	ph := placeholders(len(chunk))

	var stmt string
	var scan func(*sql.Rows) (int64, *read.EntityInfo, error)
	switch kind {
	case read.EntityArtist:
		stmt = `SELECT a.id, a.pid, a.name, a.sort_key, COALESCE(a.mbid,''),
			COALESCE(ar.track_count,0), COALESCE(ar.release_group_count,0), COALESCE(ar.total_duration_ms,0)
			FROM artist a LEFT JOIN artist_rollup ar ON ar.artist_id = a.id
			WHERE a.pid IN ` + ph
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey, &info.MBID,
				&info.ItemCount, &info.ReleaseGroupCount, &info.TotalDurationMS)
			return id, info, err
		}
	case read.EntityReleaseGroup:
		stmt = `SELECT rg.id, rg.pid, rg.title, rg.sort_key, COALESCE(rg.mbid,''),
			COALESCE(rg.type,''), COALESCE(pa.pid,''),
			COALESCE(rr.track_count,0), COALESCE(rr.total_duration_ms,0)
			FROM release_group rg
			LEFT JOIN artist pa ON pa.id = rg.primary_artist_id
			LEFT JOIN release_group_rollup rr ON rr.release_group_id = rg.id
			WHERE rg.pid IN ` + ph
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			var artistPID string
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey, &info.MBID,
				&info.Type, &artistPID, &info.ItemCount, &info.TotalDurationMS)
			info.ArtistPID = model.PID(artistPID)
			return id, info, err
		}
	case read.EntityAlbum:
		stmt = `SELECT al.id, al.pid, al.title, al.sort_key, COALESCE(al.mbid,''),
			COALESCE(al.year,0), COALESCE(rg.pid,'')
			FROM album al LEFT JOIN release_group rg ON rg.id = al.release_group_id
			WHERE al.pid IN ` + ph
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			var rgPID string
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey, &info.MBID, &info.Year, &rgPID)
			info.ReleaseGroupPID = model.PID(rgPID)
			return id, info, err
		}
	case read.EntityGenre:
		stmt = `SELECT g.id, g.pid, g.name, g.sort_key,
			COALESCE(gr.track_count,0), COALESCE(gr.total_duration_ms,0)
			FROM genre g LEFT JOIN genre_rollup gr ON gr.genre_id = g.id
			WHERE g.pid IN ` + ph
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey, &info.ItemCount, &info.TotalDurationMS)
			return id, info, err
		}
	case read.EntitySeries:
		stmt = `SELECT srs.id, srs.pid, srs.name, srs.sort_key, COALESCE(srs.mbid,'')
			FROM series srs WHERE srs.pid IN ` + ph
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey, &info.MBID)
			return id, info, err
		}
	}

	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	byID := make(map[int64]*read.EntityInfo, len(chunk))
	var ids []int64
	for rows.Next() {
		id, info, err := scan(rows)
		if err != nil {
			return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		info.Kind = kind
		out[info.PID] = info
		byID[id] = info
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return byID, ids, nil
}

// entityCounts is one entity's live-aggregated membership: item count and total
// effective duration, for the album/series kinds that have no rollup rows.
type entityCounts struct {
	items    int
	duration int64
}

// entityLiveCountsBatch aggregates the album/series member counts live for a chunk
// of entity ids, keyed by entity id, reusing the effective-duration expression so a
// virtual track prices its window exactly as EntityByPID and the rollups do. An
// entity with no members is simply absent (its info keeps a zero count).
func (s *Store) entityLiveCountsBatch(ctx context.Context, kind read.EntityKind, ids []int64, op string) (map[int64]entityCounts, error) {
	var stmt string
	switch kind {
	case read.EntityAlbum:
		stmt = `SELECT t.album_id, COUNT(DISTINCT t.item_id), COALESCE(SUM(` + itemEffectiveDurationExpr + `),0)
			FROM track t
			LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
			LEFT JOIN file f ON f.id = pf.file_id
			WHERE t.album_id IN ` + placeholders(len(ids)) + ` GROUP BY t.album_id`
	case read.EntitySeries:
		stmt = `SELECT bk.series_id, COUNT(*), COALESCE(SUM(bk.total_duration_ms),0)
			FROM book bk WHERE bk.series_id IN ` + placeholders(len(ids)) + ` GROUP BY bk.series_id`
	default:
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	out := make(map[int64]entityCounts, len(ids))
	for rows.Next() {
		var id int64
		var c entityCounts
		if err := rows.Scan(&id, &c.items, &c.duration); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// entityLibraryPIDsBatch is the batched entityLibraryPIDs: the distinct libraries
// holding each entity's member files, keyed by entity id, in library order per
// entity. It shares entityMemberSource with the single lookup, projecting and
// grouping by the member's entity id so one query serves the whole chunk.
func (s *Store) entityLibraryPIDsBatch(ctx context.Context, kind read.EntityKind, ids []int64, op string) (map[int64][]model.PID, error) {
	from, idExpr := entityMemberSource(kind)
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.read.QueryContext(ctx,
		"SELECT "+idExpr+" AS eid, l.pid"+from+
			" JOIN library l ON l.id = f.library_id WHERE "+idExpr+" IN "+placeholders(len(ids))+
			" GROUP BY eid, l.id ORDER BY eid, l.id", args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	out := make(map[int64][]model.PID, len(ids))
	for rows.Next() {
		var eid int64
		var pid string
		if err := rows.Scan(&eid, &pid); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out[eid] = append(out[eid], model.PID(pid))
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}
