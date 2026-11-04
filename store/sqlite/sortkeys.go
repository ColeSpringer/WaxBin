package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file is the sort-key repair path. A change to model.SortKey leaves every
// catalog drifted, and no normal write path can heal it: resolveArtist and
// resolveReleaseGroup return as soon as match_key finds a row, and the rollup upkeep
// rewrites an entity's key only when its derived spelling moves, so a rescan leaves an
// existing entity's key as it was. verify.go's drift check walks the same entity list,
// so the check and the repair cannot disagree about what a key should be. An item's key
// folds its title spelling, else its title; a curatable entity's folds its curated sort,
// else its derived spelling (entityspelling.go), else its name.

// sortKeyBatch is about how many rows one transaction rewrites, since an entity's rows
// always share one. Wrapping the catalog in one transaction would hold the write lock for
// the whole run and hand publishSince every changed row at once. Atomicity buys nothing
// against that: the operation is idempotent and purely derived, so an interrupted run
// just leaves the rest drifted for a second --fix.
const sortKeyBatch = 2000

// sortKey is a generated key column and the SQL text over its table's row that
// model.SortKey folds into it.
type sortKey struct{ col, text string }

// sortKeyTable is a table whose generated keys a row rewrites in one statement. as is its
// alias in the walk and on joins its row to the entity's own; the own table is aliased t
// and has no on.
type sortKeyTable struct {
	table, as, idCol, on string
	keys                 []sortKey
}

// sortKeyEntity is a kind of entity whose stale keys are rewritten in one transaction
// under one delta. tables[0] is its own table, holding the pid the delta names. At most
// one of the others may join several rows to an entity (an artist's aliases), since two
// would come back as their cross product.
type sortKeyEntity struct {
	deltaType string // the change_log entity_type, matching EditEntityFields
	curated   string // the entity_curation entity_type its sort override is stored under
	tables    []sortKeyTable
}

// sortKeyEntities is every generated key. A name's own key (title_key, name_key) folds
// the name alone, which a lookup by name compares while sort_key orders.
var sortKeyEntities = []sortKeyEntity{
	{deltaType: "item", tables: []sortKeyTable{
		{table: "playable_item", as: "t", idCol: "id", keys: []sortKey{
			{"sort_key", "COALESCE(NULLIF(t.title_sort, ''), t.title)"},
			{"title_key", "t.title"},
		}},
		// The keys beside the sort spellings, in the SQL form of artistSortKey and friends.
		{table: "track", as: "tr", idCol: "item_id", on: "tr.item_id = t.id", keys: []sortKey{
			{"artist_sort_key", "COALESCE(NULLIF(tr.artist_sort, ''), NULLIF(tr.artist, ''), tr.album_artist)"},
			{"composer_sort_key", "COALESCE(NULLIF(tr.composer_sort, ''), tr.composer)"},
		}},
		{table: "book", as: "bk", idCol: "item_id", on: "bk.item_id = t.id", keys: []sortKey{
			{"series_seq_sort", "bk.series_seq"},
			{"author_sort_key", "COALESCE(NULLIF(bk.author_sort, ''), bk.author)"},
		}},
	}},
	{deltaType: "artist", curated: string(model.MergeArtist), tables: []sortKeyTable{
		{table: "artist", as: "t", idCol: "id", keys: []sortKey{
			{"sort_key", curatedSort("name_sort", "name")},
			{"name_key", "t.name"},
		}},
		// An alias has no pid of its own, so a rewritten one is its artist's delta.
		{table: "artist_alias", as: "al", idCol: "id", on: "al.artist_id = t.id", keys: []sortKey{
			{"sort_key", "al.name"},
		}},
	}},
	{deltaType: "genre", tables: []sortKeyTable{
		{table: "genre", as: "t", idCol: "id", keys: []sortKey{{"sort_key", "t.name"}}},
	}},
	{deltaType: "release_group", curated: string(model.MergeReleaseGroup), tables: []sortKeyTable{
		{table: "release_group", as: "t", idCol: "id", keys: []sortKey{{"sort_key", curatedSort("title_sort", "title")}}},
	}},
	{deltaType: "album", curated: string(model.MergeAlbum), tables: []sortKeyTable{
		{table: "album", as: "t", idCol: "id", keys: []sortKey{
			{"sort_key", curatedSort("title_sort", "title")},
			{"title_key", "t.title"},
		}},
	}},
	{deltaType: "series", tables: []sortKeyTable{
		{table: "series", as: "t", idCol: "id", keys: []sortKey{{"sort_key", "t.name"}}},
	}},
	{deltaType: "podcast", tables: []sortKeyTable{
		{table: "podcast", as: "t", idCol: "id", keys: []sortKey{{"sort_key", "t.title"}}},
	}},
}

// curatedSort is the text a curatable entity's sort_key folds. That is the sort override
// when one is set, because applyEntityFieldTx stores model.SortKey(override) in the
// column and the raw override in entity_curation.value; recomputing from the display
// name would leave a curated entity as the one thing that never folds. Clearing an
// override stores an empty value rather than deleting the row, hence the NULLIF. Without
// one the entity's derived spelling comes before its name.
func curatedSort(spellCol, nameCol string) string {
	return "COALESCE(NULLIF(TRIM(ec.value), ''), NULLIF(t." + spellCol + ", ''), t." + nameCol + ")"
}

