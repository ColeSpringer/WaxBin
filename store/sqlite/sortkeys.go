package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file is the sort-key repair path. A change to model.SortKey leaves every
// catalog drifted, and no normal write path can heal it: resolveArtist and
// resolveReleaseGroup return as soon as match_key finds a row, and sort_key is
// only written by their INSERT, so a rescan leaves an existing entity's key as it
// was. verify.go's drift check walks the same source list, so the check and the
// repair cannot disagree about what a key should be.

// sortKeyBatch is how many rewritten rows one transaction carries. Wrapping the
// catalog in one transaction would hold the write lock for the whole run and hand
// publishSince every changed row at once. Atomicity buys nothing against that:
// the operation is idempotent and purely derived, so an interrupted run just
// leaves the rest drifted for a second --fix.
const sortKeyBatch = 2000

// sortKeySource is a table whose sort key is recomputed exactly, by replacing the
// stored key with model.SortKey of the display text the catalog still holds.
type sortKeySource struct {
	table   string // table holding the key
	keyCol  string // the generated key column
	textCol string // the display column it derives from
	idCol   string // primary key, the UPDATE target
	pidExpr string // SQL for the pid a rewritten row's delta names
	// pidJoin reaches pidExpr for a table with no pid of its own. Both joins below
	// are on non-null foreign keys, so the row set is unchanged and the drift check
	// can share the query.
	pidJoin string
	// entityType is set where a user sort override can live in entity_curation.
	entityType string
	// deltaType is the change_log entity_type: "item" for a playable_item row, else
	// the entity's table name, matching EditEntityFields.
	deltaType string
}

var sortKeySources = []sortKeySource{
	{table: "playable_item", keyCol: "sort_key", textCol: "title", idCol: "id",
		pidExpr: "t.pid", deltaType: "item"},
	{table: "artist", keyCol: "sort_key", textCol: "name", idCol: "id",
		pidExpr: "t.pid", entityType: string(model.MergeArtist), deltaType: "artist"},
	// An alias has no pid of its own, so a rewritten one emits its artist's delta.
	{table: "artist_alias", keyCol: "sort_key", textCol: "name", idCol: "id",
		pidExpr: "a.pid", pidJoin: " JOIN artist a ON a.id = t.artist_id", deltaType: "artist"},
	{table: "genre", keyCol: "sort_key", textCol: "name", idCol: "id",
		pidExpr: "t.pid", deltaType: "genre"},
	{table: "release_group", keyCol: "sort_key", textCol: "title", idCol: "id",
		pidExpr: "t.pid", entityType: string(model.MergeReleaseGroup), deltaType: "release_group"},
	{table: "album", keyCol: "sort_key", textCol: "title", idCol: "id",
		pidExpr: "t.pid", entityType: string(model.MergeAlbum), deltaType: "album"},
	{table: "series", keyCol: "sort_key", textCol: "name", idCol: "id",
		pidExpr: "t.pid", deltaType: "series"},
	{table: "podcast", keyCol: "sort_key", textCol: "title", idCol: "id",
		pidExpr: "t.pid", deltaType: "podcast"},
	// The same plain derivation, over the series sequence rather than a name.
	{table: "book", keyCol: "series_seq_sort", textCol: "series_seq", idCol: "item_id",
		pidExpr: "pi.pid", pidJoin: " JOIN playable_item pi ON pi.id = t.item_id", deltaType: "item"},
}

// textExpr is the SQL a row's sort key is regenerated from. For a curatable
// entity that is the sort override when one is set, because applyEntityFieldTx
// stores model.SortKey(override) in the column and the raw override in
// entity_curation.value; recomputing from the display name would leave a curated
// entity as the one thing that never folds. Clearing an override stores an empty
// value rather than deleting the row, hence the NULLIF.
func (src sortKeySource) textExpr() string {
	if src.entityType == "" {
		return "t." + src.textCol
	}
	return "COALESCE(NULLIF(TRIM(ec.value), ''), t." + src.textCol + ")"
}

