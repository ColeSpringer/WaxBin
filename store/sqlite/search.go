package sqlite

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
	"golang.org/x/text/unicode/norm"
)

// BM25 column weights for search_fts (kind, title, subtitle, artist, album,
// extra, credits). A larger weight makes a hit in that column dominate the score, so
// a title match outranks an artist/album match, which outranks a credit, which
// outranks the genre/extra field. kind is unindexed and carries no weight.
const searchBM25 = "bm25(search_fts, 0.0, 10.0, 4.0, 5.0, 5.0, 1.0, 2.0)"

// The search reads up to a bounded number of ranked metadata rows to fill its
// groups, scaled to the requested per-group limit (many tracks share an album, so
// the cap is well above the limit) and clamped so it stays bounded on a large
// catalog. Hitting the cap sets SearchResult.Truncated.
const (
	searchFetchPerLimit = 25
	searchFetchMin      = 500
	searchFetchMax      = 5000
)

// searchFetchCap returns the ranked-row scan cap for a per-group limit.
func searchFetchCap(limit int) int {
	cap := limit * searchFetchPerLimit
	if cap < searchFetchMin {
		cap = searchFetchMin
	}
	if cap > searchFetchMax {
		cap = searchFetchMax
	}
	return cap
}

// searchDisplayCols and searchDisplayJoins render one matched item row (its own
// fields plus the artist/album entities a track hit fans out to, by their own names).
// They are shared by the flat statement, the candidate-capped wrap and the sort-key
// lookup so the row shapes can never drift.
const searchDisplayCols = `pi.pid, pi.kind, pi.title,
		COALESCE(NULLIF(t.artist,''), bk.author, pod.title, ''), COALESCE(t.album_artist,''),
		COALESCE(art.pid,''), COALESCE(art.name,''), COALESCE(al.pid,''), COALESCE(al.title,'')`

const searchDisplayJoins = `
		LEFT JOIN track t ON t.item_id = pi.id
		LEFT JOIN book bk ON bk.item_id = pi.id
		LEFT JOIN episode ep ON ep.item_id = pi.id
		LEFT JOIN podcast pod ON pod.id = ep.podcast_id
		LEFT JOIN artist art ON art.id = t.artist_id
		LEFT JOIN album al ON al.id = t.album_id`

// searchNarrow returns the fragment narrowing matched items (the pi alias) to the
// given states and libraries, plus its bind args. It is spliced directly after a
// caller's own `JOIN playable_item pi ON ...` line, because the two rungs join pi
// on different columns and disagree about whether the join is wanted when nothing
// narrows at all. For libIDs=[7] and states=[present remote] it returns:
//
//	` AND pi.state IN (?,?) AND pi.id IN (SELECT spf.item_id FROM file sf
//		JOIN item_file spf ON spf.file_id = sf.id WHERE sf.library_id IN (?))`
//
// Both narrowings ride the pi join's ON clause. pi is INNER-joined in both rungs, so ON
// and WHERE are equivalent here, and keeping them in one contiguous fragment is what
// makes the bind order fall out of fragment order.
//
// An item counts in a library when any of its files lives there, its alternates'
// included, so a host scoping by library finds it wherever it can serve a file. One with
// no file at all (an undownloaded episode) drops out of a scoped search. The membership
// subquery seeks file_library once. Either half being empty contributes nothing.
func searchNarrow(libIDs []int64, states []model.ItemState) (string, []any) {
	if len(libIDs) == 0 && len(states) == 0 {
		return "", nil
	}
	// Exact capacity so every caller's append reallocates rather than writing into
	// a shared backing array.
	args := make([]any, 0, len(states)+len(libIDs))
	frag := ""
	if len(states) > 0 { // placeholders(0) is "()", and IN () is a syntax error
		frag += ` AND pi.state IN ` + placeholders(len(states))
		for _, st := range states {
			args = append(args, string(st))
		}
	}
	if len(libIDs) > 0 {
		frag += ` AND pi.id IN (SELECT spf.item_id FROM file sf
		JOIN item_file spf ON spf.file_id = sf.id WHERE sf.library_id IN ` + placeholders(len(libIDs)) + `)`
		for _, id := range libIDs {
			args = append(args, id)
		}
	}
	return frag, args
}

