package sqlite

import (
	"context"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file implements the audit.Store port: read-only quality queries that feed
// the auditor. Each returns raw candidate data; the audit package turns it into
// severity-ranked findings (and, for duplicates, merge suggestions).

// DuplicateArtists finds artist entities that likely should be one: they share an
// MBID (an enrichment collision left for the merge primitive) or normalize to the
// same collation sort key ("Beatles" vs "The Beatles"), which the match-key dedup
// keeps apart.
func (s *Store) DuplicateArtists(ctx context.Context) ([]model.DuplicateSet, error) {
	byMBID, err := s.artistDupSets(ctx, "mbid", "shared MBID")
	if err != nil {
		return nil, err
	}
	bySort, err := s.artistDupSets(ctx, "sort_key", "same collation key")
	if err != nil {
		return nil, err
	}
	return append(byMBID, bySort...), nil
}

func (s *Store) artistDupSets(ctx context.Context, col, reason string) ([]model.DuplicateSet, error) {
	q := `SELECT e.` + col + `, e.pid, e.name, COALESCE(r.track_count,0)
		FROM artist e
		LEFT JOIN artist_rollup r ON r.artist_id = e.id
		WHERE e.` + col + ` IN (
			SELECT ` + col + ` FROM artist
			WHERE ` + col + ` IS NOT NULL AND ` + col + ` <> ''
			GROUP BY ` + col + ` HAVING COUNT(*) > 1)
		ORDER BY e.` + col + `, COALESCE(r.track_count,0) DESC, e.pid`
	return s.scanDupSets(ctx, q, model.MergeArtist, reason)
}

// DuplicateGenres finds genre/mood entities within one facet that share a
// collation sort key but were kept apart by a differing match key.
func (s *Store) DuplicateGenres(ctx context.Context) ([]model.DuplicateSet, error) {
	q := `SELECT e.facet || char(31) || e.sort_key, e.pid, e.name, COALESCE(r.track_count,0)
		FROM genre e
		LEFT JOIN genre_rollup r ON r.genre_id = e.id
		WHERE e.facet || char(31) || e.sort_key IN (
			SELECT facet || char(31) || sort_key FROM genre GROUP BY 1 HAVING COUNT(*) > 1)
		ORDER BY 1, COALESCE(r.track_count,0) DESC, e.pid`
	return s.scanDupSets(ctx, q, model.MergeGenre, "same collation key")
}

// DuplicateAlbums finds album entities that share an MBID (two heuristic album
// rows enrichment resolved to one release).
func (s *Store) DuplicateAlbums(ctx context.Context) ([]model.DuplicateSet, error) {
	// Order each MBID group by track count DESC so scanDupSets picks the album with
	// the most tracks as the survivor (re-pointing the fewest tracks), matching the
	// "survivor = most tracks" contract the artist/genre queries honor.
	q := `SELECT e.mbid, e.pid, e.title, (SELECT COUNT(*) FROM track t WHERE t.album_id = e.id) AS tc
		FROM album e
		WHERE e.mbid IN (
			SELECT mbid FROM album WHERE mbid IS NOT NULL AND mbid <> ''
			GROUP BY mbid HAVING COUNT(*) > 1)
		ORDER BY e.mbid, tc DESC, e.pid`
	return s.scanDupSets(ctx, q, model.MergeAlbum, "shared MBID")
}

// scanDupSets buckets rows ordered by a group key into DuplicateSets. Each row is
// (groupKey, pid, name, trackCount); the survivor (first member) is the highest
// track count within a group, so re-pointing moves the fewest children.
func (s *Store) scanDupSets(ctx context.Context, q string, et model.MergeEntity, reason string) ([]model.DuplicateSet, error) {
	rows, err := s.read.QueryContext(ctx, q)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.audit", err)
	}
	defer rows.Close()
	var sets []model.DuplicateSet
	var curKey string
	var cur *model.DuplicateSet
	for rows.Next() {
		var key, pid, name string
		var tc int
		if err := rows.Scan(&key, &pid, &name, &tc); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.audit", err)
		}
		if cur == nil || key != curKey {
			sets = append(sets, model.DuplicateSet{EntityType: et, Reason: reason})
			cur = &sets[len(sets)-1]
			curKey = key
		}
		cur.Members = append(cur.Members, model.DuplicateMember{PID: model.PID(pid), Name: name, TrackCount: tc})
	}
	return sets, rows.Err()
}

