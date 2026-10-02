package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// This file implements the enrich.Store port: the iteration queries that feed the
// enrichment pass, the transactional apply methods that persist provider results
// (MBID-first, lock-respecting, provenance-recording), the response cache, and the
// coverage report. Enrichment adds entity data and fills gaps; it never overwrites
// a tagged or locked field.

const (
	// enrichProviderMusicBrainz is the provenance id for the identity spine: artist,
	// release-group, and book identity are always resolved by MusicBrainz.
	enrichProviderMusicBrainz = "musicbrainz"
	// enrichEntityLyrics is the entity_enrichment.entity_type for a per-recording
	// lyrics lookup marker, keyed by the track's item id. It is distinct from the three
	// entity types so a no-match lyrics lookup is not re-queried every run; the coverage
	// report reads it per track (a looked-up track with no lyrics), not as an entity.
	enrichEntityLyrics = "lyrics"
	// enrichEntityGroupArt is the entity_enrichment.entity_type for the group-art
	// backfill's auxiliary half, keyed by the release group's own id.
	//
	// The type column carries two vocabularies at once. Four values name an entity the
	// coverage report counts or an album's release match (artist, release_group, book,
	// album); the rest are per-pass markers borrowing the table for their own
	// granularity, keyed by whatever id that pass walks (lyrics by item id, this one by
	// release-group id). A new pass needs its own value rather than sharing an entity's:
	// notEnriched keys on (entity_type, entity_id) alone, so a shared type makes one
	// pass's no-match silence another pass's queue. The cost of a foreign type is that
	// the row outlives the entity unless someone deletes it, which is what
	// deleteArtBackfillMarkerTx is for.
	enrichEntityGroupArt = "group_art"
	// enrichEntityGroupFront is the entity_enrichment.entity_type for the group-art
	// backfill's front half, keyed by the release group's own id. The halves keep apart
	// for what model.ReleaseGroupArtBackfill says, and deleteArtBackfillMarkerTx drops both.
	enrichEntityGroupFront = "group_front"
	// enrichEntityArtistArt is the entity_enrichment.entity_type for the artist-art
	// backfill's auxiliary half, keyed by the artist's own id. Its own value for the
	// reason above: sharing the artist entity's type would let the identity pass's marker
	// silence this queue.
	enrichEntityArtistArt = "artist_art"
	// enrichEntityArtistFront is the entity_enrichment.entity_type for the artist-art
	// backfill's front half, keyed by the artist's own id. The halves keep apart as the
	// group's do, and deleteArtBackfillMarkerTx drops both.
	enrichEntityArtistFront = "artist_front"
	// enrichEntityAlbumArt is the entity_enrichment.entity_type for the album-art
	// backfill's auxiliary half, keyed by the album's own id. Its own value for the reason
	// above: sharing the album entity's type would let the release match's marker silence
	// this queue, and a released album that never matched would then never be asked about
	// its art.
	enrichEntityAlbumArt = "album_art"
	// enrichEntityAlbumFront is the entity_enrichment.entity_type for the album-art
	// backfill's front half, keyed by the album's own id; deleteArtBackfillMarkerTx drops
	// both halves.
	//
	// It accepts one tolerance the artist and release-group rungs accept too. A member
	// track's embedded cover stripped by a rescan opens an album front vacancy this
	// marker keeps closed until it expires (a miss) or until evidence arrives (a match):
	// the marker is keyed on the album, not on where the front came from, and a scan has
	// no cheap way to tell the two apart.
	enrichEntityAlbumFront = "album_front"
	// enrichEntityFields is the entity_enrichment.entity_type for the item-rung fields
	// walk, keyed by the item id. Tracks and books share the id space and share this
	// marker, which is right: an item is one kind and only ever walks one of the two
	// phases. Its own value for the reason above.
	enrichEntityFields = "fields"
	// enrichEntityAlbumFields is the entity_enrichment.entity_type for the album-rung
	// fields walk, keyed by the album's own id. Separate from the album release match's
	// "album" type so neither pass's no-match silences the other's queue.
	enrichEntityAlbumFields = "fields_album"
	// enrichProviderNone labels a marker no provider answered. entity_enrichment.provider
	// is NOT NULL and an art backfill regularly completes with nothing offered, so the row
	// names the outcome rather than storing an empty string a reader would take for a
	// missing value.
	enrichProviderNone = model.EnrichProviderNone
)

// albumResolvesFrontArt is true when an album already answers a front-cover request: its
// own art_map row, or the member track's cover ResolveArt derives one from. It mirrors
// artInChain's album rung, so a caller can ask the question outside a read without
// disagreeing with what the resolver would actually return. It reads the album as al.
const albumResolvesFrontArt = `(EXISTS (SELECT 1 FROM art_map am
		WHERE am.entity_type = 'album' AND am.entity_id = al.id AND am.role = 'front')
	OR EXISTS (SELECT 1 FROM art_map tm JOIN track t ON t.item_id = tm.entity_id
		WHERE tm.entity_type = 'track' AND tm.role = 'front' AND t.album_id = al.id))`

// trackTakesGroupGenres is true for a member track the release-group genre fill reaches:
// no genre yet and no genre lock. The fill and the queue's NeedsGenres read the same
// predicate, so a group is owed its genre rider exactly when the fill has a track to
// reach. It reads the track as t.
const trackTakesGroupGenres = `NOT EXISTS (SELECT 1 FROM item_genre ig WHERE ig.item_id = t.item_id)
	AND NOT EXISTS (SELECT 1 FROM field_provenance fp WHERE fp.item_id = t.item_id AND fp.field = 'genre' AND fp.locked = 1)`

// albumMatchEvidence are the album columns the release matcher can decide on. One list
// because three places must agree: the enrichment queue predicate, and the two writers
// that clear a stale no-match marker when new evidence lands (the scan top-up and the
// entity edit). label is deliberately absent, since it feeds no tier.
var albumMatchEvidence = []string{"barcode", "catalog_number", "media", "country"}

// albumMatchEvidencePredicate builds the "carries some matchable evidence" SQL over an
// album alias.
func albumMatchEvidencePredicate(alias string) string {
	terms := make([]string, len(albumMatchEvidence))
	for i, col := range albumMatchEvidence {
		terms[i] = "COALESCE(" + alias + "." + col + ",'') <> ''"
	}
	return "(" + strings.Join(terms, " OR ") + ")"
}

// enrichArtistBacksItems restricts artist enrichment to artists that actually back
// a track (as artist or album artist) or credit a book, so ghost artists left by a
// retag are not looked up.
const enrichArtistBacksItems = `(EXISTS (SELECT 1 FROM track t WHERE t.artist_id = a.id OR t.album_artist_id = a.id)
	OR EXISTS (SELECT 1 FROM item_contributor ic WHERE ic.artist_id = a.id))`

// enrichRGBacksItems restricts release-group enrichment to groups that back at
// least one track.
const enrichRGBacksItems = `EXISTS (SELECT 1 FROM album al JOIN track t ON t.album_id = al.id WHERE al.release_group_id = rg.id)`

// enrichAlbumBacksItems restricts the album-art backfill to albums that still hold a
// track, the ghost heuristic the other art backfills carry.
const enrichAlbumBacksItems = `EXISTS (SELECT 1 FROM track t WHERE t.album_id = al.id)`

// enrichBacksFilter returns the backs-items predicate for a walk, neutralized
// ("1=1") when the walk is scoped to explicit ids: the heuristic protects a
// full pass from ghost entities, while an explicit scope must reach exactly
// what it names. The count query applies the same rule so the heartbeat
// denominator stays in lockstep.
func enrichBacksFilter(predicate string, ids []int64) string {
	if len(ids) > 0 {
		return "1=1"
	}
	return predicate
}

// notEnriched returns the SQL predicate selecting the targets one sweep should walk.
// idExpr is the entity's id column (book keys on item_id, not id).
//
// The sweeps read off one marker probe. SweepAll takes everything, which is the forced
// run. SweepFresh takes what has no marker. SweepDeferred takes the owed lookups that
// predate DeferredBefore, every one when it is zero. SweepRetry takes the settled no-match markers stamped at or before the
// cutoff, and nothing at all when no window is configured. SweepDue is the union of the
// three, phrased as "no marker that settles the target for this run", a match, a miss
// still inside its window, or a lookup owed since the run began, so it stays a single
// NOT EXISTS.
//
// The cutoff is spliced in as an integer literal rather than bound: the predicate is
// assembled by string like the ghost heuristic, and a bound parameter here would have
// to be threaded through ten call sites in positional order.
func notEnriched(entityType, idExpr string, opts model.EnrichQueueOptions) string {
	probe := "SELECT 1 FROM entity_enrichment ee WHERE ee.entity_type = '" +
		entityType + "' AND ee.entity_id = " + idExpr
	cutoff := strconv.FormatInt(opts.MissCutoff, 10)
	owedBefore := "ee.owed = 1"
	if opts.DeferredBefore != 0 {
		owedBefore += " AND ee.enriched_at < " + strconv.FormatInt(opts.DeferredBefore, 10)
	}
	switch opts.Sweep {
	case model.SweepAll:
		return "1=1"
	case model.SweepDeferred:
		return "EXISTS (" + probe + " AND " + owedBefore + ")"
	case model.SweepRetry:
		if opts.MissCutoff == 0 {
			return "1=0"
		}
		return "EXISTS (" + probe + " AND ee.owed = 0 AND ee.matched = 0 AND ee.enriched_at <= " + cutoff + ")"
	case model.SweepDue:
		settled := "ee.owed = 0 AND (ee.matched = 1 OR ee.enriched_at > " + cutoff + ")"
		if opts.MissCutoff == 0 {
			settled = "ee.owed = 0"
		}
		if opts.DeferredBefore != 0 {
			settled = "(" + settled + ") OR (ee.owed = 1 AND ee.enriched_at >= " + strconv.FormatInt(opts.DeferredBefore, 10) + ")"
		}
		return "NOT EXISTS (" + probe + " AND (" + settled + "))"
	}
	return "NOT EXISTS (" + probe + ")"
}

// enrichIDsFilter returns an "AND col IN (...)" clause with its bound args for a
// scoped iteration query. nil ids means no scope: the clause is "" so the
// unscoped statement stays byte-identical to the pre-scope form. An EMPTY
// non-nil slice is a scope with no targets and matches nothing, mirroring the
// service layer (which skips such a phase outright) instead of silently
// widening to a full-catalog walk. Scope lists come from one item or entity (a
// handful of ids), so they never approach the bound-parameter limit.
func enrichIDsFilter(col string, ids []int64) (string, []any) {
	if ids == nil {
		return "", nil
	}
	if len(ids) == 0 {
		return " AND 1=0", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return " AND " + col + " IN " + placeholders(len(ids)), args
}

// ArtistsNeedingEnrichment returns the next keyset page of artists to enrich.
// A non-nil ids list scopes the walk to those artist rowids (keyset shape
// kept), and drops the backs-items ghost heuristic: that filter exists to keep
// a full pass from wasting lookups on retag leftovers, but a scoped caller
// pointed at the artist deliberately, so it is reached even when nothing backs
// it anymore.
func (s *Store) ArtistsNeedingEnrichment(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.ArtistsNeedingEnrichment"
	scopeClause, scopeArgs := enrichIDsFilter("a.id", ids)
	stmt := `SELECT a.id, a.pid, a.name, COALESCE(a.mbid,'')
		FROM artist a
		WHERE a.id > ? AND ` + enrichBacksFilter(enrichArtistBacksItems, ids) + ` AND ` + notEnriched(model.EnrichArtistType, "a.id", opts) + scopeClause + `
		ORDER BY a.id LIMIT ?`
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: model.EnrichArtistType}
		var pid string
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.MBID); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReleaseGroupsNeedingEnrichment returns the next keyset page of release groups to
// enrich, each with its primary-artist name. When includeRepFile is set it also
// resolves a representative member file (path + duration) for the AcoustID fallback;
// otherwise that correlated lookup is skipped entirely (the common path). A non-nil
// ids list scopes the walk to those release-group rowids and, as with artists,
// drops the backs-items ghost heuristic for the explicit targets.
func (s *Store) ReleaseGroupsNeedingEnrichment(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, includeRepFile bool, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.ReleaseGroupsNeedingEnrichment"
	// The representative file's path and duration must come from ONE row, so a single
	// correlated subquery picks the file id (deterministically, lowest first) and the
	// join reads both columns from that same file. That keeps a path from ever pairing
	// with a duration read off a different file.
	repJoin, repCols := "", "X'', 0"
	if includeRepFile {
		repJoin = ` LEFT JOIN file rf ON rf.id = (
			SELECT pf.file_id FROM item_file pf
			JOIN track t ON t.item_id = pf.item_id
			JOIN album al ON al.id = t.album_id
			WHERE al.release_group_id = rg.id AND pf.role = 'primary'
			ORDER BY pf.file_id LIMIT 1)`
		repCols = "COALESCE(rf.path, X''), COALESCE(rf.duration_ms, 0)"
	}
	scopeClause, scopeArgs := enrichIDsFilter("rg.id", ids)
	stmt := `SELECT rg.id, rg.pid, rg.title, COALESCE(rg.mbid,''), COALESCE(ar.name,''), ` + repCols + `,
		EXISTS(SELECT 1 FROM entity_curation ec WHERE ec.entity_type = 'release_group'
		       AND ec.entity_id = rg.id AND ec.field = 'art' AND ec.locked = 1),
		` + groupEnrichmentFrontHash("rg.id") + `,
		EXISTS(SELECT 1 FROM art_map am WHERE am.entity_type = 'release_group'
		       AND am.entity_id = rg.id AND am.role = 'front'),
		EXISTS(SELECT 1 FROM album al JOIN track t ON t.album_id = al.id
		       WHERE al.release_group_id = rg.id AND ` + trackTakesGroupGenres + `)
		FROM release_group rg
		LEFT JOIN artist ar ON ar.id = rg.primary_artist_id` + repJoin + `
		WHERE rg.id > ? AND ` + enrichBacksFilter(enrichRGBacksItems, ids) + ` AND ` + notEnriched(model.EnrichReleaseGroupType, "rg.id", opts) + scopeClause + `
		ORDER BY rg.id LIMIT ?`
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: model.EnrichReleaseGroupType}
		var pid string
		var path []byte
		var durMS int64
		var artLocked int
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.MBID, &t.ArtistName, &path, &durMS, &artLocked,
			&t.GroupFrontHash, &t.HasArt, &t.NeedsGenres); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		t.FilePath = string(path)
		t.DurationSec = int(durMS / 1000)
		t.ArtLocked = artLocked == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

// BooksNeedingEnrichment returns audiobooks to enrich. It requires a non-empty
// mbid: MusicBrainz text search for audiobooks is unreliable, so a book is only
// enriched when it carries an explicit release id. A catalog with no book mbids
// therefore yields nothing and costs no lookups. A non-nil ids list scopes the
// walk to those book item rowids; unlike the artist/release-group ghost
// heuristic, the mbid requirement applies to a scoped walk too, because it is a
// capability gate, not a cost filter: without a release id there is no
// resolution path at all, so a scoped mbid-less book stays skipped (its
// contributors still enrich through the artist phase).
func (s *Store) BooksNeedingEnrichment(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.BooksNeedingEnrichment"
	scopeClause, scopeArgs := enrichIDsFilter("b.item_id", ids)
	stmt := `SELECT b.item_id, pi.pid, pi.title, COALESCE(b.mbid,''), COALESCE(b.author,'')
		FROM book b JOIN playable_item pi ON pi.id = b.item_id
		WHERE b.item_id > ? AND b.mbid IS NOT NULL AND b.mbid <> '' AND ` + notEnriched(model.EnrichBookType, "b.item_id", opts) + scopeClause + `
		ORDER BY b.item_id LIMIT ?`
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: model.EnrichBookType}
		var pid string
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.MBID, &t.ArtistName); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		out = append(out, t)
	}
	return out, rows.Err()
}