// checkItemStates rejects an unknown item state, failing closed on a typo the way
// an unknown library pid does: an unknown state matches no rows, so without this a
// misspelled state reads as an empty catalog. Repeats are left alone; unlike a
// library pid, whose arity decides which query form the compiler picks, a repeated
// value in `pi.state IN (?,?)` is inert beyond one extra bind slot.
func checkItemStates(states []model.ItemState, op string) error {
	for _, st := range states {
		if !st.Valid() {
			return waxerr.New(waxerr.CodeInvalid, op,
				"unknown item state "+string(st)+"; valid: "+model.ItemStateList())
		}
	}
	return nil
}

// searchStmt builds the grouped-search statement for one option set, returning
// the statement, its bind args in clause order, and the ranked-row scan cap the
// caller's loop enforces (one row past it sets Truncated).
//
// With no candidate cap the statement is the flat FTS query, which with an empty
// scope too is the option-free statement TestSearchStmtZeroPathGolden pins. With
// maxCandidates > 0 the match is wrapped: the inner query walks it in rowid-DESC
// order and keeps the newest maxCandidates rows (+1 so pool exhaustion is
// observable), and only that pool is ranked. ORDER BY rowid DESC is deliberate:
// FTS5 optimizes rowid-order scans with the same early termination (bm25 is still
// computed only per emitted row), and search_fts rowids are playable_item ids,
// roughly insertion order, so a truncated pool is recency-biased instead of
// systematically dropping recently-added content (an unordered LIMIT would emit
// rowid ASC, oldest first). The tradeoff is documented on read.SearchOptions: candidates are the
// newest N matches, not the best-ranked N. The one extra fetched row competes in
// the ranking too, so at the truncation margin the (N+1)th-newest match can take
// a slot on rank; the result is flagged Truncated either way, and excluding that
// row exactly would cost a second pass over the pool for a one-row nicety. The
// narrowing sits inside the wrap, so a match the caller excluded by library or by
// state never consumes the pool.
func searchStmt(match string, limit, maxCandidates int, libIDs []int64, states []model.ItemState) (string, []any, int) {
	scanCap := searchFetchCap(limit)
	if maxCandidates > 0 && maxCandidates < scanCap {
		scanCap = maxCandidates
	}
	narrow, narrowArgs := searchNarrow(libIDs, states)

	if maxCandidates <= 0 {
		stmt := `SELECT ` + searchDisplayCols + `, ` + searchBM25 + ` AS score
		FROM search_fts
		JOIN playable_item pi ON pi.id = search_fts.rowid` + narrow + searchDisplayJoins + `
		WHERE search_fts MATCH ?
		ORDER BY score, pi.pid
		LIMIT ?`
		return stmt, append(narrowArgs, match, scanCap+1), scanCap
	}

	// Guarded rather than joined unconditionally: with nothing to narrow, the inner
	// query is a pure FTS scan, and a rowid join per candidate row would be wasted
	// work on the common capped path. Guarded on the fragment rather than on the
	// inputs that produced it, so adding a narrowing dimension cannot leave the
	// statement and its binds disagreeing about whether pi is joined.
	innerNarrow := ""
	if narrow != "" {
		innerNarrow = `
			JOIN playable_item pi ON pi.id = search_fts.rowid` + narrow
	}
	stmt := `SELECT ` + searchDisplayCols + `, c.score AS score
		FROM (SELECT search_fts.rowid AS rid, ` + searchBM25 + ` AS score
			FROM search_fts` + innerNarrow + `
			WHERE search_fts MATCH ?
			ORDER BY search_fts.rowid DESC
			LIMIT ?) c
		JOIN playable_item pi ON pi.id = c.rid` + searchDisplayJoins + `
		ORDER BY score, pi.pid
		LIMIT ?`
	return stmt, append(narrowArgs, match, maxCandidates+1, scanCap+1), scanCap
}

