package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// searchRow is one item's search_fts row in the catalog's own spelling.
type searchRow struct {
	kind                                  model.Kind
	title, subtitle, artist, album, extra string
	proseExtra                            bool     // extra is prose (show notes)
	credits                               []string // credited names, the artist column's dropped
}

// columns returns the row's indexed columns as stored.
func (r searchRow) columns() [6]string {
	extra := searchColumnText(r.extra)
	if r.proseExtra {
		extra = proseIndexText(r.extra)
	}
	return [6]string{
		searchColumnText(r.title), searchColumnText(r.subtitle), searchColumnText(r.artist),
		searchColumnText(r.album), extra, searchColumnText(creditText(r.artist, r.credits)),
	}
}

// writeSearchRowTx replaces an item's search row (rowid == item id). The table is
// writer-maintained with no triggers, so every write that changes what a row holds
// calls this inside its own transaction.
func writeSearchRowTx(ctx context.Context, tx *sql.Tx, itemID int64, r searchRow) error {
	return writeSearchColumnsTx(ctx, tx, itemID, r.kind, r.columns())
}

func writeSearchColumnsTx(ctx context.Context, tx *sql.Tx, itemID int64, kind model.Kind, c [6]string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM search_fts WHERE rowid = ?", itemID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO search_fts(rowid, kind, title, subtitle, artist, album, extra, credits) VALUES (?,?,?,?,?,?,?,?)",
		itemID, string(kind), c[0], c[1], c[2], c[3], c[4], c[5])
	return err
}

// trackSearchRowTx builds a track's row: its artist and album artist in the artist
// column, genre and custom tags in extra, and its composer and credited names in
// credits.
func trackSearchRowTx(ctx context.Context, q queryer, itemID int64, tr model.Track) (searchRow, error) {
	var title string
	if err := q.QueryRowContext(ctx, "SELECT title FROM playable_item WHERE id = ?", itemID).Scan(&title); err != nil {
		return searchRow{}, err
	}
	custom, err := itemCustomTagText(ctx, q, itemID)
	if err != nil {
		return searchRow{}, err
	}
	names, err := itemCreditNamesTx(ctx, q, itemID)
	if err != nil {
		return searchRow{}, err
	}
	return searchRow{
		kind: model.KindTrack, title: title,
		artist: strings.TrimSpace(tr.Artist + " " + tr.AlbumArtist), album: tr.Album,
		extra:   strings.TrimSpace(tr.Genre + " " + custom),
		credits: append([]string{tr.Composer}, names...),
	}, nil
}

// bookSearchRowTx builds a book's row: the author display the book row stores in the
// artist column, the linked series' name in album (the first spelling of it the
// catalog saw, whatever b carries), genre and custom tags in extra, and the narrator
// with every other credited name in credits.
func bookSearchRowTx(ctx context.Context, q queryer, itemID int64, b model.Book, author string) (searchRow, error) {
	var title, series string
	if err := q.QueryRowContext(ctx, `SELECT pi.title, COALESCE(se.name, '') FROM playable_item pi
		LEFT JOIN book bk ON bk.item_id = pi.id LEFT JOIN series se ON se.id = bk.series_id
		WHERE pi.id = ?`, itemID).Scan(&title, &series); err != nil {
		return searchRow{}, err
	}
	custom, err := itemCustomTagText(ctx, q, itemID)
	if err != nil {
		return searchRow{}, err
	}
	names, err := itemCreditNamesTx(ctx, q, itemID)
	if err != nil {
		return searchRow{}, err
	}
	return searchRow{
		kind: model.KindBook, title: title, subtitle: b.Subtitle, artist: author, album: series,
		extra:   strings.TrimSpace(b.Genre + " " + custom),
		credits: append([]string{b.Narrator}, names...),
	}, nil
}