// AlbumsNeedingReleaseMatch returns the next keyset page of albums that could take a
// release MBID from evidence they already carry. The gate is four-part and
// self-limiting: the album has no mbid, its release group has one (that is what the
// lookup is constrained to), it carries one of albumMatchEvidence, and it has no album
// marker yet. A non-nil ids list scopes the walk to those album rowids.
//
// Two populations qualify for nothing, which keeps the phase cheap: a Picard-tagged
// library (they all have an mbid) and an untagged one (no evidence). What is left is
// albums carrying an identifier or a medium or a country but no MUSICBRAINZ_ALBUMID:
// partial retags, transcode-stripped files, and Discogs- or beets-derived tagging.
//
// It reads rg.mbid live rather than from a snapshot, so running this after the
// release-group phase in the same pass picks up the ids that phase just filled.
func (s *Store) AlbumsNeedingReleaseMatch(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.AlbumsNeedingReleaseMatch"
	scopeClause, scopeArgs := enrichIDsFilter("al.id", ids)
	stmt := `SELECT al.id, al.pid, al.title, rg.mbid, COALESCE(al.barcode,''), COALESCE(al.catalog_number,''),
			COALESCE(al.media,''), COALESCE(al.country,''), COALESCE(ar.name,'')
		FROM album al JOIN release_group rg ON rg.id = al.release_group_id
		LEFT JOIN artist ar ON ar.id = rg.primary_artist_id
		WHERE al.id > ?
		  AND (al.mbid IS NULL OR al.mbid = '')
		  AND rg.mbid IS NOT NULL AND rg.mbid <> ''
		  AND ` + albumMatchEvidencePredicate("al") + `
		  AND ` + notEnriched(model.EnrichAlbumType, "al.id", opts) + scopeClause + `
		ORDER BY al.id LIMIT ?`
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: model.EnrichAlbumType}
		var pid string
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.ReleaseGroupMBID, &t.Barcode, &t.CatalogNumber,
			&t.Media, &t.Country, &t.ArtistName); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountEntitiesNeedingEnrichment totals the entities the phases in opts.Phases would
// process, so the heartbeat can report a real ratio. It shares the queues' predicates, so
// a SweepDue count is the union of the fresh, owed and retry sweeps by construction
// rather than by three queries a later edit could let drift. Every phase is counted only
// when the run built it, the MusicBrainz-backed ones included. A non-nil scope filters
// each per-type count to its id list, and a type with an empty list contributes zero,
// because the scoped run skips that phase entirely; the denominator stays in lockstep
// with the work that actually runs.
func (s *Store) CountEntitiesNeedingEnrichment(ctx context.Context, q model.EnrichQueueOptions, opts model.EnrichCountOptions, scope *model.EnrichScope) (int, error) {
	const op = "store.CountEntitiesNeedingEnrichment"
	type countQuery struct {
		stmt string
		args []any
	}
	var queries []countQuery
	add := func(stmt, idCol string, ids []int64) {
		if scope != nil && len(ids) == 0 {
			return
		}
		clause, args := enrichIDsFilter(idCol, ids)
		queries = append(queries, countQuery{stmt + clause, args})
	}
	var artistIDs, rgIDs, albumIDs, bookIDs, lyricsIDs, fieldsIDs []int64
	if scope != nil {
		artistIDs, rgIDs, albumIDs = scope.ArtistIDs, scope.ReleaseGroupIDs, scope.AlbumIDs
		bookIDs, lyricsIDs, fieldsIDs = scope.BookItemIDs, scope.LyricsItemIDs, scope.FieldsItemIDs
	}
	runs := func(p model.EnrichPhase) bool { return slices.Contains(opts.Phases, p) }
	// A phase a phase-scoped force names walks every target, so its count has to as
	// well; the rest are counted under the run's own sweep.
	qFor := func(p model.EnrichPhase) model.EnrichQueueOptions {
		if slices.Contains(opts.Forced, p) {
			return model.EnrichQueueOptions{Sweep: model.SweepAll}
		}
		return q
	}
	// The MusicBrainz-backed phases, counted only when the run will execute them: a
	// contact-less run walks the port phases alone.
	if runs(model.EnrichPhaseArtist) {
		add(`SELECT COUNT(*) FROM artist a WHERE `+enrichBacksFilter(enrichArtistBacksItems, artistIDs)+` AND `+notEnriched(model.EnrichArtistType, "a.id", qFor(model.EnrichPhaseArtist)), "a.id", artistIDs)
	}
	if runs(model.EnrichPhaseReleaseGroup) {
		add(`SELECT COUNT(*) FROM release_group rg WHERE `+enrichBacksFilter(enrichRGBacksItems, rgIDs)+` AND `+notEnriched(model.EnrichReleaseGroupType, "rg.id", qFor(model.EnrichPhaseReleaseGroup)), "rg.id", rgIDs)
	}
	if runs(model.EnrichPhaseAlbumRelease) {
		add(`SELECT COUNT(*) FROM album al JOIN release_group rg ON rg.id = al.release_group_id
			WHERE (al.mbid IS NULL OR al.mbid = '') AND rg.mbid IS NOT NULL AND rg.mbid <> ''
			  AND `+albumMatchEvidencePredicate("al")+`
			  AND `+notEnriched(model.EnrichAlbumType, "al.id", qFor(model.EnrichPhaseAlbumRelease)), "al.id", albumIDs)
	}
	// The group-art backfill walks release groups, so it counts under the release-group
	// scope list, the ghost heuristic and the askable slots included, the way its queue
	// does.
	if runs(model.EnrichPhaseGroupArt) && opts.GroupArt.Any() {
		add(`SELECT COUNT(*) FROM release_group rg WHERE `+enrichBacksFilter(enrichRGBacksItems, rgIDs)+`
			  AND `+groupArtNeededPredicate(opts.GroupArt, qFor(model.EnrichPhaseGroupArt)), "rg.id", rgIDs)
	}
	// The artist backfill walks artists, so it counts under the artist scope list, the
	// ghost heuristic included, the way its queue does.
	if runs(model.EnrichPhaseArtistArt) && opts.ArtistArt.Any() {
		add(`SELECT COUNT(*) FROM artist a WHERE `+enrichBacksFilter(enrichArtistBacksItems, artistIDs)+`
			  AND `+artistArtNeededPredicate(opts.ArtistArt, qFor(model.EnrichPhaseArtistArt)), "a.id", artistIDs)
	}
	// The album-art backfill walks albums, so it counts under the album scope list, the
	// ghost heuristic and the askable slots included, the way its queue does.
	if runs(model.EnrichPhaseAlbumArt) && opts.AlbumArt.Any() {
		add(`SELECT COUNT(*) FROM album al WHERE `+enrichBacksFilter(enrichAlbumBacksItems, albumIDs)+`
			  AND `+albumArtNeededPredicate(opts.AlbumArt, qFor(model.EnrichPhaseAlbumArt)), "al.id", albumIDs)
	}
	if runs(model.EnrichPhaseBook) {
		add(`SELECT COUNT(*) FROM book b WHERE b.mbid IS NOT NULL AND b.mbid <> '' AND `+notEnriched(model.EnrichBookType, "b.item_id", qFor(model.EnrichPhaseBook)), "b.item_id", bookIDs)
	}
	if runs(model.EnrichPhaseLyrics) {
		add(`SELECT COUNT(*) FROM playable_item pi JOIN track t ON t.item_id = pi.id
			WHERE `+lyricsNeededPredicate+` AND `+notEnriched(enrichEntityLyrics, "pi.id", qFor(model.EnrichPhaseLyrics)), "pi.id", lyricsIDs)
	}
	// The two fields walks share a marker and a scope list but count separately, since
	// each is gated by its own capability and either can run without the other.
	if runs(model.EnrichPhaseTrackFields) {
		add(`SELECT COUNT(*) FROM playable_item pi JOIN track t ON t.item_id = pi.id
			WHERE pi.kind = 'track' AND pi.state = 'present' AND pi.title <> '' AND t.artist <> ''
			  AND `+trackFieldsVacancy+` AND `+notEnriched(enrichEntityFields, "pi.id", qFor(model.EnrichPhaseTrackFields)), "pi.id", fieldsIDs)
	}
	if runs(model.EnrichPhaseBookFields) {
		add(`SELECT COUNT(*) FROM playable_item pi JOIN book bk ON bk.item_id = pi.id
			WHERE pi.kind = 'book' AND pi.state = 'present' AND pi.title <> ''
			  AND (bk.author <> '' OR bk.asin <> '' OR bk.isbn <> '')
			  AND `+bookFieldsVacancy+` AND `+notEnriched(enrichEntityFields, "pi.id", qFor(model.EnrichPhaseBookFields)), "pi.id", fieldsIDs)
	}
	// The album fields walk counts under the album scope list, which it shares with the
	// release match.
	if runs(model.EnrichPhaseAlbumFields) {
		add(`SELECT COUNT(*) FROM album al WHERE al.title <> ''
			  AND (COALESCE(al.label,'') = '' OR al.year IS NULL)
			  AND `+notEnriched(enrichEntityAlbumFields, "al.id", qFor(model.EnrichPhaseAlbumFields)), "al.id", albumIDs)
	}
	var total int
	for _, cq := range queries {
		var n int
		if err := s.read.QueryRowContext(ctx, cq.stmt, cq.args...).Scan(&n); err != nil {
			return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		total += n
	}
	return total, nil
}

// ApplyArtistEnrichment persists one artist's resolved data: MBID (only when the
// artist has none, so a tagged id is never overwritten), aliases, and directed
// relations to other catalog artists. A no-match still writes the marker so the
// artist is not retried every run. The identity has no rider, since an artist's art is
// the artist-art backfill's, so the marker always settles.
//
// A forced run re-applies everything, so the entity delta rides on a change (an MBID,
// alias, or relation that actually landed) rather than on the match.
func (s *Store) ApplyArtistEnrichment(ctx context.Context, in model.ArtistEnrichment) error {
	const op = "store.ApplyArtistEnrichment"
	return s.writeAliveTx(ctx, op, "artist", in.ArtistID, func(tx *sql.Tx) error {
		if !in.Matched {
			return s.markEnrichedTx(ctx, tx, model.EnrichArtistType, in.ArtistID, enrichProviderMusicBrainz, false, "")
		}
		// Shares the fill-when-empty rule with the scan path, lock probe included, so a
		// curated (or deliberately locked-empty) artist MBID survives either writer.
		wroteMBID, err := fillEntityFieldTx(ctx, tx, model.MergeArtist, "artist", "mbid",
			in.ArtistID, normMBID(in.MBID))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if wroteMBID {
			if err := artistMBIDLandedTx(ctx, tx, in.ArtistID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		aliases, err := insertAliasesTx(ctx, tx, in.ArtistID, in.SortName, in.Aliases)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		relations, err := insertRelationsTx(ctx, tx, in.ArtistID, in.Relations)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := s.markEnrichedTx(ctx, tx, model.EnrichArtistType, in.ArtistID, enrichProviderMusicBrainz, true, in.MBID); err != nil {
			return err
		}
		if !wroteMBID && aliases == 0 && relations == 0 {
			return nil
		}
		return appendChange(ctx, tx, "artist", in.PID, model.OpUpdate)
	})
}

// insertAliasesTx adds an artist's alternate names, including the MusicBrainz
// sort-name, ignoring duplicates (UNIQUE(artist_id, name)), and reports how many were
// new.
func insertAliasesTx(ctx context.Context, tx *sql.Tx, artistID int64, sortName string, aliases []string) (int, error) {
	names := aliases
	if strings.TrimSpace(sortName) != "" {
		names = append([]string{sortName}, names...)
	}
	added := 0
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		r, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO artist_alias(artist_id, name, sort_key, is_primary) VALUES (?,?,?,0)",
			artistID, name, model.SortKey(name))
		if err != nil {
			return added, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return added, err
		}
		added += int(n)
	}
	return added, nil
}

// insertRelationsTx links an artist to other catalog artists identified by MBID, and
// reports how many links were new. Targets not present in the catalog are skipped (no
// stub artists are created), so relations only ever connect entities the user actually
// has. Only the enriched artist takes a change-log delta for a new link: no read path
// serves relations yet, so the artist at the other end has nothing a tailer could
// re-fetch, and the read path that adds relations has to emit its delta too.
func insertRelationsTx(ctx context.Context, tx *sql.Tx, srcID int64, rels []model.ArtistRelationInput) (int, error) {
	added := 0
	for _, r := range rels {
		if r.TargetMBID == "" {
			continue
		}
		var dstID int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM artist WHERE mbid = ?", r.TargetMBID).Scan(&dstID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return added, err
		}
		if dstID == srcID {
			continue
		}
		// Orient the edge: normally enriched(src) -> target(dst); an inbound relation
		// (MusicBrainz reported it from the far end) reverses it so the stored
		// direction is consistent regardless of which artist was enriched.
		src, dst := srcID, dstID
		if r.Inbound {
			src, dst = dstID, srcID
		}
		res, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO artist_relation(src_id, dst_id, kind) VALUES (?,?,?)",
			src, dst, r.Kind)
		if err != nil {
			return added, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return added, err
		}
		added += int(n)
	}
	return added, nil
}

