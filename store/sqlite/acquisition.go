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
// A non-empty field of an event beats a tag; emptiness is never evidence. An item with
// neither has no row and reads as source:local.
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
// delta-sync consumer refreshes the now-attributed item. It is one of three writers
// behind two gates: this one and insertAcquisitionIfAbsentTx are automatic, so a
// standing lock makes them skip in silence the way attachArtRespectingLockTx does,
// while the curation pair (SetAcquisition/ClearAcquisition) refuses a lock out loud
// with CodeLocked unless forced.
//
// A re-record merges field by field: a non-empty incoming value replaces what stands,
// and an empty one keeps it. Emptiness is not evidence. The rule matters because a host
// settling a generic upload path passes a bare event with nothing but the mechanism, and
// an unconditional DO UPDATE would let that erase the url and id a SOURCE_URL tag had
// already established, or downgrade a real 'rss' row acquired by the podcast fetcher.
// ClearAcquisition is the way down when a row is wrong and has to go.
//
// source_type gets the same treatment through one bound parameter rather than a Go-side
// default, so that "" can mean "no claim about the mechanism" while an explicitly passed
// 'manual' still means manual and wins. First-record behavior is unchanged: an empty
// type still lands as 'manual', since an acquisition row exists only for an externally
// acquired item and 'local' is the read-side default for an item with no row at all.
//
// source_id is the one field that does not merge on its own terms, because it means
// nothing without the provider whose namespace it belongs to. An event that names a
// different provider and no id of its own drops the standing id rather than advertising
// one provider's id under another's name. Both providers have to be named for that: a
// tag-derived row carries no provider at all, and its SOURCE_ID is evidence a later event
// has said nothing to contradict.
func putAcquisitionTx(ctx context.Context, tx *sql.Tx, itemID int64, itemPID model.PID, in model.AcquisitionInput) error {
	const op = "store.PutAcquisition"
	// A curated origin outranks an automatic event, so an import over a locked item
	// leaves the curated row standing rather than merging into it.
	if locked, err := acquisitionLockedTx(ctx, tx, itemID); err != nil {
		return err
	} else if locked {
		return nil
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
		VALUES (?1, COALESCE(NULLIF(?2,''),'manual'), ?3, ?4, ?5, ?6, ?7, ?8)
		ON CONFLICT(item_id) DO UPDATE SET
			source_type = CASE WHEN ?2 <> '' THEN ?2 ELSE acquisition.source_type END,
			source_url = CASE WHEN ?3 <> '' THEN ?3 ELSE acquisition.source_url END,
			source_id = CASE WHEN ?4 <> '' THEN ?4
				WHEN ?5 <> '' AND acquisition.provider <> '' AND ?5 <> acquisition.provider THEN ''
				ELSE acquisition.source_id END,
			provider = CASE WHEN ?5 <> '' THEN ?5 ELSE acquisition.provider END,
			provider_version = CASE WHEN ?6 <> '' THEN ?6 ELSE acquisition.provider_version END,
			options_json = CASE WHEN ?8 <> '' THEN ?8 ELSE acquisition.options_json END`,
		itemID, string(in.SourceType), in.SourceURL, in.SourceID, in.Provider, in.ProviderVersion,
		acquiredAt, in.OptionsJSON); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
}

// SetAcquisition replaces an item's origin provenance with the row it is given. It is
// the corrective write the merge-wise recording path cannot express: putAcquisitionTx
// never lowers a field, so before this the only way down was a clear followed by a
// re-record, which is what the CLI's help text told people to do.
//
// The replace is authoritative. Every column is written as given, so a correction can
// empty source_url, source_id, provider, provider_version and options_json. The merge
// rule protects a standing row against a bare automatic event, not against a human who
// typed the whole thing.
//
// acquired_at differs from every other column and from putAcquisitionTx, which omits it
// from its DO UPDATE so a merge can never move the stamp. A correction has to be able to
// move it, since fixing a row that already exists is the entire point of the verb. The
// zero sentinel already means "stamp it for me" (model.AcquisitionInput), so a zero
// preserves what stands and a non-zero moves it. The two surfaces differ deliberately:
// do not make one match the other.
//
// A standing lock is refused with CodeLocked unless force is set, which is the loud gate
// the curation surfaces use where the automatic writers skip in silence. lock is applied
// after the write, and LockUnchanged is its zero value the way it is on every other
// curation verb.
func (s *Store) SetAcquisition(ctx context.Context, itemPID model.PID, in model.AcquisitionInput, lock model.LockChange, force bool) error {
	const op = "store.SetAcquisition"
	if err := checkLockChange(lock, op); err != nil {
		return err
	}
	if in.SourceType == model.SourceLocal {
		return waxerr.New(waxerr.CodeInvalid, op,
			"local is the absence of an acquisition row, not a type: use acquisition clear")
	}
	if !in.SourceType.ValidItemSource() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown acquisition source type: "+string(in.SourceType))
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !curatableFieldForKind(kind, "acquisition") {
			return waxerr.New(waxerr.CodeInvalid, op, "acquisition is not curatable on a "+kind+" item")
		}
		if !force {
			locked, err := acquisitionLockedTx(ctx, tx, itemID)
			if err != nil {
				return err
			}
			if locked {
				return waxerr.New(waxerr.CodeLocked, op, "acquisition is locked (use force to override)")
			}
		}
		acquiredAt := in.AcquiredAt
		if acquiredAt == 0 {
			acquiredAt = nowNS()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO acquisition
			(item_id, source_type, source_url, source_id, provider, provider_version, acquired_at, options_json)
			VALUES (?1,?2,?3,?4,?5,?6,?7,?8)
			ON CONFLICT(item_id) DO UPDATE SET
				source_type = ?2, source_url = ?3, source_id = ?4, provider = ?5,
				provider_version = ?6, options_json = ?8,
				acquired_at = CASE WHEN ?9 THEN ?7 ELSE acquisition.acquired_at END`,
			itemID, string(in.SourceType), in.SourceURL, in.SourceID, in.Provider,
			in.ProviderVersion, acquiredAt, in.OptionsJSON, boolInt(in.AcquiredAt != 0)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := setCurationLockTx(ctx, tx, itemID, "acquisition",
			model.Attribution{Source: model.SourceUser}, lock); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
}

// ClearAcquisition deletes an item's origin provenance, so the item falls back to the
// source it reads without a row of its own: its show's type for an episode, and local
// for everything else. It is the inverse a merge-wise upsert needs: putAcquisitionTx
// will not lower a field, so without a way to remove the row outright a wrong one could
// only ever be written over with something else wrong. Clearing an item that already
// reads the way a clear would leave it is a no-op, not an error, so a caller correcting
// a batch does not have to check first and a repeated clear is not refused by the lock
// its predecessor set.
//
// It takes the whole row, acquired_at included, and there is no narrower knife. Lowering
// one field goes through SetAcquisition, the authoritative surface where an empty value
// is a claim, rather than through a per-field clear here, which would need a sentinel for
// "make this empty" on a path whose whole rule is that empty is not evidence. The two
// coexist on purpose: this one takes the row off, that one states what it should say.
//
// LockUnchanged resolves to LockOn, which is the one place a curation verb's zero value
// is not "leave the lock alone". A set leaves a row the merge rule already refuses to
// lower, while a clear leaves nothing, so the next scan re-derives the same wrong origin
// from the file's own tags. The clear is the one that gets undone by default, and
// LockOff is the explicit opt-out. SetItemTag's clear is the precedent for a clear owning
// its own lock rule, and it points the other way: a locked-empty custom tag would block a
// later re-set from the file, while a locked-empty acquisition is exactly the point.
func (s *Store) ClearAcquisition(ctx context.Context, itemPID model.PID, lock model.LockChange, force bool) error {
	const op = "store.ClearAcquisition"
	if err := checkLockChange(lock, op); err != nil {
		return err
	}
	if lock == model.LockUnchanged {
		lock = model.LockOn
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !curatableFieldForKind(kind, "acquisition") {
			return waxerr.New(waxerr.CodeInvalid, op, "acquisition is not curatable on a "+kind+" item")
		}
		wasLocked, err := acquisitionLockedTx(ctx, tx, itemID)
		if err != nil {
			return err
		}
		var hasRow bool
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM acquisition WHERE item_id = ?)", itemID).Scan(&hasRow); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// A clear that would remove no row and leave the lock where it stands is already
		// done, so it returns before both the lock refusal and the delta. That keeps the
		// verb idempotent, which the batch contract above promises and which the default
		// lock would otherwise break: the first clear locks, and the second would meet its
		// own predecessor's lock with CodeLocked. It is setLock's idempotence guard, on
		// the pair of things this verb writes rather than on the lock alone.
		if !hasRow && wasLocked == (lock == model.LockOn) {
			return nil
		}
		if wasLocked && !force {
			return waxerr.New(waxerr.CodeLocked, op, "acquisition is locked (use force to override)")
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM acquisition WHERE item_id = ?", itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := setCurationLockTx(ctx, tx, itemID, "acquisition",
			model.Attribution{Source: model.SourceUser}, lock); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
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
// item arrived. A non-empty field of an event beats a tag; emptiness is never
// evidence. The asymmetry with putAcquisitionTx is the point: that path merges an
// event into a standing row, while this one only ever creates the first row.
//
// DO NOTHING protects a row that exists, and a cleared item has none, so the tags on
// disk would re-establish the wrong origin on the next full scan. preserveLocks is what
// makes a clear durable: with it set (every scan that is not --ignore-locks), a standing
// acquisition lock stops the insert, so a curated absence is an absence that holds.
func insertAcquisitionIfAbsentTx(ctx context.Context, tx *sql.Tx, itemID int64, in model.TagAcquisition, preserveLocks bool) (bool, error) {
	const op = "store.insertAcquisitionIfAbsent"
	if !in.Present() {
		return false, nil
	}
	if preserveLocks {
		locked, err := acquisitionLockedTx(ctx, tx, itemID)
		if err != nil {
			return false, err
		}
		if locked {
			return false, nil
		}
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

// acquisitionLockedTx reports whether an item's acquisition row is locked. The lock
// lives in field_provenance under the field name "acquisition", which is where every
// writer here already looks for the item-scoped locks beside it.
func acquisitionLockedTx(ctx context.Context, q queryer, itemID int64) (bool, error) {
	return fieldLockedTx(ctx, q, itemID, "acquisition")
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