// itemTitle is an item's title and its stated sort spelling as the row holds them.
type itemTitle struct{ title, sort string }

// titleOf is the title and spelling a put writes for item.
func titleOf(item model.PlayableItem) itemTitle { return itemTitle{item.Title, item.TitleSort} }

// itemSortKey, artistSortKey, composerSortKey and authorSortKey are the keys a stated sort
// spelling orders by: the spelling folded, or with none the name it sorts in place of.
// Every writer of a spelling or of that name stores the key beside it, and sortKeyEntities
// states the same rule in SQL for the check and the repair.
func itemSortKey(title, titleSort string) string {
	return model.SortKey(cmp.Or(titleSort, title))
}

func artistSortKey(tr model.Track) string {
	return model.SortKey(cmp.Or(tr.ArtistSort, tr.Artist, tr.AlbumArtist))
}

func composerSortKey(tr model.Track) string {
	return model.SortKey(cmp.Or(tr.ComposerSort, tr.Composer))
}

func authorSortKey(spelling, author string) string {
	return model.SortKey(cmp.Or(spelling, author))
}

// refreshSortSpellingKeysTx rewrites one item's spelling keys from its row, for a writer
// that updates a spelling or the name behind it with a statement of its own rather than
// through upsertTrack or upsertBook.
func refreshSortSpellingKeysTx(ctx context.Context, tx *sql.Tx, itemID int64, kind string) error {
	switch kind {
	case string(model.KindTrack):
		var tr model.Track
		if err := tx.QueryRowContext(ctx,
			"SELECT artist_sort, artist, album_artist, composer_sort, composer FROM track WHERE item_id = ?", itemID).
			Scan(&tr.ArtistSort, &tr.Artist, &tr.AlbumArtist, &tr.ComposerSort, &tr.Composer); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE track SET artist_sort_key = ?, composer_sort_key = ? WHERE item_id = ?",
			artistSortKey(tr), composerSortKey(tr), itemID)
		return err
	case string(model.KindBook):
		var spelling, author string
		if err := tx.QueryRowContext(ctx, "SELECT author_sort, author FROM book WHERE item_id = ?", itemID).
			Scan(&spelling, &author); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE book SET author_sort_key = ? WHERE item_id = ?",
			authorSortKey(spelling, author), itemID)
		return err
	}
	return nil
}

