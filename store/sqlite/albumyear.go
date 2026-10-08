package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// liveMembers joins a track (t) to its item (pi) when the item is not trashed. Trash
// archives an item and keeps its track row for a restore, so a trashed track is no part of
// the album it was on: every reading of an album's members (its year, the duplicate, split
// and consistency checks, the album walks and the album-year fill) goes through this one
// join. The artist and release-group rollups still count it, as trash means them to.
const liveMembers = `JOIN playable_item pi ON pi.id = t.item_id AND pi.state <> 'archived'`

// albumYearExpr is the year an album shows, model.AlbumYear's rule over the years its
// members carry: one at least half of them share, an even split going to the earlier,
// else the latest. A trashed member is no part of it, and with no member carrying a year
// it is NULL, so the album's year is its members' and nothing else: a provider's year
// reaches it only through the members it fills. al is the album; per year, n counts its
// members, d the dated members and c the most any year has.
const albumYearExpr = `(SELECT y FROM (
	SELECT t.year AS y, COUNT(*) AS n, SUM(COUNT(*)) OVER () AS d, MAX(COUNT(*)) OVER () AS c
	FROM track t ` + liveMembers + `
	WHERE t.album_id = al.id AND t.year > 0
	GROUP BY t.year)
	ORDER BY CASE WHEN c > 1 AND 2 * c >= d THEN n ELSE 0 END DESC,
		CASE WHEN c > 1 AND 2 * c >= d THEN y ELSE -y END
	LIMIT 1)`

// albumYearDriftQ selects the albums whose year is not albumYearExpr, with the year they
// should have. The "/*FILTER*/" marker takes an id filter, or nothing for every album.
const albumYearDriftQ = `SELECT id, pid, want FROM (
	SELECT al.id, al.pid, al.year AS cur, ` + albumYearExpr + ` AS want FROM album al /*FILTER*/)
	WHERE cur IS NOT want`

// AlbumYears returns the year of each of the albums that has one, for a layout that files
// an album's tracks under the album's year. An unknown pid is left out.
func (s *Store) AlbumYears(ctx context.Context, pids []model.PID) (map[model.PID]int, error) {
	const op = "store.AlbumYears"
	out := map[model.PID]int{}
	err := chunkSlice(uniquePIDs(pids), idBatchSize, func(chunk []model.PID) error {
		args := make([]any, len(chunk))
		for i, p := range chunk {
			args[i] = string(p)
		}
		rows, err := s.rdb().QueryContext(ctx,
			"SELECT pid, year FROM album WHERE year > 0 AND pid IN "+placeholders(len(chunk)), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pid string
			var year int
			if err := rows.Scan(&pid, &year); err != nil {
				return err
			}
			out[model.PID(pid)] = year
		}
		return rows.Err()
	})
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// refreshAlbumYearsTx sets each of the albums' year to albumYearExpr, writing and
// emitting an album delta only where it moves.
func refreshAlbumYearsTx(ctx context.Context, tx *sql.Tx, albumIDs []int64) error {
	return chunkSlice(albumIDs, idBatchSize, func(chunk []int64) error {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		return refreshAlbumYearsWhereTx(ctx, tx, "WHERE al.id IN "+placeholders(len(chunk)), args...)
	})
}

// refreshAllAlbumYearsTx is refreshAlbumYearsTx over every album, the repair for the
// drift VerifyDerived reports.
func refreshAllAlbumYearsTx(ctx context.Context, tx *sql.Tx) error {
	return refreshAlbumYearsWhereTx(ctx, tx, "")
}

func refreshAlbumYearsWhereTx(ctx context.Context, tx *sql.Tx, filter string, args ...any) error {
	type move struct {
		id   int64
		pid  model.PID
		year sql.NullInt64
	}
	rows, err := tx.QueryContext(ctx, strings.Replace(albumYearDriftQ, "/*FILTER*/", filter, 1), args...)
	if err != nil {
		return err
	}
	var moves []move
	for rows.Next() {
		var m move
		if err := rows.Scan(&m.id, &m.pid, &m.year); err != nil {
			rows.Close()
			return err
		}
		moves = append(moves, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range moves {
		if _, err := tx.ExecContext(ctx, "UPDATE album SET year = ? WHERE id = ?", m.year, m.id); err != nil {
			return err
		}
		if err := appendChange(ctx, tx, "album", m.pid, model.OpUpdate); err != nil {
			return err
		}
	}
	return nil
}
