package sqlite

import (
	"context"
	"sort"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// Stats assembles a library summary from the shared Facet primitive (genre/
// artist/year distributions), simple entity counts, and per-user play_state.
// topN bounds the top-genres/top-artists/most-played lists. An empty userPID uses
// the default user. Stats is read-only.
func (s *Store) Stats(ctx context.Context, userPID model.PID, topN int) (*read.Stats, error) {
	const op = "store.Stats"
	if topN <= 0 {
		topN = 10
	}
	out := &read.Stats{}

	// Entity counts and total duration. Count only entities that still back a
	// track, keeping these totals consistent with Facet-derived top lists. Retags
	// can leave orphaned entity rows behind until a later cleanup pass.
	counts := []struct {
		dst *int
		q   string
	}{
		{&out.Items, "SELECT COUNT(*) FROM playable_item WHERE kind = 'track'"},
		{&out.Books, "SELECT COUNT(*) FROM playable_item WHERE kind = 'book'"},
		// An artist counts if it backs a track OR is a book's author. This mirrors the
		// GroupArtist facet exactly (COALESCE(t.artist_id, bk.author_id)), so the
		// headline count matches the facet's bucket set. A narrator/translator/editor
		// that is never also an author or track artist is intentionally not counted,
		// because the facet does not surface them.
		{&out.Artists, `SELECT COUNT(*) FROM artist a WHERE
			EXISTS (SELECT 1 FROM track t WHERE t.artist_id = a.id OR t.album_artist_id = a.id)
			OR EXISTS (SELECT 1 FROM book b WHERE b.author_id = a.id)`},
		{&out.ReleaseGroups, `SELECT COUNT(*) FROM release_group rg WHERE EXISTS
			(SELECT 1 FROM album al JOIN track t ON t.album_id = al.id WHERE al.release_group_id = rg.id)`},
		{&out.Albums, "SELECT COUNT(*) FROM album al WHERE EXISTS (SELECT 1 FROM track t WHERE t.album_id = al.id)"},
		{&out.Genres, `SELECT COUNT(*) FROM genre g WHERE g.facet = 'genre' AND EXISTS
			(SELECT 1 FROM item_genre ig WHERE ig.genre_id = g.id)`},
	}
	for _, c := range counts {
		if err := s.read.QueryRowContext(ctx, c.q).Scan(c.dst); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	// Sum every music/audiobook item's files (all parts of a multi-file audiobook,
	// the single file of a track), so the library total reflects full running times.
	// Podcast episodes are excluded to match the track/book item counts above (they
	// are a separate medium surfaced via the podcast commands, not the music totals).
	if err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(f.duration_ms), 0) FROM item_file pf
		 JOIN file f ON f.id = pf.file_id
		 JOIN playable_item pi ON pi.id = pf.item_id
		 WHERE pi.kind <> 'episode'`).Scan(&out.TotalDuration); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// Distributions via the canonical Facet primitive, so stats and browse agree.
	// These are catalog-wide, deliberately user-agnostic (the query references no
	// per-user field), so the empty user is a no-op.
	all := query.New(query.EntityItems).Build()
	genreFacet, err := s.Facet(ctx, all, read.GroupGenre, "")
	if err != nil {
		return nil, err
	}
	out.TopGenres = topBuckets(genreFacet.Buckets, topN)
	artistFacet, err := s.Facet(ctx, all, read.GroupArtist, "")
	if err != nil {
		return nil, err
	}
	out.TopArtists = topBuckets(artistFacet.Buckets, topN)
	yearFacet, err := s.Facet(ctx, all, read.GroupYear, "")
	if err != nil {
		return nil, err
	}
	out.ByYear = yearFacet.Buckets // already chronological from the facet sort

	ps, err := s.playStats(ctx, userPID, topN)
	if err != nil {
		return nil, err
	}
	out.Play = ps
	return out, nil
}

// playStats reads the per-user, play-derived figures from play_state (indexed
// queries, never the rollups).
func (s *Store) playStats(ctx context.Context, userPID model.PID, topN int) (read.PlayStats, error) {
	const op = "store.Stats"
	var ps read.PlayStats
	userID, err := userIDByPID(ctx, s.read, userPID, op)
	if err != nil {
		return ps, err
	}
	var name string
	if err := s.read.QueryRowContext(ctx, "SELECT name FROM user WHERE id = ?", userID).Scan(&name); err != nil {
		return ps, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	ps.User = name

	if err := s.read.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(play_count), 0),
		COALESCE(SUM(CASE WHEN finished = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN starred_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM play_state WHERE user_id = ?`, userID).Scan(&ps.TotalPlays, &ps.Finished, &ps.Starred); err != nil {
		return ps, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// COALESCE the LEFT JOINed artist with the book author, so a played audiobook
	// shows its author rather than a blank artist (a book has no track row).
	rows, err := s.read.QueryContext(ctx, `SELECT pi.pid, pi.title,
		COALESCE(NULLIF(t.artist,''), bk.author, ''), p.play_count
		FROM play_state p
		JOIN playable_item pi ON pi.id = p.item_id
		LEFT JOIN track t ON t.item_id = pi.id
		LEFT JOIN book bk ON bk.item_id = pi.id
		WHERE p.user_id = ? AND p.play_count > 0
		ORDER BY p.play_count DESC, pi.sort_key LIMIT ?`, userID, topN)
	if err != nil {
		return ps, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var it read.PlayedItem
		if err := rows.Scan(&it.PID, &it.Title, &it.Artist, &it.PlayCount); err != nil {
			return ps, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		ps.MostPlayed = append(ps.MostPlayed, it)
	}
	return ps, rows.Err()
}

// YearInReview builds a per-user listening recap for one calendar year (UTC
// boundaries) from play_session history: session/minute/track totals, the tracks
// added that year, and the top artists/genres/tracks by play count. topN bounds
// each top list. An empty userPID uses the default user. Read-only.
func (s *Store) YearInReview(ctx context.Context, userPID model.PID, year, topN int) (*read.YearReview, error) {
	const op = "store.YearInReview"
	// Unix-nanosecond timestamps only span ~1678..2262, so a year outside that range
	// yields wrapped/garbage bounds. Reject it rather than return a silently bogus recap.
	if year < 1678 || year > 2261 {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "year out of range (1678-2261)")
	}
	if topN <= 0 {
		topN = 10
	}
	userID, err := userIDByPID(ctx, s.read, userPID, op)
	if err != nil {
		return nil, err
	}
	var name string
	if err := s.read.QueryRowContext(ctx, "SELECT name FROM user WHERE id = ?", userID).Scan(&name); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	lo := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	hi := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	yr := &read.YearReview{Year: year, User: name}

	// The recap covers the music/audiobook library, excluding podcast episodes, so
	// every figure (totals AND the top lists, which key on the artist/genre entities
	// episodes lack) is consistent with the catalog Stats, which also excludes episodes.
	var msPlayed int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(ps.ms_played),0), COUNT(DISTINCT ps.item_id)
		 FROM play_session ps JOIN playable_item pi ON pi.id = ps.item_id
		 WHERE ps.user_id = ? AND ps.started_at >= ? AND ps.started_at < ? AND pi.kind IN ('track','book')`,
		userID, lo, hi).Scan(&yr.Sessions, &msPlayed, &yr.TracksPlayed); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	yr.MinutesPlayed = msPlayed / 60000

	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playable_item WHERE kind IN ('track','book')
		 AND created_at >= ? AND created_at < ?`, lo, hi).Scan(&yr.NewInLibrary); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	yr.TopArtists, err = s.yearBuckets(ctx, `SELECT a.pid, a.name, COUNT(*)
		FROM play_session ps
		JOIN playable_item pi ON pi.id = ps.item_id
		LEFT JOIN track t ON t.item_id = pi.id
		LEFT JOIN book bk ON bk.item_id = pi.id
		JOIN artist a ON a.id = COALESCE(t.artist_id, bk.author_id)
		WHERE ps.user_id = ? AND ps.started_at >= ? AND ps.started_at < ? AND pi.kind IN ('track','book')
		GROUP BY a.id ORDER BY COUNT(*) DESC, a.sort_key LIMIT ?`, userID, lo, hi, topN)
	if err != nil {
		return nil, err
	}
	yr.TopGenres, err = s.yearBuckets(ctx, `SELECT g.pid, g.name, COUNT(*)
		FROM play_session ps
		JOIN playable_item pi ON pi.id = ps.item_id
		JOIN item_genre ig ON ig.item_id = ps.item_id
		JOIN genre g ON g.id = ig.genre_id
		WHERE ps.user_id = ? AND ps.started_at >= ? AND ps.started_at < ?
		  AND pi.kind IN ('track','book') AND g.facet = 'genre'
		GROUP BY g.id ORDER BY COUNT(*) DESC, g.sort_key LIMIT ?`, userID, lo, hi, topN)
	if err != nil {
		return nil, err
	}

	rows, err := s.read.QueryContext(ctx, `SELECT pi.pid, pi.title,
		COALESCE(NULLIF(t.artist,''), bk.author, ''), COUNT(*)
		FROM play_session ps
		JOIN playable_item pi ON pi.id = ps.item_id
		LEFT JOIN track t ON t.item_id = pi.id
		LEFT JOIN book bk ON bk.item_id = pi.id
		WHERE ps.user_id = ? AND ps.started_at >= ? AND ps.started_at < ? AND pi.kind IN ('track','book')
		GROUP BY pi.id ORDER BY COUNT(*) DESC, pi.sort_key LIMIT ?`, userID, lo, hi, topN)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var it read.PlayedItem
		if err := rows.Scan(&it.PID, &it.Title, &it.Artist, &it.PlayCount); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		yr.TopTracks = append(yr.TopTracks, it)
	}
	return yr, rows.Err()
}

// yearBuckets runs a (pid, name, count) aggregation and returns display buckets.
func (s *Store) yearBuckets(ctx context.Context, q string, args ...any) ([]read.Bucket, error) {
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.YearInReview", err)
	}
	defer rows.Close()
	var out []read.Bucket
	for rows.Next() {
		var pid, disp string
		var count int
		if err := rows.Scan(&pid, &disp, &count); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.YearInReview", err)
		}
		out = append(out, read.Bucket{Key: pid, Display: disp, Count: count, EntityPID: model.PID(pid)})
	}
	return out, rows.Err()
}

// topBuckets returns the n highest-count buckets, ties broken by display so the
// order is deterministic. The facet returns buckets in sort-key order; stats want
// them by magnitude.
func topBuckets(buckets []read.Bucket, n int) []read.Bucket {
	sorted := append([]read.Bucket(nil), buckets...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Count != sorted[j].Count {
			return sorted[i].Count > sorted[j].Count
		}
		return sorted[i].Display < sorted[j].Display
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}
