package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file holds the custom-tag surface: the tags a file carries that WaxBin's typed
// model does not map, plus tags a user sets directly. They live in item_tag (keyed by a
// canonical uppercase tag key, with a position for multi-valued tags) and are lockable
// under a namespaced "tag.<KEY>" field in field_provenance (Category B), so a scan does
// not re-derive a curated tag from the file.

// SetItemTag replaces one custom tag's ordered values on an item (source "user"),
// recording a lock on "tag.<KEY>" by default so a scan does not re-derive it from the
// file. Passing no values (or only whitespace) clears the tag and drops any lock, so a
// later scan re-derives it: a clear is a full forget, never a locked-empty tag. The key
// is normalized to canonical uppercase, so BPM and bpm are one tag. A reserved key (one
// WaxBin owns through the scalar, credit, or identifier APIs) is rejected with
// CodeInvalid so the caller reaches for the right surface instead. A locked tag is
// refused with CodeLocked unless force is set. It returns the canonical key stored and
// the number of values stored after trimming (0 means the tag was cleared), so a caller
// does not report a whitespace-only clear as a set.
func (s *Store) SetItemTag(ctx context.Context, itemPID model.PID, key string, values []string, source model.ProvenanceSource, lock, force bool) (string, int, error) {
	const op = "store.SetItemTag"
	canon, ok := model.CanonicalTagKey(key)
	if !ok {
		return "", 0, waxerr.New(waxerr.CodeInvalid, op, "invalid tag key: "+key)
	}
	if model.IsReservedTagKey(canon) {
		return "", 0, waxerr.New(waxerr.CodeInvalid, op,
			"tag key "+canon+" is reserved; set it through the scalar, credit, or entity edit API")
	}
	if !source.Valid() {
		return "", 0, waxerr.New(waxerr.CodeInvalid, op, "invalid provenance source: "+string(source))
	}
	// Drop values that are empty after trimming surrounding whitespace, preserving order.
	// An all-empty (or nil) list clears the tag.
	clean := make([]string, 0, len(values))
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			clean = append(clean, t)
		}
	}
	field := model.TagLockField(canon)

	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !curatableFieldForKind(kind, field) {
			return waxerr.New(waxerr.CodeInvalid, op, "custom tags are not editable on a "+kind+" item")
		}
		if !force {
			locked, err := fieldLockedTx(ctx, tx, itemID, field)
			if err != nil {
				return err
			}
			if locked {
				return waxerr.New(waxerr.CodeLocked, op, "tag "+canon+" is locked (use force to override)")
			}
		}
		if err := writeItemTagTx(ctx, tx, itemID, canon, clean); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if len(clean) == 0 {
			// A clear forgets the tag entirely, including any lock, so a scan re-derives the
			// file's value next time. This keeps the default lock from turning an accidental
			// clear (say a whitespace-only value) into a locked-empty tag that then blocks a
			// re-set. Deliberately suppressing a file tag is an explicit `lock tag.<KEY>`.
			if _, err := tx.ExecContext(ctx, "DELETE FROM field_provenance WHERE item_id=? AND field=?", itemID, field); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		} else {
			// Record the tag's provenance, whose value is the joined display list, and lock
			// it unless the caller opted out.
			if err := upsertEditProvenanceTx(ctx, tx, itemID, field, source, strings.Join(clean, "; "), lock, nowNS()); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		// Rebuild the item's search row so the custom tag is immediately searchable.
		if err := rebuildItemSearchFTSTx(ctx, tx, itemID, kind); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
	if err != nil {
		return "", 0, err
	}
	return canon, len(clean), nil
}

// writeItemTagTx replaces one key's rows with the given ordered values (an empty list
// clears the key).
func writeItemTagTx(ctx context.Context, tx *sql.Tx, itemID int64, key string, values []string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_tag WHERE item_id=? AND key=?", itemID, key); err != nil {
		return err
	}
	for i, v := range values {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO item_tag(item_id, key, value, position) VALUES (?,?,?,?)", itemID, key, v, i); err != nil {
			return err
		}
	}
	return nil
}

// ItemTags returns an item's custom tags, grouped by key in key then position order.
// A nonexistent item returns CodeNotFound (distinguished from an item with no custom
// tags, which returns an empty slice), matching FieldProvenance and EntityCuration.
func (s *Store) ItemTags(ctx context.Context, itemPID model.PID) ([]model.ItemTag, error) {
	const op = "store.ItemTags"
	itemID, err := itemIDByPIDRead(ctx, s.read, itemPID, op)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx,
		"SELECT key, value FROM item_tag WHERE item_id = ? ORDER BY key, position", itemID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.ItemTag
	byKey := map[string]int{} // key -> index in out
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if i, ok := byKey[key]; ok {
			out[i].Values = append(out[i].Values, value)
			continue
		}
		byKey[key] = len(out)
		out = append(out, model.ItemTag{Key: key, Values: []string{value}})
	}
	return out, rows.Err()
}

