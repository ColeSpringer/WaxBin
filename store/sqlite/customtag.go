package sqlite

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file holds the custom-tag surface: the tags a file carries that WaxBin's typed
// model does not map, plus tags a user sets directly. They live in item_tag (keyed by a
// canonical uppercase tag key, with a position for multi-valued tags) and are lockable
// under a namespaced "tag.<KEY>" field in field_provenance (Category B), so a scan does
// not re-derive a curated tag from the file.

// SetItemTag replaces one custom tag's ordered values on an item, attributed to attr
// (an unstated origin is a user edit) and recording the caller's lock instruction on
// "tag.<KEY>" so a scan does not re-derive it from the file. Passing no values (or only
// whitespace) clears the tag and drops any lock regardless of lock, so a
// later scan re-derives it: a clear is a full forget, never a locked-empty tag. The key
// is normalized to canonical uppercase, so KEY and key are one tag. A reserved key (one
// WaxBin owns through the scalar, credit, or identifier APIs) is rejected with
// CodeInvalid so the caller reaches for the right surface instead, and so is a key the
// scanner reads into a book field (model.BookOwnedTagField) when the item is a book,
// since a custom value under it would reach the file and shadow the field. A locked tag is
// refused with CodeLocked unless force is set. It returns the canonical key stored and
// the number of values stored after trimming (0 means the tag was cleared), so a caller
// does not report a whitespace-only clear as a set.
//
// A change to the values is owed to the item's files (model.DiagTagWriteOwed) until a
// write-back lands it, except under a key a tag write would land on another field.
func (s *Store) SetItemTag(ctx context.Context, itemPID model.PID, key string, values []string, attr model.Attribution, lock model.LockChange, force bool) (string, int, error) {
	return s.setItemTag(ctx, itemPID, key, values, attr, lock, force, true)
}

// ForgetItemTag drops a custom tag a write-back has just cleared from the files that own
// the item's tags, its primary and parts, so the catalog follows them, and clears every
// file's owed row for it; the caller notes a file still holding the tag. A locked tag is
// refused.
func (s *Store) ForgetItemTag(ctx context.Context, itemPID model.PID, key string) error {
	_, _, err := s.setItemTag(ctx, itemPID, key, nil, model.Attribution{}, model.LockUnchanged, false, false)
	return err
}