// ApplyReleaseGroupEnrichment persists one release group's resolved data: MBID
// (unless it collides with another group's, deferred to the merge gate), type,
// genres added to member items that have none (respecting genre locks, recording
// enrichment provenance), and the Cover Art Archive front cover. Touched genre
// rollups are maintained so db verify stays clean. A match whose art or genre rider
// failed or was out of the pass (in.Incomplete, in.Unasked) applies everything and leaves
// the lookup owed, so a later pass asks the rider again (settleMarkerTx).
//
// A deferred group is re-applied on a later pass and a forced run re-applies everything, so
// the group's delta rides on a change (an MBID, a type, or an image that actually
// landed) rather than on the match. The genre fill emits its own per-item deltas.
func (s *Store) ApplyReleaseGroupEnrichment(ctx context.Context, in model.ReleaseGroupEnrichment) error {
	const op = "store.ApplyReleaseGroupEnrichment"
	return s.writeAliveTx(ctx, op, "release_group", in.ReleaseGroupID, func(tx *sql.Tx) error {
		if !in.Matched {
			return s.settleMarkerTx(ctx, tx, model.EnrichReleaseGroupType, in.ReleaseGroupID, enrichProviderMusicBrainz, false, in.Incomplete, in.Unasked, "")
		}
		wroteMBID, err := setReleaseGroupMBIDTx(ctx, tx, s.log, in.ReleaseGroupID, in.MBID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// A landed id is new evidence for the group-art backfill, which walks by title and
		// may already hold a no-match from an id-less request.
		if wroteMBID {
			if err := deleteArtBackfillMarkerTx(ctx, tx, model.ArtReleaseGroup, in.ReleaseGroupID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		typeChanged := false
		if in.Type != "" {
			// release_group.type is the one entity field enrichment overwrites
			// unconditionally, so consult the entity-curation lock first: a user who
			// curated the type keeps it.
			locked, err := entityFieldLockedTx(ctx, tx, string(model.MergeReleaseGroup), in.ReleaseGroupID, "type")
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if !locked {
				r, err := tx.ExecContext(ctx, "UPDATE release_group SET type = ? WHERE id = ? AND COALESCE(type,'') <> ?",
					in.Type, in.ReleaseGroupID, in.Type)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				n, err := r.RowsAffected()
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				typeChanged = n > 0
			}
		}
		aff := newAffectedRollups()
		if err := populateReleaseGroupGenresTx(ctx, tx, in.ReleaseGroupID, in.Genres, in.GenreProvider, aff); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Reads identically to the release_group.type guard above: a user who chose this
		// group's cover keeps it, forced run or not.
		front, err := attachEntityArtUnlessLockedTx(ctx, tx, model.ArtReleaseGroup, in.ReleaseGroupID, in.Art)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		aux, err := fillEntityAuxArtTx(ctx, tx, model.ArtReleaseGroup, in.ReleaseGroupID, in.AuxArt)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !aff.empty() {
			if err := maintainRollupsTx(ctx, tx, aff, nowNS()); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if err := s.settleMarkerTx(ctx, tx, model.EnrichReleaseGroupType, in.ReleaseGroupID, enrichProviderMusicBrainz, true, in.Incomplete, in.Unasked, in.MBID); err != nil {
			return err
		}
		if !wroteMBID && !typeChanged && !front && aux == 0 {
			return nil
		}
		return appendChange(ctx, tx, "release_group", in.PID, model.OpUpdate)
	})
}

// setReleaseGroupMBIDTx sets a release group's MBID only when it has none and the
// id is not already held by another group, reporting whether the row actually took it.
// A collision means two heuristic groups resolved to one MBID; unifying them is the
// merge primitive's job, so here it is logged and left, never forced into a duplicate
// key.
//
// The bool is what the group-art marker hangs off, the way setAlbumMBIDTx's is: the
// backfill walks by title and a standing no-match marker may predate the id, so a landed
// id re-opens the ask.
func setReleaseGroupMBIDTx(ctx context.Context, tx *sql.Tx, log logger, rgID int64, mbid string) (bool, error) {
	mbid = normMBID(mbid)
	if mbid == "" {
		return false, nil
	}
	// A curated (locked) release-group MBID is left untouched, including a locked-empty
	// one the fill-when-empty guard below would otherwise refill.
	if locked, err := entityFieldLockedTx(ctx, tx, string(model.MergeReleaseGroup), rgID, "mbid"); err != nil {
		return false, err
	} else if locked {
		return false, nil
	}
	var other int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM release_group WHERE mbid = ? AND id <> ?", mbid, rgID).Scan(&other)
	if err == nil {
		log.Warn("enrichment: release-group MBID already used by another group; leaving unmerged", "mbid", mbid, "rg", rgID, "other", other)
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	r, err := tx.ExecContext(ctx, "UPDATE release_group SET mbid = ? WHERE id = ? AND (mbid IS NULL OR mbid = '')", mbid, rgID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// groupArtNeededPredicate selects the release groups the group-art backfill should ask
// about under opts, reading the group as rg: it carries a title, which is what a provider
// is asked with; its whole-entity "art" lock does not stand, which is the one lock cheap
// enough to test in SQL and the one that skips every role at apply anyway; and one half
// of its lookup is due (groupArtHalves).
//
// The MBID is a hint the request carries when the catalog has one, not a gate. Gating on
// it skipped every group MusicBrainz never matched, which is the population most likely
// to have no auxiliary art in the first place. A provider keyed on ids alone answers a
// clean miss for an id-less request and the marker records that once, so the cost of
// asking is one request per group rather than one every run.
//
// Its two callers, the queue and the count, add the shared backs-items ghost heuristic
// beside it, so the two stay in lockstep and an orphaned group carrying an mbid does not
// spend a rate-limited request or take a marker.
func groupArtNeededPredicate(slots model.ArtSlots, opts model.EnrichQueueOptions) string {
	if !slots.Any() {
		return "1=0"
	}
	front, aux := groupArtHalves(slots)
	return `rg.title <> ''
	AND NOT EXISTS (SELECT 1 FROM entity_curation ec WHERE ec.entity_type = 'release_group'
		AND ec.entity_id = rg.id AND ec.field = 'art' AND ec.locked = 1)
	AND (` + front.due(opts) + ` OR ` + aux.due(opts) + `)`
}

// groupArtHalves returns the two halves of the group-art lookup, reading the group as rg:
// the front when slots names it askable and the group holds none, the auxiliary roles
// when slots names them and one of them is empty, each under its own marker. A half
// slots leaves out is never due.
//
// A front is vacant whatever the release-group identity recorded, since that phase asks
// about no vacant front (enrich's enrichReleaseGroup). The auxiliary vacancy is
// deliberately approximate: it counts stored rows against the closed non-front
// vocabulary, so a slot held empty by its own "art.<role>" lock reads here as a vacancy
// and is skipped by fillEntityAuxArtTx at apply, which costs one marked pass per such
// group instead of a per-role lock join.
func groupArtHalves(slots model.ArtSlots) (front, aux artHalfSQL) {
	front = artHalfSQL{"0", enrichEntityGroupFront, "rg.id"}
	aux = artHalfSQL{"0", enrichEntityGroupArt, "rg.id"}
	if slots.Front {
		front.vacancy = "NOT " + groupFrontHeld
	}
	if slots.Aux {
		aux.vacancy = groupAuxVacancy
	}
	return front, aux
}

// groupFrontHeld reads, for rg, whether the group holds a front: one probe of
// art_map's primary key.
const groupFrontHeld = `EXISTS (SELECT 1 FROM art_map am WHERE am.entity_type = 'release_group'
	AND am.entity_id = rg.id AND am.role = 'front')`

// groupAuxVacancy reads, for rg, whether some auxiliary role is empty.
var groupAuxVacancy = auxArtVacancy("release_group", "rg.id")

// auxArtVacancy reads whether some auxiliary role of one entity is empty, counting its
// stored rows against the roles its type carries (model.AuxArtRolesOf).
func auxArtVacancy(entityType, idExpr string) string {
	roles := model.AuxArtRolesOf(model.ArtEntity(entityType))
	quoted := make([]string, len(roles))
	for i, r := range roles {
		quoted[i] = "'" + string(r) + "'"
	}
	return `(SELECT COUNT(*) FROM art_map am WHERE am.entity_type = '` + entityType + `'
		AND am.entity_id = ` + idExpr + ` AND am.role IN (` + strings.Join(quoted, ",") + `)) < ` +
		strconv.Itoa(len(roles))
}

// artistArtNeededPredicate selects the artists the artist-art backfill should ask about
// under opts, reading the artist as a: it carries a name, which is what a provider is
// asked with; its whole-entity "art" lock does not stand; and one half of its lookup is
// due (artistArtHalves).
//
// As with the release-group twin, the MBID is a hint rather than a gate. A local band or
// a mis-tagged name never reaches MusicBrainz, so gating on the id skipped exactly the
// artists least likely to have a portrait. A provider keyed on ids alone answers a nil
// candidate for an id-less request and the marker records the miss once.
func artistArtNeededPredicate(slots model.ArtSlots, opts model.EnrichQueueOptions) string {
	if !slots.Any() {
		return "1=0"
	}
	front, aux := artistArtHalves(slots)
	return `a.name <> ''
	AND NOT EXISTS (SELECT 1 FROM entity_curation ec WHERE ec.entity_type = 'artist'
		AND ec.entity_id = a.id AND ec.field = 'art' AND ec.locked = 1)
	AND (` + front.due(opts) + ` OR ` + aux.due(opts) + `)`
}

// artistArtHalves returns the two halves of the artist-art lookup, reading the artist as
// a: the front when slots names it askable and the artist holds none, the background
// likewise with its own vacancy, each under its own marker. A half slots leaves out is
// never due, so a provider serving fronts alone never marks a background.
//
// The auxiliary vacancy test is as approximate as the release-group one: a slot held
// empty by its own "art.<role>" lock reads here as a vacancy and is dropped at apply,
// costing one marked pass rather than a per-role lock join in the queue.
func artistArtHalves(slots model.ArtSlots) (front, aux artHalfSQL) {
	front = artHalfSQL{"0", enrichEntityArtistFront, "a.id"}
	aux = artHalfSQL{"0", enrichEntityArtistArt, "a.id"}
	if slots.Front {
		front.vacancy = `NOT EXISTS (SELECT 1 FROM art_map am WHERE am.entity_type = 'artist'
		AND am.entity_id = a.id AND am.role = 'front')`
	}
	if slots.Aux {
		aux.vacancy = artistAuxVacancy
	}
	return front, aux
}

// artistAuxVacancy reads, for a, whether some auxiliary role is empty.
var artistAuxVacancy = auxArtVacancy("artist", "a.id")

// albumArtNeededPredicate selects the albums the album-art backfill should ask about
// under opts, reading the album as al: it carries a title, an identifier, and no
// whole-entity "art" lock, and one half of its lookup is due (albumArtHalves).
//
// The identifier requirement is what separates this rung from the artist and
// release-group ones, which walk by name. The releases of one group share a title, so a
// title-only ask can only return some edition's picture, which is the wrong-edition
// failure this rung exists to avoid; and an id-keyed provider answers nil for an id-less
// request, so the marker would record nothing but noise. Any of the three identifiers
// will do: the release mbid, the barcode, or the catalog number.
func albumArtNeededPredicate(slots model.ArtSlots, opts model.EnrichQueueOptions) string {
	if !slots.Any() {
		return "1=0"
	}
	front, aux := albumArtHalves(slots)
	return `al.title <> ''
	AND (COALESCE(al.mbid,'') <> '' OR COALESCE(al.barcode,'') <> ''
		OR COALESCE(al.catalog_number,'') <> '')
	AND NOT EXISTS (SELECT 1 FROM entity_curation ec WHERE ec.entity_type = 'album'
		AND ec.entity_id = al.id AND ec.field = 'art' AND ec.locked = 1)
	AND (` + front.due(opts) + ` OR ` + aux.due(opts) + `)`
}

// albumArtHalves returns the two halves of the album-art lookup, reading the album as al:
// the front when slots names it askable and the album resolves none, the auxiliary roles
// likewise with their own vacancy, each under its own marker. A half slots leaves out is
// never due.
//
// The front clause reads the resolver's rule rather than the album's own row, since an
// album normally answers from a member track's embedded cover and asking for a front it
// would keep spends a rate-limited request on a picture nothing stores. The auxiliary
// clause is as approximate as the other backfills': a slot held empty by its own
// "art.<role>" lock reads here as a vacancy and is dropped at apply, costing one marked
// pass rather than a per-role lock join in the queue.
func albumArtHalves(slots model.ArtSlots) (front, aux artHalfSQL) {
	front = artHalfSQL{"0", enrichEntityAlbumFront, "al.id"}
	aux = artHalfSQL{"0", enrichEntityAlbumArt, "al.id"}
	if slots.Front {
		front.vacancy = "NOT " + albumResolvesFrontArt
	}
	if slots.Aux {
		aux.vacancy = albumAuxVacancy
	}
	return front, aux
}

// artHalfSQL is one half of an art backfill's lookup as its queue reads it: the gap it
// fills, "0" for a half the pass's slots leave out, and the marker it settles under,
// keyed on idExpr.
type artHalfSQL struct {
	vacancy, marker, idExpr string
}

// due reads the half as due under opts: its gap open and its marker leaving it due.
func (h artHalfSQL) due(opts model.EnrichQueueOptions) string {
	if h.vacancy == "0" {
		return "0"
	}
	return "(" + h.vacancy + " AND " + notEnriched(h.marker, h.idExpr, opts) + ")"
}

// state reads the half for the walk, whichever sweep reached the entity: 0 when it is not
// due anywhere in the run (SweepDue), else 1 for a half nothing has asked, 2 for a lookup
// an earlier pass left owed, and 3 for a miss whose window has expired (setArtHalves).
// Reporting every half due in the run lets the walk ask them together, so an entity
// whose halves fall due on different sweeps is walked once. A forced sweep reads an open
// gap as 1, since it asks everything the way a first ask does.
func (h artHalfSQL) state(opts model.EnrichQueueOptions) string {
	if h.vacancy == "0" {
		return "0"
	}
	if opts.Sweep == model.SweepAll {
		return "CASE WHEN " + h.vacancy + " THEN 1 ELSE 0 END"
	}
	on := func(sweep model.EnrichSweep) string {
		opts.Sweep = sweep
		return notEnriched(h.marker, h.idExpr, opts)
	}
	return "CASE WHEN NOT (" + h.vacancy + ") THEN 0 WHEN " + on(model.SweepFresh) + " THEN 1 WHEN " +
		on(model.SweepDeferred) + " THEN 2 WHEN " + on(model.SweepRetry) + " THEN 3 ELSE 0 END"
}

// setArtHalves reads the two half states a queue selected (artHalfSQL.state) onto t.
func setArtHalves(t *model.EnrichTarget, front, aux int) {
	t.FrontDue, t.FrontOwed, t.FrontExpired = front != 0, front == 2, front == 3
	t.AuxDue, t.AuxOwed, t.AuxExpired = aux != 0, aux == 2, aux == 3
}

// albumAuxVacancy reads, for al, whether some auxiliary role is empty.
var albumAuxVacancy = auxArtVacancy("album", "al.id")

// groupEnrichmentFrontHash is the correlated read of a release group's enrichment front
// hash, empty when it has none or its front was chosen by hand. The release-group and
// album-art queues select it so a provider can be offered the reuse of bytes it
// recognizes.
func groupEnrichmentFrontHash(groupIDCol string) string {
	return `COALESCE((SELECT am.source_hash FROM art_map am WHERE am.entity_type = 'release_group'
		AND am.entity_id = ` + groupIDCol + ` AND am.role = 'front' AND am.source = 'enrichment'), '')`
}

// AlbumsNeedingArt returns the next keyset page of albums with an empty art slot the
// registered providers could fill, for the album-art backfill. It is the album twin of
// ArtistsNeedingArtBackfill: same keyset shape, same ghost heuristic, and the same live
// read of the mbid column, so a run after the release match sends the ids that phase
// just landed along with the request.
//
// slots names which vacancies count, so an install with no aux-capable provider does not
// mark an album for a slot nothing could have answered. Each half's state rides along
// (artHalfSQL.state), so the caller asks about every half due in the run without a second
// query.
func (s *Store) AlbumsNeedingArt(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, slots model.ArtSlots, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.AlbumsNeedingArt"
	scopeClause, scopeArgs := enrichIDsFilter("al.id", ids)
	front, aux := albumArtHalves(slots)
	stmt := `SELECT al.id, al.pid, al.title, COALESCE(ar.name,''), COALESCE(al.mbid,''),
			COALESCE(al.barcode,''), COALESCE(al.catalog_number,''),
			` + front.state(opts) + `, ` + aux.state(opts) + `,
			COALESCE(rg.mbid, ''),
			` + groupEnrichmentFrontHash("rg.id") + `
		FROM album al
		LEFT JOIN release_group rg ON rg.id = al.release_group_id
		LEFT JOIN artist ar ON ar.id = rg.primary_artist_id
		WHERE al.id > ? AND ` + enrichBacksFilter(enrichAlbumBacksItems, ids) + `
		  AND ` + albumArtNeededPredicate(slots, opts) + scopeClause + `
		ORDER BY al.id LIMIT ?`
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: enrichEntityAlbumArt}
		var pid string
		var frontState, auxState int
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.ArtistName, &t.MBID,
			&t.Barcode, &t.CatalogNumber, &frontState, &auxState, &t.ReleaseGroupMBID, &t.GroupFrontHash); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		setArtHalves(&t, frontState, auxState)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ApplyAlbumArtBackfill fills an album's empty art roles and records a marker for each
// half of the lookup the walk asked (settleArtHalvesTx), the album twin of
// ApplyArtistArtBackfill. A half matched when an image for it came back, the front also by
// a group copy that landed, and the fill runs on the images whichever halves were asked.
//
// The front goes through fillAlbumArtTx, which re-reads the resolver's answer and the
// art lock inside the write, so a cover set by hand between the queue page and this
// write is not overwritten by an answer that predates it. The auxiliary half goes
// through the shared helper, so its fill-when-empty rule, per-role locks and provenance
// stamp are the same by construction.
//
// A vanished album rowid gets nothing at all, no fill and no markers: an album can be
// merged away between the queue page and the write, and a marker written for a dead
// rowid would silence whatever album inherits it.
//
// The entity delta rides on an image landing rather than on the match, as at the other
// two rungs: a matched gather can still write nothing, its roles locked or already
// filled, and a delta then would send every ChangesSince tailer to re-fetch an unchanged
// album.
func (s *Store) ApplyAlbumArtBackfill(ctx context.Context, in model.AlbumArtBackfill) error {
	const op = "store.ApplyAlbumArtBackfill"
	aux := carriedAuxArt(model.ArtAlbum, in.AuxArt)
	return s.writeAliveTx(ctx, op, "album", in.AlbumID, func(tx *sql.Tx) error {
		var wrote int
		frontMatched := in.Art != nil || in.FrontFromGroup
		if in.FrontFromGroup {
			// The group's row is copied whole rather than a picture stored: the bytes are
			// already in the store, and the provider said they are this pressing's.
			copied, wasOpen, err := fillAlbumArtFromGroupTx(ctx, tx, in.AlbumID, in.GroupFrontHash)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			switch {
			case copied:
				wrote++
			case wasOpen:
				// The group's front moved out from under the hash between the queue page
				// and here, so the album is as bare as it was. An unmatched front half is a
				// miss the retry window asks again; a matched one would silence it over a
				// vacancy.
				frontMatched = false
			}
			// A front no longer open (a cover landed by hand, or a lock) leaves no vacancy,
			// so the half stands on the provider's answer, the way a fetched cover the
			// same guards drop already does.
		}
		if in.Art != nil {
			changed, err := fillAlbumArtTx(ctx, tx, in.AlbumID, in.Art)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if changed {
				wrote++
			}
		}
		if len(aux) > 0 {
			n, err := fillEntityAuxArtTx(ctx, tx, model.ArtAlbum, in.AlbumID, aux)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			wrote += n
		}
		if err := s.settleArtHalvesTx(ctx, tx, in.AlbumID, []artHalfSettle{
			{enrichEntityAlbumFront, in.Front, frontMatched},
			{enrichEntityAlbumArt, in.Aux, len(aux) > 0},
		}); err != nil {
			return err
		}
		if wrote == 0 {
			return nil
		}
		return appendChange(ctx, tx, "album", in.PID, model.OpUpdate)
	})
}

// ArtistsNeedingArtBackfill returns the next keyset page of artists with a half of their
// art lookup due, for the artist-art backfill. It is the artist twin of
// ReleaseGroupsNeedingArt: same keyset shape, same ghost heuristic, same live read of
// the mbid column so a run after the identity phase sends the ids that phase filled
// along with the request, and the same name-keyed gate, so an artist MusicBrainz never
// matched is asked about by name. Each half's state rides along (artHalfSQL.state), so
// the caller asks about every half due in the run without a second query.
func (s *Store) ArtistsNeedingArtBackfill(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, slots model.ArtSlots, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.ArtistsNeedingArtBackfill"
	scopeClause, scopeArgs := enrichIDsFilter("a.id", ids)
	front, aux := artistArtHalves(slots)
	stmt := `SELECT a.id, a.pid, a.name, COALESCE(a.mbid,''),
			` + front.state(opts) + `, ` + aux.state(opts) + `
		FROM artist a
		WHERE a.id > ? AND ` + enrichBacksFilter(enrichArtistBacksItems, ids) + `
		  AND ` + artistArtNeededPredicate(slots, opts) + scopeClause + `
		ORDER BY a.id LIMIT ?`
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: enrichEntityArtistArt}
		var pid string
		var frontState, auxState int
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.MBID, &frontState, &auxState); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		setArtHalves(&t, frontState, auxState)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ApplyArtistArtBackfill fills an artist's empty art roles and records a marker for each
// half of the lookup the walk asked (settleArtHalvesTx), the artist twin of
// ApplyReleaseGroupArtBackfill. A half matched when an image for it came back, and the
// fill runs on the images whichever halves were asked.
//
// The auxiliary half goes through the shared helper, so its fill-when-empty rule,
// per-role locks and provenance stamp are the same by construction. The front does not
// go through attachEntityArtUnlessLockedTx, which re-points a held front past its lock;
// the gap is re-read here, inside the write, rather than trusted from the queue page, so
// a cover set by hand between the page and this write is not overwritten by an answer
// that predates it.
//
// The entity delta rides on an image landing rather than on the match: a matched gather
// can still write nothing, its roles locked or already filled, and a delta then would send
// every ChangesSince tailer to re-fetch an unchanged artist. A vanished rowid gets nothing
// at all, as at the album rung.
func (s *Store) ApplyArtistArtBackfill(ctx context.Context, in model.ArtistArtBackfill) error {
	const op = "store.ApplyArtistArtBackfill"
	aux := carriedAuxArt(model.ArtArtist, in.AuxArt)
	return s.writeAliveTx(ctx, op, "artist", in.ArtistID, func(tx *sql.Tx) error {
		var wrote int
		if in.Art != nil {
			held, err := entityHoldsArtRoleTx(ctx, tx, model.ArtArtist, in.ArtistID, model.ArtRoleFront)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			blocked, err := artFillBlockedTx(ctx, tx, model.ArtArtist, in.ArtistID, model.ArtRoleFront)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if !held && !blocked {
				changed, err := attachEntityArtTxChanged(ctx, tx, string(model.ArtArtist), in.ArtistID, in.Art)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if changed {
					wrote++
				}
			}
		}
		if len(aux) > 0 {
			n, err := fillEntityAuxArtTx(ctx, tx, model.ArtArtist, in.ArtistID, aux)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			wrote += n
		}
		if err := s.settleArtHalvesTx(ctx, tx, in.ArtistID, []artHalfSettle{
			{enrichEntityArtistFront, in.Front, in.Art != nil},
			{enrichEntityArtistArt, in.Aux, len(aux) > 0},
		}); err != nil {
			return err
		}
		if wrote == 0 {
			return nil
		}
		return appendChange(ctx, tx, "artist", in.PID, model.OpUpdate)
	})
}

// writeAliveTx runs fn in a write transaction when the row id names in table still
// exists, and writes nothing when it does not. An enrichment target can be merged away or
// deleted between its queue page and the write, and anything written for the dead rowid
// would sit on whatever entity inherits the id, or fail on a foreign key and end the run.
func (s *Store) writeAliveTx(ctx context.Context, op, table string, id int64, fn func(*sql.Tx) error) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		alive, err := entityAliveTx(ctx, tx, table, id)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !alive {
			return nil
		}
		return fn(tx)
	})
}

// entityAliveTx reports whether an entity row still exists (writeAliveTx).
func entityAliveTx(ctx context.Context, tx *sql.Tx, table string, id int64) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE id = ?", id).Scan(&n)
	return n > 0, err
}

// entityHoldsArtRoleTx reports whether an entity already has an image in one role, which
// is the fill-when-empty question the backfill has to ask inside its own write.
func entityHoldsArtRoleTx(ctx context.Context, tx *sql.Tx, entityType model.ArtEntity, entityID int64, role model.ArtRole) (bool, error) {
	var has int
	err := tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM art_map WHERE entity_type = ? AND entity_id = ? AND role = ?)",
		string(entityType), entityID, string(role)).Scan(&has)
	return has == 1, err
}