// syncItemTagsTx overlays a scan's custom tags onto an item, honoring per-key locks. A
// locked "tag.<KEY>" keeps its stored values (a scan cannot re-derive a curated tag);
// every other key is replaced by the scanned set, and a key no longer present on disk
// is dropped. It reports whether anything changed, so the caller can emit an item delta
// only for a real change. When preserveLock is false (an --ignore-locks run) the scan
// overwrites even locked tags.
func syncItemTagsTx(ctx context.Context, tx *sql.Tx, itemID int64, scanned map[string][]string, preserveLock bool) (bool, error) {
	current, err := loadItemTagsTx(ctx, tx, itemID)
	if err != nil {
		return false, err
	}
	// Fast path for the overwhelmingly common case: the file carries no custom tags and
	// the item stores none. Nothing to do, and no lock lookup needed, so a catalog of
	// plain files costs one indexed probe per scan and no more.
	if len(scanned) == 0 && len(current) == 0 {
		return false, nil
	}
	locked := map[string]bool{}
	if preserveLock {
		locked, err = lockedTagKeysTx(ctx, tx, itemID)
		if err != nil {
			return false, err
		}
	}

	// Build the desired set: the scanned values for every non-locked key (normalized to
	// canonical, non-reserved keys), plus the current values kept verbatim for a locked key.
	desired := map[string][]string{}
	for k, vs := range scanned {
		canon, ok := model.CanonicalTagKey(k)
		if !ok || model.IsReservedTagKey(canon) || locked[canon] {
			continue
		}
		clean := make([]string, 0, len(vs))
		for _, v := range vs {
			if t := strings.TrimSpace(v); t != "" {
				clean = append(clean, t)
			}
		}
		if len(clean) > 0 {
			desired[canon] = clean
		}
	}
	for k := range locked {
		if vs, ok := current[k]; ok {
			desired[k] = vs
		}
	}

	if tagSetsEqual(current, desired) {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_tag WHERE item_id=?", itemID); err != nil {
		return false, err
	}
	for key, vs := range desired {
		if err := writeItemTagTx(ctx, tx, itemID, key, vs); err != nil {
			return false, err
		}
	}
	return true, nil
}

// loadItemTagsTx reads an item's custom tags into a key -> ordered-values map.
func loadItemTagsTx(ctx context.Context, tx *sql.Tx, itemID int64) (map[string][]string, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT key, value FROM item_tag WHERE item_id=? ORDER BY key, position", itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = append(out[key], value)
	}
	return out, rows.Err()
}

// lockedTagKeysTx returns the canonical keys whose "tag.<KEY>" field is locked.
func lockedTagKeysTx(ctx context.Context, tx *sql.Tx, itemID int64) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT field FROM field_provenance WHERE item_id=? AND locked=1 AND field LIKE 'tag.%'", itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			return nil, err
		}
		if key, ok := model.CutTagPrefix(field); ok {
			out[key] = true
		}
	}
	return out, rows.Err()
}

// tagSetsEqual reports whether two key -> ordered-values maps are identical.
func tagSetsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
	}
	return true
}

// itemCustomTagText returns an item's custom tag values joined for the search row's
// extra column, so a custom tag is searchable. Keys are omitted; only the values feed
// full-text search.
func itemCustomTagText(ctx context.Context, tx *sql.Tx, itemID int64) (string, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT value FROM item_tag WHERE item_id=? ORDER BY key, position", itemID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var vals []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return "", err
		}
		vals = append(vals, v)
	}
	return strings.Join(vals, " "), rows.Err()
}

// rebuildItemSearchFTSTx rebuilds a track or book item's search row from its current
// stored state, so a change that does not go through the scan path (a custom-tag edit)
// still refreshes full-text search. A kind with no FTS producer is a no-op.
func rebuildItemSearchFTSTx(ctx context.Context, tx *sql.Tx, itemID int64, kind string) error {
	switch kind {
	case string(model.KindTrack):
		tr, _, _, err := loadTrackForEditTx(ctx, tx, itemID)
		if err != nil {
			return err
		}
		return syncSearchFTS(ctx, tx, itemID, tr)
	case string(model.KindBook):
		return rebuildBookSearchFTSTx(ctx, tx, itemID)
	default:
		return nil
	}
}
