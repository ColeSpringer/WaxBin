package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"strconv"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// EntityByPID returns the summary info for one shared entity (artist, release
// group, album, genre, or series): identity, parent links, membership counts,
// and the libraries its members' primary files live in. It is the direct read
// behind a facet bucket's or query field's EntityPID, so a consumer holding a
// pid never has to reconstruct the entity from a full facet scan.
//
// Artist, release-group, and genre counts read the maintained rollups (one PK
// seek); album and series have no rollup rows and aggregate live over their
// indexed member columns (track_album_id, book_series), reusing the effective-
// duration expression so a virtual track prices its window here exactly as it
// does in the rollups and the item view. A series sums book.total_duration_ms,
// the maintained sum of each book's parts, because a multi-file book's primary
// edge alone would undercount it. An unknown kind is CodeInvalid; an unknown
// pid is CodeNotFound.
func (s *Store) EntityByPID(ctx context.Context, kind read.EntityKind, pid model.PID) (*read.EntityInfo, error) {
	const op = "store.EntityByPID"
	if !kind.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown entity kind: "+string(kind))
	}
	info := &read.EntityInfo{Kind: kind, PID: pid}
	var id int64
	var artistPID, rgPID string
	var err error
	switch kind {
	case read.EntityArtist:
		err = s.rstmts.queryRowContext(ctx, s.rdb(), `SELECT a.id, a.name, a.sort_key, COALESCE(a.mbid,''),
			COALESCE(ar.track_count,0), COALESCE(ar.release_group_count,0), COALESCE(ar.total_duration_ms,0)
			FROM artist a LEFT JOIN artist_rollup ar ON ar.artist_id = a.id
			WHERE a.pid = ?`, string(pid)).
			Scan(&id, &info.Name, &info.SortKey, &info.MBID, &info.ItemCount, &info.ReleaseGroupCount, &info.TotalDurationMS)
	case read.EntityReleaseGroup:
		err = s.rstmts.queryRowContext(ctx, s.rdb(), `SELECT rg.id, rg.title, rg.sort_key, COALESCE(rg.mbid,''),
			COALESCE(rg.type,''), COALESCE(pa.pid,''),
			COALESCE(rr.track_count,0), COALESCE(rr.total_duration_ms,0)
			FROM release_group rg
			LEFT JOIN artist pa ON pa.id = rg.primary_artist_id
			LEFT JOIN release_group_rollup rr ON rr.release_group_id = rg.id
			WHERE rg.pid = ?`, string(pid)).
			Scan(&id, &info.Name, &info.SortKey, &info.MBID, &info.Type, &artistPID, &info.ItemCount, &info.TotalDurationMS)
	case read.EntityAlbum:
		err = s.rstmts.queryRowContext(ctx, s.rdb(), `SELECT al.id, al.title, al.sort_key, COALESCE(al.mbid,''),
			COALESCE(al.year,0), COALESCE(rg.pid,''),
			COALESCE(al.barcode,''), COALESCE(al.label,''), COALESCE(al.catalog_number,''),
			COALESCE(al.media,''), COALESCE(al.country,'')
			FROM album al LEFT JOIN release_group rg ON rg.id = al.release_group_id
			WHERE al.pid = ?`, string(pid)).
			Scan(&id, &info.Name, &info.SortKey, &info.MBID, &info.Year, &rgPID,
				&info.Barcode, &info.Label, &info.CatalogNumber, &info.Media, &info.Country)
	case read.EntityGenre:
		err = s.rstmts.queryRowContext(ctx, s.rdb(), `SELECT g.id, g.name, g.sort_key,
			COALESCE(gr.track_count,0), COALESCE(gr.total_duration_ms,0)
			FROM genre g LEFT JOIN genre_rollup gr ON gr.genre_id = g.id
			WHERE g.pid = ?`, string(pid)).
			Scan(&id, &info.Name, &info.SortKey, &info.ItemCount, &info.TotalDurationMS)
	case read.EntitySeries:
		err = s.rstmts.queryRowContext(ctx, s.rdb(),
			"SELECT srs.id, srs.name, srs.sort_key FROM series srs WHERE srs.pid = ?",
			string(pid)).
			Scan(&id, &info.Name, &info.SortKey)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such "+string(kind)+": "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	info.ArtistPID, info.ReleaseGroupPID = model.PID(artistPID), model.PID(rgPID)

	switch kind {
	case read.EntityAlbum:
		err = s.rstmts.queryRowContext(ctx, s.rdb(), `SELECT COUNT(DISTINCT t.item_id), COALESCE(SUM(`+itemEffectiveDurationExpr+`),0)
			FROM track t `+liveMembers+`
			LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
			LEFT JOIN file f ON f.id = pf.file_id
			WHERE t.album_id = ?`, id).
			Scan(&info.ItemCount, &info.TotalDurationMS)
	case read.EntitySeries:
		err = s.rstmts.queryRowContext(ctx, s.rdb(),
			"SELECT COUNT(*), COALESCE(SUM(bk.total_duration_ms),0) FROM book bk "+liveBooks+" WHERE bk.series_id = ?", id).
			Scan(&info.ItemCount, &info.TotalDurationMS)
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	libs, err := s.entityLibraryPIDs(ctx, kind, id, op)
	if err != nil {
		return nil, err
	}
	info.LibraryPIDs, info.PrimaryLibraryPIDs = libs.all, libs.primary
	if kind == read.EntityArtist {
		aliases, err := s.artistAliases(ctx, []int64{id}, op)
		if err != nil {
			return nil, err
		}
		info.Aliases = aliases[id]
	}
	return info, nil
}

// artistAliases lists the other names of each artist, by name, keyed by artist id.
func (s *Store) artistAliases(ctx context.Context, ids []int64, op string) (map[int64][]string, error) {
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), `SELECT artist_id, name FROM artist_alias
		WHERE artist_id IN `+inArray+` AND is_primary = 0 ORDER BY artist_id, name`, idArray(ids))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out[id] = append(out[id], name)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// entityTable returns the table backing an entity kind. The value is a constant
