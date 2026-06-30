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

// Search runs a grouped, BM25-ranked metadata search. It queries the metadata FTS
// once with field weighting, then derives the artist/album/track groups from the
// ranked matches: a matched track contributes itself to Tracks and its album and
// artist entities to Albums and Artists (best score wins, ties broken by scan
// order). Episode hits are reserved for transcript-backed podcast search and stay
// empty until transcripts are indexed. A query with no usable tokens returns an
// empty result, not an error.
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

	stmt := `SELECT pi.pid, pi.title, COALESCE(t.artist,''), COALESCE(t.album_artist,''),
		COALESCE(t.album,''), COALESCE(art.pid,''), COALESCE(al.pid,''), ` + searchBM25 + ` AS score
		FROM search_fts
		JOIN playable_item pi ON pi.id = search_fts.rowid
		JOIN track t ON t.item_id = pi.id
		LEFT JOIN artist art ON art.id = t.artist_id
		LEFT JOIN album al ON al.id = t.album_id
		WHERE search_fts MATCH ?
		ORDER BY score, pi.pid
		LIMIT ?`

	// Fetch one past the cap so a full result set signals truncation rather than
	// silently dropping lower-ranked albums/artists. score is tie-broken by pid so
	// equal-score rows come back in a stable, deterministic order.
	cap := searchFetchCap(limit)
	rows, err := s.read.QueryContext(ctx, stmt, match, cap+1)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()

	albumSeen := map[model.PID]bool{}
	artistSeen := map[model.PID]bool{}
	scanned := 0
	for rows.Next() {
		scanned++
		if scanned > cap {
			res.Truncated = true
			break
		}
		var pid, title, artist, albumArtist, album string
		var artistPID, albumPID model.PID
		var score float64
		if err := rows.Scan(&pid, &title, &artist, &albumArtist, &album, &artistPID, &albumPID, &score); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
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
	return res, nil
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
