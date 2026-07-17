package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// lyricLineJSON is the stored shape of one synced lyric line.
type lyricLineJSON struct {
	MS   int64  `json:"ms"`
	Text string `json:"text"`
}

// putLyricsTx writes (or clears) an item's lyrics row. It is idempotent: it
// compares the desired lyrics against the stored row and writes only on a
// difference, so it can run on every scan (catching an added/edited .lrc sidecar)
// without churning a no-op rescan. Empty/nil lyrics delete any existing row so the
// table stays sparse; synced lines are stored as a JSON array in time order. It
// reports whether it changed the stored row, so the sidecar-update seam can emit a
// delta only on a real change.
// preserveLock, when true, makes a would-be change to a user-locked lyrics row a
// no-op, so a scan/enrich pass never overwrites a curated edit. SetItemLyrics passes
// false (it is the authoritative user write).
func putLyricsTx(ctx context.Context, tx *sql.Tx, itemID int64, ly *model.Lyrics, preserveLock bool) (bool, error) {
	// Desired row (empty source means "no lyrics row").
	var wantSource, wantUnsynced, wantLines string
	wantSynced := 0
	if ly.HasContent() {
		wantSource, wantUnsynced = ly.Source, ly.Unsynced
		if len(ly.Synced) > 0 {
			wantSynced = 1
			arr := make([]lyricLineJSON, len(ly.Synced))
			for i, l := range ly.Synced {
				arr[i] = lyricLineJSON{MS: l.TimeMS, Text: l.Text}
			}
			b, err := json.Marshal(arr)
			if err != nil {
				return false, err
			}
			wantLines = string(b)
		}
	}

	// Stored row (NULL columns scan as empty strings, matching the want defaults).
	var curSource, curUnsynced, curLines sql.NullString
	var curSynced sql.NullInt64
	err := tx.QueryRowContext(ctx,
		"SELECT source, synced, unsynced, lines FROM lyrics WHERE item_id = ?", itemID).
		Scan(&curSource, &curSynced, &curUnsynced, &curLines)
	exists := !errors.Is(err, sql.ErrNoRows)
	if err != nil && exists {
		return false, err
	}

	// Decide whether a write is even needed before consulting the lock, so an
	// unchanged rescan neither writes nor pays a lock lookup.
	changeNeeded := exists
	if wantSource != "" {
		changeNeeded = !(exists && curSource.String == wantSource && int(curSynced.Int64) == wantSynced &&
			curUnsynced.String == wantUnsynced && curLines.String == wantLines)
	}
	if !changeNeeded {
		return false, nil
	}
	// A user-locked lyrics row is preserved against scan/enrich re-derivation.
	if preserveLock {
		locked, err := fieldLockedTx(ctx, tx, itemID, "lyrics")
		if err != nil {
			return false, err
		}
		if locked {
			return false, nil
		}
	}

	if wantSource == "" { // no lyrics desired
		_, derr := tx.ExecContext(ctx, "DELETE FROM lyrics WHERE item_id = ?", itemID)
		return derr == nil, derr
	}

	var linesArg any
	if wantLines != "" {
		linesArg = wantLines
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO lyrics(item_id, source, synced, unsynced, lines, updated_at) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(item_id) DO UPDATE SET
		   source=excluded.source, synced=excluded.synced, unsynced=excluded.unsynced,
		   lines=excluded.lines, updated_at=excluded.updated_at`,
		itemID, wantSource, wantSynced, nullStr(wantUnsynced), linesArg, nowNS())
	if err != nil {
		return false, err
	}
	return true, nil
}

// LyricsByItem returns an item's stored lyrics, or CodeNotFound when it has none.
func (s *Store) LyricsByItem(ctx context.Context, itemPID model.PID) (*model.Lyrics, error) {
	const op = "store.LyricsByItem"
	var source string
	var synced int
	var unsynced, lines sql.NullString
	err := s.read.QueryRowContext(ctx,
		`SELECT ly.source, ly.synced, ly.unsynced, ly.lines
		 FROM lyrics ly JOIN playable_item pi ON pi.id = ly.item_id
		 WHERE pi.pid = ?`, string(itemPID)).Scan(&source, &synced, &unsynced, &lines)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no lyrics for item: "+string(itemPID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	out := &model.Lyrics{ItemPID: itemPID, Source: source, Unsynced: unsynced.String}
	if lines.Valid && lines.String != "" {
		var arr []lyricLineJSON
		if err := json.Unmarshal([]byte(lines.String), &arr); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeInternal, op, err)
		}
		out.Synced = make([]model.SyncedLine, len(arr))
		for i, l := range arr {
			out.Synced[i] = model.SyncedLine{TimeMS: l.MS, Text: l.Text}
		}
	}
	return out, nil
}