// chosen by a validated kind, never caller text, so it is safe to concatenate.
func entityTable(kind read.EntityKind) string {
	switch kind {
	case read.EntityArtist:
		return "artist"
	case read.EntityReleaseGroup:
		return "release_group"
	case read.EntityAlbum:
		return "album"
	case read.EntityGenre:
		return "genre"
	case read.EntitySeries:
		return "series"
	}
	return ""
}

// EntityPage enumerates one kind's entities in collation order (sort_key, then pid
// as a tiebreak), keyset-paginated: the entity-side twin of QueryPage. It is the
// alphabetical index a large dimension wants, where paging a facet would re-run the
// full aggregation per page. A non-empty but malformed cursor is rejected rather
// than silently restarting, and limit <= 0 takes the default page size.
//
// It runs in two steps: walk the table's sort_key index for this page's pids, then
// hand them to EntityByPIDs and restore the keyset order. Hydrating through the
// existing batch read is deliberate. It means a page row is identical to a looked-up
// row by construction, with no second definition of what an entity summary is, and it
// already fills counts, parent links, and LibraryPIDs.
//
// The cost of those two steps is that they are two statements on the read pool, so an
// entity deleted between them is enumerated but not hydrated, and Entities comes back
// shorter than limit while HasMore and Next still describe the enumeration. No entity
// is skipped or repeated by that: the cursor still points at the last enumerated row,
// so the next page resumes correctly. Page a drain on HasMore rather than on a
// non-empty Entities, since a page whose entities were all deleted is legitimately
// empty with more to come. QueryPage has no such window because its page is one
// statement; buying that here would mean restating what an entity summary is.
func (s *Store) EntityPage(ctx context.Context, kind read.EntityKind, cursor read.Cursor, limit int) (*read.EntityPage, error) {
	const op = "store.EntityPage"
	if !kind.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown entity kind: "+string(kind))
	}
	if limit <= 0 {
		limit = defaultPageSize
	}
	// A validated whitelist value, never user text. The empty check is the loud
	// failure for a kind added to EntityKind.Valid but not to entityTable, which would
	// otherwise interpolate nothing and surface as a SQL syntax error.
	table := entityTable(kind)
	if table == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no table for entity kind: "+string(kind))
	}

	var args []any
	if cursor != "" {
		sk, pid, ok := cursor.Decode()
		if !ok {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "malformed page cursor")
		}
		args = append(args, sk, string(pid))
	}
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), entityPageStmt(table, cursor != "", limit), args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var pids []model.PID
	var sortKeys []string
	for rows.Next() {
		var pid, sortKey string
		if err := rows.Scan(&pid, &sortKey); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		pids = append(pids, model.PID(pid))
		sortKeys = append(sortKeys, sortKey)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	page := &read.EntityPage{Kind: kind}
	// The extra row only signals a further page; the next cursor points at the last
	// row this page returns, not the dropped probe.
	if len(pids) > limit {
		pids, sortKeys = pids[:limit], sortKeys[:limit]
		page.HasMore = true
		page.Next = read.EncodeCursor(sortKeys[limit-1], pids[limit-1])
	}
	if len(pids) == 0 {
		return page, nil
	}
	byPID, err := s.EntityByPIDs(ctx, kind, pids)
	if err != nil {
		return nil, err
	}
	page.Entities = make([]*read.EntityInfo, 0, len(pids))
	for _, pid := range pids {
		if info, ok := byPID[pid]; ok {
			page.Entities = append(page.Entities, info)
		}
	}
	return page, nil
}