// query is the walk over e's entities in id order: the pid, then each table's row id
// (NULL where a joined table has no row) followed by each key's text and stored value.
// An entity with several rows in a joined table comes back once per row. Names are
// internal constants, so they are interpolated; entity_type is bound, like everywhere
// else in the store.
func (e sortKeyEntity) query() (string, []any) {
	cols := []string{"t.pid"}
	from := []string{e.tables[0].table + " t"}
	var args []any
	if e.curated != "" {
		from = append(from, "LEFT JOIN entity_curation ec ON ec.entity_type = ? AND ec.entity_id = t.id AND ec.field = 'sort'")
		args = append(args, e.curated)
	}
	for _, tb := range e.tables {
		if tb.on != "" {
			from = append(from, "LEFT JOIN "+tb.table+" "+tb.as+" ON "+tb.on)
		}
		cols = append(cols, tb.as+"."+tb.idCol)
		for _, k := range tb.keys {
			cols = append(cols, k.text, tb.as+"."+k.col)
		}
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + strings.Join(from, " ") + " ORDER BY t." + e.tables[0].idCol, args
}

// sortKeyRow is one row's stale keys: the columns to rewrite, the value each was read
// with, and the one it folds to.
type sortKeyRow struct {
	table, idCol string
	id           int64
	cols         []string
	was, want    []string
}

// sortKeyFix is one entity's stale keys.
type sortKeyFix struct {
	deltaType string
	pid       model.PID
	rows      []sortKeyRow
}

// keys is how many keys the fix rewrites.
func (f sortKeyFix) keys() int {
	n := 0
	for _, r := range f.rows {
		n += len(r.cols)
	}
	return n
}

// walkSortKeys streams e's entities and hands fn each one holding a key that no longer
// folds from its text. Its own errors are wrapped under op, and fn's come back as they
// are. The read pool and the write connection are separate handles, so rows stream from
// one while fn's updates commit on the other.
func (s *Store) walkSortKeys(ctx context.Context, op string, e sortKeyEntity, fn func(sortKeyFix) error) error {
	q, args := e.query()
	rows, err := s.rdb().QueryContext(ctx, q, args...)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var pid string
	ids := make([]sql.NullInt64, len(e.tables))
	vals := make([][]sql.NullString, len(e.tables)) // each key's text, then its stored value
	dest := []any{&pid}
	for i, tb := range e.tables {
		vals[i] = make([]sql.NullString, 2*len(tb.keys))
		dest = append(dest, &ids[i])
		for j := range vals[i] {
			dest = append(dest, &vals[i][j])
		}
	}
	// prev is the previous result row's ids. Past an entity's first row, a table whose id
	// has not changed was only repeated by the join of several rows, and is already read.
	prev := make([]int64, len(e.tables))
	var fix sortKeyFix
	for n := 0; rows.Next(); n++ {
		if err := rows.Scan(dest...); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		first := n == 0 || ids[0].Int64 != prev[0]
		if first {
			if len(fix.rows) > 0 {
				if err := fn(fix); err != nil {
					return err
				}
			}
			fix = sortKeyFix{deltaType: e.deltaType, pid: model.PID(pid)}
		}
		for i, tb := range e.tables {
			if !ids[i].Valid || (!first && ids[i].Int64 == prev[i]) {
				continue
			}
			row := sortKeyRow{table: tb.table, idCol: tb.idCol, id: ids[i].Int64}
			for j, k := range tb.keys {
				if want, was := model.SortKey(vals[i][2*j].String), vals[i][2*j+1].String; want != was {
					row.cols = append(row.cols, k.col)
					row.was = append(row.was, was)
					row.want = append(row.want, want)
				}
			}
			if len(row.cols) > 0 {
				fix.rows = append(fix.rows, row)
			}
		}
		for i := range ids {
			prev[i] = ids[i].Int64
		}
	}
	if err := rows.Err(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if len(fix.rows) > 0 {
		return fn(fix)
	}
	return nil
}

// RefreshSortKeys rewrites every stored sort key that model.SortKey no longer
// generates and returns how many keys it rewrote. It is the repair for the sort-key
// drift VerifyDerived reports, which counts keys the same way. Only rows whose keys
// actually move are written, so a catalog already in step does no work and floods no
// change log.
//
// A row's stale keys are rewritten in one statement, and an entity's rows (an item's
// track or book row, an artist's aliases) in one transaction under one delta. That is
// the entity's own delta and not one per member item, unlike EditEntityFields: fanning
// out during a bulk refold would multiply the delta count by the catalog's items without
// adding information, and so would a delta per key.
//
// Outstanding page cursors are silently invalidated. read.Cursor encodes the
// row's sort key and QueryPage/EntityPage resume by comparing against it, so a
// cursor held across a fix names a key that no longer exists and the next page
// skips or repeats rows with no error raised. Consumers should re-page from the
// start.
func (s *Store) RefreshSortKeys(ctx context.Context) (int, error) {
	const op = "store.RefreshSortKeys"
	p := &sortKeyPatch{store: s, op: op}
	for _, e := range sortKeyEntities {
		if err := s.walkSortKeys(ctx, op, e, func(f sortKeyFix) error { return p.add(ctx, f) }); err != nil {
			return p.written, err
		}
	}
	if err := p.flush(ctx); err != nil {
		return p.written, err
	}
	return p.written, nil
}

// sortKeyPatch buffers stale entities and rewrites them in bounded batches.
type sortKeyPatch struct {
	store   *Store
	op      string
	pending []sortKeyFix
	rows    int // rows pending
	written int // keys rewritten
}

func (p *sortKeyPatch) add(ctx context.Context, f sortKeyFix) error {
	p.pending = append(p.pending, f)
	if p.rows += len(f.rows); p.rows < sortKeyBatch {
		return nil
	}
	return p.flush(ctx)
}

// flush commits one batch. Statements are prepared per transaction and reused,
// because the driver otherwise re-parses each one for every row; a batch spans a
// handful of distinct updates, one per table and set of stale keys.
//
// Each update is guarded on the keys that were read. The rows are read outside the
// write transaction, so a concurrent writer can land fresh keys in between, and
// without the guard this would put stale ones back. An entity emits its delta once,
// when any of its rows was rewritten.
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

		for _, f := range p.pending {
			changed := false
			for _, r := range f.rows {
				q := "UPDATE " + r.table + " SET " + strings.Join(r.cols, " = ?, ") + " = ? WHERE " +
					r.idCol + " = ? AND " + strings.Join(r.cols, " = ? AND ") + " = ?"
				st, ok := updates[q]
				if !ok {
					if st, err = tx.PrepareContext(ctx, q); err != nil {
						return waxerr.Wrap(waxerr.CodeIO, p.op, err)
					}
					updates[q] = st
				}
				args := make([]any, 0, 2*len(r.cols)+1)
				for _, k := range r.want {
					args = append(args, k)
				}
				args = append(args, r.id)
				for _, k := range r.was {
					args = append(args, k)
				}
				res, err := st.ExecContext(ctx, args...)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, p.op, err)
				}
				// Someone rewrote the row after it was read; leave their keys and say nothing.
				if n, err := res.RowsAffected(); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, p.op, err)
				} else if n == 0 {
					continue
				}
				applied += len(r.cols)
				changed = true
			}
			if !changed {
				continue
			}
			if _, err := changes.ExecContext(ctx,
				nowNS(), f.deltaType, string(f.pid), string(model.OpUpdate)); err != nil {
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
	p.rows = 0
	return nil
}