// episodeSearchRow builds an episode's row: the show's title stands in as artist and
// album, and the HTML-stripped description goes to extra as prose.
func episodeSearchRow(fe model.FeedEpisode, podcastTitle string) searchRow {
	return searchRow{
		kind: model.KindEpisode, title: fe.Title, artist: podcastTitle, album: podcastTitle,
		extra: stripHTML(fe.Description), proseExtra: true,
	}
}

// storedSearchRowTx builds an item's row from what the catalog stores, the row every
// write path indexes once its own writes land. ok is false for an item with no row of
// its kind.
func storedSearchRowTx(ctx context.Context, q queryer, itemID int64, kind string) (searchRow, bool, error) {
	switch kind {
	case string(model.KindTrack):
		tr, err := readTrackColumnsTx(ctx, q, itemID)
		if errors.Is(err, sql.ErrNoRows) {
			return searchRow{}, false, nil
		}
		if err != nil {
			return searchRow{}, false, err
		}
		r, err := trackSearchRowTx(ctx, q, itemID, tr)
		return r, err == nil, err
	case string(model.KindBook):
		var b model.Book
		var author string
		err := q.QueryRowContext(ctx, "SELECT subtitle, author, narrator, genre FROM book WHERE item_id = ?", itemID).
			Scan(&b.Subtitle, &author, &b.Narrator, &b.Genre)
		if errors.Is(err, sql.ErrNoRows) {
			return searchRow{}, false, nil
		}
		if err != nil {
			return searchRow{}, false, err
		}
		r, err := bookSearchRowTx(ctx, q, itemID, b, author)
		return r, err == nil, err
	case string(model.KindEpisode):
		var fe model.FeedEpisode
		var podcastTitle string
		err := q.QueryRowContext(ctx, `SELECT pi.title, ep.description, pod.title
			FROM playable_item pi JOIN episode ep ON ep.item_id = pi.id
			JOIN podcast pod ON pod.id = ep.podcast_id WHERE pi.id = ?`, itemID).Scan(&fe.Title, &fe.Description, &podcastTitle)
		if errors.Is(err, sql.ErrNoRows) {
			return searchRow{}, false, nil
		}
		if err != nil {
			return searchRow{}, false, err
		}
		return episodeSearchRow(fe, podcastTitle), true, nil
	default:
		return searchRow{}, false, nil
	}
}

// rebuildItemSearchFTSTx rewrites an item's search row from its stored state, for a
// write that changes what the row holds without passing through the item's own put
// (a credit or custom-tag edit, a merge, an enrichment genre fill).
func rebuildItemSearchFTSTx(ctx context.Context, tx *sql.Tx, itemID int64, kind string) error {
	r, ok, err := storedSearchRowTx(ctx, tx, itemID, kind)
	if err != nil || !ok {
		return err
	}
	return writeSearchRowTx(ctx, tx, itemID, r)
}

