package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// resolveAndLinkEntities resolves the normalized entities a scanned track
// implies (artist, album-artist, release_group, album, genres), links them onto
// the track/item, and refreshes the item's FTS row. New entities are emitted to
// the change_log so delta consumers can update browse/facet caches. It runs
// inside the PutScannedTrack write transaction.
//
// affected collects the artists the credit resolved to, which the caller's rollup
// maintenance then recomputes. It is not optional: a featured artist backs no
// track's artist_id, so without a rollup row of its own db verify reads the missing
// row as -1 against a recompute of 0 and reports permanent drift.
//
// priorPath and fileID are what the album re-key reconciliation corroborates a folder
// move with: the path this scan relinked the file from, empty when it was found where it
// already was, and the file's id for the organize journal. An edit path has neither, and
// passes "" and 0.
func resolveAndLinkEntities(ctx context.Context, tx *sql.Tx, log logger, itemID int64, tr model.Track, filePath []byte, priorPath string, fileID int64, affected *affectedRollups) error {
	artistID, err := resolveTrackArtists(ctx, tx, itemID, tr, affected)
	if err != nil {
		return err
	}
	anchor, rgKey, albumKey := albumChainKeys(tr, filePath)
	// The ids follow the same fallback the anchor made in albumChainKeys.
	albumArtistIDs := tr.MBAlbumArtistIDs
	if strings.TrimSpace(tr.AlbumArtist) == "" {
		albumArtistIDs = tr.MBArtistIDs
	}
	// Files routinely tag MUSICBRAINZ_ARTISTID and omit the album-artist one. When the
	// two credits are the same string they name the same artists, so the track's ids
	// apply, and without them a joint credit would split here after collapsing above.
	// Only when the track stated no list of its own: there tr.Artist is not the whole
	// credit, and lending the ids would stamp an artist the arity rule just refused.
	if len(albumArtistIDs) == 0 && len(tr.Artists) == 0 &&
		identity.MatchKey(anchor) == identity.MatchKey(tr.Artist) {
		albumArtistIDs = tr.MBArtistIDs
	}
	// The PRIMARY of the credit, not an entity named for the whole string, so
	// "Jay-Z & Alicia Keys" resolves to Jay-Z and takes his id. Only the entity
	// changes: resolveAlbumChain still keys on the raw string, so every release
	// group and album keeps the match_key it already has.
	var albumArtistID int64
	if names, ids := creditNames(anchor, nil, albumArtistIDs); len(names) > 0 {
		albumArtistID, err = resolveArtist(ctx, tx, names[0], ids[0])
		if err != nil {
			return err
		}
	}

	albumID, err := resolveAlbumChain(ctx, tx, log, tr, rgKey, albumKey, albumArtistID, affected)
	if err != nil {
		return err
	}

	// The album the track sat on before this resolve, read while the FK still points at
	// it. upsertTrack never writes album_id, so this is exactly what the last resolve
	// left, and a track row created moments ago reads NULL.
	var priorAlbumID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT album_id FROM track WHERE item_id=?", itemID).Scan(&priorAlbumID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE track SET artist_id=?, album_artist_id=?, album_id=? WHERE item_id=?",
		nullInt64(artistID), nullInt64(albumArtistID), nullInt64(albumID), itemID); err != nil {
		return err
	}
	// A folder move re-keys the album, so carry the drained row's identity and
	// attachments onto the new one rather than leaving it to ghost (scanreconcile.go).
	// The merge it may perform repoints this track's FK, so nothing below may hold on
	// to albumID.
	if err := reconcileAlbumRekeyTx(ctx, tx, priorAlbumID.Int64, albumID, priorPath, fileID, affected); err != nil {
		return err
	}

	if err := syncItemGenres(ctx, tx, itemID, tr.Genres, tr.Genre); err != nil {
		return err
	}
	return syncSearchFTS(ctx, tx, itemID, tr)
}