// entityPageStmt walks table's sort_key index for one page of limit entities and one more,
// which only signals a further page, after a cursor's (sort_key, pid) when afterCursor is
// set. The row-value comparison is the shape QueryPage uses: the planner drives it off the
// sort_key index directly and it needs only two binds. The limit is written into the text
// for the statement cache (searchStmt says why).
func entityPageStmt(table string, afterCursor bool, limit int) string {
	stmt := "SELECT pid, sort_key FROM " + table
	if afterCursor {
		stmt += " WHERE (sort_key, pid) > (?, ?)"
	}
	return stmt + " ORDER BY sort_key, pid LIMIT " + strconv.Itoa(limit+1)
}

// artistMembersSub pairs every item with each artist it files under on any artist
// dimension: its performing artist and album artist, a book's first author, and every
// artist credited on it in any role, which is where the creditArtist, author and
// narrator buckets come from and where the DB-only roles live. A featured artist, an
// album-artist-only artist, a co-author or a narrator is reachable from a bucket, so it
// has to be reachable here too, or a host scoping entities by library would hide it. The
// first and third arms repeat the contributor rows a put writes beside them, and the
// readers join each row to its files, so the arms are deduplicated here (UNION, one sort
// of the candidate rows) rather than after the joins; they stay rather than trusting
// the rows to be complete, since the artist facet reads the columns, not the rows. Each
// arm seeks its own index once the reader's predicate on am.artist_id is pushed into
// it, which TestArtistMemberSourceSeeksEveryArm pins.
const artistMembersSub = `SELECT t.item_id AS item_id, t.artist_id AS artist_id FROM track t WHERE t.artist_id IS NOT NULL
	UNION SELECT t.item_id, t.album_artist_id FROM track t WHERE t.album_artist_id IS NOT NULL
	UNION SELECT bk.item_id, bk.author_id FROM book bk WHERE bk.author_id IS NOT NULL
	UNION SELECT ic.item_id, ic.artist_id FROM item_contributor ic`