// Search runs a grouped, BM25-ranked metadata search. It queries the metadata FTS
// once with field weighting, then derives the groups from the ranked matches: a
// matched track contributes itself to Tracks and its album and artist entities to
// Albums and Artists, books and episodes form their own groups, and transcript-body
// hits are appended to Episodes after the metadata hits. The entity groups add the
// artists and albums whose own name is the query (searchEntitiesByName), which the
// ranked rows can miss when better-scoring items fill the pool, and are then ordered
// by how each name meets the query (searchTier), first-seen order breaking ties. A
// query of symbols alone, which has no word to match, is looked up by sort key
// instead (searchItemsBySortKey); an empty one returns an empty result, not an error.
// opt.MaxCandidates bounds the match pool, while opt.Libraries and opt.States narrow
// it and the entity lookups alike (see read.SearchOptions for all three contracts),
// and an unknown library or state is refused whatever the query holds.
func (s *Store) Search(ctx context.Context, queryStr string, opt read.SearchOptions) (*read.SearchResult, error) {
	const op = "store.Search"
	res := &read.SearchResult{Query: queryStr}
	limit := opt.Limit
	if limit <= 0 {
		limit = 20
	}
	libIDs, err := s.libraryIDsByPIDs(ctx, opt.Libraries, op)
	if err != nil {
		return nil, err
	}
	if err := checkItemStates(opt.States, op); err != nil {
		return nil, err
	}
	g := newSearchGroups(res, limit, queryStr)
	match := ftsMatchQuery(queryStr)
	if match == "" {
		if g.q.sort == "" {
			return res, nil
		}
		if err := s.searchItemsBySortKey(ctx, g.q.sort, limit, opt.MaxCandidates, libIDs, opt.States, g); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	} else if err := s.searchMatches(ctx, match, limit, opt.MaxCandidates, libIDs, opt.States, g); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := s.searchEntitiesByName(ctx, queryStr, limit, libIDs, opt.States, g); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	g.rankEntities()

	// Append transcript-body hits to the Episodes group, after the metadata hits, so
	// a title match outranks a body match. Episodes already surfaced by metadata are
	// skipped to avoid duplicates.
	if match != "" && len(res.Episodes) < limit {
		if err := s.searchTranscripts(ctx, match, limit, opt.MaxCandidates, libIDs, opt.States, g.episodeSeen, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// searchMatches feeds the ranked FTS rows to the groups. Both limits fetch one past
// their cap so a full result set signals truncation (a spent candidate pool or a
// spent ranked-row scan alike) rather than silently dropping matches. score is
// tie-broken by pid so equal-score rows come back in a stable, deterministic order.
func (s *Store) searchMatches(ctx context.Context, match string, limit, maxCandidates int, libIDs []int64, states []model.ItemState, g *searchGroups) error {
	stmt, args, cap := searchStmt(match, limit, maxCandidates, libIDs, states)
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	scanned := 0
	for rows.Next() {
		scanned++
		if scanned > cap {
			g.res.Truncated = true
			break
		}
		if err := g.scanItem(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// searchGroups fills a result's groups from matched rows in rank order. Artists and
// albums are gathered whole and cut to the limit only once rankEntities has ordered
// them. best is the best score of the rows read, which an entity found by its own name
// alone takes.
type searchGroups struct {
	res                                *read.SearchResult
	limit                              int
	q                                  nameKeys
	best                               float64
	artists, albums                    []read.SearchHit
	albumSeen, artistSeen, episodeSeen map[model.PID]bool
}

func newSearchGroups(res *read.SearchResult, limit int, query string) *searchGroups {
	return &searchGroups{res: res, limit: limit, q: newNameKeys(query, true),
		albumSeen: map[model.PID]bool{}, artistSeen: map[model.PID]bool{}, episodeSeen: map[model.PID]bool{}}
}

// scanItem reads one row of searchDisplayCols plus a score and files it.
func (g *searchGroups) scanItem(rows *sql.Rows) error {
	var pid, kind, title, artist, albumArtist, artistPID, artistName, albumPID, albumTitle string
	var score float64
	if err := rows.Scan(&pid, &kind, &title, &artist, &albumArtist, &artistPID, &artistName, &albumPID, &albumTitle, &score); err != nil {
		return err
	}
	g.best = min(g.best, score)
	res := g.res
	hit := read.SearchHit{PID: model.PID(pid), Kind: kind, Title: title, Subtitle: artist, Score: score}
	switch kind {
	// A book has no track/artist/album entities, so it forms its own group keyed
	// by title with its author as the subtitle, rather than the Tracks group.
	case string(model.KindBook):
		if len(res.Books) < g.limit {
			res.Books = append(res.Books, hit)
		}
		return nil
	// An episode forms the Episodes group, keyed by title with its podcast as the
	// subtitle. These are the title/metadata hits; transcript-body hits are appended
	// after, so a title match always outranks a body match.
	case string(model.KindEpisode):
		g.episodeSeen[hit.PID] = true
		if len(res.Episodes) < g.limit {
			res.Episodes = append(res.Episodes, hit)
		}
		return nil
	}
	if len(res.Tracks) < g.limit {
		res.Tracks = append(res.Tracks, hit)
	}
	if albumArtist == "" {
		albumArtist = artist
	}
	g.addAlbum(read.SearchHit{PID: model.PID(albumPID), Kind: "album", Title: albumTitle, Subtitle: albumArtist, Score: score})
	g.addArtist(read.SearchHit{PID: model.PID(artistPID), Kind: "artist", Title: artistName, Score: score})
	return nil
}

func (g *searchGroups) addAlbum(h read.SearchHit) {
	if h.PID != "" && !g.albumSeen[h.PID] {
		g.albumSeen[h.PID] = true
		g.albums = append(g.albums, h)
	}
}

func (g *searchGroups) addArtist(h read.SearchHit) {
	if h.PID != "" && !g.artistSeen[h.PID] {
		g.artistSeen[h.PID] = true
		g.artists = append(g.artists, h)
	}
}

// rankEntities orders the gathered artists and albums by tier, keeping first-seen
// order within one, marks the exact ones, and cuts each group to the limit.
func (g *searchGroups) rankEntities() {
	g.res.Artists = rankByTier(g.artists, g.q, g.limit)
	g.res.Albums = rankByTier(g.albums, g.q, g.limit)
}

func rankByTier(hits []read.SearchHit, q nameKeys, limit int) []read.SearchHit {
	type ranked struct {
		hit  read.SearchHit
		tier int
	}
	rs := make([]ranked, len(hits))
	for i, h := range hits {
		rs[i] = ranked{h, searchTier(newNameKeys(h.Title, q.key == ""), q)}
		rs[i].hit.Exact = rs[i].tier == 0
	}
	slices.SortStableFunc(rs, func(a, b ranked) int { return a.tier - b.tier })
	out := make([]read.SearchHit, 0, min(limit, len(rs)))
	for _, r := range rs[:min(limit, len(rs))] {
		out = append(out, r.hit)
	}
	return out
}

// nameKeys is a name or a query as the tiers compare it: its match key with kana
// folded and compatibility forms read plainly, that key with its spaces dropped, and
// its sort key, which is all a query of symbols alone has (so a name needs one only
// then).
type nameKeys struct{ key, joined, sort string }

func newNameKeys(s string, withSort bool) nameKeys {
	k := foldKana(identity.MatchKey(norm.NFKC.String(s)))
	nk := nameKeys{key: k, joined: strings.ReplaceAll(k, " ", "")}
	if withSort {
		nk.sort = model.SortKey(s)
	}
	return nk
}

// searchTier ranks an entity's own name against the query: 0 the name is the query
// (equal match keys, or equal once their spaces are dropped, so "jayz" is Jay-Z), 1
// the query begins a word of the name, 2 every query word is in the name, 3 the rest.
func searchTier(name, q nameKeys) int {
	switch {
	case q.key == "":
		if name.sort == q.sort {
			return 0
		}
		return 3
	case name.key == q.key || name.joined == q.joined:
		return 0
	case strings.Contains(" "+name.key, " "+q.key):
		return 1
	}
	for _, w := range strings.Fields(q.key) {
		if !strings.Contains(name.joined, w) {
			return 3
		}
	}
	return 2
}

// searchItemsBySortKey answers a query of symbols alone ("$", "÷", "!!!"), which
// tokenizes to nothing, by looking its sort key up among item titles and track artists,
// under the same narrowing. An artist named without a letter or digit has no entity of
// its own, so a track's artist display is read for it, whole or as one name of a
// comma-joined list, as well as its sort, which an artist sort tag can spell otherwise
// ("Chk Chk Chk" for "!!!"). Newest first, it reads one row past its cap as the ranked
// search does, so a full result sets Truncated, and a smaller candidate cap is the cap.
func (s *Store) searchItemsBySortKey(ctx context.Context, key string, limit, maxCandidates int, libIDs []int64, states []model.ItemState, g *searchGroups) error {
	narrow, narrowArgs := searchNarrow(libIDs, states)
	cap := searchFetchCap(limit)
	if maxCandidates > 0 && maxCandidates < cap {
		cap = maxCandidates
	}
	rows, err := s.read.QueryContext(ctx, itemsBySortKeyQ(narrow), append(append([]any{key}, narrowArgs...), cap+1)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for n := 1; rows.Next(); n++ {
		if n > cap {
			g.res.Truncated = true
			break
		}
		if err := g.scanItem(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// itemsBySortKeyQ is the statement searchItemsBySortKey runs, binding the key once as
// ?1. The lookups drive (CROSS JOIN keeps them outermost), so a library narrowing is a
// residual test rather than a walk of every item in the library. The display has no
// index, so that arm reads the track table, which only a query of symbols alone pays.
func itemsBySortKeyQ(narrow string) string {
	return `SELECT ` + searchDisplayCols + `, 0.0
		FROM (SELECT id FROM playable_item WHERE sort_key = ?1
			UNION SELECT item_id FROM track WHERE artist_sort = ?1
			UNION SELECT item_id FROM track WHERE instr(artist, ?1) > 0
				AND instr(', ' || artist || ', ', ', ' || ?1 || ', ') > 0) m
		CROSS JOIN playable_item pi ON pi.id = m.id` + narrow + searchDisplayJoins + `
		ORDER BY pi.id DESC LIMIT ?`
}

// searchEntitiesByName adds the artists and albums whose own name is the query to the
// gathered groups, under the same narrowing: an artist by its match key with the
// spaces dropped (artistLookupKeys), or by its sort key, and an album by its sort key
// (an album keeps no match key of its title). A query of symbols alone looks up albums
// only, since such a name has no match key and never becomes an artist; an album gets
// one when an MBID keys it. Every lookup is an equality on an indexed column.
func (s *Store) searchEntitiesByName(ctx context.Context, query string, limit int, libIDs []int64, states []model.ItemState, g *searchGroups) error {
	narrow, narrowArgs := searchNarrow(libIDs, states)
	if keys := artistLookupKeys(query); len(keys) > 0 {
		args := append(anySlice(keys), g.q.sort)
		artists, err := s.searchEntities(ctx, artistByNameQ(narrow, len(keys)),
			append(append(append(args, narrowArgs...), narrowArgs...), limit)...)
		if err != nil {
			return err
		}
		for _, h := range artists {
			h.Kind, h.Score = "artist", g.best
			g.addArtist(h)
		}
	}
	if g.q.sort == "" {
		return nil
	}
	albums, err := s.searchEntities(ctx, albumByNameQ(narrow),
		append(append([]any{g.q.sort}, narrowArgs...), limit)...)
	if err != nil {
		return err
	}
	for _, h := range albums {
		h.Kind, h.Score = "album", g.best
		g.addAlbum(h)
	}
	return nil
}

// artistLookupKeys returns the keys a stored artist's match key, its spaces dropped, is
// looked up by: the query's own, and the query's read past compatibility forms, each in
// both kana. A stored key keeps the name's own forms, so these are the spellings of a
// name the tiers read as the query (newNameKeys) that a lookup can reach.
func artistLookupKeys(query string) []string {
	var keys []string
	for _, s := range []string{query, norm.NFKC.String(query)} {
		k := strings.ReplaceAll(identity.MatchKey(s), " ", "")
		if k == "" {
			continue
		}
		for _, v := range []string{k, foldKana(k), strings.Map(model.KatakanaOf, k)} {
			if !slices.Contains(keys, v) {
				keys = append(keys, v)
			}
		}
	}
	return keys
}

// artistByNameQ looks artists up by match key with the spaces dropped (the
// artist_joined_key index), against keys bound values, which equal match keys also
// pass, and by sort key, keeping one credited on, or the album artist of, a track the
// narrowing keeps. narrow is spliced twice, so its args bind twice.
func artistByNameQ(narrow string, keys int) string {
	return `SELECT art.pid, art.name, '' FROM artist art
		WHERE (replace(art.match_key, ' ', '') IN ` + placeholders(keys) + ` OR art.sort_key = ?)
		AND (EXISTS (SELECT 1 FROM item_contributor ic JOIN track t ON t.item_id = ic.item_id
			JOIN playable_item pi ON pi.id = ic.item_id` + narrow + ` WHERE ic.artist_id = art.id)
		OR EXISTS (SELECT 1 FROM track t JOIN playable_item pi ON pi.id = t.item_id` + narrow + `
			WHERE t.album_artist_id = art.id))
		ORDER BY art.pid LIMIT ?`
}

// albumByNameQ looks albums up by sort key, keeping one that holds a track the
// narrowing keeps.
func albumByNameQ(narrow string) string {
	return `SELECT al.pid, al.title, ` + albumSubtitle + ` FROM album al
		WHERE al.sort_key = ? AND EXISTS (SELECT 1 FROM track t JOIN playable_item pi ON pi.id = t.item_id` + narrow + `
			WHERE t.album_id = al.id)
		ORDER BY al.pid LIMIT ?`
}

// albumSubtitle renders an album's artist for a hit found by the album itself rather
// than through one of its tracks, as a track hit renders it.
const albumSubtitle = `COALESCE((SELECT COALESCE(NULLIF(t.album_artist,''), t.artist) FROM track t
		WHERE t.album_id = al.id ORDER BY t.item_id LIMIT 1), '')`

// searchEntities reads (pid, title, subtitle) entity hits.
func (s *Store) searchEntities(ctx context.Context, q string, args ...any) ([]read.SearchHit, error) {
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []read.SearchHit
	for rows.Next() {
		var h read.SearchHit
		var pid string
		if err := rows.Scan(&pid, &h.Title, &h.Subtitle); err != nil {
			return nil, err
		}
		h.PID = model.PID(pid)
		out = append(out, h)
	}
	return out, rows.Err()
}

// searchTranscripts adds episodes whose stored transcript matches, ranked by the
// transcript FTS, skipping episodes already in the Episodes group. It over-fetches
// by the already-seen count so that when a term also matches many episode titles,
// the top transcript rows being already-seen does not starve transcript-only hits
// that have room in the group. It honors every search knob: a library scope keeps
// a transcript hit from leaking an episode whose file lives outside the scope
// (an undownloaded one included), a state narrowing does the same for an episode
// the caller excluded, and a candidate cap bounds the ranked pool with the same
// rowid-DESC recency bias (transcript_fts rowids follow transcript insertion
// order). Exhausting the transcript pool does not set Truncated; the small
// Episodes group cap already bounds what a fuller pool could add.
func (s *Store) searchTranscripts(ctx context.Context, match string, limit, maxCandidates int, libIDs []int64, states []model.ItemState, seen map[model.PID]bool, res *read.SearchResult) error {
	const op = "store.Search"
	narrow, narrowArgs := searchNarrow(libIDs, states)
	var stmt string
	var args []any
	if maxCandidates <= 0 {
		stmt = `SELECT pi.pid, pi.title, p.title, bm25(transcript_fts) AS score
		 FROM transcript_fts
		 JOIN playable_item pi ON pi.id = transcript_fts.episode_id` + narrow + `
		 JOIN episode e ON e.item_id = pi.id
		 JOIN podcast p ON p.id = e.podcast_id
		 WHERE transcript_fts MATCH ?
		 ORDER BY score, pi.pid
		 LIMIT ?`
		args = append(narrowArgs, match, limit+len(seen))
	} else {
		innerNarrow := ""
		if narrow != "" {
			innerNarrow = `
			JOIN playable_item pi ON pi.id = transcript_fts.episode_id` + narrow
		}
		// The inner LIMIT is maxCandidates, not maxCandidates+1, unlike the metadata
		// rung: this rung never reports pool exhaustion, so it has no probe row to
		// fetch. The asymmetry is deliberate rather than an oversight.
		stmt = `SELECT pi.pid, pi.title, p.title, c.score AS score
		 FROM (SELECT transcript_fts.episode_id AS eid, bm25(transcript_fts) AS score
			FROM transcript_fts` + innerNarrow + `
			WHERE transcript_fts MATCH ?
			ORDER BY transcript_fts.rowid DESC
			LIMIT ?) c
		 JOIN playable_item pi ON pi.id = c.eid
		 JOIN episode e ON e.item_id = pi.id
		 JOIN podcast p ON p.id = e.podcast_id
		 ORDER BY score, pi.pid
		 LIMIT ?`
		args = append(narrowArgs, match, maxCandidates, limit+len(seen))
	}
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	for rows.Next() {
		if len(res.Episodes) >= limit {
			break
		}
		var pid, title, podcast string
		var score float64
		if err := rows.Scan(&pid, &title, &podcast, &score); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if seen[model.PID(pid)] {
			continue
		}
		seen[model.PID(pid)] = true
		res.Episodes = append(res.Episodes, read.SearchHit{
			PID: model.PID(pid), Kind: "episode", Title: title, Subtitle: podcast, Score: score,
		})
	}
	return rows.Err()
}

// ftsMatchQuery turns a user search string into a safe FTS5 MATCH expression. The
// input is NFC-normalized and split on whitespace, and each word becomes a group of
// alternatives joined by OR: its tokens as a prefix phrase (letters, digits and marks,
// as the tokenizer keeps them, lowercased), then the alternate forms the index holds
// beside a word (searchWordForms), and either a phrase of unit pairs for a word that is
// one run of an unspaced script, which finds it inside a longer run, or for a word of
// several tokens those tokens anywhere in the row, since the words either side of
// inner punctuation can sit apart ("rock&roll", "Artist-Title"). Groups are joined by
// AND. Every term is a double-quoted string built from token characters only, so no
// quote, operator or column filter reaches FTS5 from the input. A word with no letter
// or digit is dropped, and "" means nothing is left to match.
func ftsMatchQuery(input string) string {
	var groups []string
	for _, word := range strings.Fields(norm.NFC.String(input)) {
		if g := wordMatch(word); g != "" {
			groups = append(groups, g)
		}
	}
	return strings.Join(groups, " AND ")
}

func wordMatch(word string) string {
	f := searchWordForms(word)
	var alts []string
	add := func(term string) {
		if !slices.Contains(alts, term) {
			alts = append(alts, term)
		}
	}
	for _, form := range []string{strings.Join(f.tokens, " "), strings.Join(f.compat, " "), f.joined} {
		if form == "" {
			continue
		}
		add(`"` + form + `"*`)
		if k := foldKana(form); k != form {
			add(`"` + k + `"*`)
		}
	}
	switch base := f.base(); {
	case len(base) == 1:
		if pairs := runPairs(foldKana(base[0])); pairs != nil {
			add(`"` + strings.Join(pairs, " ") + `"`)
		}
	case len(base) > 1:
		terms := make([]string, len(base))
		for i, tok := range base {
			terms[i] = `"` + foldKana(tok) + `"*`
		}
		add("(" + strings.Join(terms, " AND ") + ")")
	}
	switch len(alts) {
	case 0:
		return ""
	case 1:
		return alts[0]
	}
	return "(" + strings.Join(alts, " OR ") + ")"
}