// artistMBIDLandedTx re-opens the artist-art backfill for an artist whose MBID has just
// been filled in. The walk is keyed on the name, so no-match markers can be standing on
// an artist that has since acquired an id: the request that earned them went out
// without one, and a provider keyed on ids alone answered nothing it could have answered
// with the id in hand. A landed id is therefore new evidence, and it is the one re-ask
// trigger with three writers (the scan's tag fill, the identity phase's fill, and the
// entity edit), so the rule lives here rather than three times over.
//
// The identity phase's call matters most after a contact-less run: those runs ask by name
// and leave art markers with no identity marker beside them, so the first run with a
// contact configured is exactly where an id lands on a marked artist. The identity phase
// runs before the backfill in the same pass, so that artist is re-asked with its id in
// the same run.
func artistMBIDLandedTx(ctx context.Context, tx *sql.Tx, artistID int64) error {
	return deleteArtBackfillMarkerTx(ctx, tx, model.ArtArtist, artistID)
}

// deleteAlbumFieldsMarkerTx drops one album's fields-walk marker. It is keyed by an album
// rowid under its own entity_type, so neither the orphan sweep's delete nor a merge's
// marker union reaches it, and album rowids are reused: without this a new album
// inheriting the id would be silently skipped by a dead album's answer.
func deleteAlbumFieldsMarkerTx(ctx context.Context, tx *sql.Tx, albumID int64) error {
	return clearEntityMarkerTx(ctx, tx, enrichEntityAlbumFields, albumID)
}

// artBackfillHalfTypes names the front and auxiliary half markers of an entity type's art
// backfill, empty for a type with none.
func artBackfillHalfTypes(entityType model.ArtEntity) (front, aux string) {
	switch entityType {
	case model.ArtReleaseGroup:
		return enrichEntityGroupFront, enrichEntityGroupArt
	case model.ArtArtist:
		return enrichEntityArtistFront, enrichEntityArtistArt
	case model.ArtAlbum:
		return enrichEntityAlbumFront, enrichEntityAlbumArt
	}
	return "", ""
}

// deleteArtBackfillHalfTx drops the one half marker a role belongs to, for a curation write
// that opened that role alone: the front half for the front, the auxiliary half for a role
// the entity carries, and nothing for a role it does not.
func deleteArtBackfillHalfTx(ctx context.Context, tx *sql.Tx, entityType model.ArtEntity, entityID int64, role model.ArtRole) error {
	front, aux := artBackfillHalfTypes(entityType)
	switch {
	case front == "":
		return nil
	case role == model.ArtRoleFront:
		return clearEntityMarkerTx(ctx, tx, front, entityID)
	case slices.Contains(model.AuxArtRolesOf(entityType), role):
		return clearEntityMarkerTx(ctx, tx, aux, entityID)
	}
	return nil
}

// deleteArtBackfillMarkerTx drops both halves of an entity's art backfill markers, and is
// a no-op for a type with no backfill. The markers live under their own entity_types, so
// neither the orphan sweep's delete (which keys on the entity's own type) nor a merge's
// marker union reaches them, and entity rowids are reused: without this a new entity
// inheriting the id would be silently skipped by a dead one's markers. The curation
// writers and an identifier landing call it too, since a clear, an unlock, or a new id
// opens a vacancy the markers say was already asked about.
func deleteArtBackfillMarkerTx(ctx context.Context, tx *sql.Tx, entityType model.ArtEntity, entityID int64) error {
	front, aux := artBackfillHalfTypes(entityType)
	if front == "" {
		return nil
	}
	if err := clearEntityMarkerTx(ctx, tx, front, entityID); err != nil {
		return err
	}
	return clearEntityMarkerTx(ctx, tx, aux, entityID)
}

// ReleaseGroupsNeedingArt returns the next keyset page of release groups with a half of
// their art lookup due, for the group-art backfill, each with its title and
// primary-artist name, which is what a name-keyed provider is asked with, and its MBID
// when the catalog has one. It is the release-group twin of ArtistsNeedingArtBackfill.
// A non-nil ids list scopes the walk to those release-group rowids and, as with the
// release-group pass, drops the backs-items ghost heuristic for the explicit targets;
// the rest of the gate still applies.
//
// It reads rg.mbid live rather than from a snapshot, so running this after the
// release-group phase in the same pass sends the ids that phase just filled along with
// the request. Each half's state rides along (artHalfSQL.state), so the caller asks about
// every half due in the run without a second query.
func (s *Store) ReleaseGroupsNeedingArt(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, slots model.ArtSlots, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.ReleaseGroupsNeedingArt"
	scopeClause, scopeArgs := enrichIDsFilter("rg.id", ids)
	front, aux := groupArtHalves(slots)
	stmt := `SELECT rg.id, rg.pid, rg.title, COALESCE(rg.mbid,''), COALESCE(ar.name,''),
			` + front.state(opts) + `, ` + aux.state(opts) + `
		FROM release_group rg
		LEFT JOIN artist ar ON ar.id = rg.primary_artist_id
		WHERE rg.id > ? AND ` + enrichBacksFilter(enrichRGBacksItems, ids) + `
		  AND ` + groupArtNeededPredicate(slots, opts) + scopeClause + `
		ORDER BY rg.id LIMIT ?`
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: enrichEntityGroupArt}
		var pid string
		var frontState, auxState int
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.MBID, &t.ArtistName, &frontState, &auxState); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		setArtHalves(&t, frontState, auxState)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ApplyReleaseGroupArtBackfill fills a release group's empty art roles and records a
// marker for each half of the lookup the walk asked (settled by settleMarkerTx), the
// release-group twin of ApplyArtistArtBackfill. A half matched when an image for it came
// back, and the fill runs on the images whichever halves were asked.
//
// The front is fill-when-empty: the gap is re-read here, inside the write, rather than
// trusted from the queue page, so a cover set by hand in between is not overwritten, and
// it answers to the front's lock. It does not go through attachEntityArtUnlessLockedTx,
// which re-points a held front and is right only for the identity pass that owns the
// slot. The auxiliary half goes through the shared helper, so its fill-when-empty rule,
// per-role locks and provenance stamp are the same by construction.
//
// The entity delta rides on an image actually landing rather than on the match: a
// matched gather can still write nothing, its roles locked or already filled, and a
// delta then would send every ChangesSince tailer to re-fetch an unchanged group. A
// vanished rowid gets nothing at all, as at the album rung.
func (s *Store) ApplyReleaseGroupArtBackfill(ctx context.Context, in model.ReleaseGroupArtBackfill) error {
	const op = "store.ApplyReleaseGroupArtBackfill"
	aux := carriedAuxArt(model.ArtReleaseGroup, in.AuxArt)
	return s.writeAliveTx(ctx, op, "release_group", in.ReleaseGroupID, func(tx *sql.Tx) error {
		var wrote int
		if in.Art != nil {
			held, err := entityHoldsArtRoleTx(ctx, tx, model.ArtReleaseGroup, in.ReleaseGroupID, model.ArtRoleFront)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			blocked, err := artFillBlockedTx(ctx, tx, model.ArtReleaseGroup, in.ReleaseGroupID, model.ArtRoleFront)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if !held && !blocked {
				changed, err := attachEntityArtTxChanged(ctx, tx, string(model.ArtReleaseGroup), in.ReleaseGroupID, in.Art)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if changed {
					wrote++
				}
			}
		}
		if len(aux) > 0 {
			n, err := fillEntityAuxArtTx(ctx, tx, model.ArtReleaseGroup, in.ReleaseGroupID, aux)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			wrote += n
		}
		if err := s.settleArtHalvesTx(ctx, tx, in.ReleaseGroupID, []artHalfSettle{
			{enrichEntityGroupFront, in.Front, in.Art != nil},
			{enrichEntityGroupArt, in.Aux, len(aux) > 0},
		}); err != nil {
			return err
		}
		if wrote == 0 {
			return nil
		}
		return appendChange(ctx, tx, "release_group", in.PID, model.OpUpdate)
	})
}

// artHalfSettle is one half of an art backfill's lookup as its apply settles it: the
// half's marker type, what the walk said about it, and whether an image for it came back.
type artHalfSettle struct {
	typ     string
	half    model.ArtHalf
	matched bool
}

// settleArtHalvesTx records a marker for each half the walk asked, matched when an image
// for it came back and naming the provider that answered it, and leaves a half it did not
// ask as it stood. The art backfills share it, so their halves settle alike at every rung.
func (s *Store) settleArtHalvesTx(ctx context.Context, tx *sql.Tx, entityID int64, halves []artHalfSettle) error {
	for _, h := range halves {
		if !h.half.Asked {
			continue
		}
		provider := strings.TrimSpace(h.half.Provider)
		if provider == "" {
			provider = enrichProviderNone
		}
		if err := s.settleMarkerTx(ctx, tx, h.typ, entityID, provider, h.matched && !h.half.Unasked, h.half.Incomplete, h.half.Unasked, ""); err != nil {
			return err
		}
	}
	return nil
}

