package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// The art walk answers which stored picture an entity's art request lands on. It is the
// one definition of the fallback chain, read by a single resolve and by a batch alike:
//
//	track          own cover -> album -> release group -> artist -> genre
//	album          own row -> a member track's cover -> release group -> its primary artist
//	release group  own row -> its primary artist
//	episode        own cover -> podcast
//	artist, genre, podcast, playlist, and a book in the track slot: own row alone
//
// A track's artist rung is its album's release group's primary artist, else its album
// artist, else its artist, and its genre rung is its lowest-id genre. Only the front
// role climbs; every other role answers at the requested entity or not at all.
//
// It runs a rung at a time over the whole set rather than a chain per entity, so the
// members of one album cost that album's rungs once, and an entity answered at its own
// rung costs the one statement that found it.

// artHit is the art_map row a walk landed on: the source to serve, the level that
// answered, whether an album's answer came from a member track, and the row's provenance.
type artHit struct {
	hash      string
	level     model.ArtEntity
	derived   bool
	source    model.ProvenanceSource
	provider  string
	sourceURL string
	updatedAt int64
}

// artFront is one entity's answer. An entity the walk knows and found nothing for has ok
// false; one it does not know has no entry at all.
type artFront struct {
	hit artHit
	ok  bool
}

// artHitCols reads an art_map row, aliased am, in hitScan's order.
const artHitCols = `am.source_hash, am.source, am.provider, am.source_url, am.updated_at`

// hitScan holds artHitCols during a scan. A left join that found no row leaves them NULL.
type hitScan struct {
	hash, source, provider, url sql.NullString
	updated                     sql.NullInt64
}

func (h *hitScan) dests() []any {
	return []any{&h.hash, &h.source, &h.provider, &h.url, &h.updated}
}

func (h *hitScan) hit(level model.ArtEntity, derived bool) (artHit, bool) {
	if !h.hash.Valid {
		return artHit{}, false
	}
	return artHit{
		hash: h.hash.String, level: level, derived: derived,
		source: model.ProvenanceSource(h.source.String), provider: h.provider.String,
		sourceURL: h.url.String, updatedAt: h.updated.Int64,
	}, true
}

// albumMemberFronts selects from the front covers of an album's member tracks, the rows
// its derived rung picks among. The album id follows it.
//
// The join order is pinned. With no statistics the planner rates entity_type = 'track'
// as selective as an album's member list and, left to choose, starts from it: every
// track cover in the catalog, each checked against the album.
const albumMemberFronts = ` FROM track t CROSS JOIN art_map tm
		ON tm.entity_type = 'track' AND tm.entity_id = t.item_id AND tm.role = 'front'
	WHERE t.album_id = `

const (
	// The track slot holds a track's cover and a book's. An episode's lives in its own
	// slot (artEntityIDTx is the write side of the same rule), so it is no entity here.
	artItemEntry = `SELECT pi.pid, pi.id, pi.kind, t.album_id, t.album_artist_id, t.artist_id, ` + artHitCols + `
		FROM playable_item pi LEFT JOIN track t ON t.item_id = pi.id
		LEFT JOIN art_map am ON am.entity_type = 'track' AND am.entity_id = pi.id AND am.role = ?
		WHERE pi.kind <> 'episode' AND pi.pid IN ` + inArray

	artEpisodeEntry = `SELECT pi.pid, ep.podcast_id, ` + artHitCols + `
		FROM playable_item pi JOIN episode ep ON ep.item_id = pi.id
		LEFT JOIN art_map am ON am.entity_type = 'episode' AND am.entity_id = pi.id AND am.role = ?
		WHERE pi.pid IN ` + inArray

	// The album's own row with the parents the rungs above it need, by pid or by id.
	artAlbumNodes = `SELECT al.pid, al.id, al.release_group_id, rg.primary_artist_id, ` + artHitCols + `
		FROM album al LEFT JOIN release_group rg ON rg.id = al.release_group_id
		LEFT JOIN art_map am ON am.entity_type = 'album' AND am.entity_id = al.id AND am.role = ?
		WHERE `

	artGroupEntry = `SELECT rg.pid, rg.primary_artist_id, ` + artHitCols + `
		FROM release_group rg
		LEFT JOIN art_map am ON am.entity_type = 'release_group' AND am.entity_id = rg.id AND am.role = ?
		WHERE rg.pid IN ` + inArray

	artOwnFronts = `SELECT am.entity_id, ` + artHitCols + ` FROM art_map am
		WHERE am.entity_type = ? AND am.role = 'front' AND am.entity_id IN ` + inArray

	// The cover of the album's earliest-cataloged member that has one. The members come
	// off their index in that order, so the read stops at the first cover, and the pick
	// stays on a member through a change of its cover.
	artDerivedFronts = `SELECT a.value, ` + artHitCols + ` FROM json_each(?) a
		JOIN art_map am ON am.rowid = (SELECT tm.rowid` + albumMemberFronts + `a.value ORDER BY t.item_id LIMIT 1)`

	artGenreFronts = `SELECT g.item_id, ` + artHitCols + ` FROM (
			SELECT ig.item_id AS item_id, MIN(ig.genre_id) AS genre_id FROM item_genre ig
			WHERE ig.item_id IN ` + inArray + ` GROUP BY ig.item_id) g
		JOIN art_map am ON am.entity_type = 'genre' AND am.entity_id = g.genre_id AND am.role = 'front'`
)

