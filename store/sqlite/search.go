package sqlite

import (
	"context"
	"strings"
	"unicode"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// BM25 column weights for search_fts (kind, title, subtitle, artist, album,
// extra). A larger weight makes a hit in that column dominate the score, so a
// title match outranks an artist/album match, which outranks the genre/extra
// field. kind is metadata-only and carries no weight.
const searchBM25 = "bm25(search_fts, 0.0, 10.0, 4.0, 5.0, 5.0, 1.0)"

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
// fields plus the artist/album entities a track hit fans out to). They are shared
// by the flat statement and the candidate-capped wrap so the two row shapes can
// never drift.
const searchDisplayCols = `pi.pid, pi.kind, pi.title,
		COALESCE(NULLIF(t.artist,''), bk.author, pod.title, ''), COALESCE(t.album_artist,''),
		COALESCE(t.album,''), COALESCE(art.pid,''), COALESCE(al.pid,'')`

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
//	` AND pi.state IN (?,?)
//		JOIN item_file spf ON spf.item_id = pi.id AND spf.role = 'primary'
//		JOIN file sf ON sf.id = spf.file_id AND sf.library_id IN (?)`
//
// The states ride the pi join's ON clause, so they must come before the scope
// joins: the ON clause ends at the next JOIN keyword. pi is INNER-joined in both
// rungs, so ON and WHERE are equivalent here, and keeping both narrowings in one
// contiguous fragment is what makes the bind order fall out of fragment order.
//
// The library joins are INNER for the usual reason: an item whose primary backing
// file lives elsewhere, or that has no file at all (an undownloaded episode),
// drops out of a scoped search. Either half being empty contributes nothing.
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
		frag += `
		JOIN item_file spf ON spf.item_id = pi.id AND spf.role = 'primary'
		JOIN file sf ON sf.id = spf.file_id AND sf.library_id IN ` + placeholders(len(libIDs))
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
// With no candidate cap the statement is the flat FTS query (byte-identical to
// the pre-option one when the scope is empty too). With maxCandidates > 0 the
// match is wrapped: the inner query walks it in rowid-DESC order and keeps the
// newest maxCandidates rows (+1 so pool exhaustion is observable), and only that
// pool is ranked. ORDER BY rowid DESC is deliberate: FTS5 optimizes rowid-order
// scans with the same early termination (bm25 is still computed only per emitted
// row), and search_fts rowids are playable_item ids, roughly insertion order, so
// a truncated pool is recency-biased instead of systematically dropping
// recently-added content (an unordered LIMIT would emit rowid ASC, oldest
// first). The tradeoff is documented on read.SearchOptions: candidates are the
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
// once with field weighting, then derives the artist/album/track groups from the
// ranked matches: a matched track contributes itself to Tracks and its album and
// artist entities to Albums and Artists (best score wins, ties broken by scan
// order). Episode metadata hits land in Episodes, with transcript-body hits
// appended after them. A query with no usable tokens returns an empty result,
// not an error. opt.MaxCandidates bounds the match pool, while opt.Libraries and
// opt.States narrow it (see read.SearchOptions for all three contracts). Because
// the entity groups are derived from matched item rows, an item-level narrowing
// carries into Artists and Albums for free.
func (s *Store) Search(ctx context.Context, queryStr string, opt read.SearchOptions) (*read.SearchResult, error) {
	const op = "store.Search"
	res := &read.SearchResult{Query: queryStr}
	match := ftsMatchQuery(queryStr)
	if match == "" {
		return res, nil
	}
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

	// Both limits fetch one past their cap so a full result set signals truncation
	// (a spent candidate pool or a spent ranked-row scan alike) rather than
	// silently dropping matches. score is tie-broken by pid so equal-score rows
	// come back in a stable, deterministic order.
	stmt, args, cap := searchStmt(match, limit, opt.MaxCandidates, libIDs, opt.States)
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()

	albumSeen := map[model.PID]bool{}
	artistSeen := map[model.PID]bool{}
	episodeSeen := map[model.PID]bool{}
	scanned := 0
	for rows.Next() {
		scanned++
		if scanned > cap {
			res.Truncated = true
			break
		}
		var pid, kind, title, artist, albumArtist, album string
		var artistPID, albumPID model.PID
		var score float64
		if err := rows.Scan(&pid, &kind, &title, &artist, &albumArtist, &album, &artistPID, &albumPID, &score); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// A book has no track/artist/album entities, so it forms its own group keyed
		// by title with its author as the subtitle, rather than the Tracks group.
		if kind == string(model.KindBook) {
			if len(res.Books) < limit {
				res.Books = append(res.Books, read.SearchHit{
					PID: model.PID(pid), Kind: "book", Title: title, Subtitle: artist, Score: score,
				})
			}
			continue
		}
		// An episode forms the Episodes group, keyed by title with its podcast as the
		// subtitle. These are the title/metadata hits; transcript-body hits are appended
		// after, so a title match always outranks a body match.
		if kind == string(model.KindEpisode) {
			episodeSeen[model.PID(pid)] = true
			if len(res.Episodes) < limit {
				res.Episodes = append(res.Episodes, read.SearchHit{
					PID: model.PID(pid), Kind: "episode", Title: title, Subtitle: artist, Score: score,
				})
			}
			continue
		}
		if len(res.Tracks) < limit {
			res.Tracks = append(res.Tracks, read.SearchHit{
				PID: model.PID(pid), Kind: "track", Title: title, Subtitle: artist, Score: score,
			})
		}
		if albumPID != "" && !albumSeen[albumPID] && len(res.Albums) < limit {
			albumSeen[albumPID] = true
			sub := albumArtist
			if sub == "" {
				sub = artist
			}
			res.Albums = append(res.Albums, read.SearchHit{
				PID: albumPID, Kind: "album", Title: album, Subtitle: sub, Score: score,
			})
		}
		if artistPID != "" && !artistSeen[artistPID] && len(res.Artists) < limit {
			artistSeen[artistPID] = true
			res.Artists = append(res.Artists, read.SearchHit{
				PID: artistPID, Kind: "artist", Title: artist, Score: score,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// Append transcript-body hits to the Episodes group, after the metadata hits, so
	// a title match outranks a body match. Episodes already surfaced by metadata are
	// skipped to avoid duplicates.
	if len(res.Episodes) < limit {
		if err := s.searchTranscripts(ctx, match, limit, opt.MaxCandidates, libIDs, opt.States, episodeSeen, res); err != nil {
			return nil, err
		}
	}
	return res, nil
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

// ftsMatchQuery turns a user search string into a safe FTS5 MATCH expression: it
// keeps only letter/digit runes (folding everything else to token boundaries),
// lowercases (which also neutralizes the AND/OR/NOT/NEAR operators, since FTS5
// keywords are uppercase-only), and emits each token as a prefix bareword joined
// by implicit AND. Because no quotes or syntax metacharacters survive, the result
// can never be a malformed or injected FTS expression. It returns "" when the
// input has no usable tokens.
func ftsMatchQuery(input string) string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(input) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	if len(tokens) == 0 {
		return ""
	}
	var b strings.Builder
	for i, tk := range tokens {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(tk)
		b.WriteByte('*') // prefix match for type-ahead friendliness
	}
	return b.String()
}