// query builds the streaming read for one source. lead is any extra leading
// column list (the drift check needs none); the recompute text and the stored key
// always come last. Table and column names are internal constants, so they are
// interpolated; entity_type is bound, like everywhere else in the store.
func (src sortKeySource) query(lead string) (string, []any) {
	q := "SELECT " + lead + src.textExpr() + ", t." + src.keyCol +
		" FROM " + src.table + " t" + src.pidJoin
	var args []any
	if src.entityType != "" {
		q += " LEFT JOIN entity_curation ec ON ec.entity_type = ? AND ec.entity_id = t.id AND ec.field = 'sort'"
		args = append(args, src.entityType)
	}
	return q, args
}

// sortKeyRefold is a column holding a key derived from a sort tag rather than a
// display name. The tag is not recoverable from the catalog, so these are refolded
// in place; recomputing from the display column would destroy a "Beatles, The"
// style tag. All of them live on a playable_item subtype, so the delta is the
// item's.
type sortKeyRefold struct {
	table string // subtype table, keyed by item_id
	col   string
	// lockField names the field_provenance field whose lock exempts a row. An
	// explicit composer_sort or author_sort edit stores the literal user string
	// rather than SortKey(value), so refolding a locked one would rewrite what the
	// user typed. artist_sort has no lock to check (see credits.go).
	lockField string
}

var sortKeyRefolds = []sortKeyRefold{
	{table: "track", col: "artist_sort"},
	{table: "track", col: "composer_sort", lockField: "composer_sort"},
	{table: "book", col: "author_sort", lockField: "author_sort"},
}

func (r sortKeyRefold) query() (string, []any) {
	q := "SELECT t.item_id, pi.pid, t." + r.col + " FROM " + r.table + " t" +
		" JOIN playable_item pi ON pi.id = t.item_id WHERE t." + r.col + " <> ''"
	var args []any
	if r.lockField != "" {
		q += " AND NOT EXISTS (SELECT 1 FROM field_provenance fp" +
			" WHERE fp.item_id = t.item_id AND fp.field = ? AND fp.locked = 1)"
		args = append(args, r.lockField)
	}
	return q, args
}

// RefreshSortKeys rewrites every stored sort key that model.SortKey no longer
// generates and returns the number of rows rewritten. It is the repair for the
// sort-key drift VerifyDerived reports. Only rows whose key actually moves are
// written, and only those emit a delta, so a catalog already in step does no work
// and floods no change log.
//
// A rewritten entity emits its own delta and not one per member item, unlike
// EditEntityFields: fanning out during a bulk refold would multiply the delta
// count by the catalog's items without adding information.
//
// Outstanding page cursors are silently invalidated. read.Cursor encodes the
// row's sort key and QueryPage/EntityPage resume by comparing against it, so a
// cursor held across a fix names a key that no longer exists and the next page
// skips or repeats rows with no error raised. Consumers should re-page from the
// start.
func (s *Store) RefreshSortKeys(ctx context.Context) (int, error) {
	const op = "store.RefreshSortKeys"
	p := &sortKeyPatch{store: s, op: op}
	for _, src := range sortKeySources {
		if err := s.recomputeSortKeys(ctx, p, src); err != nil {
			return p.written, err
		}
	}
	for _, r := range sortKeyRefolds {
		if err := s.refoldSortKeys(ctx, p, r); err != nil {
			return p.written, err
		}
	}
	if err := p.flush(ctx); err != nil {
		return p.written, err
	}
	return p.written, nil
}

// recomputeSortKeys streams one source table and queues the rows whose key moved.
// s.read and s.write are separate handles, so rows stream from one while updates
// commit on the other and only a bounded batch is ever buffered.
func (s *Store) recomputeSortKeys(ctx context.Context, p *sortKeyPatch, src sortKeySource) error {
	q, args := src.query("t." + src.idCol + ", " + src.pidExpr + ", ")
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, p.op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var pid, text, stored string
		if err := rows.Scan(&id, &pid, &text, &stored); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, p.op, err)
		}
		want := model.SortKey(text)
		if want == stored {
			continue
		}
		if err := p.add(ctx, sortKeyUpdate{
			table: src.table, col: src.keyCol, idCol: src.idCol, id: id,
			was: stored, key: want, deltaType: src.deltaType, pid: model.PID(pid),
		}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, p.op, err)
	}
	return nil
}