// artTable names the table an art entity's pid is looked up in.
func artTable(typ model.ArtEntity) string {
	if typ == model.ArtTrack || typ == model.ArtEpisode {
		return "playable_item"
	}
	return string(typ)
}

// artWalk resolves art in one role for entities of one type, at most idBatchSize of
// them. The answer holds an entry for every pid that names an entity of the type.
func (s *Store) artWalk(ctx context.Context, typ model.ArtEntity, role model.ArtRole, pids []model.PID, op string) (map[model.PID]artFront, error) {
	w := artWalker{s: s, ctx: ctx, op: op, role: role, out: make(map[model.PID]artFront, len(pids))}
	var err error
	switch typ {
	case model.ArtTrack:
		err = w.items(pids)
	case model.ArtEpisode:
		err = w.episodes(pids)
	case model.ArtAlbum:
		err = w.albums(pids)
	case model.ArtReleaseGroup:
		err = w.groups(pids)
	case model.ArtArtist, model.ArtGenre, model.ArtPodcast, model.ArtPlaylist:
		err = w.own(typ, pids)
	default:
		err = waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(typ))
	}
	if err != nil {
		return nil, err
	}
	return w.out, nil
}

type artWalker struct {
	s    *Store
	ctx  context.Context
	op   string
	role model.ArtRole
	out  map[model.PID]artFront
}

// climbs reports whether the walk goes past the requested entity.
func (w *artWalker) climbs() bool { return w.role == model.ArtRoleFront }

func (w *artWalker) query(stmt string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := w.s.rstmts.queryContext(w.ctx, w.s.rdb(), stmt, args...)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, w.op, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, w.op, err)
		}
	}
	if err := rows.Err(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, w.op, err)
	}
	return nil
}

// ownFronts reads the front rows entities of one type hold themselves, by id.
func (w *artWalker) ownFronts(typ model.ArtEntity, ids []int64) (map[int64]artHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	hits := make(map[int64]artHit, len(ids))
	err := w.query(artOwnFronts, func(rows *sql.Rows) error {
		var id int64
		var h hitScan
		if err := rows.Scan(append([]any{&id}, h.dests()...)...); err != nil {
			return err
		}
		hits[id], _ = h.hit(typ, false)
		return nil
	}, string(typ), idArray(ids))
	return hits, err
}

func (w *artWalker) own(typ model.ArtEntity, pids []model.PID) error {
	stmt := `SELECT e.pid, ` + artHitCols + ` FROM ` + artTable(typ) + ` e
		LEFT JOIN art_map am ON am.entity_type = ? AND am.entity_id = e.id AND am.role = ?
		WHERE e.pid IN ` + inArray
	return w.query(stmt, func(rows *sql.Rows) error {
		var pid model.PID
		var h hitScan
		if err := rows.Scan(append([]any{&pid}, h.dests()...)...); err != nil {
			return err
		}
		hit, ok := h.hit(typ, false)
		w.out[pid] = artFront{hit, ok}
		return nil
	}, string(typ), string(w.role), pidArray(pids))
}

// albumNode is an album partway up its chain: its own answer once one is found, and the
// release group and that group's primary artist, each 0 when absent.
type albumNode struct {
	pid           model.PID
	id            int64
	group, artist int64
	artFront
}

func (w *artWalker) albumNodes(where string, role model.ArtRole, list string) ([]*albumNode, error) {
	var nodes []*albumNode
	err := w.query(artAlbumNodes+where+inArray, func(rows *sql.Rows) error {
		n := &albumNode{}
		var group, artist sql.NullInt64
		var h hitScan
		if err := rows.Scan(append([]any{&n.pid, &n.id, &group, &artist}, h.dests()...)...); err != nil {
			return err
		}
		n.group, n.artist = group.Int64, artist.Int64
		n.hit, n.ok = h.hit(model.ArtAlbum, false)
		nodes = append(nodes, n)
		return nil
	}, string(role), list)
	return nodes, err
}