// entityMemberSource returns the FROM/JOIN clause reaching every file of an entity's
// member items, alternates included the way the library query field reaches them, and
// the expression selecting each member's entity id. It is shared by the single and
// batched library-pid lookups so the two can never diverge about which items back an
// entity. Artist membership is artistMembersSub, wider than the artist facet's
// itemArtistIDExpr: every dimension that hands out an artist pid counts. A fileless
// member contributes nothing (the INNER file join).
func entityMemberSource(kind read.EntityKind) (from, idExpr string) {
	switch kind {
	case read.EntityArtist:
		return ` FROM (` + artistMembersSub + `) am
			JOIN item_file pf ON pf.item_id = am.item_id
			JOIN file f ON f.id = pf.file_id`, "am.artist_id"
	case read.EntityReleaseGroup:
		return ` FROM track t
			JOIN album al ON al.id = t.album_id
			JOIN item_file pf ON pf.item_id = t.item_id
			JOIN file f ON f.id = pf.file_id`, "al.release_group_id"
	case read.EntityAlbum:
		return ` FROM track t
			JOIN item_file pf ON pf.item_id = t.item_id
			JOIN file f ON f.id = pf.file_id`, "t.album_id"
	case read.EntityGenre:
		return ` FROM item_genre ig
			JOIN item_file pf ON pf.item_id = ig.item_id
			JOIN file f ON f.id = pf.file_id`, "ig.genre_id"
	case read.EntitySeries:
		return ` FROM book bk
			JOIN item_file pf ON pf.item_id = bk.item_id
			JOIN file f ON f.id = pf.file_id`, "bk.series_id"
	}
	return "", ""
}

// entityLibraryPIDsStmt selects the distinct libraries holding one entity's member files,
// in library order, each with whether a member's primary file sits there;
// entityLibraryPIDsBatchStmt is the same over an id array, keyed by entity id. The plan
// test reads both.
func entityLibraryPIDsStmt(kind read.EntityKind) string {
	from, idExpr := entityMemberSource(kind)
	return "SELECT l.pid, MAX(pf.role = 'primary')" + from + " JOIN library l ON l.id = f.library_id WHERE " + idExpr + " = ? GROUP BY l.id ORDER BY l.id"
}

func entityLibraryPIDsBatchStmt(kind read.EntityKind) string {
	from, idExpr := entityMemberSource(kind)
	return "SELECT " + idExpr + " AS eid, l.pid, MAX(pf.role = 'primary')" + from +
		" JOIN library l ON l.id = f.library_id WHERE " + idExpr + " IN " + inArray +
		" GROUP BY eid, l.id ORDER BY eid, l.id"
}

// entityLibraries is one entity's libraries in library order: every library holding a
// member's file, and among them the ones holding a member's primary file
// (read.EntityInfo.LibraryPIDs and PrimaryLibraryPIDs).
type entityLibraries struct{ all, primary []model.PID }

func (e *entityLibraries) add(pid string, primary bool) {
	e.all = append(e.all, model.PID(pid))
	if primary {
		e.primary = append(e.primary, model.PID(pid))
	}
}