func (s *Store) setItemTag(ctx context.Context, itemPID model.PID, key string, values []string, attr model.Attribution, lock model.LockChange, force, owe bool) (string, int, error) {
	const op = "store.SetItemTag"
	canon, ok := model.CanonicalTagKey(key)
	if !ok {
		return "", 0, waxerr.New(waxerr.CodeInvalid, op, "invalid tag key: "+key)
	}
	if model.IsReservedTagKey(canon) {
		return "", 0, waxerr.New(waxerr.CodeInvalid, op,
			"tag key "+canon+" is reserved; set it through the scalar, credit, or entity edit API")
	}
	attr, err := checkFieldAttribution(attr, op)
	if err != nil {
		return "", 0, err
	}
	if err := checkLockChange(lock, op); err != nil {
		return "", 0, err
	}
	// An all-empty (or nil) list clears the tag.
	clean := model.CleanTagValues(values)
	field := model.TagLockField(canon)

	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !curatableFieldForKind(kind, field) {
			return waxerr.New(waxerr.CodeInvalid, op, "custom tags are not editable on a "+kind+" item")
		}
		if kind == string(model.KindBook) {
			if f, ok := model.BookOwnedTagField(canon); ok {
				return waxerr.New(waxerr.CodeInvalid, op,
					"tag key "+canon+" is the "+f+" field on a book; set it through the scalar edit API")
			}
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
		before, err := itemTagValuesTx(ctx, tx, itemID, canon)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := writeItemTagTx(ctx, tx, itemID, canon, clean); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		switch {
		case !owe:
			if err := deleteOwedTx(ctx, tx, "file_id IN (SELECT file_id FROM item_file WHERE item_id = ?)", itemID, []string{field}); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		case !model.IsRetargetedTagKey(canon) && !slices.Equal(before, clean):
			if err := noteOwedItemTx(ctx, tx, itemID, kind, []string{field}); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if len(clean) == 0 {
			// A clear forgets the tag entirely, including any lock, whatever lock instruction
			// came with it, so a scan re-derives the file's value next time. This keeps the
			// default lock from turning an accidental clear (say a whitespace-only value) into
			// a locked-empty tag that then blocks a re-set, and it is why this is the one
			// curation write LockUnchanged does not govern. Deliberately suppressing a file
			// tag is an explicit `lock tag.<KEY>`.
			if _, err := tx.ExecContext(ctx, "DELETE FROM field_provenance WHERE item_id=? AND field=?", itemID, field); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		} else {
			// Record the tag's provenance, whose value is the joined display list, under the
			// caller's attribution and lock instruction.
			if err := upsertEditProvenanceTx(ctx, tx, itemID, field, attr, strings.Join(clean, "; "), lock, nowNS()); err != nil {
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

// itemTagValuesTx reads one custom tag's stored values in order, nil when the item has none.
func itemTagValuesTx(ctx context.Context, tx *sql.Tx, itemID int64, key string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT value FROM item_tag WHERE item_id=? AND key=? ORDER BY position", itemID, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
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
	itemID, err := itemIDByPIDRead(ctx, s.rdb(), itemPID, op)
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb().QueryContext(ctx,
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
// only for a real change, and the keys whose values it replaced with the file's. When
// preserveLock is false (an --ignore-locks run) the scan overwrites even locked tags.
func syncItemTagsTx(ctx context.Context, tx *sql.Tx, itemID int64, scanned map[string][]string, preserveLock bool) (bool, []string, error) {
	current, err := loadItemTagsTx(ctx, tx, itemID)
	if err != nil {
		return false, nil, err
	}
	// Fast path for the overwhelmingly common case: the file carries no custom tags and
	// the item stores none. Nothing to do, and no lock lookup needed, so a catalog of
	// plain files costs one indexed probe per scan and no more.
	if len(scanned) == 0 && len(current) == 0 {
		return false, nil, nil
	}
	// Every tag.<KEY> provenance row this item has. Loading them all rather than only the
	// locked ones costs the same query and is what lets the reserved sweep and the stale
	// sweep below reach an unlocked row too; the lock decisions read the same map, since a
	// key with no row and a key with an unlocked row both answer false.
	tagFields, err := tagProvenanceTx(ctx, tx, itemID)
	if err != nil {
		return false, nil, err
	}
	locked := map[string]bool{}
	if preserveLock {
		for k, row := range tagFields {
			locked[k] = row.locked
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
		if clean := model.CleanTagValues(vs); len(clean) > 0 {
			desired[canon] = clean
		}
	}
	// A locked key keeps its values unless it has since become reserved: SetItemTag
	// rejects a reserved key, so re-adding it here would store a tag nothing could edit.
	// Dropping it is what makes newly reserving a key take effect.
	var stale []string
	for k, row := range tagFields {
		// A reserved key's provenance row goes whether or not it was locked.
		// IsCuratableField refuses a reserved tag.<KEY> field, so once the values are gone
		// neither SetItemTag nor UnlockField can reach the row and it would sit there
		// unremovable except by raw SQL.
		if model.IsReservedTagKey(k) {
			if _, err := tx.ExecContext(ctx, "DELETE FROM field_provenance WHERE item_id=? AND field=?",
				itemID, model.TagLockField(k)); err != nil {
				return false, nil, err
			}
			continue
		}
		if row.locked && preserveLock {
			if vs, ok := current[k]; ok {
				desired[k] = vs
			}
			continue
		}
		// A tag enrichment set, or a normalize pass respelled, stays while the file lacks
		// the key, the rule a scalar fill follows (overlayStoredTrackTx).
		if _, stated := desired[k]; !stated && fillSource(row.source) {
			if vs, ok := current[k]; ok && row.value == strings.Join(vs, "; ") {
				desired[k] = vs
				continue
			}
		}
		// A row stating a value the scan replaced or dropped is re-derived, the way a
		// scalar's is (retireProvenanceTx). A lock-only row states none.
		if row.source != string(model.SourceTag) && row.value != strings.Join(desired[k], "; ") {
			stale = append(stale, model.TagLockField(k))
		}
	}
	if len(stale) > 0 {
		if _, err := retireProvenanceTx(ctx, tx, itemID, stale, preserveLock); err != nil {
			return false, nil, err
		}
	}

	if tagSetsEqual(current, desired) {
		return len(stale) > 0, nil, nil
	}
	var replaced []string
	for k, vs := range current {
		if !slices.Equal(vs, desired[k]) {
			replaced = append(replaced, k)
		}
	}
	for k := range desired {
		if _, held := current[k]; !held {
			replaced = append(replaced, k)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_tag WHERE item_id=?", itemID); err != nil {
		return false, nil, err
	}
	for key, vs := range desired {
		if err := writeItemTagTx(ctx, tx, itemID, key, vs); err != nil {
			return false, nil, err
		}
	}
	return true, replaced, nil
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

// tagProvenanceRow is one "tag.<KEY>" provenance row: its lock, its source, and the
// joined values it records.
type tagProvenanceRow struct {
	locked        bool
	source, value string
}

// tagProvenanceTx returns an item's "tag.<KEY>" provenance rows by canonical key.
func tagProvenanceTx(ctx context.Context, tx *sql.Tx, itemID int64) (map[string]tagProvenanceRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT field, locked, source, COALESCE(value, '')
		FROM field_provenance WHERE item_id=? AND field LIKE 'tag.%'`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]tagProvenanceRow{}
	for rows.Next() {
		var field string
		var row tagProvenanceRow
		if err := rows.Scan(&field, &row.locked, &row.source, &row.value); err != nil {
			return nil, err
		}
		if key, ok := model.CutTagPrefix(field); ok {
			out[key] = row
		}
	}
	return out, rows.Err()
}

// reservedTagProvenanceQuery appends to verb a field_provenance clause matching every
// "tag.<KEY>" row whose key is reserved, and returns it with the bound field names. The
// reserved set lives in Go, so it has to be spelled out as placeholders; building both
// the count and the delete from one helper keeps db verify's report and its repair on
// the same set.
func reservedTagProvenanceQuery(verb string) (string, []any) {
	keys := model.ReservedTagKeys()
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = model.TagLockField(k)
	}
	return verb + " FROM field_provenance WHERE field IN " + placeholders(len(keys)), args
}

// GCReservedTagProvenance deletes the "tag.<KEY>" provenance rows whose key WaxBin has
// since reserved, returning how many went. Reserving a key makes its rows unreachable:
// SetItemTag and UnlockField both refuse a reserved field, and syncItemTagsTx sweeps
// them on the next scan only for an item that has custom tags on either side, since its
// fast path returns first. That fast path's one indexed probe per plain file is worth
// keeping, so the stranded rows are reclaimed here instead, under `db verify --fix`.
func (s *Store) GCReservedTagProvenance(ctx context.Context) (int, error) {
	const op = "store.GCReservedTagProvenance"
	q, args := reservedTagProvenanceQuery("DELETE")
	var n int
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		c, _ := r.RowsAffected()
		n = int(c)
		return nil
	})
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return n, nil
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
func itemCustomTagText(ctx context.Context, q queryer, itemID int64) (string, error) {
	rows, err := q.QueryContext(ctx,
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

// strandedTagKeys returns the item_tag keys and "tag.<KEY>" provenance fields that
// model.CanonicalTagKey no longer accepts as stored, and the "tag.<KEY>" owed keys no
// write-back can pay: under such a key, or one WaxBin has since reserved, or one a tag
// write puts onto another field. The key rule follows the tag library's, so a tightening
// (1.6 dropped '~') leaves rows nothing can query, edit, or unlock. syncItemTagsTx keeps a
// locked one on purpose, since its value has no other home, so db verify reports them and
// --fix reclaims them here.
func (s *Store) strandedTagKeys(ctx context.Context) (keys, fields, owed []string, err error) {
	read := func(q string, args []any, stranded func(string) bool) ([]string, error) {
		rows, err := s.rdb().QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			if stranded(v) {
				out = append(out, v)
			}
		}
		return out, rows.Err()
	}
	rejected := func(k string) bool {
		canon, ok := model.CanonicalTagKey(k)
		return !ok || canon != k
	}
	if keys, err = read("SELECT DISTINCT key FROM item_tag", nil, rejected); err != nil {
		return nil, nil, nil, err
	}
	if fields, err = read("SELECT DISTINCT field FROM field_provenance WHERE field LIKE 'tag.%'", nil, func(f string) bool {
		k, ok := model.CutTagPrefix(f)
		return !ok || rejected(k)
	}); err != nil {
		return nil, nil, nil, err
	}
	owed, err = read("SELECT DISTINCT tag_key FROM file_diagnostic WHERE origin = ? AND code = ? AND tag_key LIKE 'tag.%'",
		[]any{string(model.OriginEdit), string(model.DiagTagWriteOwed)}, func(f string) bool {
			k, ok := model.CutTagPrefix(f)
			return !ok || rejected(k) || model.IsReservedTagKey(k) || model.IsRetargetedTagKey(k)
		})
	return keys, fields, owed, err
}

// strandedOwedWhere names the owed rows under the keys strandedTagKeys returned.
func strandedOwedWhere(owed []string) (string, []any) {
	args := append([]any{string(model.OriginEdit), string(model.DiagTagWriteOwed)}, anySlice(owed)...)
	return " FROM file_diagnostic WHERE origin = ? AND code = ? AND tag_key IN " + placeholders(len(owed)), args
}

// countStrandedTagKeyRows counts the rows strandedTagKeys names, for db verify.
func (s *Store) countStrandedTagKeyRows(ctx context.Context) (int, error) {
	keys, fields, owed, err := s.strandedTagKeys(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	count := func(q string, args ...any) error {
		var c int
		if err := s.rdb().QueryRowContext(ctx, q, args...).Scan(&c); err != nil {
			return err
		}
		n += c
		return nil
	}
	if len(keys) > 0 {
		if err := count("SELECT COUNT(*) FROM item_tag WHERE key IN "+placeholders(len(keys)), anySlice(keys)...); err != nil {
			return 0, err
		}
	}
	if len(fields) > 0 {
		if err := count("SELECT COUNT(*) FROM field_provenance WHERE field IN "+placeholders(len(fields)), anySlice(fields)...); err != nil {
			return 0, err
		}
	}
	if len(owed) > 0 {
		where, args := strandedOwedWhere(owed)
		if err := count("SELECT COUNT(*)"+where, args...); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// GCStrandedTagKeys deletes the rows strandedTagKeys names, returning how many went,
// and rebuilds the search row of every item whose values it deleted.
func (s *Store) GCStrandedTagKeys(ctx context.Context) (int, error) {
	const op = "store.GCStrandedTagKeys"
	keys, fields, owed, err := s.strandedTagKeys(ctx)
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if len(keys) == 0 && len(fields) == 0 && len(owed) == 0 {
		return 0, nil
	}
	var n int64
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		if len(keys) > 0 {
			items, err := searchItemsTx(ctx, tx, `SELECT DISTINCT it.item_id, pi.kind FROM item_tag it
				JOIN playable_item pi ON pi.id = it.item_id WHERE it.key IN `+placeholders(len(keys)), anySlice(keys)...)
			if err != nil {
				return err
			}
			r, err := tx.ExecContext(ctx, "DELETE FROM item_tag WHERE key IN "+placeholders(len(keys)), anySlice(keys)...)
			if err != nil {
				return err
			}
			c, _ := r.RowsAffected()
			n += c
			for _, it := range items {
				if err := rebuildItemSearchFTSTx(ctx, tx, it.id, it.kind); err != nil {
					return err
				}
			}
		}
		if len(fields) > 0 {
			r, err := tx.ExecContext(ctx, "DELETE FROM field_provenance WHERE field IN "+placeholders(len(fields)), anySlice(fields)...)
			if err != nil {
				return err
			}
			c, _ := r.RowsAffected()
			n += c
		}
		if len(owed) > 0 {
			where, args := strandedOwedWhere(owed)
			r, err := tx.ExecContext(ctx, "DELETE"+where, args...)
			if err != nil {
				return err
			}
			c, _ := r.RowsAffected()
			n += c
		}
		return nil
	})
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return int(n), nil
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