// creditNames turns one credit into its artists paired positionally with the ids the
// file listed, as equal-length slices. A stated list is verbatim; otherwise the credit
// is split, and a split of several names against exactly one id is the file overruling
// the guess ("Earth, Wind & Fire"), so it collapses back to the artist it names.
func creditNames(raw string, stated, ids []string) ([]string, []string) {
	if len(stated) > 0 {
		return stated, model.CreditMBIDs(stated, ids)
	}
	names := identity.SplitPerformerCredit(raw)
	if len(names) > 1 && len(ids) == 1 && strings.TrimSpace(ids[0]) != "" {
		return []string{strings.TrimSpace(raw)}, []string{strings.TrimSpace(ids[0])}
	}
	return names, model.CreditMBIDs(names, ids)
}

// resolveTrackArtists resolves a track's performing credit into one artist entity per
// name and rewrites its RoleArtist contributor rows, returning the primary artist's
// id for track.artist_id.
//
// It replaces ONLY the artist role, never the whole contributor set, diverging from
// resolveContributors on the book path: a track's other music credits have no
// denormalized column and no scan-side input, so a wholesale delete would destroy a
// curated producer list on the next content change. Within the role it rewrites
// unconditionally rather than diffing, matching syncItemGenres, since a compare would
// have to run against resolved ids and so would do the resolve anyway.
//
// The returned id is the PRIMARY artist, which is what book.author_id already means.
// Not a combined entity, because that entity is the bug this closes. Not NULL, which
// would drop the track out of the artist facet, artist_rollup, and every drilldown
// that reads artist_id.
func resolveTrackArtists(ctx context.Context, tx *sql.Tx, itemID int64, tr model.Track, affected *affectedRollups) (int64, error) {
	names, ids := creditNames(tr.Artist, tr.Artists, tr.MBArtistIDs)

	prior, err := contributorArtistIDsForRole(ctx, tx, itemID, model.RoleArtist)
	if err != nil {
		return 0, err
	}
	for _, aid := range prior {
		affected.artists[aid] = true
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_contributor WHERE item_id=? AND role=?", itemID, string(model.RoleArtist)); err != nil {
		return 0, err
	}

	var primary int64
	seen := make(map[int64]bool, len(names))
	pos := 0
	for i, name := range names {
		aid, err := resolveArtist(ctx, tx, name, ids[i])
		if err != nil {
			return 0, err
		}
		if aid == 0 || seen[aid] {
			continue
		}
		seen[aid] = true
		affected.artists[aid] = true
		if primary == 0 {
			primary = aid
		}
		// position is the credited order with no gaps for names that dropped out.
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO item_contributor(item_id, artist_id, role, position) VALUES (?,?,?,?)",
			itemID, aid, string(model.RoleArtist), pos); err != nil {
			return 0, err
		}
		pos++
	}
	return primary, nil
}