// climbAlbums answers the albums their own row left open from a member track's cover,
// then from their release group's row. The artist rung is the caller's, since a track
// and an album reach different artists.
func (w *artWalker) climbAlbums(nodes []*albumNode) error {
	var open []int64
	byID := make(map[int64]*albumNode, len(nodes))
	for _, n := range nodes {
		byID[n.id] = n
		if !n.ok {
			open = append(open, n.id)
		}
	}
	if len(open) == 0 {
		return nil
	}
	if err := w.query(artDerivedFronts, func(rows *sql.Rows) error {
		var id int64
		var h hitScan
		if err := rows.Scan(append([]any{&id}, h.dests()...)...); err != nil {
			return err
		}
		n := byID[id]
		n.hit, n.ok = h.hit(model.ArtAlbum, true)
		return nil
	}, idArray(open)); err != nil {
		return err
	}
	var groups []int64
	seen := map[int64]bool{}
	for _, n := range nodes {
		if !n.ok && n.group != 0 && !seen[n.group] {
			seen[n.group] = true
			groups = append(groups, n.group)
		}
	}
	hits, err := w.ownFronts(model.ArtReleaseGroup, groups)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if hit, found := hits[n.group]; found && !n.ok {
			n.hit, n.ok = hit, true
		}
	}
	return nil
}

