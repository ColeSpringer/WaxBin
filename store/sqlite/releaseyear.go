package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
)

// itemReleaseYearExpr is the year playable_item.release_year holds for the item pi: its
// track's, else its book's, else 0, which is an episode's too. The drift check and its
// repair both read it.
const itemReleaseYearExpr = `COALESCE((SELECT rt.year FROM track rt WHERE rt.item_id = pi.id),
	(SELECT rb.year FROM book rb WHERE rb.item_id = pi.id), 0)`

const releaseYearDriftQ = `SELECT COUNT(*) FROM playable_item pi WHERE pi.release_year <> ` + itemReleaseYearExpr

// setReleaseYearTx keeps an item's release_year at the year its track or book row was
// just written with by an edit, writing only when it moves. A scan put carries the year
// on the item row's own write (upsertItem) and needs no call.
func setReleaseYearTx(ctx context.Context, tx *sql.Tx, itemID int64, year int) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE playable_item SET release_year = ? WHERE id = ? AND release_year <> ?", year, itemID, year)
	return err
}

// refreshAllReleaseYearsTx is the repair for the drift VerifyDerived reports. The column
// orders the newest list, so each item it moves gets an update delta, as a refold of a sort
// key does. It returns how many it moved.
func refreshAllReleaseYearsTx(ctx context.Context, tx *sql.Tx) (int, error) {
	type move struct {
		id   int64
		pid  model.PID
		year int
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, pid, want FROM (
		SELECT pi.id, pi.pid, pi.release_year AS cur, `+itemReleaseYearExpr+` AS want FROM playable_item pi)
		WHERE cur <> want`)
	if err != nil {
		return 0, err
	}
	var moves []move
	for rows.Next() {
		var m move
		if err := rows.Scan(&m.id, &m.pid, &m.year); err != nil {
			rows.Close()
			return 0, err
		}
		moves = append(moves, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, m := range moves {
		if err := setReleaseYearTx(ctx, tx, m.id, m.year); err != nil {
			return 0, err
		}
		if err := appendChange(ctx, tx, "item", m.pid, model.OpUpdate); err != nil {
			return 0, err
		}
	}
	return len(moves), nil
}