// carriedAuxArt keeps the auxiliary images whose role the entity type carries
// (model.AuxArtRolesOf), so a role the rung has no slot for is neither stored nor counted
// toward the auxiliary half.
func carriedAuxArt(entity model.ArtEntity, aux map[model.ArtRole]*model.ArtImage) map[model.ArtRole]*model.ArtImage {
	roles := model.AuxArtRolesOf(entity)
	var out map[model.ArtRole]*model.ArtImage
	for role, img := range aux {
		if !slices.Contains(roles, role) {
			continue
		}
		if out == nil {
			out = make(map[model.ArtRole]*model.ArtImage, len(aux))
		}
		out[role] = img
	}
	return out
}

// ApplyAlbumReleaseMatch persists one album's matched release id, filled only when
// the album has none and the id is not already held by another album, mirroring
// setReleaseGroupMBIDTx. A no-match records the marker too, so a second run does not
// re-search an album nothing could identify, following the per-recording lyrics marker
// precedent. An inconclusive lookup (in.Incomplete) fills nothing and records the lookup
// as owed, since it decided nothing.
//
// in.Provider records which tier decided it, so an edition match (weaker evidence, see
// enrich/release.go) stays distinguishable and undoable. Empty falls back to the spine.
//
// A declined write still marks, and still records the id the provider named. That looks
// like a lie and is not: the marker says what the LOOKUP found, the way every phase's
// matched flag does (ApplyArtistEnrichment discards its own fill result too), and the
// retained id is what a human needs to resolve the collision the decline reported. What
// it does mean is that the album stops being queued, which for a curated mbid is the
// user's stated wish and for a collision is merge's problem to settle.
//
// It writes no art. The album-art queue keys on the stored id, so a declined write is
// never asked about with an id the album does not hold, and an album carrying a curated
// id is asked about with the user's id rather than the one this phase would have used.
//
// It deliberately does not write media or country back from the matched release.
// resolveAlbum is fill-when-empty, so an enrichment-written media=CD would permanently
// shadow the file's real MEDIA=Vinyl, the column would stop meaning "what the tags said",
// and a setAlbumMBIDTx that declines on a lock or collision would leave the album queued
// carrying enrichment's own output as evidence. Per-edition detail is album.mbid plus a
// lookup, which is what a match buys.
//
// The marker is the thing to know before adding another album-level pass. notEnriched
// keys on (entity_type, entity_id) with no per-pass granularity, so a later pass would
// inherit this row and skip every album this one merely failed to match; give it its
// own entity_type, as lyrics and the album-art backfill did. What keeps a retagged
// album re-queueable without a second marker type is fillAlbumIdentifiersTx deleting
// an unmatched marker on new evidence.
func (s *Store) ApplyAlbumReleaseMatch(ctx context.Context, in model.AlbumReleaseMatch) error {
	const op = "store.ApplyAlbumReleaseMatch"
	provider := in.Provider
	if provider == "" {
		provider = enrichProviderMusicBrainz
	}
	return s.writeAliveTx(ctx, op, "album", in.AlbumID, func(tx *sql.Tx) error {
		// An inconclusive lookup decided nothing, so nothing is filled and the lookup is
		// recorded as owed in place of a standing marker, which is what lets a forced
		// re-ask that hit a short read be asked again next pass rather than wait out the
		// window.
		if in.Incomplete {
			return s.settleMarkerTx(ctx, tx, model.EnrichAlbumType, in.AlbumID, provider, false, true, false, "")
		}
		if !in.Matched {
			return s.markEnrichedTx(ctx, tx, model.EnrichAlbumType, in.AlbumID, provider, false, "")
		}
		wrote, err := setAlbumMBIDTx(ctx, tx, s.log, in.AlbumID, in.MBID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// A landed id is new evidence for the art rung, which walks by identifier: an
		// album asked about while it carried only a barcode gets asked again now that a
		// provider keyed on the release mbid could answer. It follows setReleaseGroupMBIDTx,
		// which re-opens the artist backfill the same way.
		if wrote {
			if err := deleteArtBackfillMarkerTx(ctx, tx, model.ArtAlbum, in.AlbumID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if err := s.markEnrichedTx(ctx, tx, model.EnrichAlbumType, in.AlbumID, provider, true, in.MBID); err != nil {
			return err
		}
		return appendChange(ctx, tx, "album", in.PID, model.OpUpdate)
	})
}

// setAlbumMBIDTx sets an album's MBID only when it has none and the id is not
// already held by another album, reporting whether the row actually took it. It is
// setReleaseGroupMBIDTx one rung down: a collision means two heuristic albums resolved
// to one release, which is the merge primitive's job to unify, so here it is logged and
// left rather than forced into a duplicate the entity-edit surface would refuse.
//
// The bool exists because an id that actually landed is evidence downstream: it re-opens
// the album-art queue, which walks by identifier and may have asked while the album had
// none. A declined write is evidence of nothing.
func setAlbumMBIDTx(ctx context.Context, tx *sql.Tx, log logger, albumID int64, mbid string) (bool, error) {
	mbid = normMBID(mbid)
	if mbid == "" {
		return false, nil
	}
	if locked, err := entityFieldLockedTx(ctx, tx, string(model.MergeAlbum), albumID, "mbid"); err != nil {
		return false, err
	} else if locked {
		return false, nil
	}
	var other int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM album WHERE mbid = ? AND id <> ?", mbid, albumID).Scan(&other)
	if err == nil {
		log.Warn("enrichment: release MBID already used by another album; leaving unmerged", "mbid", mbid, "album", albumID, "other", other)
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	r, err := tx.ExecContext(ctx, "UPDATE album SET mbid = ? WHERE id = ? AND (mbid IS NULL OR mbid = '')", mbid, albumID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// populateReleaseGroupGenresTx attaches the release group's genres to member items
// that carry no genre and whose genre field is not locked, recording enrichment
// provenance (with the attributing provider) and collecting the touched genres for
// rollup maintenance. It never overwrites a tagged or user genre.
func populateReleaseGroupGenresTx(ctx context.Context, tx *sql.Tx, rgID int64, genres []string, provider string, aff *affectedRollups) error {
	if len(genres) == 0 {
		return nil
	}
	gids := make([]int64, 0, len(genres))
	names := make([]string, 0, len(genres))
	for _, name := range genres {
		gid, err := resolveGenre(ctx, tx, model.FacetGenre, name)
		if err != nil {
			return err
		}
		if gid != 0 {
			gids = append(gids, gid)
			names = append(names, name)
			aff.genres[gid] = true
		}
	}
	if len(gids) == 0 {
		return nil
	}
	// The denormalized track.genre feeds the item display, the `--genre` query
	// filter, and (on the next scan) the FTS row; set it too so an enrichment genre
	// is visible everywhere the facet/browse item_genre links already surface it,
	// not only in genre browse.
	genreDisplay := strings.Join(names, "; ")
	rows, err := tx.QueryContext(ctx, `SELECT pi.id, pi.pid
		FROM track t JOIN album al ON al.id = t.album_id JOIN playable_item pi ON pi.id = t.item_id
		WHERE al.release_group_id = ? AND `+trackTakesGroupGenres, rgID)
	if err != nil {
		return err
	}
	type memberItem struct {
		id  int64
		pid model.PID
	}
	var items []memberItem
	for rows.Next() {
		var id int64
		var pid string
		if err := rows.Scan(&id, &pid); err != nil {
			rows.Close()
			return err
		}
		items = append(items, memberItem{id, model.PID(pid)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	now := nowNS()
	for _, it := range items {
		for _, gid := range gids {
			if _, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO item_genre(item_id, genre_id) VALUES (?,?)", it.id, gid); err != nil {
				return err
			}
		}
		// Fill the denormalized display column only when empty (never overwriting a
		// tag; the member query already excluded items that carry a genre).
		if _, err := tx.ExecContext(ctx,
			"UPDATE track SET genre = ? WHERE item_id = ? AND (genre IS NULL OR genre = '')", genreDisplay, it.id); err != nil {
			return err
		}
		// Record that genres came from enrichment, and which provider supplied the
		// display-primary genre, so future organize/enrichment respects them and a
		// consumer can attribute the value. The value stays empty (genres are
		// multi-valued via item_genre); the row exists to carry the source, provider,
		// and lock. provider is stored NULL when untracked so it stays sparse.
		if _, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, provider, locked, updated_at)
			VALUES (?, 'genre', 'enrichment', ?, 0, ?)
			ON CONFLICT(item_id, field) DO UPDATE SET source = 'enrichment', provider = excluded.provider, updated_at = excluded.updated_at`,
			it.id, nullStr(provider), now); err != nil {
			return err
		}
		if err := appendChange(ctx, tx, "item", it.pid, model.OpUpdate); err != nil {
			return err
		}
	}
	return nil
}

// ApplyBookEnrichment fills an audiobook's external identifiers and publisher from
// a MusicBrainz release, only where the field is currently empty so a tagged value
// is never overwritten. Identifier values are normalized first, and a value that
// fails its format check is skipped with a warning rather than stored or allowed
// to abort the apply. MusicBrainz releases regularly carry a plain barcode where
// a book ISBN is expected, and before this check that barcode landed in the isbn
// column as-is; now only a real ISBN (or ASIN) fills the field.
//
// What it writes outlives a rescan while the part says nothing for the field
// (overlayStoredBookTx), and a value the file states replaces it.
func (s *Store) ApplyBookEnrichment(ctx context.Context, in model.BookEnrichment) error {
	const op = "store.ApplyBookEnrichment"
	return s.writeAliveTx(ctx, op, "playable_item", in.BookItemID, func(tx *sql.Tx) error {
		if !in.Matched {
			return s.markEnrichedTx(ctx, tx, model.EnrichBookType, in.BookItemID, enrichProviderMusicBrainz, false, "")
		}
		// Fill-when-empty for each field.
		for _, f := range []struct {
			col, val string
		}{
			{"asin", in.ASIN}, {"isbn", in.ISBN}, {"publisher", in.Publisher},
		} {
			val := strings.TrimSpace(f.val)
			if val == "" {
				continue
			}
			norm, ok := model.NormalizeIdentifierField(f.col, val)
			if !ok {
				s.log.Warn("enrichment: skipping malformed book identifier", "field", f.col, "value", val, "book", in.PID)
				continue
			}
			// A curated value keeps, including one locked deliberately empty, which the
			// fill-when-empty WHERE alone would refill. The genre pass guards the same way.
			locked, lerr := fieldLockedTx(ctx, tx, in.BookItemID, f.col)
			if lerr != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, lerr)
			}
			if locked {
				continue
			}
			// isbn carries a derived key column that every lookup compares on, so the two
			// move together or a filled ISBN is invisible to the book resolver.
			set := f.col + " = ?"
			args := []any{norm}
			if f.col == "isbn" {
				set += ", isbn_key = ?"
				args = append(args, identity.ISBNKey(norm))
			}
			args = append(args, in.BookItemID)
			r, err := tx.ExecContext(ctx,
				"UPDATE book SET "+set+" WHERE item_id = ? AND ("+f.col+" = '' OR "+f.col+" IS NULL)",
				args...)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// Attribute what landed, the way the genre pass does. Unlocked, so a later
			// tag still wins; the row is what the on-disk write-back selects on, and
			// what tells a consumer the value came from a provider rather than the file.
			if n, _ := r.RowsAffected(); n > 0 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, provider, locked, updated_at)
					VALUES (?, ?, 'enrichment', ?, 0, ?)
					ON CONFLICT(item_id, field) DO UPDATE SET source = 'enrichment',
					  provider = excluded.provider, updated_at = excluded.updated_at`,
					in.BookItemID, f.col, enrichProviderMusicBrainz, nowNS()); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
		}
		if err := s.markEnrichedTx(ctx, tx, model.EnrichBookType, in.BookItemID, enrichProviderMusicBrainz, true, in.MBID); err != nil {
			return err
		}
		return appendChange(ctx, tx, "item", in.PID, model.OpUpdate)
	})
}

// lyricsNeededPredicate selects tracks eligible for a lyrics lookup: a present track
// that carries both a title and an artist (a lyrics provider keys on both) and has no
// lyrics row yet. Requiring a non-empty artist keeps an untagged track out of the set,
// so a lookup that would only ever miss for lack of local metadata never writes a
// negative marker that would then wrongly skip the track once it is retagged. It reads
// pi (playable_item) and t (track), so a caller must join both under those aliases.
const lyricsNeededPredicate = `pi.kind = 'track' AND pi.state = 'present' AND pi.title <> ''
	AND COALESCE(t.artist,'') <> ''
	AND NOT EXISTS (SELECT 1 FROM lyrics ly WHERE ly.item_id = pi.id)`

// ItemsNeedingLyrics returns the next keyset page of present tracks that have no
// lyrics row yet (and, unless force, have not been looked up), each carrying the
// title, artist, album, and duration a lyrics provider keys on. It mirrors the
// entity keyset queries: a forced re-run rewrites the marker rather than removing the
// track from the set, so the walk still advances and terminates. A non-nil ids list
// scopes the walk to those item rowids; the fill-when-empty predicate still applies,
// so a scoped item that already carries lyrics is not returned.
func (s *Store) ItemsNeedingLyrics(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.ItemsNeedingLyrics"
	// A virtual track plays only its window of the shared file, so the duration a lyrics
	// provider keys on must be that window (itemEffectiveDurationExpr), not the whole
	// file. Otherwise a 3-minute cue track carved from an hour-long rip is looked up as
	// 3600s and never matches. The primary edge is aliased pf so the shared expression
	// applies.
	scopeClause, scopeArgs := enrichIDsFilter("pi.id", ids)
	stmt := `SELECT pi.id, pi.pid, pi.title, COALESCE(t.artist,''), COALESCE(t.album,''),
			COALESCE(` + itemEffectiveDurationExpr + `, 0)
		FROM playable_item pi
		JOIN track t ON t.item_id = pi.id
		LEFT JOIN item_file pf ON pf.item_id = pi.id AND pf.role = 'primary'
		LEFT JOIN file f ON f.id = pf.file_id
		WHERE pi.id > ? AND ` + lyricsNeededPredicate + `
		  AND ` + notEnriched(enrichEntityLyrics, "pi.id", opts) + scopeClause + `
		ORDER BY pi.id LIMIT ?`
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: enrichEntityLyrics}
		var pid string
		var durMS int64
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.ArtistName, &t.Album, &durMS); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		t.DurationSec = int(durMS / 1000)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ApplyLyricsEnrichment attaches a track's resolved lyrics and records the
// per-recording marker. Lyrics are written only when the item still has none
// (fill-when-empty, re-checked inside the transaction so a sidecar added since the
// iteration query is never clobbered); a no-match writes only the marker so the track
// is not re-queried each run, and a lookup a provider failed is recorded as owed instead
// (settleMarkerTx). A match emits an item change delta.
func (s *Store) ApplyLyricsEnrichment(ctx context.Context, in model.LyricsEnrichment) error {
	const op = "store.ApplyLyricsEnrichment"
	return s.writeAliveTx(ctx, op, "playable_item", in.ItemID, func(tx *sql.Tx) error {
		if in.Matched && in.Lyrics.HasContent() {
			var exists int
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM lyrics WHERE item_id = ?", in.ItemID).Scan(&exists); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if exists == 0 {
				if _, err := putLyricsTx(ctx, tx, in.ItemID, in.Lyrics, true, false); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if err := appendChange(ctx, tx, "item", in.PID, model.OpUpdate); err != nil {
					return err
				}
			}
		}
		return s.settleMarkerTx(ctx, tx, enrichEntityLyrics, in.ItemID, in.Provider, in.Matched && !in.Unasked, in.Incomplete, in.Unasked, "")
	})
}

// markEnrichedTx upserts the sparse enrichment marker for an entity, recording which
// provider resolved it. The identity spine (artist/release-group/book) passes
// "musicbrainz"; a per-recording lyrics marker passes the lyrics provider (or "" on a
// no-match). An empty provider stores as "" so the NOT NULL column is satisfied.
//
// The upsert refreshes enriched_at, which is what re-arms the retry window after a
// re-asked miss: the column is the last lookup, not the first, so a target nothing
// answers for falls due again one window from each attempt rather than every run.
func (s *Store) markEnrichedTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, provider string, matched bool, mbid string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at, owed)
		VALUES (?,?,?,?,?,?,0)
		ON CONFLICT(entity_type, entity_id) DO UPDATE SET
		  provider = excluded.provider, matched = excluded.matched, mbid = excluded.mbid,
		  enriched_at = excluded.enriched_at, owed = 0`,
		entityType, entityID, provider, boolInt(matched), nullStr(strings.TrimSpace(mbid)), nowNS())
	return err
}

// identityMarker reports whether entityType is an identity rung with a rider. The
// release-group walk is the one asker of its group's genres, so a genre provider out of
// the pass leaves the identity owed, where a port pass records a miss the retry window
// re-asks. The artist identity has no rider: its art is the artist-art backfill's.
func identityMarker(entityType string) bool {
	return entityType == model.EnrichReleaseGroupType
}

// settleMarkerTx records what one lookup decided. failed says a provider it called failed
// with a slot it could have filled still open, and unasked that a provider serving such a
// slot was out of the pass.
//
// A failure leaves the lookup owed: the marker keeps what the walk found, replacing a
// standing marker, so a later pass asks again, where a miss would wait out the retry
// window and a standing match would keep a forced re-ask that hit an outage from ever
// re-asking. The release-group identity is owed for an unasked rider too, since its walk
// is that rider's asker (see identityMarker); a port pass records that as a miss the
// retry window re-asks.
// The owed marker is dated from the failure, and an unasked walk of a lookup already
// owed leaves the date alone, since nothing asked for it. Anything else is an answer and
// upserts the marker as markEnrichedTx does.
func (s *Store) settleMarkerTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, provider string, matched, failed, unasked bool, mbid string) error {
	if !failed && !(unasked && identityMarker(entityType)) {
		return s.markEnrichedTx(ctx, tx, entityType, entityID, provider, matched, mbid)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at, owed)
		VALUES (?,?,?,?,?,?,1)
		ON CONFLICT(entity_type, entity_id) DO UPDATE SET
		  provider = excluded.provider, matched = excluded.matched, mbid = excluded.mbid,
		  enriched_at = CASE WHEN ? AND entity_enrichment.owed = 1
		    THEN entity_enrichment.enriched_at ELSE excluded.enriched_at END,
		  owed = 1`,
		entityType, entityID, provider, boolInt(matched), nullStr(strings.TrimSpace(mbid)), nowNS(), boolInt(!failed))
	return err
}

