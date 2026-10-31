package sqlite

import (
	"cmp"
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
)

// An album, a release group and an artist derive a sort spelling from what their live
// members state: the spelling the most of them state, a tie going to the one the earliest
// member states and then to the lesser spelling, or none when no member states one. The
// entity's key folds its curated sort, else that spelling, else its name, which is the
// rule sortKeySources checks.
//
// A member spells an album and its release group in its ALBUMSORT. It spells an artist
// only where its credit names that artist alone, since the spelling of a joint credit
// spells the whole credit and not the primary artist the credit resolves to: a sole
// artist credit's ARTISTSORT, the ALBUMARTISTSORT of an album-artist credit naming one
// artist (wb_sole_credit), and a sole author's author sort.

// entitySpelling is one kind's derived spelling: its table (also its rollup_dirty kind
// and change_log type), the columns of its name and spelling, and the rows (s, item_id) of
// the spellings the members of entity e state.
type entitySpelling struct {
	table, nameCol, col string
	pool                string
}

var (
	albumSpelling = entitySpelling{table: "album", nameCol: "title", col: "title_sort", pool: `
		SELECT t.album_sort AS s, t.item_id AS item_id FROM track t ` + liveMembers + `
		WHERE t.album_id = e.id AND t.album_sort <> ''`}
	releaseGroupSpelling = entitySpelling{table: "release_group", nameCol: "title", col: "title_sort", pool: `
		SELECT t.album_sort AS s, t.item_id AS item_id FROM album al JOIN track t ON t.album_id = al.id ` + liveMembers + `
		WHERE al.release_group_id = e.id AND t.album_sort <> ''`}
	artistSpelling = entitySpelling{table: "artist", nameCol: "name", col: "name_sort", pool: `
		SELECT t.artist_sort AS s, t.item_id AS item_id FROM track t ` + liveMembers + `
		WHERE t.artist_id = e.id AND t.artist_sort <> ''
			AND NOT EXISTS (SELECT 1 FROM item_contributor ic
				WHERE ic.item_id = t.item_id AND ic.role = 'artist' AND ic.artist_id <> e.id)
		UNION ALL
		SELECT t.album_artist_sort, t.item_id FROM track t ` + liveMembers + `
		WHERE t.album_artist_id = e.id AND t.album_artist_sort <> ''
			AND (t.album_artist = e.name OR wb_sole_credit(t.album_artist, e.match_key))
		UNION ALL
		SELECT bk.author_sort, bk.item_id FROM book bk ` + liveBooks + `
		WHERE bk.author_id = e.id AND bk.author_sort <> ''
			AND NOT EXISTS (SELECT 1 FROM item_contributor ic
				WHERE ic.item_id = bk.item_id AND ic.role = 'author' AND ic.artist_id <> e.id)`}
)

// entitySpellings lists the kinds in the order a whole-catalog refresh walks them.
var entitySpellings = []entitySpelling{albumSpelling, releaseGroupSpelling, artistSpelling}

// derived is the spelling e's members state, an empty string when none does. A member
// stating one spelling in two of the pool's arms (an artist's ARTISTSORT and
// ALBUMARTISTSORT) counts once for it.
func (sp entitySpelling) derived() string {
	return `COALESCE((SELECT s FROM (SELECT s, COUNT(DISTINCT item_id) AS n, MIN(item_id) AS first FROM (` + sp.pool + `) GROUP BY s)
		ORDER BY n DESC, first, s LIMIT 1), '')`
}

// spellingFor returns the derived spelling of a curatable entity kind.
func spellingFor(et model.MergeEntity) (entitySpelling, bool) {
	switch et {
	case model.MergeAlbum:
		return albumSpelling, true
	case model.MergeReleaseGroup:
		return releaseGroupSpelling, true
	case model.MergeArtist:
		return artistSpelling, true
	}
	return entitySpelling{}, false
}