// albumChainKeys derives the identity of the album chain a track implies: the
// release-group anchor credit and the release-group and album keys. It is the one
// place the chain-key derivation lives, shared by resolveAlbumChain (via
// resolveAndLinkEntities) and the edit-rename pre-pass (editrename.go), so the two
// cannot drift on any input. Extracting only the anchor would leave the not-grouped
// guard behind: the raw ReleaseGroupKey of an anchorless titled track is a key no
// resolution path ever computes, and a caller deriving through the raw identity
// functions would key entities onto phantoms. Empty keys mean the track does not
// group; the anchor comes back regardless, since the album-artist entity resolves
// from it either way.
func albumChainKeys(tr model.Track, filePath []byte) (anchor, rgKey, albumKey string) {
	// The album-artist anchors the release group; fall back to the track artist
	// when a track carries no explicit album-artist (the common single-artist case).
	anchor = tr.AlbumArtist
	if strings.TrimSpace(anchor) == "" {
		// The FIRST stated artist, not the joined display. This string reaches
		// ReleaseGroupKey below, and a file that repeated its ARTIST frame used to
		// anchor its album on that first value, so joining here would re-key every
		// heuristically-grouped album in such a library on the next scan.
		anchor = tr.Artist
		if len(tr.Artists) > 0 {
			anchor = tr.Artists[0]
		}
	}
	artistMatchKey := identity.MatchKey(anchor)
	if artistMatchKey == "" && tr.MBReleaseGroupID == "" {
		// No artist and no MBID (fully untagged): do not group. A title-only
		// release group would collide unrelated untagged albums (e.g. two
		// different "Greatest Hits"), so the track stays ungrouped until an
		// artist or MBID is known.
		return anchor, "", ""
	}
	rgKey = identity.ReleaseGroupKey(tr.MBReleaseGroupID, artistMatchKey, tr.Album)
	if rgKey == "" {
		return anchor, "", "" // non-album single: not grouped
	}
	// Key the album by release group, year, and folder, not disc total. Multi-disc
	// albums are often tagged inconsistently, and including disc_total would split
	// one edition into separate album rows. The folder already disambiguates
	// editions; disc_total is still recorded for display. A MusicBrainz release id
	// keys the album directly when present.
	albumKey = identity.AlbumKey(tr.MBReleaseID, rgKey, tr.Year, 0, filepath.Dir(string(filePath)))
	return anchor, rgKey, albumKey
}

// resolveAlbumChain resolves the release_group and album for a track from the keys
// albumChainKeys derived, returning the album id (0 when the track does not group).
// MusicBrainz ids, when present, key the group/release directly (MBID-first), so two
// heuristic guesses for one release unify on the same id.
//
// The keys carry the RAW anchor credit while albumArtistID is the entity its primary
// resolved to. Keeping them separate is what let the album-artist credit split
// without re-keying anything: only the name reaches ReleaseGroupKey.
func resolveAlbumChain(ctx context.Context, tx *sql.Tx, log logger, tr model.Track, rgKey, albumKey string, albumArtistID int64, affected *affectedRollups) (int64, error) {
	if rgKey == "" {
		return 0, nil
	}
	rgID, err := resolveReleaseGroup(ctx, tx, log, rgKey, tr.Album, albumArtistID, tr.MBReleaseGroupID, affected)
	if err != nil {
		return 0, err
	}
	return resolveAlbum(ctx, tx, log, albumKey, rgID, tr)
}