// byArtist answers what is still open from an artist's own row. artistOf names each open
// pid's artist rung.
func (w *artWalker) byArtist(artistOf map[model.PID]int64) error {
	if len(artistOf) == 0 {
		return nil
	}
	var ids []int64
	seen := map[int64]bool{}
	for _, id := range artistOf {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	hits, err := w.ownFronts(model.ArtArtist, ids)
	if err != nil {
		return err
	}
	for pid, id := range artistOf {
		if hit, ok := hits[id]; ok {
			w.out[pid] = artFront{hit, true}
		}
	}
	return nil
}

func (w *artWalker) albums(pids []model.PID) error {
	nodes, err := w.albumNodes("al.pid IN ", w.role, pidArray(pids))
	if err != nil {
		return err
	}
	if w.climbs() {
		if err := w.climbAlbums(nodes); err != nil {
			return err
		}
	}
	artistOf := map[model.PID]int64{}
	for _, n := range nodes {
		w.out[n.pid] = n.artFront
		if !n.ok && n.artist != 0 && w.climbs() {
			artistOf[n.pid] = n.artist
		}
	}
	return w.byArtist(artistOf)
}

func (w *artWalker) groups(pids []model.PID) error {
	artistOf := map[model.PID]int64{}
	err := w.query(artGroupEntry, func(rows *sql.Rows) error {
		var pid model.PID
		var artist sql.NullInt64
		var h hitScan
		if err := rows.Scan(append([]any{&pid, &artist}, h.dests()...)...); err != nil {
			return err
		}
		hit, ok := h.hit(model.ArtReleaseGroup, false)
		w.out[pid] = artFront{hit, ok}
		if !ok && artist.Valid && w.climbs() {
			artistOf[pid] = artist.Int64
		}
		return nil
	}, string(w.role), pidArray(pids))
	if err != nil {
		return err
	}
	return w.byArtist(artistOf)
}

func (w *artWalker) episodes(pids []model.PID) error {
	podcastOf := map[model.PID]int64{}
	var podcasts []int64
	seen := map[int64]bool{}
	err := w.query(artEpisodeEntry, func(rows *sql.Rows) error {
		var pid model.PID
		var podcast int64
		var h hitScan
		if err := rows.Scan(append([]any{&pid, &podcast}, h.dests()...)...); err != nil {
			return err
		}
		hit, ok := h.hit(model.ArtEpisode, false)
		w.out[pid] = artFront{hit, ok}
		if !ok && w.climbs() {
			podcastOf[pid] = podcast
			if !seen[podcast] {
				seen[podcast] = true
				podcasts = append(podcasts, podcast)
			}
		}
		return nil
	}, string(w.role), pidArray(pids))
	if err != nil {
		return err
	}
	hits, err := w.ownFronts(model.ArtPodcast, podcasts)
	if err != nil {
		return err
	}
	for pid, id := range podcastOf {
		if hit, ok := hits[id]; ok {
			w.out[pid] = artFront{hit, true}
		}
	}
	return nil
}

// items walks the track slot, which holds a track's cover and a book's. A book stops at
// its own cover: an author's picture is a portrait and a genre's a category tile, neither
// of them a jacket, so a coverless book answers nothing and a host draws its placeholder.
func (w *artWalker) items(pids []model.PID) error {
	type item struct {
		pid                        model.PID
		id                         int64
		album, albumArtist, artist int64
	}
	var open []item
	err := w.query(artItemEntry, func(rows *sql.Rows) error {
		var it item
		var kind string
		var album, albumArtist, artist sql.NullInt64
		var h hitScan
		if err := rows.Scan(append([]any{&it.pid, &it.id, &kind, &album, &albumArtist, &artist}, h.dests()...)...); err != nil {
			return err
		}
		hit, ok := h.hit(model.ArtTrack, false)
		w.out[it.pid] = artFront{hit, ok}
		if !ok && w.climbs() && kind != string(model.KindBook) {
			it.album, it.albumArtist, it.artist = album.Int64, albumArtist.Int64, artist.Int64
			open = append(open, it)
		}
		return nil
	}, string(w.role), pidArray(pids))
	if err != nil || len(open) == 0 {
		return err
	}

	var albumIDs []int64
	seen := map[int64]bool{}
	for _, it := range open {
		if it.album != 0 && !seen[it.album] {
			seen[it.album] = true
			albumIDs = append(albumIDs, it.album)
		}
	}
	albums := map[int64]*albumNode{}
	if len(albumIDs) > 0 {
		nodes, err := w.albumNodes("al.id IN ", model.ArtRoleFront, idArray(albumIDs))
		if err != nil {
			return err
		}
		if err := w.climbAlbums(nodes); err != nil {
			return err
		}
		for _, n := range nodes {
			albums[n.id] = n
		}
	}

	artistOf := map[model.PID]int64{}
	idOf := make(map[model.PID]int64, len(open))
	for _, it := range open {
		idOf[it.pid] = it.id
		artist := int64(0)
		if n := albums[it.album]; n != nil {
			if n.ok {
				w.out[it.pid] = n.artFront
				continue
			}
			artist = n.artist
		}
		if artist == 0 {
			artist = it.albumArtist
		}
		if artist == 0 {
			artist = it.artist
		}
		if artist != 0 {
			artistOf[it.pid] = artist
		}
	}
	if err := w.byArtist(artistOf); err != nil {
		return err
	}

	var genreless []int64
	pidOf := map[int64]model.PID{}
	for _, it := range open {
		if !w.out[it.pid].ok {
			genreless = append(genreless, it.id)
			pidOf[it.id] = it.pid
		}
	}
	if len(genreless) == 0 {
		return nil
	}
	return w.query(artGenreFronts, func(rows *sql.Rows) error {
		var id int64
		var h hitScan
		if err := rows.Scan(append([]any{&id}, h.dests()...)...); err != nil {
			return err
		}
		hit, ok := h.hit(model.ArtGenre, false)
		w.out[pidOf[id]] = artFront{hit, ok}
		return nil
	}, idArray(genreless))
}

// frontHashes answers the front cover hash of any number of entities of one type, by
// pid. It is the one place a batch of pids is chunked, walked and read for its hashes,
// behind ArtHashes and every read shape that carries an ArtHash. An entity that resolves
// no cover, or a pid that names none, has no entry.
func (s *Store) frontHashes(ctx context.Context, typ model.ArtEntity, pids []model.PID, op string) (map[model.PID]string, error) {
	out := make(map[model.PID]string, len(pids))
	err := chunkSlice(uniquePIDs(pids), idBatchSize, func(chunk []model.PID) error {
		fronts, err := s.artWalk(ctx, typ, model.ArtRoleFront, chunk, op)
		if err != nil {
			return err
		}
		for pid, f := range fronts {
			if f.ok {
				out[pid] = f.hit.hash
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ArtHashes answers the content hash of the front cover each reference resolves to, the
// SourceHash a ResolveArt of it would carry, walking the fallback chain as ResolveArt
// does and loading no picture. A reference that resolves no cover, or names no entity,
// has no entry. An unknown entity type is CodeInvalid.
//
// The walk runs a statement per rung, and a large set in chunks, so an answer is not one
// snapshot of the catalog, as a ResolveArt never was.
func (s *Store) ArtHashes(ctx context.Context, refs []model.EntityRef) (map[model.EntityRef]string, error) {
	const op = "store.ArtHashes"
	byType := map[model.ArtEntity][]model.PID{}
	for _, ref := range refs {
		if !ref.Type.Valid() {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(ref.Type))
		}
		byType[ref.Type] = append(byType[ref.Type], ref.PID)
	}
	out := make(map[model.EntityRef]string, len(refs))
	for typ, pids := range byType {
		hashes, err := s.frontHashes(ctx, typ, pids, op)
		if err != nil {
			return nil, err
		}
		for pid, hash := range hashes {
			out[model.EntityRef{Type: typ, PID: pid}] = hash
		}
	}
	return out, nil
}