// SplitAlbums finds one album title by one artist spread across multiple album
// entities: the same normalized (album-artist, album title) maps to more than one
// album_id (its tracks split by folder or inconsistent tags).
func (s *Store) SplitAlbums(ctx context.Context) ([]model.SplitAlbum, error) {
	const q = `SELECT LOWER(t.album_artist) || char(31) || LOWER(t.album),
			t.album_artist, t.album, al.pid, al.title, COUNT(*)
		FROM track t
		JOIN album al ON al.id = t.album_id
		WHERE t.album <> '' AND t.album_artist <> '' AND t.album_id IS NOT NULL
		  AND LOWER(t.album_artist) || char(31) || LOWER(t.album) IN (
			SELECT LOWER(album_artist) || char(31) || LOWER(album) FROM track
			WHERE album <> '' AND album_artist <> '' AND album_id IS NOT NULL
			GROUP BY 1 HAVING COUNT(DISTINCT album_id) > 1)
		GROUP BY 1, al.id
		ORDER BY 1, COUNT(*) DESC`
	rows, err := s.read.QueryContext(ctx, q)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.SplitAlbums", err)
	}
	defer rows.Close()
	var out []model.SplitAlbum
	var curKey string
	var cur *model.SplitAlbum
	for rows.Next() {
		var key, artist, title, pid, albTitle string
		var tc int
		if err := rows.Scan(&key, &artist, &title, &pid, &albTitle, &tc); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.SplitAlbums", err)
		}
		if cur == nil || key != curKey {
			out = append(out, model.SplitAlbum{Artist: artist, Title: title})
			cur = &out[len(out)-1]
			curKey = key
		}
		cur.Albums = append(cur.Albums, model.DuplicateMember{PID: model.PID(pid), Name: albTitle, TrackCount: tc})
	}
	return out, rows.Err()
}

// InconsistentAlbums finds album entities whose member tracks disagree on
// metadata that is not part of the album identity key: the compilation flag and
// disc total. Year and album-artist are part of album identity, so tracks that
// disagree on them land in separate album rows, which SplitAlbums reports as a
// split album. A mixed compilation flag or disc total within one album is a real
// tagging inconsistency worth surfacing.
func (s *Store) InconsistentAlbums(ctx context.Context) ([]model.AlbumIssue, error) {
	const q = `SELECT al.pid, al.title,
			COUNT(DISTINCT t.compilation),
			COUNT(DISTINCT NULLIF(t.disc_total,0))
		FROM album al
		JOIN track t ON t.album_id = al.id
		JOIN playable_item pi ON pi.id = t.item_id
		WHERE pi.kind = 'track'
		GROUP BY al.id
		HAVING COUNT(DISTINCT t.compilation) > 1 OR COUNT(DISTINCT NULLIF(t.disc_total,0)) > 1
		ORDER BY al.sort_key`
	rows, err := s.read.QueryContext(ctx, q)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.InconsistentAlbums", err)
	}
	defer rows.Close()
	var out []model.AlbumIssue
	for rows.Next() {
		var pid, title string
		var comps, discs int
		if err := rows.Scan(&pid, &title, &comps, &discs); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.InconsistentAlbums", err)
		}
		var parts []string
		if comps > 1 {
			parts = append(parts, "mixed compilation flag")
		}
		if discs > 1 {
			parts = append(parts, plural(discs, "distinct disc total"))
		}
		out = append(out, model.AlbumIssue{
			AlbumPID: model.PID(pid), Title: title, Problem: strings.Join(parts, ", "),
		})
	}
	return out, rows.Err()
}

func plural(n int, noun string) string {
	s := ""
	if n != 1 {
		s = "s"
	}
	return strconv.Itoa(n) + " " + noun + s
}

// itemsMissingArtWhere is the shared predicate for the missing-art count and
// sample: a present track/book with no cover anywhere in the v1.0 art chain
// (its own map, its album's direct or track-derived cover, or the release-group
// cover enrichment populates). Artist/genre rungs have no v1.0 art source, so
// they are not checked (they are always empty and would false-positive nothing).
const itemsMissingArtWhere = `
	FROM playable_item pi
	LEFT JOIN track t ON t.item_id = pi.id
	LEFT JOIN album al ON al.id = t.album_id
	WHERE pi.kind IN ('track','book') AND pi.state = 'present'
	  AND NOT EXISTS (SELECT 1 FROM art_map am WHERE am.entity_type='track' AND am.entity_id=pi.id)
	  AND NOT EXISTS (SELECT 1 FROM art_map am WHERE am.entity_type='album' AND am.entity_id=t.album_id)
	  AND NOT EXISTS (SELECT 1 FROM art_map am JOIN track t2 ON t2.item_id=am.entity_id
			WHERE am.entity_type='track' AND t2.album_id=t.album_id)
	  AND NOT EXISTS (SELECT 1 FROM art_map am WHERE am.entity_type='release_group' AND am.entity_id=al.release_group_id)`