// resolveArtist finds-or-creates an artist by normalized match key, returning
// its id (0 when name is blank). Artists dedup by name (MBID-first unification of
// same-named artists is enrichment's job); a known MBID is recorded on the new
// row so enrichment and Subsonic artist info have it. A new artist is logged.
//
// Name-keyed is settled, despite release groups and albums being MBID-first. Keying
// artists that way does not remove duplicates, it changes which ones you get: an
// artist tagged across half a library forks into an mbid-keyed row and a name-keyed
// one, splitting on tag coverage rather than spelling, which is worse because partial
// tagging is normal. identity.ArtistKey spells the MBID-first form and is never called.
//
// An existing row with no id of its own takes one from a tag that now supplies it,
// so a Picard pass over a library scanned before it tagged lands the ids on a
// rescan. book.go and credits.go resolve contributors with mbid="", so those calls
// short-circuit and never write an id. The track-artist path pairs ids to names
// positionally through model.CreditMBIDs, so each split artist takes its own; the
// album-artist path resolves one entity for the whole credit and so passes an id
// only when the file named exactly one.
//
// Nothing enforces uniqueness on artist.mbid, so this can leave two rows sharing
// one, exactly as ApplyArtistEnrichment can. That is what audit's duplicate_artist
// reports and what merge fixes.
func resolveArtist(ctx context.Context, tx *sql.Tx, name, mbid string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	mk := identity.MatchKey(name)
	if mk == "" {
		return 0, nil
	}
	mbid = normMBID(mbid)
	var id int64
	var pid, cur string
	err := tx.QueryRowContext(ctx,
		"SELECT id, pid, COALESCE(mbid,'') FROM artist WHERE match_key = ?", mk).Scan(&id, &pid, &cur)
	if err == nil {
		// Free in both steady states, an artist that already has an id and the
		// untagged majority: the fill is reached at most once per artist per scan,
		// when a tag supplies an id the row lacks.
		if cur != "" || mbid == "" {
			return id, nil
		}
		wrote, err := fillEntityFieldTx(ctx, tx, model.MergeArtist, "artist", "mbid", id, mbid)
		if err != nil {
			return 0, err
		}
		// Only a real write emits a delta, so a no-op rescan stays change_log-silent.
		// A Picard retag is the most common way an id lands on an artist that already
		// took a name-keyed art no-match, so the marker is re-opened here too.
		if wrote {
			if err := artistMBIDLandedTx(ctx, tx, id); err != nil {
				return 0, err
			}
			return id, appendChange(ctx, tx, "artist", model.PID(pid), model.OpUpdate)
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	newPID := model.NewPID()
	r, err := tx.ExecContext(ctx,
		"INSERT INTO artist(pid, name, sort_key, match_key, mbid) VALUES (?,?,?,?,?)",
		string(newPID), name, model.SortKey(name), mk, nullStr(mbid))
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "artist", newPID, model.OpCreate)
}

// resolveReleaseGroup finds-or-creates a release group by its identity key, adopting a
// row that already holds the key's MusicBrainz id when the key itself misses.
//
// An existing row adopts the newly resolved primary artist when it differs. It has to:
// before the album-artist credit split, a joint credit resolved to one entity named
// for the whole string, and leaving that pointer would keep the combined artist alive,
// holding the release-group count that belongs to the real primary. The guard makes
// the steady state a no-op, so only the transition writes.
//
// Last-writer-wins is deliberate for the one case where tracks can disagree, an
// MBID-keyed group whose members carry different ALBUMARTIST tags. Under a heuristic
// key they cannot disagree, since the album-artist name is part of the key.
//
// The secondary lookup exists because enrichment fills the mbid column without moving
// the row's key, so a file that later arrives carrying that id computes an mbid: key
// that no row has. Adoption joins it to the row already holding its identity instead of
// forking a second one, and writes nothing, so there is no delta to reconcile. See
// adoptEntityByMBIDTx for why forward re-keying is not the answer.
func resolveReleaseGroup(ctx context.Context, tx *sql.Tx, log logger, key, title string, primaryArtistID int64, mbid string, affected *affectedRollups) (int64, error) {
	var id int64
	var curPrimary sql.NullInt64
	err := tx.QueryRowContext(ctx,
		"SELECT id, primary_artist_id FROM release_group WHERE match_key = ?", key).Scan(&id, &curPrimary)
	if errors.Is(err, sql.ErrNoRows) {
		adopted, aerr := adoptEntityByMBIDTx(ctx, tx, log, "release_group", key)
		if aerr != nil {
			return 0, aerr
		}
		if adopted != 0 {
			err = tx.QueryRowContext(ctx,
				"SELECT id, primary_artist_id FROM release_group WHERE id = ?", adopted).Scan(&id, &curPrimary)
		}
	}
	if err == nil {
		if primaryArtistID != 0 && curPrimary.Int64 != primaryArtistID {
			if _, err := tx.ExecContext(ctx,
				"UPDATE release_group SET primary_artist_id = ? WHERE id = ?", primaryArtistID, id); err != nil {
				return 0, err
			}
			// Both ends move a release_group_count, so both rollups need recomputing.
			if curPrimary.Valid {
				affected.artists[curPrimary.Int64] = true
			}
			affected.artists[primaryArtistID] = true
			// The group's artist is readable state (entity info serves it, and browse
			// keys on it), so a delta-sync consumer needs to be told it moved. Only the
			// transition reaches here, so a settled catalog stays change_log-silent.
			var pid string
			if err := tx.QueryRowContext(ctx,
				"SELECT pid FROM release_group WHERE id = ?", id).Scan(&pid); err != nil {
				return 0, err
			}
			if err := appendChange(ctx, tx, "release_group", model.PID(pid), model.OpUpdate); err != nil {
				return 0, err
			}
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	pid := model.NewPID()
	r, err := tx.ExecContext(ctx,
		"INSERT INTO release_group(pid, title, sort_key, primary_artist_id, type, match_key, mbid) VALUES (?,?,?,?,?,?,?)",
		string(pid), title, model.SortKey(title), nullInt64(primaryArtistID), "album", key, nullStr(normMBID(mbid)))
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "release_group", pid, model.OpCreate)
}

// resolveAlbum finds-or-creates a specific release/edition by its identity key,
// recording its disc total and MusicBrainz release id when known. As at the group rung,
// a key carrying a release id that matches nothing adopts the row already holding that
// id in its column rather than forking a second one.
//
// An existing row takes any of barcode/label/catalog_number/media/country it still
// lacks from tags that now supply them: they are not part of identity.AlbumKey, so a
// late tag pass hits the row already there. The year is topped up the same way even
// though it is a key segment, because an mbid key ignores it: without the top-up a
// year-less first file left the column NULL over members that carried theirs. The mbid
// is not filled here because it IS part of the key.
//
// Two shapes still fork, and both are narrower than what adoption closes. A file
// carrying a release-group id but no release id computes al:mbid:<group>\x1f…, which is
// not an mbid: key, so it lands on an album row of its own under the right group. And a
// partial retag splits as it always has, since the untagged members keep computing the
// heuristic key. The common Picard case carries both ids and adopts at both rungs.
//
// Adopting here can also leave a childless release group behind: if the file's anchor
// resolves a different group, resolveReleaseGroup mints one, then the album adopted here
// belongs to another group and a hit never repoints release_group_id. orphan.go sweeps
// the empty group (album before release_group), so it costs a create/delete delta pair
// per scan rather than leaking a row.
//
// media is first-scanned-file-wins, so a CD+DVD set whose CD track scanned first reads
// "CD". That is acceptable, not merely unfixed: the release matcher accepts a release
// when any of its mediums matches, so the true CD+DVD release and a CD-only sibling both
// survive and the tier refuses on the tie. It costs a decision, not a wrong one, among
// the editions MusicBrainz knows. Aggregating the album's distinct media would not help,
// since discriminating on the SET means requiring a release to cover every value, and
// narrowing is the shape the matcher rejects.
func resolveAlbum(ctx context.Context, tx *sql.Tx, log logger, key string, releaseGroupID int64, tr model.Track) (int64, error) {
	if key == "" {
		return 0, nil
	}
	const sel = `SELECT id, pid, COALESCE(barcode,''), COALESCE(label,''),
		COALESCE(catalog_number,''), COALESCE(media,''), COALESCE(country,''), COALESCE(year,0)
		FROM album WHERE `
	var id int64
	var pid, curBarcode, curLabel, curCatNo, curMedia, curCountry string
	var curYear int
	err := tx.QueryRowContext(ctx, sel+"match_key = ?", key).
		Scan(&id, &pid, &curBarcode, &curLabel, &curCatNo, &curMedia, &curCountry, &curYear)
	if errors.Is(err, sql.ErrNoRows) {
		adopted, aerr := adoptEntityByMBIDTx(ctx, tx, log, "album", key)
		if aerr != nil {
			return 0, aerr
		}
		if adopted != 0 {
			err = tx.QueryRowContext(ctx, sel+"id = ?", adopted).
				Scan(&id, &pid, &curBarcode, &curLabel, &curCatNo, &curMedia, &curCountry, &curYear)
		}
	}
	if err == nil {
		return id, fillAlbumIdentifiersTx(ctx, tx, id, model.PID(pid), tr,
			curBarcode, curLabel, curCatNo, curMedia, curCountry, curYear)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	newPID := model.NewPID()
	r, err := tx.ExecContext(ctx,
		`INSERT INTO album(pid, release_group_id, title, sort_key, year, disc_total, mbid,
			barcode, label, catalog_number, media, country, match_key) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(newPID), nullInt64(releaseGroupID), tr.Album, model.SortKey(tr.Album), nullInt(tr.Year),
		nullInt(tr.DiscTotal), nullStr(normMBID(tr.MBReleaseID)),
		nullStr(strings.TrimSpace(tr.Barcode)), nullStr(strings.TrimSpace(tr.Label)),
		nullStr(strings.TrimSpace(tr.CatalogNumber)), nullStr(strings.TrimSpace(tr.Media)),
		nullStr(strings.TrimSpace(tr.Country)), key)
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "album", newPID, model.OpCreate)
}

// adoptEntityByMBIDTx looks a release group or album up by its mbid column when an
// mbid:-form identity key found nothing, returning 0 when no row holds that id. table
// is a caller constant, never caller text.
//
// The invariant it establishes: match_key is the primary identity index, and the mbid
// column is a secondary one, consulted only when an mbid: key misses. db verify does not
// compare the two, so nothing else has to be taught the rule.
//
// Adoption, not a forward re-key onto the mbid key, and the difference is not a
// preference:
//
//   - At the album rung a forward re-key is a permanent fork. reconcileAlbumRekeyTx
//     refuses to carry it back (scanreconcile.go requires both keys to be heuristic,
//     since an mbid-keyed album carries no folder segment), so the album's untagged
//     members would compute the heuristic key, miss the now-mbid-keyed row, and mint a
//     fresh album that keeps accumulating them.
//   - At the group rung it churns forever. An untagged member computes rg:…, misses, and
//     inserts a new release group, while resolveAlbum still hits its album by that
//     album's unchanged al:rg:… key and a hit never repoints release_group_id. The new
//     group is born childless, orphan.go sweeps it, and the next scan mints it again.
//
// Nothing enforces uniqueness on either mbid column (see 03_entities.sql), so more than
// one row can answer. The lowest id wins for determinism and the extra hits are logged,
// which is the shape setAlbumMBIDTx already uses for its own collision; audit's
// duplicate_album and duplicate_release_group report the pair for merge to collapse.
//
// It writes nothing, so it needs no lock check: the EditEntityFields clear escape hatch
// empties the column, which makes a cleared-and-locked entity invisible here by
// construction.
func adoptEntityByMBIDTx(ctx context.Context, tx *sql.Tx, log logger, table, key string) (int64, error) {
	mbid, ok := strings.CutPrefix(key, mbidKeyPrefix)
	if !ok || mbid == "" {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT id FROM "+table+" WHERE mbid = ? ORDER BY id LIMIT 2", mbid)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > 1 {
		log.Warn("scan: MBID held by more than one entity; adopting the lowest",
			"table", table, "mbid", mbid, "adopted", ids[0], "other", ids[1])
	}
	return ids[0], nil
}

// fillEntityFieldTx fills one empty entity column and reports whether it wrote. The
// lock keeps a curated value, including a deliberately empty one the WHERE clause
// alone would refill. table and column are caller constants, never caller text.
func fillEntityFieldTx(ctx context.Context, tx *sql.Tx, entityType model.MergeEntity,
	table, column string, id int64, value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	locked, err := entityFieldLockedTx(ctx, tx, string(entityType), id, column)
	if err != nil || locked {
		return false, err
	}
	r, err := tx.ExecContext(ctx,
		"UPDATE "+table+" SET "+column+" = ? WHERE id = ? AND ("+column+" IS NULL OR "+column+" = '')",
		value, id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// fillAlbumIdentifiersTx tops up an existing album's release identifiers, the two
// descriptive edition columns, and the year, emitting one delta if anything landed. The
// cur values come from resolveAlbum's own lookup, so the common no-op costs nothing extra.
//
// The year goes through the same fill as the identifiers, so a curation lock on it would
// hold if one were ever written; today an album's year is curated through its members,
// whose locks the album fields walk reads. The column has integer affinity, so the text
// the shared fill binds is stored as the integer.
//
// A column the release matcher can decide on also clears a stale no-match marker, since
// a retag that adds a barcode must re-queue an album the weak edition tier failed on and
// the marker has no per-pass granularity. Only an unmatched one is cleared, because a
// matched album already carries the id this would re-search for: re-queueing it could
// write nothing (every mbid writer fills only when empty) and would discard the record of
// which tier decided it. Undoing a match is a deliberate act, and clearing the album's
// mbid through the entity-edit surface is what does it.
func fillAlbumIdentifiersTx(ctx context.Context, tx *sql.Tx, id int64, pid model.PID,
	tr model.Track, curBarcode, curLabel, curCatNo, curMedia, curCountry string, curYear int) error {
	var wrote, newEvidence bool
	for _, f := range []struct{ column, cur, val string }{
		{"barcode", curBarcode, tr.Barcode},
		{"label", curLabel, tr.Label},
		{"catalog_number", curCatNo, tr.CatalogNumber},
		{"media", curMedia, tr.Media},
		{"country", curCountry, tr.Country},
	} {
		if f.cur != "" {
			continue
		}
		w, err := fillEntityFieldTx(ctx, tx, model.MergeAlbum, "album", f.column, id,
			strings.TrimSpace(f.val))
		if err != nil {
			return err
		}
		wrote = wrote || w
		if w && slices.Contains(albumMatchEvidence, f.column) {
			newEvidence = true
		}
	}
	if newEvidence {
		if err := clearUnmatchedAlbumMarkerTx(ctx, tx, id); err != nil {
			return err
		}
	}
	if curYear == 0 && tr.Year != 0 {
		w, err := fillEntityFieldTx(ctx, tx, model.MergeAlbum, "album", "year", id, strconv.Itoa(tr.Year))
		if err != nil {
			return err
		}
		wrote = wrote || w
	}
	if !wrote {
		return nil
	}
	return appendChange(ctx, tx, "album", pid, model.OpUpdate)
}

// clearUnmatchedEntityMarkerTx removes one entity's no-match enrichment marker so a
// pass re-queues it on new evidence. It is a no-op on a matched marker, which records
// a real write.
func clearUnmatchedEntityMarkerTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM entity_enrichment WHERE entity_type = ? AND entity_id = ? AND matched = 0",
		entityType, entityID)
	return err
}

// clearUnmatchedAlbumMarkerTx removes an album's no-match enrichment marker so a pass
// re-queues it. It is a no-op on a matched marker; see fillAlbumIdentifiersTx.
func clearUnmatchedAlbumMarkerTx(ctx context.Context, tx *sql.Tx, albumID int64) error {
	return clearUnmatchedEntityMarkerTx(ctx, tx, model.EnrichAlbumType, albumID)
}

// entityMarkerMatchedTx reports whether an entity's enrichment marker records a match,
// so the undo path knows whether enrichment ever wrote anything to take back.
func entityMarkerMatchedTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64) (bool, error) {
	var matched int
	err := tx.QueryRowContext(ctx,
		"SELECT matched FROM entity_enrichment WHERE entity_type = ? AND entity_id = ?",
		entityType, entityID).Scan(&matched)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return matched == 1, err
}

// albumMarkerMatchedTx reports whether an album's enrichment marker records a match; see
// entityMarkerMatchedTx.
func albumMarkerMatchedTx(ctx context.Context, tx *sql.Tx, albumID int64) (bool, error) {
	return entityMarkerMatchedTx(ctx, tx, model.EnrichAlbumType, albumID)
}

// clearEntityMarkerTx removes an entity's enrichment marker whatever it recorded, for the
// callers that undo a match outright by clearing the MusicBrainz id it was made on.
func clearEntityMarkerTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM entity_enrichment WHERE entity_type = ? AND entity_id = ?",
		entityType, entityID)
	return err
}

// clearAlbumMarkerTx removes an album's enrichment marker whatever it recorded; see
// clearEntityMarkerTx.
func clearAlbumMarkerTx(ctx context.Context, tx *sql.Tx, albumID int64) error {
	return clearEntityMarkerTx(ctx, tx, model.EnrichAlbumType, albumID)
}

// resolveGenre finds-or-creates a genre/mood entity by (facet, match key).
func resolveGenre(ctx context.Context, tx *sql.Tx, facet model.GenreFacet, name string) (int64, error) {
	mk := identity.MatchKey(name)
	if mk == "" {
		return 0, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM genre WHERE facet = ? AND match_key = ?", string(facet), mk).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	pid := model.NewPID()
	r, err := tx.ExecContext(ctx,
		"INSERT INTO genre(pid, facet, name, match_key, sort_key) VALUES (?,?,?,?,?)",
		string(pid), string(facet), name, mk, model.SortKey(name))
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "genre", pid, model.OpCreate)
}

// syncItemGenres replaces an item's genre set. The adapter usually supplies
// already-split genres; rawGenre is re-split as a fallback (e.g. a single tag
// holding "Rock; Pop"). Replace semantics so a retag that drops a genre updates
// the links. Duplicates are removed by match key, preserving first-seen casing.
func syncItemGenres(ctx context.Context, tx *sql.Tx, itemID int64, genres []string, rawGenre string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_genre WHERE item_id = ?", itemID); err != nil {
		return err
	}
	names := dedupGenres(genres)
	if len(names) == 0 {
		names = identity.SplitGenres(rawGenre)
	}
	for _, name := range names {
		gid, err := resolveGenre(ctx, tx, model.FacetGenre, name)
		if err != nil {
			return err
		}
		if gid == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO item_genre(item_id, genre_id) VALUES (?, ?)", itemID, gid); err != nil {
			return err
		}
	}
	return nil
}

// dedupGenres splits any residual separators in already-listed genres and
// removes duplicates by match key, preserving first-seen display casing. A
// provider that returns one "Rock/Pop" element is still normalized to two.
func dedupGenres(genres []string) []string {
	var out []string
	seen := make(map[string]bool, len(genres))
	for _, g := range genres {
		for _, name := range identity.SplitGenres(g) {
			mk := identity.MatchKey(name)
			if mk == "" || seen[mk] {
				continue
			}
			seen[mk] = true
			out = append(out, name)
		}
	}
	return out
}

// syncSearchFTS rebuilds the item's metadata FTS row (rowid == item id). The
// table is writer-maintained with no triggers, so the delete-then-insert keeps
// it consistent inside the mutating transaction.
func syncSearchFTS(ctx context.Context, tx *sql.Tx, itemID int64, tr model.Track) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM search_fts WHERE rowid = ?", itemID); err != nil {
		return err
	}
	var title string
	if err := tx.QueryRowContext(ctx, "SELECT title FROM playable_item WHERE id = ?", itemID).Scan(&title); err != nil {
		return err
	}
	artist := strings.TrimSpace(tr.Artist + " " + tr.AlbumArtist)
	custom, err := itemCustomTagText(ctx, tx, itemID)
	if err != nil {
		return err
	}
	extra := strings.TrimSpace(tr.Genre + " " + custom)
	_, err = tx.ExecContext(ctx,
		"INSERT INTO search_fts(rowid, kind, title, subtitle, artist, album, extra) VALUES (?,?,?,?,?,?,?)",
		itemID, string(model.KindTrack), title, "", artist, tr.Album, extra)
	return err
}
