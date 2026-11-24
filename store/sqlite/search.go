package sqlite

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
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
	return min(max(limit*searchFetchPerLimit, searchFetchMin), searchFetchMax)
}

// searchRowsQ renders the items a ranked statement keeps. ranked selects them as (rid,
// pid, kind, title, score) rows already cut to its limit, and order restates its order
// over those columns, so that SQLite reads the rows in the order they come and sorts only
// an item's own credits. A row holds the item's own fields and the entities it brings to
// the groups, by their own names: the album, series or show it sits in with that one's
// byline (a track's album artist, a show's author), its first artist or author, a
// track's album artist where that is someone else, and one artist it credits. So an item
// repeats once for each credit, a track's artist credit before its other credits and each
// role in credited order, and comes once when it has none. The joins sit outside the
// ranking so that a match the limit drops pays for none of them, and every item statement
// is built here so the row shapes can never drift.
func searchRowsQ(ranked, order string) string {
	return `SELECT r.pid, r.kind, r.title,
		COALESCE(NULLIF(t.artist,''), bk.author, pod.title, ''), COALESCE(t.album_artist, pod.author, ''),
		COALESCE(al.pid, ser.pid, pod.pid, ''), COALESCE(al.title, ser.name, pod.title, ''),
		COALESCE(art.pid,''), COALESCE(art.name,''), COALESCE(aart.pid,''), COALESCE(aart.name,''),
		COALESCE(ic.role,''), COALESCE(ca.pid,''), COALESCE(ca.name,''), r.score
		FROM (` + ranked + `) r
		LEFT JOIN track t ON t.item_id = r.rid
		LEFT JOIN book bk ON bk.item_id = r.rid
		LEFT JOIN episode ep ON ep.item_id = r.rid
		LEFT JOIN podcast pod ON pod.id = ep.podcast_id
		LEFT JOIN album al ON al.id = t.album_id
		LEFT JOIN series ser ON ser.id = bk.series_id
		LEFT JOIN artist art ON art.id = ` + itemArtistIDExpr + `
		LEFT JOIN artist aart ON aart.id = NULLIF(t.album_artist_id, t.artist_id)
		LEFT JOIN item_contributor ic ON ic.item_id = r.rid
		LEFT JOIN artist ca ON ca.id = ic.artist_id
		ORDER BY ` + order + `, ic.role <> 'artist', ic.role, ic.position`
}

// rankedCols is what a ranked statement selects beside its score, from its pi.
const rankedCols = `pi.id AS rid, pi.pid AS pid, pi.kind AS kind, pi.title AS title`

// searchScope is what narrows a search's matched items: the libraries their files sit
// in, the libraries their primary files sit in, their states, and for an episode its
// show. shows nil keeps every show's episodes; an empty non-nil shows keeps none.
type searchScope struct {
	libIDs        []int64
	primaryLibIDs []int64
	states        []model.ItemState
	shows         []int64
}