// The owed-lookup statements every ordinary pass runs, named so a test can check both
// seek the owed rows' index.
const (
	owedLookupsExist  = "SELECT EXISTS(SELECT 1 FROM entity_enrichment WHERE owed = 1)"
	expireOwedLookups = "UPDATE entity_enrichment SET owed = 0 WHERE owed = 1 AND enriched_at <= ?"
)

// ExpireDeferredLookups settles every lookup owed since at or before cutoff as it stands:
// an identity stays the match MusicBrainz made, and a port lookup that found nothing
// becomes a miss, dated from when it became owed, which the retry window re-asks. That
// bounds an owed lookup nothing asks again, such as one whose slot was filled some other
// way, which no queue would ever select.
//
// It returns 0 when nothing is left owed, having written nothing, so a pass with nothing
// owed only reads. Otherwise it returns the instant the pass's owed sweep measures
// against, a stamp from the store's clock: later than every marker already written and
// earlier than every one written after it, so the sweep asks exactly the lookups earlier
// passes left owed, even on a wall clock that has not ticked since the last of them.
func (s *Store) ExpireDeferredLookups(ctx context.Context, cutoff int64) (int64, error) {
	const op = "store.ExpireDeferredLookups"
	var owed int
	if err := s.read.QueryRowContext(ctx, owedLookupsExist).Scan(&owed); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if owed == 0 {
		return 0, nil
	}
	var asOf int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, expireOwedLookups, cutoff); err != nil {
			return err
		}
		var left int
		if err := tx.QueryRowContext(ctx, owedLookupsExist).Scan(&left); err != nil {
			return err
		}
		if left == 1 {
			asOf = nowNS()
		}
		return nil
	})
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return asOf, nil
}

// ExpiredMissesExist reports whether any no-match marker is old enough to be re-asked.
// It is one scan of the marker table, which a run uses to decide whether to walk a retry
// sweep at all: without it every phase re-queries its own base table for a population
// that is usually empty, which is most of the cost of a nightly pass with nothing to do.
//
// It ignores which pass a marker belongs to, so a run whose only due markers are a
// phase it does not run still walks the sweep and finds nothing. That is the
// conservative direction: the answer is never "no" while something is due.
func (s *Store) ExpiredMissesExist(ctx context.Context, cutoff int64) (bool, error) {
	const op = "store.ExpiredMissesExist"
	if cutoff == 0 {
		return false, nil
	}
	var found int
	err := s.read.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM entity_enrichment WHERE owed = 0 AND matched = 0 AND enriched_at <= ?)",
		cutoff).Scan(&found)
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return found == 1, nil
}

// EnrichmentCacheGet returns a cached provider payload by key.
func (s *Store) EnrichmentCacheGet(ctx context.Context, key string) ([]byte, bool, error) {
	var payload []byte
	err := s.read.QueryRowContext(ctx, "SELECT payload FROM enrichment_cache WHERE cache_key = ?", key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, waxerr.Wrap(waxerr.CodeIO, "store.EnrichmentCacheGet", err)
	}
	return payload, true, nil
}

// EnrichmentCachePut stores a provider payload under key, replacing any prior value.
func (s *Store) EnrichmentCachePut(ctx context.Context, key string, payload []byte) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO enrichment_cache(cache_key, payload, fetched_at) VALUES (?,?,?)
			 ON CONFLICT(cache_key) DO UPDATE SET payload = excluded.payload, fetched_at = excluded.fetched_at`,
			key, payload, nowNS())
		return err
	})
}

// EnrichmentCoverage reports how many entities of each type have been enriched, and
// how many present tracks hold lyrics or had them looked up.
func (s *Store) EnrichmentCoverage(ctx context.Context) (model.EnrichmentCoverage, error) {
	const op = "store.EnrichmentCoverage"
	var cov model.EnrichmentCoverage
	// Only the three entity types are entity coverage. The per-pass markers sharing the
	// table (the per-recording lyrics lookup, the per-album release match, the three art
	// backfills, the two fields walks) are fill-when-empty side channels, and the WHERE
	// excludes them; the lyrics marker is read again below, per track. An identity owed a
	// rider still matched, so it counts.
	rows, err := s.read.QueryContext(ctx,
		`SELECT entity_type, COUNT(*), COALESCE(SUM(matched),0) FROM entity_enrichment
		 WHERE entity_type IN ('artist','release_group','book') GROUP BY entity_type`)
	if err != nil {
		return cov, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var count, matched int
		if err := rows.Scan(&typ, &count, &matched); err != nil {
			return cov, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		cov.Matched += matched
		switch typ {
		case model.EnrichArtistType:
			cov.Artists = count
		case model.EnrichReleaseGroupType:
			cov.ReleaseGroups = count
		case model.EnrichBookType:
			cov.Books = count
		}
	}
	if err := rows.Err(); err != nil {
		return cov, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// The denominator is every present track, not the lyrics phase's own queue, which
	// also wants a title and an artist.
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(ly.item_id),
		        COALESCE(SUM(CASE WHEN ly.item_id IS NULL AND ee.owed = 0 AND ee.matched = 0 THEN 1 ELSE 0 END), 0)
		 FROM playable_item pi
		 LEFT JOIN lyrics ly ON ly.item_id = pi.id
		 LEFT JOIN entity_enrichment ee ON ee.entity_type = ? AND ee.entity_id = pi.id
		 WHERE pi.kind = 'track' AND pi.state = 'present'`, enrichEntityLyrics).
		Scan(&cov.Tracks, &cov.TracksWithLyrics, &cov.TracksLyricsAsked); err != nil {
		return cov, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return cov, nil
}