// ItemsMissingArt returns a sample (up to limit) of present items with no
// resolvable cover, plus the total count.
func (s *Store) ItemsMissingArt(ctx context.Context, limit int) ([]model.ItemRef, int, error) {
	var total int
	if err := s.read.QueryRowContext(ctx, "SELECT COUNT(*) "+itemsMissingArtWhere).Scan(&total); err != nil {
		return nil, 0, waxerr.Wrap(waxerr.CodeIO, "store.ItemsMissingArt", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	rows, err := s.read.QueryContext(ctx,
		"SELECT pi.pid, pi.title, pi.kind "+itemsMissingArtWhere+" ORDER BY pi.sort_key LIMIT ?", limit)
	if err != nil {
		return nil, 0, waxerr.Wrap(waxerr.CodeIO, "store.ItemsMissingArt", err)
	}
	defer rows.Close()
	var out []model.ItemRef
	for rows.Next() {
		var pid, title, kind string
		if err := rows.Scan(&pid, &title, &kind); err != nil {
			return nil, 0, waxerr.Wrap(waxerr.CodeIO, "store.ItemsMissingArt", err)
		}
		out = append(out, model.ItemRef{PID: model.PID(pid), Title: title, Kind: model.Kind(kind)})
	}
	return out, total, rows.Err()
}

// CountItemsMissingReplayGain counts audio files with no essence-matched loudness
// measurement, restricted to the files the analyze pass actually processes. It
// mirrors the analyze selection (audio, has an essence hash, not in the internal
// podcast library), so it never reports podcast episodes as fixable by
// `waxbin analyze`, which skips them. On an un-analyzed music catalog this is
// every track/book file, so the audit reports it at info severity.
func (s *Store) CountItemsMissingReplayGain(ctx context.Context) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM file f
		WHERE f.kind = 'audio' AND f.essence_hash IS NOT NULL
		  AND f.library_id NOT IN (SELECT id FROM library WHERE mode = 'podcast')
		  AND NOT EXISTS (SELECT 1 FROM loudness l WHERE l.essence_hash = f.essence_hash)`).Scan(&n)
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.CountItemsMissingReplayGain", err)
	}
	return n, nil
}

// AuditFiles returns every catalogued file's path, kind, content hash, and owning
// item, for the filesystem-level checks (bad filenames, orphan sidecars, path
// conflicts, integrity/corrupt audio). These are file-level checks, so it yields
// exactly one row per file.
//
// The primary-file join is gated on if2.start_frames IS NULL, mirroring the portable
// export: a single-file CUE album's shared file is backed by N virtual-track primary
// edges, so an ungated join returns that file N times. Every consumer then treats one
// file as N: the path-conflict check groups those rows by folded path, finds more than
// one, and reports the file as colliding with ITSELF at error severity, which exits
// the CLI non-zero for anyone with a rip; integrity re-hashes the same bytes N times
// and inflates FilesChecked; corrupt-audio re-decodes them N times; and any real
// finding is emitted N times over.
//
// Gating to whole-file edges yields one (or zero) primary row per file, so a
// virtual-track-backed file reports an empty owning item rather than an arbitrary
// sibling. That is the truthful answer: N items share the file and none of them owns
// it, and a finding about the file must not be pinned on whichever track sorted
// first. The file itself is still audited, since the join is LEFT and the file row
// survives with no item, and every finding still carries its path.
func (s *Store) AuditFiles(ctx context.Context) ([]model.AuditFileInfo, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT f.pid, f.path, f.display_path, f.kind, f.content_hash,
			COALESCE(pi.pid,'')
		FROM file f
		LEFT JOIN item_file if2 ON if2.file_id = f.id AND if2.role = 'primary' AND if2.start_frames IS NULL
		LEFT JOIN playable_item pi ON pi.id = if2.item_id
		ORDER BY f.id`)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.AuditFiles", err)
	}
	defer rows.Close()
	var out []model.AuditFileInfo
	for rows.Next() {
		var fi model.AuditFileInfo
		var pid, kind, itemPID string
		if err := rows.Scan(&pid, &fi.Path, &fi.DisplayPath, &kind, &fi.ContentHash, &itemPID); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.AuditFiles", err)
		}
		fi.PID = model.PID(pid)
		fi.Kind = model.FileKind(kind)
		fi.ItemPID = model.PID(itemPID)
		out = append(out, fi)
	}
	return out, rows.Err()
}

// DerivedDrift runs the derived-data consistency check and returns its drift
// counts (FTS/rollups/sort keys), mapped to a model type so the audit can fold it
// in without depending on the store's report type.
func (s *Store) DerivedDrift(ctx context.Context) (model.DerivedDrift, error) {
	rep, err := s.VerifyDerived(ctx)
	if err != nil {
		return model.DerivedDrift{}, err
	}
	return model.DerivedDrift{
		ItemsMissingFTS:         rep.ItemsMissingFTS,
		OrphanFTSRows:           rep.OrphanFTSRows,
		ArtistRollupDrift:       rep.ArtistRollupDrift,
		GenreRollupDrift:        rep.GenreRollupDrift,
		ReleaseGroupRollupDrift: rep.ReleaseGroupRollupDrift,
		SortKeyDrift:            rep.SortKeyDrift,
		BookDurationDrift:       rep.BookDurationDrift,
	}, nil
}
