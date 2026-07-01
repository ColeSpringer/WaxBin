package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// PutAcquisition records (or replaces) an item's origin provenance by item pid. The
// row is sparse: it exists only for an externally-acquired item, so a locally-scanned
// file never has one. AcquiredAt is stamped on first record and preserved on a
// re-record (it is the historical acquisition time, not a last-touched time).
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO acquisition
		(item_id, source_type, source_url, source_id, provider, provider_version, acquired_at, options_json)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET
			source_type=excluded.source_type, source_url=excluded.source_url,
			source_id=excluded.source_id, provider=excluded.provider,
			provider_version=excluded.provider_version,
			options_json=excluded.options_json`,
		itemID, string(st), in.SourceURL, in.SourceID, in.Provider, in.ProviderVersion,
		nowNS(), in.OptionsJSON); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
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