// searchNarrow returns the fragment narrowing matched items (the pi alias) to a scope,
// plus its bind args. It is spliced directly after a caller's own `JOIN playable_item pi
// ON ...` line, because the two rungs join pi on different columns and disagree about
// whether the join is wanted when nothing narrows at all. For libIDs=[7],
// primaryLibIDs=[7], states=[present remote] and shows=[3] it returns:
//
//	` AND pi.state IN (?,?) AND EXISTS (SELECT 1 FROM item_file spf JOIN file sf ON sf.id = spf.file_id
//		WHERE spf.item_id = pi.id AND sf.library_id IN (SELECT value FROM json_each(?)))
//		AND EXISTS (SELECT 1 FROM item_file spf JOIN file sf ON sf.id = spf.file_id
//		WHERE spf.item_id = pi.id AND spf.role = 'primary' AND sf.library_id IN (SELECT value FROM json_each(?)))
//		AND (pi.kind <> 'episode' OR EXISTS (SELECT 1 FROM episode se
//		WHERE se.item_id = pi.id AND se.podcast_id IN (SELECT value FROM json_each(?))))`
//
// Every narrowing rides the pi join's ON clause. pi is INNER-joined in both rungs, so ON
// and WHERE are equivalent here, and keeping them in one contiguous fragment is what
// makes the bind order fall out of fragment order.
//
// An item counts in a library when any of its files lives there, its alternates'
// included, so a host scoping by library finds it wherever it can serve a file; the
// primary form counts it in its primary file's library alone, where the item view places
// it. One with no file at all (an undownloaded episode) drops out of either scope. Each
// id list binds as one JSON array, so a caller's list of any length runs one statement
// text. A library or a show is asked of each item in turn, by a seek of the item's own
// file edges or episode row, rather than looked up in a list of the scope's members:
// SQLite builds such a list whole before the first row, a walk of every file in the
// library on each statement, where a search reads far fewer items than a library holds.
// A part left empty contributes nothing, except an empty non-nil show list, which keeps
// every episode out.
func searchNarrow(sc searchScope) (string, []any) {
	if len(sc.libIDs) == 0 && len(sc.primaryLibIDs) == 0 && len(sc.states) == 0 && sc.shows == nil {
		return "", nil
	}
	// Exact capacity so every caller's append reallocates rather than writing into
	// a shared backing array.
	n := len(sc.states)
	for _, list := range [][]int64{sc.libIDs, sc.primaryLibIDs, sc.shows} {
		if len(list) > 0 {
			n++
		}
	}
	args := make([]any, 0, n)
	frag := ""
	if len(sc.states) > 0 { // placeholders(0) is "()", and IN () is a syntax error
		frag += ` AND pi.state IN ` + placeholders(len(sc.states))
		for _, st := range sc.states {
			args = append(args, string(st))
		}
	}
	if len(sc.libIDs) > 0 {
		frag += libraryMembersFrag("")
		args = append(args, idArray(sc.libIDs))
	}
	if len(sc.primaryLibIDs) > 0 {
		frag += libraryMembersFrag(" AND spf.role = 'primary'")
		args = append(args, idArray(sc.primaryLibIDs))
	}
	switch {
	case sc.shows == nil:
	case len(sc.shows) == 0:
		frag += ` AND pi.kind <> 'episode'`
	default:
		frag += ` AND (pi.kind <> 'episode' OR EXISTS (SELECT 1 FROM episode se
		WHERE se.item_id = pi.id AND se.podcast_id IN ` + inArray + `))`
		args = append(args, idArray(sc.shows))
	}
	return frag, args
}

// libraryMembersFrag narrows to the items with a file in a bound list of libraries,
// through every edge or, with edge naming the primary role, the primary alone. The list
// binds as one JSON array (inArray).
func libraryMembersFrag(edge string) string {
	return ` AND EXISTS (SELECT 1 FROM item_file spf JOIN file sf ON sf.id = spf.file_id
		WHERE spf.item_id = pi.id` + edge + ` AND sf.library_id IN ` + inArray + `)`
}

