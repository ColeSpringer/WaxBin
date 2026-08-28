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

// DuplicateAlbums finds album entities that share a MusicBrainz release id.
func (s *Store) DuplicateAlbums(ctx context.Context) ([]model.DuplicateSet, error) {
	// Order each group by track count DESC so scanDupSets picks the album with the
	// most tracks as the survivor (re-pointing the fewest tracks), matching the
	// "survivor = most tracks" contract the artist/genre queries honor.
	return s.scanDupSets(ctx, effectiveMBIDDupQuery("album",
		"(SELECT COUNT(*) FROM track t WHERE t.album_id = k.id)"),
		model.MergeAlbum, "shared MBID")
}

// DuplicateReleaseGroups finds release-group entities that share a MusicBrainz
// release-group id. Ordered by member-album count DESC, which is the group rung's
// reading of "survivor = the row holding the most", so a merge repoints the fewest
// albums.
func (s *Store) DuplicateReleaseGroups(ctx context.Context) ([]model.DuplicateSet, error) {
	return s.scanDupSets(ctx, effectiveMBIDDupQuery("release_group",
		"(SELECT COUNT(*) FROM album a WHERE a.release_group_id = k.id)"),
		model.MergeReleaseGroup, "shared MBID")
}

// effectiveMBIDDupQuery builds the duplicate scan for an entity that keys MBID-first,
// grouping on an EFFECTIVE id: the mbid column when it is filled, otherwise the id
// carried in an mbid: match_key. Grouping on the column alone would miss exactly the
// population these finders exist for, a pair where one row holds the id in its column
// (enrichment put it there without moving the key) and the other in its key.
//
// The bare = grouping is only right because every write site folds the id to lowercase
// (normMBID) and identity's keys already do; a case-divergent spelling would read here
// as a different identity. table and count are caller constants, never caller text.
func effectiveMBIDDupQuery(table, count string) string {
	return `WITH keyed AS (
			SELECT id, pid, title,
				COALESCE(NULLIF(mbid,''),
					CASE WHEN match_key LIKE 'mbid:%' THEN substr(match_key, 6) END) AS mb
			FROM ` + table + `)
		SELECT k.mb, k.pid, k.title, ` + count + ` AS n
		FROM keyed k
		WHERE k.mb IS NOT NULL AND k.mb <> '' AND k.mb IN (
			SELECT mb FROM keyed WHERE mb IS NOT NULL AND mb <> ''
			GROUP BY mb HAVING COUNT(*) > 1)
		ORDER BY k.mb, n DESC, k.pid`
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
// sample: a present track/book with no front cover anywhere in the v1.0 art chain
// (its own map, its album's direct or track-derived cover, or the release-group
// cover enrichment populates). The role filter matters now that an entity can
// carry several slots: a lone booklet scan is not a cover, so it must not hide
// the item from this report. That is the same front-only predicate the has_art
// query field applies (fields.go). Artist/genre rungs are not checked: nothing in a
// scan or enrich pass fills them, so in practice they are empty. A user-set artist or
// genre cover does resolve for the items under it, so an item covered only that way is
// still reported here; covering that would mean restating the resolver's artist rung
// (the release group's artist, else the album artist, else the track artist) in SQL and
// keeping the two in step. A playlist is an art entity too, but it is deliberately
// absent here: this report is item-scoped, and a playlist without a cover is normal
// rather than a finding.
// These arms stay track/book-scoped with the literal 'track' slot; the
// kind-switched slot expression (itemArtSlotExpr) exists for predicates that
// also cover episodes.
const itemsMissingArtWhere = `
	FROM playable_item pi
	LEFT JOIN track t ON t.item_id = pi.id
	LEFT JOIN album al ON al.id = t.album_id
	WHERE pi.kind IN ('track','book') AND pi.state = 'present'
	  AND NOT EXISTS (SELECT 1 FROM art_map am WHERE am.entity_type='track' AND am.entity_id=pi.id AND am.role='front')
	  AND NOT EXISTS (SELECT 1 FROM art_map am WHERE am.entity_type='album' AND am.entity_id=t.album_id AND am.role='front')
	  AND NOT EXISTS (SELECT 1 FROM art_map am JOIN track t2 ON t2.item_id=am.entity_id
			WHERE am.entity_type='track' AND am.role='front' AND t2.album_id=t.album_id)
	  AND NOT EXISTS (SELECT 1 FROM art_map am WHERE am.entity_type='release_group' AND am.entity_id=al.release_group_id AND am.role='front')`

// sampleItemRefs is the count-then-sample shape the list-style audit checks share.
// where is a package constant beginning at FROM, never caller text; a non-positive
// limit counts without sampling. The pid tiebreak is load-bearing: sort_key is not
// unique, so ordering by it alone returns a different subset run to run.
func (s *Store) sampleItemRefs(ctx context.Context, op, where string, limit int) ([]model.ItemRef, int, error) {
	var total int
	if err := s.read.QueryRowContext(ctx, "SELECT COUNT(*) "+where).Scan(&total); err != nil {
		return nil, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if total == 0 || limit <= 0 {
		return nil, total, nil
	}
	rows, err := s.read.QueryContext(ctx,
		"SELECT pi.pid, pi.title, pi.kind "+where+" ORDER BY pi.sort_key, pi.pid LIMIT ?", limit)
	if err != nil {
		return nil, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.ItemRef
	for rows.Next() {
		var pid, title, kind string
		if err := rows.Scan(&pid, &title, &kind); err != nil {
			return nil, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, model.ItemRef{PID: model.PID(pid), Title: title, Kind: model.Kind(kind)})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, total, nil
}

// ItemsMissingArt returns a sample (up to limit) of present items with no
// resolvable cover, plus the total count.
func (s *Store) ItemsMissingArt(ctx context.Context, limit int) ([]model.ItemRef, int, error) {
	return s.sampleItemRefs(ctx, "store.ItemsMissingArt", itemsMissingArtWhere, limit)
}

// itemsMissingMBIDWhere selects present tracks and books with no MusicBrainz identity
// anywhere on the chain: not their own recording/release id, and not their album's or
// release group's. A track whose release group enrichment matched is therefore not
// reported, the way the missing-art predicate walks its own fallback chain. Episodes
// stay out: a podcast's identity is a feed GUID and no MusicBrainz id will ever exist
// for one.
//
// artist.mbid is deliberately not on the chain even though an item resolves through an
// artist. Coverage here means the item can be resolved to a specific recording,
// release, or release group. An artist id identifies the credited artist and says
// nothing about which of their works this is, so counting it would report clean on
// items nothing can be fetched for.
//
// Unlike itemsMissingArtWhere this joins book, which cannot fan the row out: book is
// unique on item_id (upsertBook's ON CONFLICT(item_id)), so it is at most one row like
// every other join here.
const itemsMissingMBIDWhere = `
	FROM playable_item pi
	LEFT JOIN track t ON t.item_id = pi.id
	LEFT JOIN book bk ON bk.item_id = pi.id
	LEFT JOIN album al ON al.id = t.album_id
	LEFT JOIN release_group rg ON rg.id = al.release_group_id
	WHERE pi.kind IN ('track','book') AND pi.state = 'present'
	  AND COALESCE(NULLIF(t.mbid,''), NULLIF(bk.mbid,'')) IS NULL
	  AND COALESCE(NULLIF(al.mbid,''), NULLIF(rg.mbid,'')) IS NULL`

// ItemsMissingMBID returns a sample (up to limit) of present items with no
// MusicBrainz identity, plus the total count.
// It samples per item like ItemsMissingArt rather than returning a bare count,
// because the pid is what a caller acts on. That makes it the loudest check on an
// untagged library, which is accepted: Sample bounds it.
func (s *Store) ItemsMissingMBID(ctx context.Context, limit int) ([]model.ItemRef, int, error) {
	return s.sampleItemRefs(ctx, "store.ItemsMissingMBID", itemsMissingMBIDWhere, limit)
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
		BookISBNKeyDrift:        rep.BookISBNKeyDrift,
	}, nil
}