// RebuildSearchIndex rewrites every search row that differs from what its item's
// stored state builds, writes the rows items lack and drops the rows no item backs,
// returning how many rows it wrote or dropped. It is the repair `db verify --fix`
// runs: a write path that changed what a row holds without rebuilding it leaves the
// index stale, and only this notices (the verify counts rows, it does not read them).
// Items are compared sortKeyBatch at a time inside a read transaction, so a catalog in
// step never takes the write lock; the rows that differ are rewritten in one write
// transaction per batch, each compared again there, so a write landing in between is
// not undone. No delta is emitted: the index is not part of any item a consumer reads.
func (s *Store) RebuildSearchIndex(ctx context.Context) (int, error) {
	const op = "store.RebuildSearchIndex"
	written := 0
	var after int64
	for {
		batch, stale, err := s.staleSearchRows(ctx, after)
		if err != nil {
			return written, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].id
		if len(stale) == 0 {
			continue
		}
		n := 0
		err = s.writeTx(ctx, func(tx *sql.Tx) error {
			n = 0
			for _, it := range stale {
				w, err := refreshSearchRowTx(ctx, tx, it.id, it.kind)
				if err != nil {
					return err
				}
				if w {
					n++
				}
			}
			return nil
		})
		if err != nil {
			return written, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		written += n
	}

	const orphanQ = "FROM search_fts WHERE rowid NOT IN (SELECT id FROM playable_item)"
	var orphans int64
	if err := s.read.QueryRowContext(ctx, "SELECT COUNT(*) "+orphanQ).Scan(&orphans); err != nil {
		return written, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if orphans > 0 {
		err := s.writeTx(ctx, func(tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, "DELETE "+orphanQ)
			if err != nil {
				return err
			}
			orphans, err = r.RowsAffected()
			return err
		})
		if err != nil {
			return written, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return written + int(orphans), nil
}

// staleSearchRows reads the next sortKeyBatch items after the id after and returns
// them with the ones whose search row is missing or differs from what their stored
// state builds, all read in one transaction so each item is compared against one
// state of the catalog.
func (s *Store) staleSearchRows(ctx context.Context, after int64) (batch, stale []searchItem, err error) {
	tx, err := s.read.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	batch, err = searchItemsTx(ctx, tx, "SELECT id, kind FROM playable_item WHERE id > ? ORDER BY id LIMIT ?", after, sortKeyBatch)
	if err != nil {
		return nil, nil, err
	}
	for _, it := range batch {
		_, want, err := searchRowDrift(ctx, tx, it.id, it.kind)
		if err != nil {
			return nil, nil, err
		}
		if want != nil {
			stale = append(stale, it)
		}
	}
	return batch, stale, nil
}

// searchRowDrift builds an item's row from its stored state and compares it with the
// stored search row, returning the columns to write when the row is missing or
// differs, and nil when it is in step or the item has no row of its kind.
func searchRowDrift(ctx context.Context, q queryer, itemID int64, kind string) (model.Kind, *[6]string, error) {
	r, ok, err := storedSearchRowTx(ctx, q, itemID, kind)
	if err != nil || !ok {
		return "", nil, err
	}
	want := r.columns()
	var haveKind string
	var have [6]string
	err = q.QueryRowContext(ctx, `SELECT COALESCE(kind, ''), COALESCE(title, ''), COALESCE(subtitle, ''),
		COALESCE(artist, ''), COALESCE(album, ''), COALESCE(extra, ''), COALESCE(credits, '')
		FROM search_fts WHERE rowid = ?`, itemID).Scan(&haveKind, &have[0], &have[1], &have[2], &have[3], &have[4], &have[5])
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", nil, err
	case haveKind == string(r.kind) && have == want:
		return "", nil, nil
	}
	return r.kind, &want, nil
}

// refreshSearchRowTx rewrites an item's row when it is missing or differs from what
// the stored state builds, reporting whether it wrote.
func refreshSearchRowTx(ctx context.Context, tx *sql.Tx, itemID int64, kind string) (bool, error) {
	k, want, err := searchRowDrift(ctx, tx, itemID, kind)
	if err != nil || want == nil {
		return false, err
	}
	return true, writeSearchColumnsTx(ctx, tx, itemID, k, *want)
}

// creditText joins the credited names the headline column does not name already,
// each once.
func creditText(headline string, names []string) string {
	seen := " " + identity.MatchKey(headline) + " "
	var out []string
	for _, n := range names {
		k := identity.MatchKey(n)
		if k == "" || strings.Contains(seen, " "+k+" ") {
			continue
		}
		out = append(out, n)
		seen += k + " "
	}
	return strings.Join(out, " ")
}

// itemCreditNamesTx returns an item's credited names in role, then credited, order.
func itemCreditNamesTx(ctx context.Context, q queryer, itemID int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.name FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id
		WHERE ic.item_id = ? ORDER BY ic.role, ic.position`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// searchItem is an item whose search row a write rebuilds.
type searchItem struct {
	id   int64
	kind string
}

// searchItemsTx reads (item id, kind) pairs, draining its cursor before returning so
// the caller can write to the same transaction.
func searchItemsTx(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]searchItem, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []searchItem
	for rows.Next() {
		var it searchItem
		if err := rows.Scan(&it.id, &it.kind); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
