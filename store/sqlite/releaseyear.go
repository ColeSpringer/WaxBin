package sqlite

import (
	"context"
	"database/sql"
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
// orders the newest list and nothing an item view shows, so it emits no delta.
func refreshAllReleaseYearsTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE playable_item AS pi SET release_year = `+itemReleaseYearExpr+
		` WHERE pi.release_year <> `+itemReleaseYearExpr)
	return err
}