// refoldSortKeys streams one tag-derived column and queues the rows whose folded
// form differs from what is stored.
//
// RefoldKey, not Fold: padNumbers has only ever padded ASCII, so a "Track ٢" tag
// sits in the column as "track ٢" and Fold alone would leave "track 2" where a
// later rescan writes "track 0000000002". Not SortKey either, because
// stripArticle is not idempotent.
//
// Two rare cases still will not match what a later rescan writes, and both
// self-heal when the file is rescanned: a fullwidth "ｔｈｅ ｂｅａｔｌｅｓ" refolds to
// "the beatles" while a rescan strips the now-ASCII article, and a run mixing
// ASCII with non-ASCII digits folds wider than the pad width so it stays
// unpadded.
func (s *Store) refoldSortKeys(ctx context.Context, p *sortKeyPatch, r sortKeyRefold) error {
	q, args := r.query()
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, p.op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var pid, stored string
		if err := rows.Scan(&id, &pid, &stored); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, p.op, err)
		}
		want := model.RefoldKey(stored)
		if want == stored {
			continue
		}
		if err := p.add(ctx, sortKeyUpdate{
			table: r.table, col: r.col, idCol: "item_id", id: id,
			was: stored, key: want, deltaType: "item", pid: model.PID(pid),
		}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, p.op, err)
	}
	return nil
}

type sortKeyUpdate struct {
	table     string
	col       string
	idCol     string
	id        int64
	was       string // the key read, bound as an optimistic guard
	key       string
	deltaType string
	pid       model.PID
}

// sortKeyPatch buffers rewritten rows and commits them in bounded batches.
type sortKeyPatch struct {
	store   *Store
	op      string
	pending []sortKeyUpdate
	written int
}

func (p *sortKeyPatch) add(ctx context.Context, u sortKeyUpdate) error {
	p.pending = append(p.pending, u)
	if len(p.pending) < sortKeyBatch {
		return nil
	}
	return p.flush(ctx)
}

// flush commits one batch. Statements are prepared per transaction and reused,
// because the driver otherwise re-parses each one for every row; a batch spans at
// most a couple of distinct updates, since rows arrive grouped by source.
//
// Each update is guarded on the key that was read. The rows are read outside the
// write transaction, so a concurrent writer can land a fresh key in between, and
// without the guard this would put the stale one back.
func (p *sortKeyPatch) flush(ctx context.Context) error {
	if len(p.pending) == 0 {
		return nil
	}
	applied := 0
	err := p.store.writeTx(ctx, func(tx *sql.Tx) error {
		applied = 0
		changes, err := tx.PrepareContext(ctx, appendChangeSQL)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, p.op, err)
		}
		defer changes.Close()
		updates := map[string]*sql.Stmt{}
		defer func() {
			for _, st := range updates {
				st.Close()
			}
		}()

		for _, u := range p.pending {
			q := "UPDATE " + u.table + " SET " + u.col + " = ? WHERE " + u.idCol + " = ? AND " + u.col + " = ?"
			st, ok := updates[q]
			if !ok {
				if st, err = tx.PrepareContext(ctx, q); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, p.op, err)
				}
				updates[q] = st
			}
			res, err := st.ExecContext(ctx, u.key, u.id, u.was)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, p.op, err)
			}
			// Someone rewrote the row after it was read; leave their key and say nothing.
			if n, err := res.RowsAffected(); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, p.op, err)
			} else if n == 0 {
				continue
			}
			applied++
			if _, err := changes.ExecContext(ctx,
				nowNS(), u.deltaType, string(u.pid), string(model.OpUpdate)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, p.op, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	p.written += applied
	p.pending = p.pending[:0]
	return nil
}