// podcastIDsByPIDs resolves a show list to podcast ids in one statement, keeping a nil
// list nil and an empty one empty, since the two narrow differently. An unknown pid is
// CodeNotFound.
func (s *Store) podcastIDsByPIDs(ctx context.Context, pids []model.PID, op string) ([]int64, error) {
	if pids == nil {
		return nil, nil
	}
	ids := make(map[model.PID]int64, len(pids))
	if len(pids) > 0 {
		rows, err := s.rstmts.queryContext(ctx, s.rdb(), "SELECT pid, id FROM podcast WHERE pid IN "+inArray, pidArray(pids))
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var pid string
			var id int64
			if err := rows.Scan(&pid, &id); err != nil {
				return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			ids[model.PID(pid)] = id
		}
		if err := rows.Err(); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	out := make([]int64, 0, len(pids))
	for _, pid := range pids {
		id, ok := ids[pid]
		if !ok {
			return nil, waxerr.New(waxerr.CodeNotFound, op, "no such podcast: "+string(pid))
		}
		out = append(out, id)
	}
	return out, nil
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
// the statement, its bind args in clause order, and the ranked-item scan cap the
// caller's loop enforces (one item past it sets Truncated). Its limits are written into
// the text, as every statement the cache keeps writes them: SQLite plans with a bound
// LIMIT's value, so binding one has the statement prepared again on every run.
//
// With no candidate cap the ranking is the flat FTS query, which with an empty
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
func searchStmt(match string, limit, maxCandidates int, sc searchScope) (string, []any, int) {
	scanCap := searchScanCap(limit, maxCandidates)
	narrow, narrowArgs := searchNarrow(sc)
	const order = "r.score, r.pid"

	if maxCandidates <= 0 {
		ranked := `SELECT ` + rankedCols + `, ` + searchBM25 + ` AS score
			FROM search_fts
			JOIN playable_item pi ON pi.id = search_fts.rowid` + narrow + `
			WHERE search_fts MATCH ?
			ORDER BY score, pid
			LIMIT ` + strconv.Itoa(scanCap+1)
		return searchRowsQ(ranked, order), append(narrowArgs, match), scanCap
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
	ranked := `SELECT ` + rankedCols + `, c.score AS score
			FROM (SELECT search_fts.rowid AS rid, ` + searchBM25 + ` AS score
				FROM search_fts` + innerNarrow + `
				WHERE search_fts MATCH ?
				ORDER BY search_fts.rowid DESC
				LIMIT ` + strconv.Itoa(maxCandidates+1) + `) c
			JOIN playable_item pi ON pi.id = c.rid
			ORDER BY score, pid
			LIMIT ` + strconv.Itoa(scanCap+1)
	return searchRowsQ(ranked, order), append(narrowArgs, match), scanCap
}

// searchScanCap is how many ranked rows a search reads before it reports truncation: the
// fetch cap for its limit, or the candidate cap when that is smaller.
func searchScanCap(limit, maxCandidates int) int {
	scanCap := searchFetchCap(limit)
	if maxCandidates > 0 && maxCandidates < scanCap {
		scanCap = maxCandidates
	}
	return scanCap
}

// Search runs a grouped, BM25-ranked metadata search. It queries the metadata FTS
// once with field weighting, then derives the groups from the ranked matches: a
// matched track, book or episode fills its own group, with transcript-body hits
// appended to Episodes after the metadata hits, and brings the entities it belongs to
// into theirs, which are a track's album and artists, a book's series, authors and
// narrators, and an episode's show. The entity groups
// add the entities whose own name is the query (searchEntitiesByName), which the ranked
// rows can miss when better-scoring items fill the pool, and are then ordered by how
// each name meets the query (searchTier), first-seen order breaking ties. A
// query of symbols alone, which has no word to match, is looked up by sort key
// instead (searchItemsBySortKey); an empty one returns an empty result, not an error.
// opt.MaxCandidates bounds the match pool, while opt.Libraries, opt.PrimaryLibraries,
// opt.States and opt.Podcasts narrow it and the entity lookups alike (see
// read.SearchOptions for their contracts), and an unknown library, state or show is
// refused whatever the query holds. Each hit carries the cover it resolves (ArtHash).
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
	primaryLibIDs, err := s.libraryIDsByPIDs(ctx, opt.PrimaryLibraries, op)
	if err != nil {
		return nil, err
	}
	if err := checkItemStates(opt.States, op); err != nil {
		return nil, err
	}
	shows, err := s.podcastIDsByPIDs(ctx, opt.Podcasts, op)
	if err != nil {
		return nil, err
	}
	sc := searchScope{libIDs: libIDs, primaryLibIDs: primaryLibIDs, states: opt.States, shows: shows}
	g := newSearchGroups(res, limit, queryStr)
	match := ftsMatchQuery(queryStr)
	if match == "" {
		if g.q.sort == "" {
			return res, nil
		}
		if err := s.searchItemsBySortKey(ctx, g.q.sort, limit, opt.MaxCandidates, sc, g); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	} else if err := s.searchMatches(ctx, match, limit, opt.MaxCandidates, sc, g); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := s.searchEntitiesByName(ctx, queryStr, limit, sc, g); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	g.rankEntities()
	if err := s.fillSeriesAuthors(ctx, res.Series, sc); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// Append transcript-body hits to the Episodes group, after the metadata hits, so
	// a title match outranks a body match. Episodes already surfaced by metadata are
	// skipped to avoid duplicates.
	if match != "" && len(res.Episodes) < limit {
		if err := s.searchTranscripts(ctx, match, limit, opt.MaxCandidates, sc, g.episodeSeen, res); err != nil {
			return nil, err
		}
	}
	if err := s.fillSearchArt(ctx, res, op); err != nil {
		return nil, err
	}
	return res, nil
}

// fillSearchArt sets each hit's ArtHash, with one art walk per entity type the hits are
// asked for art under, so the tracks and the books share theirs, as the artists, the
// authors and the narrators do.
func (s *Store) fillSearchArt(ctx context.Context, res *read.SearchResult, op string) error {
	groups := res.Groups()
	byType := map[model.ArtEntity][]model.PID{}
	for _, hits := range groups {
		for _, h := range hits {
			if ref, ok := h.ArtRef(); ok {
				byType[ref.Type] = append(byType[ref.Type], h.PID)
			}
		}
	}
	hashes := make(map[model.ArtEntity]map[model.PID]string, len(byType))
	for typ, pids := range byType {
		got, err := s.frontHashes(ctx, typ, pids, op)
		if err != nil {
			return err
		}
		hashes[typ] = got
	}
	for _, hits := range groups {
		for i := range hits {
			if ref, ok := hits[i].ArtRef(); ok {
				hits[i].ArtHash = hashes[ref.Type][ref.PID]
			}
		}
	}
	return nil
}

// searchMatches feeds the ranked FTS rows to the groups. Both limits fetch one past
// their cap so a full result set signals truncation (a spent candidate pool or a
// spent ranked-row scan alike) rather than silently dropping matches. score is
// tie-broken by pid so equal-score rows come back in a stable, deterministic order.
func (s *Store) searchMatches(ctx context.Context, match string, limit, maxCandidates int, sc searchScope, g *searchGroups) error {
	stmt, args, scanCap := searchStmt(match, limit, maxCandidates, sc)
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), stmt, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	return g.readItems(rows, scanCap)
}

// searchGroups fills a result's groups from the rows of an item statement, in rank order.
// The entity groups are gathered whole and cut to the limit only once rankEntities has
// ordered them. best is the best score of the items read, which an entity found by its
// own name alone takes, and named remembers whether the query names each artist asked.
type searchGroups struct {
	res                *read.SearchResult
	limit              int
	q                  nameKeys
	best               float64
	artists, albums    entityHits
	authors, narrators entityHits
	series, podcasts   entityHits
	episodeSeen        map[model.PID]bool
	named              map[model.PID]bool
}

func newSearchGroups(res *read.SearchResult, limit int, query string) *searchGroups {
	return &searchGroups{res: res, limit: limit, q: newNameKeys(query, true),
		episodeSeen: map[model.PID]bool{}, named: map[model.PID]bool{},
		artists: entityHits{kind: "artist"}, albums: entityHits{kind: "album"},
		authors: entityHits{kind: "author"}, narrators: entityHits{kind: "narrator"},
		series: entityHits{kind: "series"}, podcasts: entityHits{kind: "podcast"}}
}

// entityHits is one entity group as gathered: each entity once, in first-seen order.
type entityHits struct {
	kind string
	hits []read.SearchHit
	seen map[model.PID]bool
}

func (e *entityHits) add(h read.SearchHit) {
	if h.PID == "" || e.seen[h.PID] {
		return
	}
	if e.seen == nil {
		e.seen = map[model.PID]bool{}
	}
	e.seen[h.PID] = true
	h.Kind = e.kind
	e.hits = append(e.hits, h)
}

// itemRow is one row of searchRowsQ.
type itemRow struct {
	pid, kind, title, subtitle, byline                  string
	inPID, inTitle, firstPID, firstName, albumArtistPID string
	albumArtistName, role, creditPID, creditName        string
	score                                               float64
}

// entity is one of a row's entities as a hit, scored as its item.
func (r *itemRow) entity(pid, title string) read.SearchHit {
	return read.SearchHit{PID: model.PID(pid), Title: title, Score: r.score}
}

// readItems files the rows of an item statement, the item where a row begins one and the
// credit every row holds, and stops at the first item past scanCap, which marks the
// result truncated. A track's album artist waits for the track's last row, so that it
// follows the credits.
func (g *searchGroups) readItems(rows *sql.Rows, scanCap int) error {
	items, last := 0, ""
	var albumArtist read.SearchHit
	for rows.Next() {
		var r itemRow
		if err := rows.Scan(&r.pid, &r.kind, &r.title, &r.subtitle, &r.byline, &r.inPID, &r.inTitle,
			&r.firstPID, &r.firstName, &r.albumArtistPID, &r.albumArtistName,
			&r.role, &r.creditPID, &r.creditName, &r.score); err != nil {
			return err
		}
		if r.pid != last {
			g.addNamed(albumArtist)
			albumArtist = read.SearchHit{}
			if items == scanCap {
				g.res.Truncated = true
				break
			}
			items, last = items+1, r.pid
			g.addItem(&r)
			albumArtist = r.entity(r.albumArtistPID, r.albumArtistName)
		}
		g.addCredit(&r)
	}
	g.addNamed(albumArtist)
	return rows.Err()
}

// addItem files an item in its own group, and in theirs the album, series or show it sits
// in and its first artist or author.
func (g *searchGroups) addItem(r *itemRow) {
	g.best = min(g.best, r.score)
	res := g.res
	hit := read.SearchHit{PID: model.PID(r.pid), Kind: r.kind, Title: r.title, Subtitle: r.subtitle, Score: r.score}
	in := r.entity(r.inPID, r.inTitle)
	in.Subtitle = r.byline
	switch r.kind {
	// An episode forms the Episodes group, keyed by title with its podcast as the
	// subtitle. These are the title/metadata hits; transcript-body hits are appended
	// after, so a title match always outranks a body match.
	case string(model.KindEpisode):
		g.episodeSeen[hit.PID] = true
		if len(res.Episodes) < g.limit {
			res.Episodes = append(res.Episodes, hit)
		}
		g.podcasts.add(in)
	// A book forms its own group, keyed by title with its author as the subtitle. Its
	// series is subtitled once the group is cut (fillSeriesAuthors).
	case string(model.KindBook):
		if len(res.Books) < g.limit {
			res.Books = append(res.Books, hit)
		}
		g.series.add(in)
		g.authors.add(r.entity(r.firstPID, r.firstName))
	default:
		if len(res.Tracks) < g.limit {
			res.Tracks = append(res.Tracks, hit)
		}
		if r.byline == "" {
			in.Subtitle = r.subtitle
		}
		g.albums.add(in)
		g.artists.add(r.entity(r.firstPID, r.firstName))
	}
}

// addCredit files the artist a row credits its item with. The artists of a track's artist
// credit join Artists whatever their names. Its album artist (readItems) and the artists
// it credits in any other role, a performer's included, join only when the query names
// them, so a title match does not list its producers. A book's authors and narrators
// fill their groups, from the rows the author and narrator facets read, and its other
// credits have no group.
func (g *searchGroups) addCredit(r *itemRow) {
	credit, role := r.entity(r.creditPID, r.creditName), model.ContributorRole(r.role)
	switch book := r.kind == string(model.KindBook); {
	case book && role == model.RoleAuthor:
		g.authors.add(credit)
	case book && role == model.RoleNarrator:
		g.narrators.add(credit)
	case book:
	case role == model.RoleArtist:
		g.artists.add(credit)
	default:
		g.addNamed(credit)
	}
}

// addNamed files an artist in Artists when the query names it: the name is the query, or
// each word of the query begins a word of the name, which is how the match itself reads
// a word. A name that merely holds the query's letters is not named, or typing "son"
// would list every Jason a matched track credits.
func (g *searchGroups) addNamed(h read.SearchHit) {
	if h.PID == "" {
		return
	}
	named, ok := g.named[h.PID]
	if !ok {
		named = searchTier(newNameKeys(h.Title, g.q.key == ""), g.q) == 0 || wordsBegin(g.q.key, h.Title)
		g.named[h.PID] = named
	}
	if named {
		g.artists.add(h)
	}
}

// wordsBegin reports whether each word of a query's key begins a word of name, read as
// the index holds a name: its tokens, and each written word run together ("K.R.I.T." is
// krit). A query word that is a run of an unspaced script is looked for anywhere inside
// one, where the match finds it.
func wordsBegin(queryKey, name string) bool {
	if queryKey == "" {
		return false
	}
	var forms []string
	for word := range strings.FieldsSeq(norm.NFKC.String(name)) {
		k := foldKana(identity.MatchKey(word))
		forms = append(forms, strings.Fields(k)...)
		forms = append(forms, strings.ReplaceAll(k, " ", ""))
	}
	for w := range strings.FieldsSeq(queryKey) {
		inside := strings.HasPrefix
		if runPairs(w) != nil {
			inside = strings.Contains
		}
		if !slices.ContainsFunc(forms, func(f string) bool { return inside(f, w) }) {
			return false
		}
	}
	return true
}

// rankEntities orders each gathered entity group by tier, keeping first-seen order within
// one, marks the exact ones, and cuts the group to the limit.
func (g *searchGroups) rankEntities() {
	r := g.res
	for _, e := range []struct {
		into *[]read.SearchHit
		from entityHits
	}{
		{&r.Artists, g.artists}, {&r.Albums, g.albums}, {&r.Authors, g.authors},
		{&r.Narrators, g.narrators}, {&r.Series, g.series}, {&r.Podcasts, g.podcasts},
	} {
		*e.into = rankByTier(e.from.hits, g.q, g.limit)
	}
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
	for w := range strings.FieldsSeq(q.key) {
		if !strings.Contains(name.joined, w) {
			return 3
		}
	}
	return 2
}

// searchItemsBySortKey answers a query of symbols alone ("$", "÷", "!!!"), which
// tokenizes to nothing, by looking its sort key up among item titles (their title_key,
// since a title spelling moves the item's sort_key) and track artists,
// under the same narrowing. An artist named without a letter or digit has no entity of
// its own, so a track's artist display is read for it, whole or as one name of a
// comma-joined list, as well as its sort, which an artist sort tag can spell otherwise
// ("Chk Chk Chk" for "!!!"). Newest first, it reads one item past its cap as the ranked
// search does, so a full result sets Truncated, and a smaller candidate cap is the cap.
func (s *Store) searchItemsBySortKey(ctx context.Context, key string, limit, maxCandidates int, sc searchScope, g *searchGroups) error {
	narrow, narrowArgs := searchNarrow(sc)
	scanCap := searchScanCap(limit, maxCandidates)
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), itemsBySortKeyQ(narrow, scanCap+1), append([]any{key}, narrowArgs...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	return g.readItems(rows, scanCap)
}

// itemsBySortKeyQ is the statement searchItemsBySortKey runs, binding the key once as
// ?1 and reading at most fetch items. The lookups drive (CROSS JOIN keeps them
// outermost), so a library narrowing is a residual test rather than a walk of every item
// in the library. The display has no index, so that arm reads the track table, which
// only a query of symbols alone pays.
func itemsBySortKeyQ(narrow string, fetch int) string {
	return searchRowsQ(`SELECT `+rankedCols+`, 0.0 AS score
			FROM (SELECT id FROM playable_item WHERE title_key = ?1
				UNION SELECT item_id FROM track WHERE artist_sort_key = ?1
				UNION SELECT item_id FROM track WHERE instr(artist, ?1) > 0
					AND instr(', ' || artist || ', ', ', ' || ?1 || ', ') > 0) m
			CROSS JOIN playable_item pi ON pi.id = m.id`+narrow+`
			ORDER BY rid DESC LIMIT `+strconv.Itoa(fetch), "r.rid DESC")
}

// searchEntitiesByName adds the entities whose own name is the query to the gathered
// groups, under the same narrowing, each by its nameLookup. A person is looked up once
// for each group: Artists keeps one a track credits, Authors and Narrators one a book
// credits in that role. A query of symbols alone looks up no person, since such a name
// has no match key and never becomes an artist, and the others by their sort keys alone.
// Every lookup is an equality on an indexed column.
func (s *Store) searchEntitiesByName(ctx context.Context, query string, limit int, sc searchScope, g *searchGroups) error {
	narrow, narrowArgs := searchNarrow(sc)
	lookup := func(e *entityHits, q string, args ...[]any) error {
		hits, err := s.searchEntities(ctx, q, slices.Concat(args...)...)
		if err != nil {
			return err
		}
		for _, h := range hits {
			h.Score = g.best
			e.add(h)
		}
		return nil
	}
	keys, sortKey := anySlice(joinedLookupKeys(query)), []any{g.q.sort}
	if len(keys) > 0 {
		err := lookup(&g.artists, artistByName.q("''", trackCredited(narrow), len(keys), limit), keys, sortKey, narrowArgs, narrowArgs)
		if err != nil {
			return err
		}
		for _, p := range []struct {
			e    *entityHits
			role model.ContributorRole
		}{{&g.authors, model.RoleAuthor}, {&g.narrators, model.RoleNarrator}} {
			err := lookup(p.e, artistByName.q("''", bookCredited(narrow), len(keys), limit), keys, sortKey, narrowArgs, []any{string(p.role)})
			if err != nil {
				return err
			}
		}
	}
	if g.q.sort == "" {
		return nil
	}
	// The album's subtitle is in the select list, so its narrowing binds first.
	if err := lookup(&g.albums, albumByName.q(albumSubtitle(narrow), albumHeld(narrow), len(keys), limit),
		narrowArgs, keys, sortKey, narrowArgs); err != nil {
		return err
	}
	if err := lookup(&g.series, seriesByName.q("''", seriesHeld(narrow), len(keys), limit), keys, sortKey, narrowArgs); err != nil {
		return err
	}
	member, memberArgs, ok := showMember(sc)
	if !ok {
		return nil
	}
	return lookup(&g.podcasts, podcastByName.q("pod.author", member, len(keys), limit), keys, sortKey, memberArgs)
}

// joinedLookupKeys returns the keys a stored joined key (joinedKey) is looked up by: the
// query's own, and the query's read past compatibility forms, each in both kana. A
// stored key keeps the name's own forms, so these are the spellings of a name the tiers
// read as the query (newNameKeys) that a lookup can reach.
func joinedLookupKeys(query string) []string {
	var keys []string
	for _, s := range []string{query, norm.NFKC.String(query)} {
		k := joinedKey(s)
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

// nameLookup is how one kind of entity is found by its own name: by its joined key
// (joinedKey), which reads past how inner punctuation was typed, so that "jayz" is Jay-Z
// and "whats going on" is What's Going On, and by its name's sort key, which reads past
// a leading article and is all a name of symbols alone has. The name keys are compared,
// not the sort keys a curated sort or a spelling moves; a series and a show take
// neither, so their sort keys are their names'.
type nameLookup struct{ from, pid, name, joined, key string }

var (
	artistByName  = nameLookup{"artist art", "art.pid", "art.name", "replace(art.match_key, ' ', '')", "art.name_key"}
	albumByName   = nameLookup{"album al", "al.pid", "al.title", "al.title_joined_key", "al.title_key"}
	seriesByName  = nameLookup{"series ser", "ser.pid", "ser.name", "replace(ser.match_key, ' ', '')", "ser.sort_key"}
	podcastByName = nameLookup{"podcast pod", "pod.pid", "pod.title", "pod.title_joined_key", "pod.sort_key"}
)

// q is the lookup's statement: the pid, name and subtitle of at most limit entities named
// by any of keys bound joined keys or by one sort key, bound after them, or with no keys
// by the sort key alone, that member admits. An empty member admits all.
func (l nameLookup) q(subtitle, member string, keys, limit int) string {
	named := l.key + ` = ?`
	if keys > 0 {
		named = `(` + l.joined + ` IN ` + placeholders(keys) + ` OR ` + l.key + ` = ?)`
	}
	if member != "" {
		named += `
		AND ` + member
	}
	return `SELECT ` + l.pid + `, ` + l.name + `, ` + subtitle + ` FROM ` + l.from + `
		WHERE ` + named + `
		ORDER BY ` + l.pid + ` LIMIT ` + strconv.Itoa(limit)
}

// holdsItem admits an entity when the narrowing keeps one of its items, which are the
// rows of from that where picks, each naming its item by itemID.
func holdsItem(from, itemID, where, narrow string) string {
	return `EXISTS (SELECT 1 FROM ` + from + ` JOIN playable_item pi ON pi.id = ` + itemID + narrow + `
			WHERE ` + where + `)`
}

// trackCredited admits an artist credited on, or the album artist of, a track the
// narrowing keeps. narrow is spliced twice, so its args bind twice.
func trackCredited(narrow string) string {
	return `(` + holdsItem("item_contributor ic JOIN track t ON t.item_id = ic.item_id", "ic.item_id", "ic.artist_id = art.id", narrow) + `
		OR ` + holdsItem("track t", "t.item_id", "t.album_artist_id = art.id", narrow) + `)`
}

// bookCredited admits an artist credited in one role, bound after the narrowing's args,
// on a book the narrowing keeps, which is who that role's facet counts.
func bookCredited(narrow string) string {
	return holdsItem("item_contributor ic JOIN book bk ON bk.item_id = ic.item_id", "ic.item_id",
		"ic.artist_id = art.id AND ic.role = ?", narrow)
}

func albumHeld(narrow string) string {
	return holdsItem("track t", "t.item_id", "t.album_id = al.id", narrow)
}

func seriesHeld(narrow string) string {
	return holdsItem("book bk", "bk.item_id", "bk.series_id = ser.id", narrow)
}

// showMember returns what admits a show to the lookup by name, with its args. A library,
// a placement and a state are an episode's to meet, so under any of them a show must
// hold an episode the narrowing keeps. Under none a show needs no episode, so one is
// found before its first arrives, and a show list is asked of the show itself; ok is
// false when that list is empty and admits no show.
func showMember(sc searchScope) (member string, args []any, ok bool) {
	if len(sc.libIDs)+len(sc.primaryLibIDs)+len(sc.states) > 0 {
		narrow, narrowArgs := searchNarrow(sc)
		return holdsItem("episode ep", "ep.item_id", "ep.podcast_id = pod.id", narrow), narrowArgs, true
	}
	switch {
	case sc.shows == nil:
		return "", nil, true
	case len(sc.shows) == 0:
		return "", nil, false
	}
	return "pod.id IN " + inArray, []any{idArray(sc.shows)}, true
}

// firstItem renders col of the first of an entity's items the narrowing keeps, the rows
// of from that where picks in itemID order, or ” with none. Reading it under the
// narrowing keeps a subtitle from naming an artist or an author out of an item the
// search was told to leave out.
func firstItem(col, from, itemID, where, narrow string) string {
	return `COALESCE((SELECT ` + col + ` FROM ` + from + ` JOIN playable_item pi ON pi.id = ` + itemID + narrow + `
			WHERE ` + where + ` ORDER BY ` + itemID + ` LIMIT 1), '')`
}

// albumSubtitle renders an album's artist for a hit found by the album itself rather
// than through one of its tracks, as a track hit renders it.
func albumSubtitle(narrow string) string {
	return firstItem("COALESCE(NULLIF(t.album_artist,''), t.artist)", "track t", "t.item_id", "t.album_id = al.id", narrow)
}

// seriesAuthorsQ reads the author of each series of a bound pid array, binding the
// narrowing's args first.
func seriesAuthorsQ(narrow string) string {
	return `SELECT ser.pid, ` + firstItem("bk.author", "book bk", "bk.item_id", "bk.series_id = ser.id", narrow) + `
		FROM series ser WHERE ser.pid IN ` + inArray
}

// fillSeriesAuthors subtitles each series hit with the author of the first of its books
// the narrowing keeps, so a series reads the same whichever of its books found it.
func (s *Store) fillSeriesAuthors(ctx context.Context, hits []read.SearchHit, sc searchScope) error {
	if len(hits) == 0 {
		return nil
	}
	narrow, narrowArgs := searchNarrow(sc)
	pids := make([]model.PID, len(hits))
	for i, h := range hits {
		pids[i] = h.PID
	}
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), seriesAuthorsQ(narrow), append(narrowArgs, pidArray(pids))...)
	if err != nil {
		return err
	}
	defer rows.Close()
	authors := make(map[model.PID]string, len(hits))
	for rows.Next() {
		var pid, author string
		if err := rows.Scan(&pid, &author); err != nil {
			return err
		}
		authors[model.PID(pid)] = author
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range hits {
		hits[i].Subtitle = authors[hits[i].PID]
	}
	return nil
}

// searchEntities reads (pid, title, subtitle) entity hits.
func (s *Store) searchEntities(ctx context.Context, q string, args ...any) ([]read.SearchHit, error) {
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), q, args...)
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
// transcript FTS, skipping episodes already in the Episodes group. It runs only while
// that group holds fewer than limit, which means every episode the metadata rows saw is
// in it, and an episode has one transcript row, so limit rows always hold enough unseen
// ones to fill the group; a fixed fetch also keeps the statement one text for the cache.
// It honors every search knob: a library scope keeps a transcript hit from leaking an
// episode whose file lives outside the scope (an undownloaded one included), a state
// narrowing or a show list does the same for an episode the caller excluded, and a
// candidate cap bounds the ranked pool with the same rowid-DESC recency bias
// (transcript_fts rowids follow transcript insertion order).
// Exhausting the transcript pool does not set Truncated; the small Episodes group cap
// already bounds what a fuller pool could add.
func (s *Store) searchTranscripts(ctx context.Context, match string, limit, maxCandidates int, sc searchScope, seen map[model.PID]bool, res *read.SearchResult) error {
	const op = "store.Search"
	narrow, narrowArgs := searchNarrow(sc)
	stmt := transcriptsStmt(narrow, maxCandidates, limit)
	rows, err := s.rstmts.queryContext(ctx, s.rdb(), stmt, append(narrowArgs, match)...)
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

// transcriptsStmt is the statement searchTranscripts runs, reading at most fetch rows.
func transcriptsStmt(narrow string, maxCandidates, fetch int) string {
	if maxCandidates <= 0 {
		return `SELECT pi.pid, pi.title, p.title, bm25(transcript_fts) AS score
		 FROM transcript_fts
		 JOIN playable_item pi ON pi.id = transcript_fts.episode_id` + narrow + `
		 JOIN episode e ON e.item_id = pi.id
		 JOIN podcast p ON p.id = e.podcast_id
		 WHERE transcript_fts MATCH ?
		 ORDER BY score, pi.pid
		 LIMIT ` + strconv.Itoa(fetch)
	}
	innerNarrow := ""
	if narrow != "" {
		innerNarrow = `
			JOIN playable_item pi ON pi.id = transcript_fts.episode_id` + narrow
	}
	// The inner LIMIT is maxCandidates, not maxCandidates+1, unlike the metadata
	// rung: this rung never reports pool exhaustion, so it has no probe row to
	// fetch. The asymmetry is deliberate rather than an oversight.
	return `SELECT pi.pid, pi.title, p.title, c.score AS score
		 FROM (SELECT transcript_fts.episode_id AS eid, bm25(transcript_fts) AS score
			FROM transcript_fts` + innerNarrow + `
			WHERE transcript_fts MATCH ?
			ORDER BY transcript_fts.rowid DESC
			LIMIT ` + strconv.Itoa(maxCandidates) + `) c
		 JOIN playable_item pi ON pi.id = c.eid
		 JOIN episode e ON e.item_id = pi.id
		 JOIN podcast p ON p.id = e.podcast_id
		 ORDER BY score, pi.pid
		 LIMIT ` + strconv.Itoa(fetch)
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
	for word := range strings.FieldsSeq(norm.NFC.String(input)) {
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