// EnrichScopeForItem resolves one item into the enrichment targets a scoped pass
// should touch. A track scopes to its artist and album artist, its album's release
// group, and its own lyrics lookup; a book scopes to its contributors and its own
// identifier fill. An episode is CodeUnsupported: episode metadata is feed-owned
// and synced, not enriched. An unknown pid is CodeNotFound.
func (s *Store) EnrichScopeForItem(ctx context.Context, itemPID model.PID) (*model.EnrichScope, error) {
	const op = "store.EnrichScopeForItem"
	var itemID int64
	var kind string
	err := s.read.QueryRowContext(ctx,
		"SELECT id, kind FROM playable_item WHERE pid = ?", string(itemPID)).Scan(&itemID, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(itemPID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	scope := &model.EnrichScope{}
	switch model.Kind(kind) {
	case model.KindTrack:
		var artistID, albumArtistID, albumID sql.NullInt64
		err := s.read.QueryRowContext(ctx,
			"SELECT artist_id, album_artist_id, album_id FROM track WHERE item_id = ?", itemID).
			Scan(&artistID, &albumArtistID, &albumID)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if artistID.Valid {
			scope.ArtistIDs = append(scope.ArtistIDs, artistID.Int64)
		}
		// The album artist counts when it is a different entity, or when it is the
		// only artist the track has (an untagged primary artist stores NULL).
		if albumArtistID.Valid && (!artistID.Valid || albumArtistID.Int64 != artistID.Int64) {
			scope.ArtistIDs = append(scope.ArtistIDs, albumArtistID.Int64)
		}
		if albumID.Valid {
			scope.AlbumIDs = append(scope.AlbumIDs, albumID.Int64)
			var rgID sql.NullInt64
			err := s.read.QueryRowContext(ctx,
				"SELECT release_group_id FROM album WHERE id = ?", albumID.Int64).Scan(&rgID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if rgID.Valid {
				scope.ReleaseGroupIDs = append(scope.ReleaseGroupIDs, rgID.Int64)
			}
		}
		scope.LyricsItemIDs = append(scope.LyricsItemIDs, itemID)
		scope.FieldsItemIDs = append(scope.FieldsItemIDs, itemID)
	case model.KindBook:
		rows, err := s.read.QueryContext(ctx,
			"SELECT DISTINCT artist_id FROM item_contributor WHERE item_id = ? ORDER BY artist_id", itemID)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			scope.ArtistIDs = append(scope.ArtistIDs, id)
		}
		if err := rows.Err(); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		scope.BookItemIDs = append(scope.BookItemIDs, itemID)
		scope.FieldsItemIDs = append(scope.FieldsItemIDs, itemID)
	default:
		return nil, waxerr.New(waxerr.CodeUnsupported, op,
			"cannot scope enrichment to a "+kind+" (episode metadata is feed-owned)")
	}
	return scope, nil
}

// EnrichScopeForEntity resolves one shared entity into a scoped pass's targets: an
// artist to itself, a release group to itself, and an album to itself (the release
// match, the album fields walk, and the album-art backfill) and to its parent release
// group, which carries the type and the genres. Other entity kinds have no enrichment
// provider and are CodeUnsupported; an unknown pid is CodeNotFound.
func (s *Store) EnrichScopeForEntity(ctx context.Context, kind read.EntityKind, pid model.PID) (*model.EnrichScope, error) {
	const op = "store.EnrichScopeForEntity"
	scope := &model.EnrichScope{}
	switch kind {
	case read.EntityArtist:
		var id int64
		err := s.read.QueryRowContext(ctx, "SELECT id FROM artist WHERE pid = ?", string(pid)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, waxerr.New(waxerr.CodeNotFound, op, "no such artist: "+string(pid))
		}
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		scope.ArtistIDs = append(scope.ArtistIDs, id)
	case read.EntityReleaseGroup:
		var id int64
		err := s.read.QueryRowContext(ctx, "SELECT id FROM release_group WHERE pid = ?", string(pid)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, waxerr.New(waxerr.CodeNotFound, op, "no such release group: "+string(pid))
		}
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		scope.ReleaseGroupIDs = append(scope.ReleaseGroupIDs, id)
	case read.EntityAlbum:
		var id int64
		var rgID sql.NullInt64
		err := s.read.QueryRowContext(ctx,
			"SELECT id, release_group_id FROM album WHERE pid = ?", string(pid)).Scan(&id, &rgID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, waxerr.New(waxerr.CodeNotFound, op, "no such album: "+string(pid))
		}
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// A null parent only happens when the release group was deleted out from
		// under the album; surface it rather than run a pass that touches nothing.
		if !rgID.Valid {
			return nil, waxerr.New(waxerr.CodeNotFound, op, "album has no release group: "+string(pid))
		}
		// Both rungs: the group carries the type and genres, the album itself is what
		// the release match, the fields walk, and the art backfill work on.
		scope.AlbumIDs = append(scope.AlbumIDs, id)
		scope.ReleaseGroupIDs = append(scope.ReleaseGroupIDs, rgID.Int64)
	default:
		return nil, waxerr.New(waxerr.CodeUnsupported, op,
			"cannot scope enrichment to a "+string(kind)+" (want artist, release_group, or album)")
	}
	return scope, nil
}

// limitOr defaults a non-positive limit to a sane batch cap.
func limitOr(limit int) int {
	if limit <= 0 {
		return 100
	}
	return limit
}

// logger is the minimal logging surface the tx helpers need (satisfied by
// *slog.Logger).
type logger interface {
	Warn(msg string, args ...any)
}

// enrichedTagSelect finds every file an enrichment write-back should stamp: the items
// carrying enrichment field_provenance rows, joined to their backing files, for each file
// whose settle stamp is older than the newest of those rows. A book repeats across its
// parts because asin, isbn, and edition feed identity.BookKey, so parts disagreeing on
// them would not group on the next scan; a track has one file. The primary part sorts
// first, so a caller can abandon a book whose primary fails before touching the rest.
//
// The settle stamp (file.enrich_settled_at) is what keeps this from being a full-library
// rewrite, and what makes it complete: a landed write settles the file at the newest
// value it carried, so nothing reopens it until enrichment fills something newer, while
// a write that failed, a pass that was canceled, or a pass that ran with write-tags off
// leaves the file owed. Inferring the set from a pass's start time instead lost every
// one of those, since the fills are fill-when-empty and a filled field's updated_at never
// moves again.
//
// One aggregated subquery names an item's enrichment-written fields, rather than a LEFT
// JOIN per field: the fields walks made that list open-ended, and the columns are read
// unconditionally with Go keeping only the ones the concat names. Locked rows are
// excluded because a curated value did not come from the file, and a field the user
// tagged or edited carries a different source and never appears. Every enrichment field
// on an owed item is written, the older ones along with the newest: a file being
// rewritten anyway costs nothing more to stamp fully.
//
// A track's row also carries its album's enrichment label and when it was written, so
// the one rewrite stamps both and the file is owed by whichever is newer than the stamp.
// The label lives on the album row, so it has no field_provenance row of its own; the
// members with no enrichment field to carry it on are EnrichedAlbumLabelFiles' to write.
// The label term counts only while the album still holds a label, the guard the label
// select applies for the same reason: a merge moves the loser's curation row onto a
// survivor whose own label column is empty, and the write settles at the label's stamp
// only when there is a label to write, so an unguarded term would leave that file owed on
// every pass for good.
//
// The item_file join drops virtual tracks (start_frames IS NULL): one shared file backs
// N cue-carved tracks, so an ungated join would rewrite that file once per track, each
// after the first carrying a stale size/mtime for the optimistic update. A file several
// items share survives that join, so it comes back carrying the label select's shared
// flag and the caller refuses it once rather than rewriting it per item. The settle stamp
// is per file while the newest value is per item, so writing one item's values would
// settle the file past the other's and lose them.
//
// The /*SCOPE*/ marker takes a scoped run's reach clause; see enrichWriteScopeClause.
const enrichedTagSelect = `SELECT pi.pid, f.pid, f.library_id, pi.kind, f.path, f.size, f.mtime_ns,
		CASE WHEN itf.role = 'primary' THEN 1 ELSE 0 END,
		CASE WHEN ` + fileSharedOrVirtualExpr + ` THEN 1 ELSE 0 END,
		fpw.fields, fpw.newest,
		CASE WHEN lab.entity_id IS NULL THEN '' ELSE COALESCE(al.label,'') END, COALESCE(lab.updated_at, 0),
		COALESCE(t.genre,''), COALESCE(NULLIF(CAST(t.bpm AS TEXT),'0'),''),
		COALESCE(t.isrc,''), COALESCE(t.composer,''), COALESCE(NULLIF(CAST(t.year AS TEXT),'0'),''),
		COALESCE(bk.asin,''), COALESCE(bk.isbn,''), COALESCE(bk.publisher,''),
		COALESCE(bk.genre,''), COALESCE(NULLIF(CAST(bk.year AS TEXT),'0'),''), COALESCE(bk.narrator,''),
		COALESCE(bk.subtitle,''), COALESCE(bk.edition,''), COALESCE(bk.description,'')
	FROM playable_item pi
	JOIN item_file itf ON itf.item_id = pi.id AND itf.start_frames IS NULL
	JOIN file f ON f.id = itf.file_id
	JOIN library flib ON flib.id = f.library_id AND flib.read_only = ?
	JOIN (SELECT item_id, GROUP_CONCAT(field) AS fields, MAX(updated_at) AS newest
	      FROM field_provenance WHERE source = 'enrichment' AND locked = 0
	      GROUP BY item_id) fpw ON fpw.item_id = pi.id
	LEFT JOIN book bk ON bk.item_id = pi.id
	LEFT JOIN track t ON t.item_id = pi.id
	LEFT JOIN album al ON al.id = t.album_id
	LEFT JOIN entity_curation lab ON ` + enrichmentLabelRowJoin + `
	WHERE pi.state = 'present' AND pi.kind IN ('track','book')
	  AND (fpw.newest > f.enrich_settled_at
	       OR (COALESCE(al.label,'') <> '' AND COALESCE(lab.updated_at, 0) > f.enrich_settled_at))/*SCOPE*/
	ORDER BY pi.id, CASE WHEN itf.role = 'primary' THEN 0 ELSE 1 END, itf.position, f.id`

// enrichmentLabelRowJoin joins an album alias al to its enrichment-written, unlocked
// label curation row as lab. A locked row is left out for the reason the item select
// leaves one out: a curated value did not come from the file.
const enrichmentLabelRowJoin = `lab.entity_type = 'album' AND lab.entity_id = al.id AND lab.field = 'label'
	  AND lab.source = 'enrichment' AND lab.locked = 0`

// enrichWriteScopeClause is the reach of a scoped enrichment run, for the write-back
// selects: the run's items, the members of its albums, and the members of its release
// groups' albums, which together are every item the run could have filled a written
// field or an album label on. Artists are left out on purpose, since artist enrichment
// fills nothing the write-back writes and an artist reaches every track it is credited
// on. A nil scope is a full run and reaches everything. Each list binds as one JSON
// array, so a reach of any size stays one statement and never widens.
func enrichWriteScopeClause(scope *model.EnrichScope, itemCol, albumCol string) (string, []any) {
	if scope == nil {
		return "", nil
	}
	uniq := func(lists ...[]int64) []int64 {
		return slices.Compact(slices.Sorted(slices.Values(slices.Concat(lists...))))
	}
	// One item list: a scope names the same item for its fields and its lyrics. Albums
	// and release groups each come from two phases, so they repeat before this.
	items := uniq(scope.FieldsItemIDs, scope.BookItemIDs, scope.LyricsItemIDs)
	albums := uniq(scope.AlbumIDs)
	groups := uniq(scope.ReleaseGroupIDs)
	var parts []string
	var args []any
	in := func(ids []int64) string {
		args = append(args, idArray(ids))
		return " IN (SELECT value FROM json_each(?))"
	}
	if len(items) > 0 {
		parts = append(parts, itemCol+in(items))
	}
	if len(albums) > 0 {
		parts = append(parts, albumCol+in(albums))
	}
	if len(groups) > 0 {
		parts = append(parts, albumCol+" IN (SELECT id FROM album WHERE release_group_id"+in(groups)+")")
	}
	if len(parts) == 0 {
		return " AND 1=0", nil
	}
	return " AND (" + strings.Join(parts, " OR ") + ")", args
}

// idArray renders ids as a JSON array, for json_each.
func idArray(ids []int64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	b.WriteByte(']')
	return b.String()
}

// enrichedTagFieldOrder is the field each scanned value column belongs to, per kind. It
// is the one place the select's column order and the field names are tied together.
var enrichedTagFieldOrder = map[model.Kind][]string{
	model.KindTrack: {"genre", "bpm", "isrc", "composer", "year"},
	model.KindBook:  {"asin", "isbn", "publisher", "genre", "year", "narrator", "subtitle", "edition", "description"},
}

// EnrichmentWriteback returns the files owed an enrichment write, with the values to
// write: every file whose item carries an enrichment value newer than the file's settle
// stamp, or only those within scope's reach when scope is not nil. A row with every value
// empty is returned rather than dropped: the provenance rows outlived their values, and
// the caller settles the file with nothing to write, since writing nothing is not the
// same as clearing the tag and a file left owed would be scanned past on every pass.
func (s *Store) EnrichmentWriteback(ctx context.Context, scope *model.EnrichScope) ([]model.EnrichedTagRow, error) {
	return s.enrichmentWriteback(ctx, scope, false)
}

// enrichmentWriteback is EnrichmentWriteback over the libraries whose read-only flag is
// readOnly.
func (s *Store) enrichmentWriteback(ctx context.Context, scope *model.EnrichScope, readOnly bool) ([]model.EnrichedTagRow, error) {
	const op = "store.EnrichmentWriteback"
	clause, args := enrichWriteScopeClause(scope, "pi.id", "t.album_id")
	rows, err := s.read.QueryContext(ctx, strings.Replace(enrichedTagSelect, "/*SCOPE*/", clause, 1),
		append([]any{readOnly}, args...)...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichedTagRow
	for rows.Next() {
		var r model.EnrichedTagRow
		var primary, shared int
		var written string
		var trackGenre, bpm, isrc, composer, trackYear string
		var asin, isbn, publisher, bookGenre, bookYear, narrator, subtitle, edition, description string
		if err := rows.Scan(&r.ItemPID, &r.FilePID, &r.LibraryID, &r.Kind, &r.Path, &r.Size, &r.MTimeNS,
			&primary, &shared, &written, &r.Newest, &r.Label, &r.LabelUpdatedAt,
			&trackGenre, &bpm, &isrc, &composer, &trackYear,
			&asin, &isbn, &publisher, &bookGenre, &bookYear, &narrator,
			&subtitle, &edition, &description); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		r.IsPrimary = primary == 1
		r.Shared = shared == 1
		values := []string{trackGenre, bpm, isrc, composer, trackYear}
		if r.Kind == model.KindBook {
			values = []string{asin, isbn, publisher, bookGenre, bookYear, narrator, subtitle, edition, description}
		}
		enriched := make(map[string]bool, 8)
		for _, f := range strings.Split(written, ",") {
			enriched[f] = true
		}
		// A named field with an empty value means the provenance row outlived the value (a
		// scan put retires the rows it re-derives, so this is some other write), and writing
		// nothing is not the same as clearing the tag.
		for i, f := range enrichedTagFieldOrder[r.Kind] {
			if !enriched[f] || values[i] == "" {
				continue
			}
			if r.Fields == nil {
				r.Fields = make(map[string]string, len(values))
			}
			r.Fields[f] = values[i]
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// itemFieldsVacancy is the per-kind vacancy predicate for the fields walk: an item is
// worth asking about while any field in its fill set is still empty. The two halves are
// derived from model.EnrichFillFields rather than restated, so a field added there
// widens the queue with it and one removed narrows it.
//
// It reads the track as t and the book as bk. A column with no entry here is a field the
// fill set names and this table does not, which is a build-time gap rather than a silent
// skip: buildItemFieldsVacancy panics on it, at package init, where a test run sees it.
var itemFieldVacancyCols = map[string]string{
	"bpm":         "(t.bpm IS NULL OR t.bpm = 0)",
	"isrc":        "t.isrc = ''",
	"composer":    "t.composer = ''",
	"publisher":   "bk.publisher = ''",
	"year":        "bk.year IS NULL",
	"description": "bk.description = ''",
	"narrator":    "bk.narrator = ''",
	"subtitle":    "bk.subtitle = ''",
	"edition":     "bk.edition = ''",
	"asin":        "bk.asin = ''",
	"isbn":        "bk.isbn = ''",
}

var (
	trackFieldsVacancy = buildItemFieldsVacancy(model.KindTrack)
	bookFieldsVacancy  = buildItemFieldsVacancy(model.KindBook)
)

func buildItemFieldsVacancy(kind model.Kind) string {
	fields := make([]string, 0, 8)
	for f := range model.EnrichFillFields(kind) {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	parts := make([]string, len(fields))
	for i, f := range fields {
		expr, ok := itemFieldVacancyCols[f]
		if !ok {
			panic("no vacancy column for the " + string(kind) + " fill field " + f)
		}
		parts[i] = expr
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// ItemsNeedingFields returns the next keyset page of items whose fill set still has a
// gap, for the item-rung fields walk. kind selects the track or the book walk; they
// share a marker and an id space but not a vacancy test or a set of hints.
//
// The guards are the lyrics queue's, for its reason: a present item, a non-empty title,
// and enough identity for a provider to key on (a track's artist, or one of a book's
// author, asin, isbn). An item with nothing to ask with would only ever miss, and a
// permanent marker recording that would then wrongly skip it once it is retagged.
func (s *Store) ItemsNeedingFields(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, kind model.Kind, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.ItemsNeedingFields"
	scopeClause, scopeArgs := enrichIDsFilter("pi.id", ids)
	var stmt string
	switch kind {
	case model.KindTrack:
		stmt = `SELECT pi.id, pi.pid, pi.title, COALESCE(t.artist,''), COALESCE(t.album,''),
				COALESCE(` + itemEffectiveDurationExpr + `, 0), t.isrc, COALESCE(t.mbid,''), '', ''
			FROM playable_item pi
			JOIN track t ON t.item_id = pi.id
			LEFT JOIN item_file pf ON pf.item_id = pi.id AND pf.role = 'primary'
			LEFT JOIN file f ON f.id = pf.file_id
			WHERE pi.id > ? AND pi.kind = 'track' AND pi.state = 'present' AND pi.title <> ''
			  AND t.artist <> '' AND ` + trackFieldsVacancy + `
			  AND ` + notEnriched(enrichEntityFields, "pi.id", opts) + scopeClause + `
			ORDER BY pi.id LIMIT ?`
	case model.KindBook:
		// No duration and no file joins for it: enrichBookFields keys on the title, the
		// author, and the identifiers, so computing an effective duration here would be
		// two joins per page for a value nothing sends.
		stmt = `SELECT pi.id, pi.pid, pi.title, COALESCE(bk.author,''), '',
				0, '', COALESCE(bk.mbid,''), bk.asin, bk.isbn
			FROM playable_item pi
			JOIN book bk ON bk.item_id = pi.id
			WHERE pi.id > ? AND pi.kind = 'book' AND pi.state = 'present' AND pi.title <> ''
			  AND (bk.author <> '' OR bk.asin <> '' OR bk.isbn <> '') AND ` + bookFieldsVacancy + `
			  AND ` + notEnriched(enrichEntityFields, "pi.id", opts) + scopeClause + `
			ORDER BY pi.id LIMIT ?`
	default:
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no fields walk for kind "+string(kind))
	}
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: enrichEntityFields}
		var pid string
		var durMS int64
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.ArtistName, &t.Album, &durMS,
			&t.ISRC, &t.MBID, &t.ASIN, &t.ISBN); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		t.DurationSec = int(durMS / 1000)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ApplyItemFields writes the scalar fields a provider supplied for one track or book and
// records the fields marker (settled by settleMarkerTx). It keeps only the keys in the kind's fill set that are
// currently empty and unlocked, both re-read inside the transaction so a value tagged or
// locked since the queue page is never overwritten.
//
// Each survivor is normalized and validated one key at a time. normalizeEdits trims,
// folds an identifier, and rejects per key, and a value that fails validation is logged
// and skipped rather than aborting: an enrichment pass answers for many items and one
// provider's malformed bpm must not cost the rest of the batch, which is the rule
// ApplyBookEnrichment already follows for a malformed identifier. Nothing surviving
// writes the marker alone, so the item is not re-asked every run.
//
// No rename pre-pass runs, and none is needed: the fill sets exclude every identity key
// by construction, so a fill can never move an item onto another entity chain.
func (s *Store) ApplyItemFields(ctx context.Context, in model.ItemFieldsEnrichment) error {
	const op = "store.ApplyItemFields"
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = enrichProviderNone
	}
	return s.writeAliveTx(ctx, op, "playable_item", in.ItemID, func(tx *sql.Tx) error {
		kind, fields, norm, err := s.acceptedItemFieldsTx(ctx, tx, in, op)
		if err != nil {
			return err
		}
		if len(fields) > 0 {
			affected := newAffectedRollups()
			attr := model.Attribution{Source: model.SourceEnrichment, Provider: provider}
			if _, err := applyItemEditTx(ctx, tx, s.log, in.PID, in.ItemID, kind,
				fields, norm, attr, model.LockUnchanged, op, affected); err != nil {
				return err
			}
			// applyItemEditTx stamps one attribution across the batch, and two providers
			// commonly split a fields answer, so re-stamp the rows whose own provider
			// differs. Same upsert, so this only rewrites the provider on those rows.
			now := nowNS()
			for _, f := range fields {
				p := in.Providers[f]
				if p == "" || p == provider {
					continue
				}
				fa := model.Attribution{Source: model.SourceEnrichment, Provider: p}
				if err := upsertEditProvenanceTx(ctx, tx, in.ItemID, f, fa, norm[f], model.LockUnchanged, now); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
			if !affected.empty() {
				if err := maintainRollupsTx(ctx, tx, affected, nowNS()); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
		}
		return s.settleMarkerTx(ctx, tx, enrichEntityFields, in.ItemID, provider, in.Matched && !in.Unasked, in.Incomplete, in.Unasked, "")
	})
}

// acceptedItemFieldsTx narrows a provider's offer to the keys this item will actually
// take: in the kind's fill set, currently empty, not locked, normalizing and validating
// each survivor on a scratch copy of the row. A key that fails any of those is dropped
// with a Warn rather than failing the pass. It returns the item's kind plus the sorted
// field names and normalized values applyItemEditTx expects.
func (s *Store) acceptedItemFieldsTx(ctx context.Context, tx *sql.Tx, in model.ItemFieldsEnrichment, op string) (string, []string, map[string]string, error) {
	if len(in.Fields) == 0 {
		return "", nil, nil, nil
	}
	var kind string
	if err := tx.QueryRowContext(ctx, "SELECT kind FROM playable_item WHERE id = ?", in.ItemID).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, nil, waxerr.New(waxerr.CodeNotFound, op, "no such item")
		}
		return "", nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	fill := model.EnrichFillFields(model.Kind(kind))
	if len(fill) == 0 {
		return kind, nil, nil, nil
	}
	locked, err := lockedFieldSetTx(ctx, tx, in.ItemID)
	if err != nil {
		return "", nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// The current row, both to test the vacancy the queue only approximated and as the
	// scratch copy each candidate value is validated against.
	var track model.Track
	var book model.Book
	switch kind {
	case string(model.KindTrack):
		track, _, _, err = loadTrackForEditTx(ctx, tx, in.ItemID)
	case string(model.KindBook):
		book, _, err = loadBookForEditTx(ctx, tx, in.ItemID)
	}
	if err != nil {
		return "", nil, nil, err
	}

	fields := make([]string, 0, len(in.Fields))
	norm := make(map[string]string, len(in.Fields))
	for f, v := range in.Fields {
		if !fill[f] || locked[f] || strings.TrimSpace(v) == "" {
			continue
		}
		if !itemFieldEmpty(kind, f, &track, &book) {
			continue
		}
		// A provider handing back a full release date must not burn the marker on a
		// parse failure, so a leading four-digit year is folded out before the edit
		// vocabulary sees it.
		if f == "year" {
			v = leadingYear(v)
		}
		// One key at a time: normalizeEdits rejects the whole map on a malformed
		// identifier, which for a batch of provider guesses should cost that key alone.
		names, one, err := normalizeEdits(map[string]string{f: v}, op)
		if err != nil || len(names) == 0 {
			s.log.Warn("enrichment: skipping a malformed field value", "field", f, "value", v, "item", in.PID, "err", err)
			continue
		}
		scratchTrack, scratchBook := track, book
		switch kind {
		case string(model.KindTrack):
			err = applyTrackEdit(&scratchTrack, f, one[f], op)
		case string(model.KindBook):
			err = applyBookEdit(&scratchBook, f, one[f], op)
		}
		if err != nil {
			s.log.Warn("enrichment: skipping an invalid field value", "field", f, "value", one[f], "item", in.PID, "err", err)
			continue
		}
		fields = append(fields, f)
		norm[f] = one[f]
	}
	sort.Strings(fields)
	return kind, fields, norm, nil
}

// itemFieldEmpty re-reads one fill field's vacancy from the loaded row, inside the
// write. The queue's predicate said the item had some gap; this says whether this
// particular field is the gap, so a value tagged since the queue page is not overwritten.
func itemFieldEmpty(kind, field string, tr *model.Track, bk *model.Book) bool {
	switch kind {
	case string(model.KindTrack):
		switch field {
		case "bpm":
			return tr.BPM == 0
		case "isrc":
			return tr.ISRC == ""
		case "composer":
			return tr.Composer == ""
		}
	case string(model.KindBook):
		switch field {
		case "publisher":
			return bk.Publisher == ""
		case "year":
			return bk.Year == 0
		case "description":
			return bk.Description == ""
		case "narrator":
			return bk.Narrator == ""
		case "subtitle":
			return bk.Subtitle == ""
		case "edition":
			return bk.Edition == ""
		case "asin":
			return bk.ASIN == ""
		case "isbn":
			return bk.ISBN == ""
		}
	}
	return false
}

// leadingYear folds a provider's release date down to the year the edit vocabulary
// parses. A four-digit prefix is taken as the year ("1975-09-12" is 1975); anything else
// passes through unchanged so a genuinely malformed value still fails validation and is
// reported as one.
func leadingYear(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 4 {
		return v
	}
	for i := 0; i < 4; i++ {
		if v[i] < '0' || v[i] > '9' {
			return v
		}
	}
	if len(v) == 4 {
		return v
	}
	if c := v[4]; c >= '0' && c <= '9' {
		return v
	}
	return v[:4]
}

// AlbumsNeedingFields returns the next keyset page of albums missing a label or a year,
// for the album-rung fields walk, each with the title, primary-artist name, and the
// identifiers a provider keys on: the mbid, the barcode, and the catalog number, which
// is the sibling the release matcher's second tier searches by and what a
// Discogs-shaped provider keys on. A title is required for the reason the art queues
// require one: an album with nothing to ask with would only ever miss.
func (s *Store) AlbumsNeedingFields(ctx context.Context, opts model.EnrichQueueOptions, afterID int64, limit int, ids []int64) ([]model.EnrichTarget, error) {
	const op = "store.AlbumsNeedingFields"
	scopeClause, scopeArgs := enrichIDsFilter("al.id", ids)
	stmt := `SELECT al.id, al.pid, al.title, COALESCE(ar.name,''), COALESCE(al.mbid,''),
			COALESCE(al.barcode,''), COALESCE(al.catalog_number,'')
		FROM album al
		LEFT JOIN release_group rg ON rg.id = al.release_group_id
		LEFT JOIN artist ar ON ar.id = rg.primary_artist_id
		WHERE al.id > ? AND al.title <> ''
		  AND (COALESCE(al.label,'') = '' OR al.year IS NULL)
		  AND ` + notEnriched(enrichEntityAlbumFields, "al.id", opts) + scopeClause + `
		ORDER BY al.id LIMIT ?`
	args := append(append([]any{afterID}, scopeArgs...), limitOr(limit))
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: enrichEntityAlbumFields}
		var pid string
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.ArtistName, &t.MBID, &t.Barcode, &t.CatalogNumber); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ApplyAlbumFields writes the scalar fields a provider supplied for one album and records
// the album fields marker (settled by settleMarkerTx). Only label and year are accepted (model.AlbumFillFields), and
// the two land by different routes because they sit at different rungs.
//
// label is an album column: a fill-when-empty write plus a curation row naming the
// provider, the way every other entity-rung enrichment value lands. It survives a forced
// rescan, since the scan's own top-up is fill-when-empty and never clears it.
//
// year participates in the album identity key, so it cannot be written per member without
// forking the album. It goes through the uniform whole-album edit instead, which moves the
// album and its release group in place. The album row is the first fill-when-empty test:
// the scan tops its year up from any member carrying one, and a member's year cleared
// later leaves the column set over year-less members, which is an album that has its year
// and must not take a provider's. The members are the second, because that is where the
// value lands: any member already carrying a year vetoes (a merge keeps a survivor's NULL
// year over the loser's tagged members), as does any member not present (an archived
// member vetoes the in-place rewrite and the fallback would split the album) or any member
// whose locks make it unwritable.
//
// Like label, a filled year survives a rescan of members whose files say nothing for it,
// since each member keeps an enrichment fill until its file states one
// (overlayStoredTrackTx). A member whose file states its own year, or is retagged onto
// another album, leaves the fill behind, and the heuristic key follows.
//
// Barcode, catalog number, media, and country are refused on purpose: they are the
// evidence the MusicBrainz release matcher searches by, and a provider's guess must not
// drive it (the reasoning ApplyAlbumReleaseMatch already carries).
func (s *Store) ApplyAlbumFields(ctx context.Context, in model.AlbumFieldsEnrichment) error {
	const op = "store.ApplyAlbumFields"
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = enrichProviderNone
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		// The album can vanish between the queue page and this write (a merge or an orphan
		// sweep in another writer), and a dead rowid gets nothing: no fill, no marker, no
		// delta.
		var curYear sql.NullInt64
		err := tx.QueryRowContext(ctx, "SELECT year FROM album WHERE id = ?", in.AlbumID).Scan(&curYear)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		wrote, alive := false, true
		// The year goes FIRST, because it is the one fill that can move the album onto a
		// key another album already holds, which merges this row away. A label written
		// before that would go down with the row: the column is on the album, and the
		// merge does not carry it to the survivor.
		if v := strings.TrimSpace(in.Fields["year"]); v != "" && !curYear.Valid {
			did, stillThere, err := s.applyAlbumYearTx(ctx, tx, in, leadingYear(v), provider, op)
			if err != nil {
				return err
			}
			wrote, alive = did, stillThere
		}
		// A merged-away album's rowid names a row that no longer exists, so nothing more
		// belongs on the dead id: no label, no marker, no delta. The merge emitted the
		// survivor's delta, and the survivor carries its own label state and its own
		// marker, so it is asked about a label on its own terms rather than inheriting an
		// answer aimed at the row that just disappeared.
		if !alive {
			return nil
		}
		if v := strings.TrimSpace(in.Fields["label"]); v != "" {
			ok, err := fillEntityFieldTx(ctx, tx, model.MergeAlbum, "album", "label", in.AlbumID, v)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if ok {
				lp := in.Providers["label"]
				if lp == "" {
					lp = provider
				}
				if err := upsertEntityCurationTx(ctx, tx, string(model.MergeAlbum), in.AlbumID, "label",
					model.Attribution{Source: model.SourceEnrichment, Provider: lp}, v,
					model.LockUnchanged, nowNS()); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				wrote = true
			}
		}
		if err := s.settleMarkerTx(ctx, tx, enrichEntityAlbumFields, in.AlbumID, provider, in.Matched && !in.Unasked, in.Incomplete, in.Unasked, ""); err != nil {
			return err
		}
		if wrote {
			return appendChange(ctx, tx, "album", in.PID, model.OpUpdate)
		}
		return nil
	})
}

// applyAlbumYearTx runs the whole-album year fill for an album the caller found year-less,
// reporting whether it wrote and whether the album row still exists afterwards (a taken
// key merges this album into the incumbent and the row is gone). See ApplyAlbumFields for
// why the veto is member-level.
func (s *Store) applyAlbumYearTx(ctx context.Context, tx *sql.Tx, in model.AlbumFieldsEnrichment, year, provider, op string) (bool, bool, error) {
	var members []model.PID
	rows, err := tx.QueryContext(ctx, `SELECT pi.pid, pi.state, COALESCE(t.year, 0)
		FROM track t JOIN playable_item pi ON pi.id = t.item_id
		WHERE t.album_id = ? ORDER BY pi.id`, in.AlbumID)
	if err != nil {
		return false, true, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var pid, state string
		var y int
		if err := rows.Scan(&pid, &state, &y); err != nil {
			return false, true, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if y != 0 || state != string(model.StatePresent) {
			return false, true, nil
		}
		members = append(members, model.PID(pid))
	}
	if err := rows.Err(); err != nil {
		return false, true, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if len(members) == 0 {
		return false, true, nil
	}

	names, norm, err := normalizeEdits(map[string]string{"year": year}, op)
	if err != nil {
		s.log.Warn("enrichment: skipping a malformed album year", "value", year, "album", in.PID, "err", err)
		return false, true, nil
	}
	targets := make([]editEntry, 0, len(members))
	for _, pid := range members {
		targets = append(targets, editEntry{pid: pid, fields: names, norm: norm})
	}
	// skipLocked probes each member's locks as it validates, so a locked year anywhere
	// under the album vetoes the fill without a separate per-member lock read.
	var skipped []model.PID
	entries, err := collectEditEntriesTx(ctx, tx, targets, false, true, op, &skipped)
	if err != nil {
		return false, true, err
	}
	if len(skipped) > 0 || len(entries) != len(members) {
		return false, true, nil
	}
	attr := model.Attribution{Source: model.SourceEnrichment, Provider: provider}
	if _, err := applyEditEntriesTx(ctx, tx, s.log, entries, attr, model.LockUnchanged, false, op); err != nil {
		if waxerr.Is(err, waxerr.CodeInvalid) {
			s.log.Warn("enrichment: skipping an invalid album year", "value", year, "album", in.PID, "err", err)
			return false, true, nil
		}
		return false, true, err
	}
	var alive int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM album WHERE id = ?", in.AlbumID).Scan(&alive); err != nil {
		return false, true, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return true, alive > 0, nil
}

// EnrichedAlbumLabelFiles returns the member files still owed an enrichment-written
// album label, from one query over the label curation rows and their members: the
// label, when enrichment wrote it, and the file as a write needs it. A file is owed
// while the label is newer than its settle stamp; scope, when not nil, bounds the
// members to a scoped run's reach. The label lands on the album row rather than on any
// item, so it carries no field_provenance row; a member with enrichment fields of its
// own takes the label through EnrichmentWriteback instead, and the caller strikes
// those from this set.
//
// Only a present item's file is returned, since a missing one's write would fail on
// every pass and never settle. A shared or virtual file is returned and flagged, since
// the caller records the refusal and settles it; it is never opened. The rows come in
// album order, so a file two albums somehow claim is planned for the lower one.
func (s *Store) EnrichedAlbumLabelFiles(ctx context.Context, scope *model.EnrichScope) ([]model.EntityFieldFile, error) {
	return s.enrichedAlbumLabelFiles(ctx, scope, false)
}

// enrichedAlbumLabelFiles is EnrichedAlbumLabelFiles over the libraries whose read-only
// flag is readOnly.
func (s *Store) enrichedAlbumLabelFiles(ctx context.Context, scope *model.EnrichScope, readOnly bool) ([]model.EntityFieldFile, error) {
	const op = "store.EnrichedAlbumLabelFiles"
	clause, args := enrichWriteScopeClause(scope, "pi.id", "al.id")
	args = append([]any{readOnly}, args...)
	rows, err := s.read.QueryContext(ctx, `SELECT DISTINCT al.id, f.pid, f.library_id, f.path, f.size, f.mtime_ns,
			COALESCE(al.label,''), lab.updated_at,
			CASE WHEN `+fileSharedOrVirtualExpr+` THEN 1 ELSE 0 END
		FROM entity_curation lab
		JOIN album al ON `+enrichmentLabelRowJoin+`
		JOIN track t ON t.album_id = al.id
		JOIN playable_item pi ON pi.id = t.item_id AND pi.state = 'present'
		JOIN item_file itf ON itf.item_id = t.item_id AND itf.role IN ('primary', 'alternate')
		JOIN file f ON f.id = itf.file_id
		JOIN library flib ON flib.id = f.library_id AND flib.read_only = ?
		WHERE COALESCE(al.label,'') <> '' AND lab.updated_at > f.enrich_settled_at`+clause+`
		ORDER BY al.id, f.pid`, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EntityFieldFile
	for rows.Next() {
		v := model.EntityFieldFile{EntityType: model.MergeAlbum, Field: "label"}
		var albumID int64
		var shared int
		if err := rows.Scan(&albumID, &v.FilePID, &v.LibraryID, &v.Path, &v.Size, &v.MTimeNS, &v.Value, &v.UpdatedAt, &shared); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		v.Shared = shared == 1
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// EnrichmentHeldReadOnly counts the files owed an enrichment write, a tag or an album
// label, that the write-backs leave alone because their library is read-only, within
// scope's reach as EnrichmentWriteback reads it.
func (s *Store) EnrichmentHeldReadOnly(ctx context.Context, scope *model.EnrichScope) (int, error) {
	rows, err := s.enrichmentWriteback(ctx, scope, true)
	if err != nil {
		return 0, err
	}
	labels, err := s.enrichedAlbumLabelFiles(ctx, scope, true)
	if err != nil {
		return 0, err
	}
	held := make(map[model.PID]bool, len(rows)+len(labels))
	for _, r := range rows {
		held[r.FilePID] = true
	}
	for _, f := range labels {
		held[f.FilePID] = true
	}
	return len(held), nil
}

// SettleEnrichmentWrite records that the enrichment tag write-back has settled every
// enrichment value on a file up to upTo (the newest one it carried, or recorded as
// unable to land), so only a newer value reopens the file. The stamp never moves back,
// and a file that vanished since the select is not an error: there is nothing left to
// settle.
func (s *Store) SettleEnrichmentWrite(ctx context.Context, filePID model.PID, upTo int64) error {
	const op = "store.SettleEnrichmentWrite"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE file SET enrich_settled_at = MAX(enrich_settled_at, ?) WHERE pid = ?",
			upTo, string(filePID)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
}