// refreshSpellingsTx recomputes the derived spelling of each of ids and the key it and the
// curated sort give, writing what moved. It returns how many spellings moved and the pids
// of the entities whose key moved, which a caller with no delta of its own for them emits.
func refreshSpellingsTx(ctx context.Context, tx *sql.Tx, sp entitySpelling, ids []int64) (int, []model.PID, error) {
	spelled := 0
	var keyed []model.PID
	err := chunkSlice(ids, idBatchSize, func(chunk []int64) error {
		n, k, err := refreshSpellingsWhereTx(ctx, tx, sp, "WHERE e.id IN "+inArray, idArray(chunk))
		spelled, keyed = spelled+n, append(keyed, k...)
		return err
	})
	return spelled, keyed, err
}

// refreshSpellingsWhereTx is refreshSpellingsTx over the entities filter admits, every one
// when it is empty. It reads each entity once and keeps only the ones that moved, since the
// key is folded in Go.
func refreshSpellingsWhereTx(ctx context.Context, tx *sql.Tx, sp entitySpelling, filter string, args ...any) (int, []model.PID, error) {
	type move struct {
		id                   int64
		pid                  model.PID
		spelling, key        string
		spellMoved, keyMoved bool
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.pid, e.`+sp.nameCol+`, e.`+sp.col+`, e.sort_key, `+sp.derived()+`,
			COALESCE(TRIM(ec.value), '')
		FROM `+sp.table+` e LEFT JOIN entity_curation ec ON ec.entity_type = '`+sp.table+`' AND ec.entity_id = e.id
			AND ec.field = 'sort' `+filter, args...)
	if err != nil {
		return 0, nil, err
	}
	var moves []move
	for rows.Next() {
		var m move
		var name, stored, storedKey, curated string
		if err := rows.Scan(&m.id, &m.pid, &name, &stored, &storedKey, &m.spelling, &curated); err != nil {
			rows.Close()
			return 0, nil, err
		}
		m.key = model.SortKey(cmp.Or(curated, m.spelling, name))
		m.spellMoved, m.keyMoved = m.spelling != stored, m.key != storedKey
		if m.spellMoved || m.keyMoved {
			moves = append(moves, m)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	spelled := 0
	var keyed []model.PID
	for _, m := range moves {
		if _, err := tx.ExecContext(ctx, "UPDATE "+sp.table+" SET "+sp.col+" = ?, sort_key = ? WHERE id = ?",
			m.spelling, m.key, m.id); err != nil {
			return 0, nil, err
		}
		if m.spellMoved {
			spelled++
		}
		if m.keyMoved {
			keyed = append(keyed, m.pid)
		}
	}
	return spelled, keyed, nil
}

// settleSpellingsTx is refreshSpellingsTx for a write with no delta of its own for the
// entities, emitting one for each whose key moved. It returns how many spellings and how
// many keys moved.
func settleSpellingsTx(ctx context.Context, tx *sql.Tx, sp entitySpelling, ids []int64) (int, int, error) {
	n, keyed, err := refreshSpellingsTx(ctx, tx, sp, ids)
	if err != nil {
		return 0, 0, err
	}
	return n, len(keyed), emitKeyMovesTx(ctx, tx, sp, keyed)
}

// refreshAllSpellingsTx settles every entity's spelling, for RefreshRollups, in one read
// of each kind's table.
func refreshAllSpellingsTx(ctx context.Context, tx *sql.Tx) (spelled, keyed int, err error) {
	for _, sp := range entitySpellings {
		n, moved, err := refreshSpellingsWhereTx(ctx, tx, sp, "")
		if err != nil {
			return 0, 0, err
		}
		if err := emitKeyMovesTx(ctx, tx, sp, moved); err != nil {
			return 0, 0, err
		}
		spelled, keyed = spelled+n, keyed+len(moved)
	}
	return spelled, keyed, nil
}

// emitKeyMovesTx emits the delta of each entity whose key a spelling refresh moved.
func emitKeyMovesTx(ctx context.Context, tx *sql.Tx, sp entitySpelling, pids []model.PID) error {
	for _, pid := range pids {
		if err := appendChange(ctx, tx, sp.table, pid, model.OpUpdate); err != nil {
			return err
		}
	}
	return nil
}