// entityLibraryPIDs returns the libraries holding an entity's member items' files.
func (s *Store) entityLibraryPIDs(ctx context.Context, kind read.EntityKind, id int64, op string) (entityLibraries, error) {
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), entityLibraryPIDsStmt(kind), id)
	if err != nil {
		return entityLibraries{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out entityLibraries
	for rows.Next() {
		var pid string
		var primary bool
		if err := rows.Scan(&pid, &primary); err != nil {
			return entityLibraries{}, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out.add(pid, primary)
	}
	if err := rows.Err(); err != nil {
		return entityLibraries{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// EntityNames returns the name the catalog keeps for each artist, or each album's title,
// by pid, without the counts EntityByPIDs gathers; an unknown pid is left out.
func (s *Store) EntityNames(ctx context.Context, kind read.EntityKind, pids []model.PID) (map[model.PID]string, error) {
	const op = "store.EntityNames"
	var stmt string
	switch kind {
	case read.EntityArtist:
		stmt = "SELECT pid, name FROM artist WHERE pid IN "
	case read.EntityAlbum:
		stmt = "SELECT pid, title FROM album WHERE pid IN "
	default:
		return nil, waxerr.New(waxerr.CodeInvalid, op, "names are read for artists and albums, not "+string(kind))
	}
	keys := make([]string, len(pids))
	for i, p := range pids {
		keys[i] = string(p)
	}
	byKey, err := s.namesByKey(ctx, op, stmt, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[model.PID]string, len(byKey))
	for k, name := range byKey {
		out[model.PID(k)] = name
	}
	return out, nil
}

// ArtistNames returns the name the catalog keeps for each artist it holds under the given
// match keys (identity.MatchKey), a key a merge folded answering with its survivor's.
func (s *Store) ArtistNames(ctx context.Context, matchKeys []string) (map[string]string, error) {
	return s.foldedNamesByKey(ctx, "store.ArtistNames", "artist", "name", matchKeys)
}

// AlbumTitles returns the title of each album the catalog holds under the given identity
// keys (identity.AlbumKey), a key a merge folded answering with its survivor's.
func (s *Store) AlbumTitles(ctx context.Context, keys []string) (map[string]string, error) {
	return s.foldedNamesByKey(ctx, "store.AlbumTitles", "album", "title", keys)
}

// foldedNamesByKey reads the name column of the table's rows under keys, and for a key no
// row holds, the name of the entity a fold of it names, as a scan of that key resolves it.
// table is a caller constant.
func (s *Store) foldedNamesByKey(ctx context.Context, op, table, name string, keys []string) (map[string]string, error) {
	out, err := s.namesByKey(ctx, op, "SELECT match_key, "+name+" FROM "+table+" WHERE match_key IN ", keys)
	if err != nil {
		return nil, err
	}
	var missed []string
	for _, k := range keys {
		if _, ok := out[k]; !ok {
			missed = append(missed, k)
		}
	}
	if len(missed) == 0 {
		return out, nil
	}
	folded, err := s.namesByKey(ctx, op, "SELECT f.key, e."+name+" FROM entity_fold f JOIN "+table+
		" e ON e.pid = f.entity_pid WHERE f.entity_type = '"+table+"' AND f.key IN ", missed)
	if err != nil {
		return nil, err
	}
	maps.Copy(out, folded)
	return out, nil
}

// FilePIDsByPath returns the pid of the file the catalog holds at each path it holds,
// keyed by the path, in bounded batches.
func (s *Store) FilePIDsByPath(ctx context.Context, paths [][]byte) (map[string]model.PID, error) {
	const op = "store.FilePIDsByPath"
	out := map[string]model.PID{}
	err := chunkSlice(paths, idBatchSize, func(chunk [][]byte) error {
		args := make([]any, len(chunk))
		for i, p := range chunk {
			args[i] = p
		}
		rows, err := s.rdb().QueryContext(ctx, "SELECT path, pid FROM file WHERE path IN "+placeholders(len(chunk)), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var path []byte
			var pid string
			if err := rows.Scan(&path, &pid); err != nil {
				return err
			}
			out[string(path)] = model.PID(pid)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// namesByKey runs stmt, a select of a key and a name ending in "IN ", over keys in bounded
// batches.
func (s *Store) namesByKey(ctx context.Context, op, stmt string, keys []string) (map[string]string, error) {
	seen := make(map[string]bool, len(keys))
	unique := keys[:0:0]
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			unique = append(unique, k)
		}
	}
	out := map[string]string{}
	err := chunkSlice(unique, idBatchSize, func(chunk []string) error {
		args := make([]any, len(chunk))
		for i, k := range chunk {
			args[i] = k
		}
		rows, err := s.rdb().QueryContext(ctx, stmt+placeholders(len(chunk)), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k, name string
			if err := rows.Scan(&k, &name); err != nil {
				return err
			}
			out[k] = name
		}
		return rows.Err()
	})
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// EntityByPIDs is the batched form of EntityByPID: it returns summary info for many
// entities of a single kind, keyed by pid. It retires the one-EntityByPID-per-hit
// cost a consumer pays hydrating a page of entity pids, such as a restricted-user
// entity search that scopes each hit to the user's libraries through
// EntityInfo.LibraryPIDs. An unknown or repeated pid is dropped (a repeat collapses
// to one entry). The caller batches within a single kind. Like ItemsByPIDs it reads
// at most idBatchSize pids a statement, so a larger set spans several statements and
// is NOT an atomic snapshot. It runs the same three reads EntityByPID
// does, each batched: the base identity/rollup row, the live album/series aggregate,
// and the member libraries. Field-for-field it matches EntityByPID (an
// entities-by-pids parity test pins that).
func (s *Store) EntityByPIDs(ctx context.Context, kind read.EntityKind, pids []model.PID) (map[model.PID]*read.EntityInfo, error) {
	const op = "store.EntityByPIDs"
	if !kind.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown entity kind: "+string(kind))
	}
	if len(pids) == 0 {
		return nil, nil
	}
	unique := uniquePIDs(pids)
	out := make(map[model.PID]*read.EntityInfo, len(unique))
	err := chunkSlice(unique, idBatchSize, func(chunk []model.PID) error {
		byID, ids, err := s.entityBaseBatch(ctx, kind, chunk, out, op)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		// Album and series carry no rollup rows, so their counts aggregate live,
		// keyed by entity id (the other kinds read counts from the base rollup query).
		if kind == read.EntityAlbum || kind == read.EntitySeries {
			counts, err := s.entityLiveCountsBatch(ctx, kind, ids, op)
			if err != nil {
				return err
			}
			for id, c := range counts {
				if info := byID[id]; info != nil {
					info.ItemCount, info.TotalDurationMS = c.items, c.duration
				}
			}
		}
		libs, err := s.entityLibraryPIDsBatch(ctx, kind, ids, op)
		if err != nil {
			return err
		}
		for id, e := range libs {
			if info := byID[id]; info != nil {
				info.LibraryPIDs, info.PrimaryLibraryPIDs = e.all, e.primary
			}
		}
		if kind == read.EntityArtist {
			aliases, err := s.artistAliases(ctx, ids, op)
			if err != nil {
				return err
			}
			for id, names := range aliases {
				if info := byID[id]; info != nil {
					info.Aliases = names
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// entityBaseBatch runs the per-kind base identity/rollup query for one chunk of
// pids, keying each found entity into out by pid and returning an id->info map plus
// the found ids (both driving the live-count and library follow-ups). It mirrors
// EntityByPID's base switch column for column, adding the entity's own pid so the
// result can be keyed without a second lookup.
func (s *Store) entityBaseBatch(ctx context.Context, kind read.EntityKind, chunk []model.PID, out map[model.PID]*read.EntityInfo, op string) (map[int64]*read.EntityInfo, []int64, error) {
	var stmt string
	var scan func(*sql.Rows) (int64, *read.EntityInfo, error)
	switch kind {
	case read.EntityArtist:
		stmt = `SELECT a.id, a.pid, a.name, a.sort_key, COALESCE(a.mbid,''),
			COALESCE(ar.track_count,0), COALESCE(ar.release_group_count,0), COALESCE(ar.total_duration_ms,0)
			FROM artist a LEFT JOIN artist_rollup ar ON ar.artist_id = a.id
			WHERE a.pid IN ` + inArray
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey, &info.MBID,
				&info.ItemCount, &info.ReleaseGroupCount, &info.TotalDurationMS)
			return id, info, err
		}
	case read.EntityReleaseGroup:
		stmt = `SELECT rg.id, rg.pid, rg.title, rg.sort_key, COALESCE(rg.mbid,''),
			COALESCE(rg.type,''), COALESCE(pa.pid,''),
			COALESCE(rr.track_count,0), COALESCE(rr.total_duration_ms,0)
			FROM release_group rg
			LEFT JOIN artist pa ON pa.id = rg.primary_artist_id
			LEFT JOIN release_group_rollup rr ON rr.release_group_id = rg.id
			WHERE rg.pid IN ` + inArray
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			var artistPID string
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey, &info.MBID,
				&info.Type, &artistPID, &info.ItemCount, &info.TotalDurationMS)
			info.ArtistPID = model.PID(artistPID)
			return id, info, err
		}
	case read.EntityAlbum:
		stmt = `SELECT al.id, al.pid, al.title, al.sort_key, COALESCE(al.mbid,''),
			COALESCE(al.year,0), COALESCE(rg.pid,''),
			COALESCE(al.barcode,''), COALESCE(al.label,''), COALESCE(al.catalog_number,''),
			COALESCE(al.media,''), COALESCE(al.country,'')
			FROM album al LEFT JOIN release_group rg ON rg.id = al.release_group_id
			WHERE al.pid IN ` + inArray
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			var rgPID string
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey, &info.MBID, &info.Year, &rgPID,
				&info.Barcode, &info.Label, &info.CatalogNumber, &info.Media, &info.Country)
			info.ReleaseGroupPID = model.PID(rgPID)
			return id, info, err
		}
	case read.EntityGenre:
		stmt = `SELECT g.id, g.pid, g.name, g.sort_key,
			COALESCE(gr.track_count,0), COALESCE(gr.total_duration_ms,0)
			FROM genre g LEFT JOIN genre_rollup gr ON gr.genre_id = g.id
			WHERE g.pid IN ` + inArray
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey, &info.ItemCount, &info.TotalDurationMS)
			return id, info, err
		}
	case read.EntitySeries:
		stmt = `SELECT srs.id, srs.pid, srs.name, srs.sort_key
			FROM series srs WHERE srs.pid IN ` + inArray
		scan = func(rows *sql.Rows) (int64, *read.EntityInfo, error) {
			var id int64
			info := &read.EntityInfo{}
			err := rows.Scan(&id, &info.PID, &info.Name, &info.SortKey)
			return id, info, err
		}
	}

	rows, err := s.rstmts.queryContext(ctx, s.rdb(), stmt, pidArray(chunk))
	if err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	byID := make(map[int64]*read.EntityInfo, len(chunk))
	var ids []int64
	for rows.Next() {
		id, info, err := scan(rows)
		if err != nil {
			return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		info.Kind = kind
		out[info.PID] = info
		byID[id] = info
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return byID, ids, nil
}

// entityCounts is one entity's live-aggregated membership: item count and total
// effective duration, for the album/series kinds that have no rollup rows.
type entityCounts struct {
	items    int
	duration int64
}

// entityLiveCountsBatch aggregates the album/series member counts live for a chunk
// of entity ids, keyed by entity id, reusing the effective-duration expression so a
// virtual track prices its window exactly as EntityByPID and the rollups do. An
// entity with no members is simply absent (its info keeps a zero count).
func (s *Store) entityLiveCountsBatch(ctx context.Context, kind read.EntityKind, ids []int64, op string) (map[int64]entityCounts, error) {
	var stmt string
	switch kind {
	case read.EntityAlbum:
		stmt = `SELECT t.album_id, COUNT(DISTINCT t.item_id), COALESCE(SUM(` + itemEffectiveDurationExpr + `),0)
			FROM track t ` + liveMembers + `
			LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
			LEFT JOIN file f ON f.id = pf.file_id
			WHERE t.album_id IN ` + inArray + ` GROUP BY t.album_id`
	case read.EntitySeries:
		stmt = `SELECT bk.series_id, COUNT(*), COALESCE(SUM(bk.total_duration_ms),0)
			FROM book bk ` + liveBooks + ` WHERE bk.series_id IN ` + inArray + ` GROUP BY bk.series_id`
	default:
		return nil, nil
	}
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), stmt, idArray(ids))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	out := make(map[int64]entityCounts, len(ids))
	for rows.Next() {
		var id int64
		var c entityCounts
		if err := rows.Scan(&id, &c.items, &c.duration); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// entityLibraryPIDsBatch is the batched entityLibraryPIDs: the libraries holding each
// entity's member files, keyed by entity id, in library order per entity. It shares
// entityMemberSource with the single lookup, projecting and grouping by the member's
// entity id so one query serves the whole chunk.
func (s *Store) entityLibraryPIDsBatch(ctx context.Context, kind read.EntityKind, ids []int64, op string) (map[int64]*entityLibraries, error) {
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), entityLibraryPIDsBatchStmt(kind), idArray(ids))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	out := make(map[int64]*entityLibraries, len(ids))
	for rows.Next() {
		var eid int64
		var pid string
		var primary bool
		if err := rows.Scan(&eid, &pid, &primary); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		e := out[eid]
		if e == nil {
			e = &entityLibraries{}
			out[eid] = e
		}
		e.add(pid, primary)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}
