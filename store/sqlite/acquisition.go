package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// PutAcquisition records (or replaces) an item's origin provenance by item pid.
//
// The row is sparse. It exists only for an item with evidence of external origin,
// either an acquisition WaxBin performed or the file's own SOURCE_URL/SOURCE_ID tags.
// Evidence from an event always wins over evidence from a tag. An item with neither
// has no row and reads as source:local.
//
// AcquiredAt is stamped on first record and preserved on a re-record (it is the
// historical acquisition time, not a last-touched time).
func (s *Store) PutAcquisition(ctx context.Context, itemPID model.PID, in model.AcquisitionInput) error {
	const op = "store.PutAcquisition"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, err := idByPIDTx(ctx, tx, "playable_item", itemPID, op)
		if err != nil {
			return err
		}
		return putAcquisitionTx(ctx, tx, itemID, itemPID, in)
	})
}

// PutAcquisitionForFile records origin provenance against the item backing the file
// at path, resolving the item from the file's primary edge. It is the import path's
// stamp: after a placed file is cataloged, its item gets the acquisition row. It is a
// no-op (CodeNotFound) when no cataloged item owns that path.
func (s *Store) PutAcquisitionForFile(ctx context.Context, path []byte, in model.AcquisitionInput) error {
	const op = "store.PutAcquisitionForFile"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var itemID int64
		var itemPID model.PID
		err := tx.QueryRowContext(ctx, `SELECT pi.id, pi.pid
			FROM file f JOIN item_file itf ON itf.file_id = f.id AND itf.role='primary'
			JOIN playable_item pi ON pi.id = itf.item_id
			WHERE f.path = ? LIMIT 1`, path).Scan(&itemID, &itemPID)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "no cataloged item backs the placed file")
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return putAcquisitionTx(ctx, tx, itemID, itemPID, in)
	})
}

// putAcquisitionTx writes the acquisition row and emits an item update delta so a
// delta-sync consumer refreshes the now-attributed item.
func putAcquisitionTx(ctx context.Context, tx *sql.Tx, itemID int64, itemPID model.PID, in model.AcquisitionInput) error {
	const op = "store.PutAcquisition"
	st := in.SourceType
	if st == "" {
		// An acquisition row exists only for an externally acquired item, so a missing
		// source type is "manual" (acquired by unspecified means), never "local". Local
		// is the read-side default for an item with no acquisition row.
		st = model.SourceManual
	}
	// Zero is the documented "stamp it for me" sentinel, so it never reaches the NOT
	// NULL column; a caller that knows the real acquisition time keeps it. The
	// ON CONFLICT below omits acquired_at, so a re-record preserves the first one.
	acquiredAt := in.AcquiredAt
	if acquiredAt == 0 {
		acquiredAt = nowNS()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO acquisition
		(item_id, source_type, source_url, source_id, provider, provider_version, acquired_at, options_json)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET
			source_type=excluded.source_type, source_url=excluded.source_url,
			source_id=excluded.source_id, provider=excluded.provider,
			provider_version=excluded.provider_version,
			options_json=excluded.options_json`,
		itemID, string(st), in.SourceURL, in.SourceID, in.Provider, in.ProviderVersion,
		acquiredAt, in.OptionsJSON); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
}

// insertAcquisitionIfAbsentTx records tag-derived origin provenance for an item that
// has no acquisition row yet, and reports whether it inserted one. It runs inside the
// scan's own write transaction, so the row is atomic with the item's creation and the
// single-writer contract is not broken by a second write tx per scanned file.
//
// It uses DO NOTHING rather than DO UPDATE, and the reason is source_type and
// provider clobbering rather than acquired_at. The ON CONFLICT in putAcquisitionTx
// already omits acquired_at, so first-acquisition time survives there either way. But
// a tag is copyable and is re-derived on every full scan, so an update would let one
// rescan of a downloaded podcast episode overwrite its real source_type of 'rss' and
// its provider with a bare 'manual', destroying the authoritative record of how the
// item arrived. Evidence from an acquisition event always wins over evidence from a
// tag.
func insertAcquisitionIfAbsentTx(ctx context.Context, tx *sql.Tx, itemID int64, in model.TagAcquisition) (bool, error) {
	const op = "store.insertAcquisitionIfAbsent"
	if !in.Present() {
		return false, nil
	}
	acquiredAt := in.AcquiredAt
	if acquiredAt == 0 {
		// No usable ACQUISITION_DATE. Scan time is an approximation, but one that admits
		// as much. File mtime would not be: it tracks the last retag, so it would state a
		// wrong date with confidence, and the DO NOTHING above would then keep that wrong
		// value for good.
		acquiredAt = nowNS()
	}
	// source_type is 'manual' (acquired by unspecified means) and provider is empty.
	// The tags evidence external origin but say nothing about the mechanism, and
	// 'manual' is the established word for that. Do not invent a 'tagged' type.
	res, err := tx.ExecContext(ctx, `INSERT INTO acquisition
		(item_id, source_type, source_url, source_id, provider, provider_version, acquired_at, options_json)
		VALUES (?,?,?,?,'','',?,'')
		ON CONFLICT(item_id) DO NOTHING`,
		itemID, string(model.SourceManual), in.SourceURL, in.SourceID, acquiredAt)
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return n > 0, nil
}

// AcquisitionByItem returns an item's origin provenance, or CodeNotFound when the
// item was locally scanned (it has no acquisition row).
func (s *Store) AcquisitionByItem(ctx context.Context, itemPID model.PID) (*model.Acquisition, error) {
	const op = "store.AcquisitionByItem"
	var a model.Acquisition
	var st string
	err := s.read.QueryRowContext(ctx, `SELECT acq.source_type, acq.source_url, acq.source_id,
		acq.provider, acq.provider_version, acq.acquired_at, acq.options_json
		FROM acquisition acq JOIN playable_item pi ON pi.id = acq.item_id
		WHERE pi.pid = ?`, string(itemPID)).Scan(&st, &a.SourceURL, &a.SourceID,
		&a.Provider, &a.ProviderVersion, &a.AcquiredAt, &a.OptionsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "item has no acquisition provenance")
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	a.SourceType = model.SourceType(st)
	return &a, nil
}
