package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// This file holds the album re-key reconciliation the scan path needs. A heuristic
// album key embeds the folder, the year, and the release group above it, so moving a
// release on disk or retagging its title, artist, or year keys every member onto a fresh
// album row while the old row drains to zero tracks and ghosts with its pid, curation,
// art, and stars until a manual GC destroys it. A scan resolves one file at a time and
// has no batch to check all-members against, the way the edit-path rename pre-pass does,
// so the detection lives at the write that re-keys: reconcileAlbumRekeyTx runs inside the
// same transaction, just after the track's album FK moves, and does its work on the file
// whose move empties the old row. That also catches a latent ghost, since an organize
// without retag only rewrites file paths and the re-key waits for a later forced or
// content-changing scan.
//
// Eight guards stand between a drained row and the carry, and every one of them must
// pass: both album ids are present and different, the old row holds no tracks at all,
// both rows still exist, both keys are heuristic (an mbid-keyed album carries no folder
// and survives a move untouched), something corroborates that the two rows are one
// release, the destination key has a folder segment, the two albums do not carry
// different MusicBrainz ids, and neither do the release groups above them. The folder
// segment matters on its own: an empty one is a key no scan of a real file computes, so
// moving the old row onto it would strand its attachments on the next resolve. The two
// id guards are there because a heuristic key says nothing about the mbid column, which
// enrichment fills without re-keying, so two rows naming genuinely different releases
// can reach this point with the retag evidence looking sound. Conflicting ids refuse the
// carry outright, on either rung.
//
// The corroboration is what keeps two albums that merely landed on one key from folding
// together, and any one of these signals is enough:
//
//   - the two rows sit under one release group, which a plain move leaves standing and
//     no retag of the title or artist does
//   - both keys carry the same folder segment, which is the retag in place
//   - the file reached this scan through an essence relink out of the folder the old key
//     names, which is the retag done with a move
//   - the file's newest committed organize_journal row runs from the old key's folder to
//     the new key's, which is the organize this scan is catching up with. Both ends are
//     compared, so a row left by some earlier hop corroborates nothing, which is what
//     the journal needs given that nothing ever prunes it.
//
// The comparison is fuzzy rather than exact. It runs on match keys, which fold
// punctuation and separators to single spaces, so two genuinely different folders can
// fold to one string. What carries the rest of the weight is the precondition: the old
// row is empty, the new one is where its members just went, and the guards above have
// already refused everything that is not a heuristic pair of album rows.
//
// The relink signal is the loosest of the four. An album whose members lived in the
// folder its key names satisfies it by construction, since a relink of any of them runs
// out of that folder, so moving those files into another release's folder and retagging
// them there reads as corroborated. What stands between that and a wrong fold is how
// specific the old key's folder segment is, plus the id refusals above.
//
// What carries: when the destination row is bare the old row survives, keeps its id and
// pid, and adopts the new key and display columns, which is the in-place rename analogue.
// Because the folder-segment signal alone is corroboration enough, this can also fold two
// heuristic albums from different release groups that merely share a folder, when a retag
// drains one onto the other: the bare destination merges away and the drained row's pid
// takes over its key. When the destination already carries attachments of its own it
// survives instead and the drained row folds into it through mergeEntityTx: locked-wins
// curation, survivor-wins art per role, star fold, mbid union, loser OpDelete. Either way
// what the old row held reaches a live album. A retag that re-keyed the release group as
// well moves the surviving album under the group its new key names, and a group the carry
// leaves holding no albums folds together with that one, so the ghost does not simply
// move up a level. Which of the two groups survives follows the album rung: a
// destination group this scan just created gives way to the old one, which keeps its pid
// and adopts the new key, while a destination holding attachments or albums of its own
// survives instead. Artist rows are untouched, since a move never re-keys them.
//
// What falls through to the ghost, silently and on purpose:
//
//   - a partial move, where the old row still holds members. The reconciliation waits
//     for the file that drains it, so a half-moved release stays split until the rest
//     arrives, and stays split for good if it never does.
//   - an album holding duplicate copies of a file, which never relinks: the essence
//     match is deliberately exactly-one, so a copied file scans as a new file and the
//     old row keeps a member
//   - an album whose members scatter across several folders, where the destination of
//     the last re-keyed file takes the carry. A batch edit that splits one album across
//     several titles inside one folder ends the same way. It is one live album rather
//     than a ghost, which is what matters; picking a better winner is not worth the
//     machinery for a layout a user made by hand.
//   - two editions of one release group trading folders in a single scan, where each
//     drain reconciles onto whatever row the other left behind. The attachments stay on
//     live albums, but which edition ends up wearing which is the order the files were
//     walked in.

// rekeyAlbum is the state of one side of a candidate re-key, read before any write so
// the merge branch can adopt values from a row it is about to delete.
type rekeyAlbum struct {
	id    int64
	pid   string
	key   string
	title string
	mbid  sql.NullString
	year  sql.NullInt64
	rgID  sql.NullInt64
}

// rekeyGroup is the state of one side of a release-group fold, read for the same
// reason: the surviving row adopts the identity of the one being deleted.
type rekeyGroup struct {
	id       int64
	pid      string
	key      string
	title    string
	mbid     sql.NullString
	artistID sql.NullInt64
}

// reconcileAlbumRekeyTx carries an album's identity and attachments onto the row a scan
// just re-keyed its members to. It takes the album the track sat on before the FK update
// and the one it sits on after, plus the two corroborating signals the caller holds: the
// path the file was relinked from (empty when it was found where it already was) and its
// file id, for the organize journal. Any guard failure returns nil and leaves the old row
// to ghost.
func reconcileAlbumRekeyTx(ctx context.Context, tx *sql.Tx, priorAlbumID, newAlbumID int64, priorPath string, fileID int64, affected *affectedRollups) error {
	if priorAlbumID == 0 || newAlbumID == 0 || priorAlbumID == newAlbumID {
		return nil
	}
	var members int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM track WHERE album_id=?", priorAlbumID).Scan(&members); err != nil {
		return err
	}
	if members != 0 {
		return nil
	}
	prior, err := loadRekeyAlbumTx(ctx, tx, priorAlbumID)
	if err != nil || prior == nil {
		return err
	}
	dest, err := loadRekeyAlbumTx(ctx, tx, newAlbumID)
	if err != nil || dest == nil {
		return err
	}
	// Only a heuristic pair can be a folder or retag re-key; an mbid-keyed album carries
	// no folder segment and survives both untouched.
	if !strings.HasPrefix(prior.key, heuristicAlbumKeyPrefix) ||
		!strings.HasPrefix(dest.key, heuristicAlbumKeyPrefix) {
		return nil
	}
	corroborated, err := rekeyCorroboratedTx(ctx, tx, prior, dest, priorPath, fileID)
	if err != nil || !corroborated {
		return err
	}
	// An empty folder segment is a key no scan of a real file computes, so moving the
	// old row onto it would strand its attachments on the next resolve.
	if albumKeyFolder(dest.key) == "" {
		return nil
	}
	// Two different MusicBrainz ids are two different releases, whatever the folders
	// say. A row keeps its heuristic key while enrichment fills the mbid column, so a
	// pair like this reaches here with every guard above satisfied.
	if mbidsConflict(prior.mbid, dest.mbid) {
		return nil
	}
	// The same rule for the groups above them, which the strip topology leaves
	// identified over heuristic albums.
	conflict, err := releaseGroupMBIDConflictTx(ctx, tx, prior.rgID, dest.rgID)
	if err != nil || conflict {
		return err
	}

	attached, err := albumHasAttachmentsTx(ctx, tx, dest.id)
	if err != nil {
		return err
	}
	// A retag re-keys the release group as well, so the two rows can sit under different
	// ones and both rollups are dirty.
	if prior.rgID.Valid {
		affected.rgs[prior.rgID.Int64] = true
	}
	if dest.rgID.Valid {
		affected.rgs[dest.rgID.Int64] = true
	}

	if attached {
		// The destination is an album a user already curated, so it keeps its identity
		// and absorbs the drained row. This is also the answer to files moving into a
		// folder that already holds a release: without it the old row's curation would
		// be dropped on the floor rather than merged.
		if _, err := mergeEntityTx(ctx, tx, model.MergeAlbum, "album",
			model.PID(dest.pid), model.PID(prior.pid)); err != nil {
			return err
		}
		return foldDrainedReleaseGroupTx(ctx, tx, prior, dest, affected)
	}

	// The destination carries no attachments of its own, so the old row survives with
	// its pid and takes over the new key. That is usually the row this scan just created
	// under the new folder; when it is instead a populated but uncurated album, the
	// drained curated row keeps its pid and absorbs it, which is the same carry seen
	// from the other side. mergeEntityTx moves the members (the current track's FK
	// included) onto the survivor and deletes the other row, which frees the key for the
	// rewrite below.
	if _, err := mergeEntityTx(ctx, tx, model.MergeAlbum, "album",
		model.PID(prior.pid), model.PID(dest.pid)); err != nil {
		return err
	}
	// The release group goes with the key. Nothing else moves it (an album merge repoints
	// tracks, not the album's own group), so without this the survivor would wear a key
	// naming one group while pointing at another, and every later scan would fork it off
	// again.
	rgID := prior.rgID
	if dest.rgID.Valid {
		rgID = dest.rgID
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE album SET match_key=?, title=?, year=?, release_group_id=? WHERE id=?",
		dest.key, dest.title, dest.year, rgID, prior.id); err != nil {
		return err
	}
	if err := refreshEntitySortKeyTx(ctx, tx, model.MergeAlbum, "album", prior.id); err != nil {
		return err
	}
	// No delta here: mergeEntityTx already wrote the survivor's OpUpdate, and a consumer
	// refetches after the transaction commits, so it reads the rewritten key.
	return foldDrainedReleaseGroupTx(ctx, tx, prior, dest, affected)
}

// rekeyCorroboratedTx reports whether anything beyond the coincidence of a drain ties
// the two rows together. See the signals enumerated at the top of this file; any one of
// them is enough. Both folder segments must be non-empty before any of the folder
// comparisons mean anything, since two empty ones match each other and say nothing.
func rekeyCorroboratedTx(ctx context.Context, tx *sql.Tx, prior, dest *rekeyAlbum, priorPath string, fileID int64) (bool, error) {
	if prior.rgID.Valid && dest.rgID.Valid && prior.rgID.Int64 == dest.rgID.Int64 {
		return true, nil
	}
	priorFolder, destFolder := albumKeyFolder(prior.key), albumKeyFolder(dest.key)
	if priorFolder == "" || destFolder == "" {
		return false, nil
	}
	if priorFolder == destFolder {
		return true, nil
	}
	if priorPath != "" && identity.MatchKey(filepath.Dir(priorPath)) == priorFolder {
		return true, nil
	}
	return organizeMovedFolderTx(ctx, tx, fileID, priorFolder, destFolder)
}

// organizeMovedFolderTx reports whether the file's newest committed organize move runs
// from priorFolder to destFolder. The journal keeps every move ever made, so only the
// newest row can describe the hop this re-key is catching up with, and both of its ends
// are compared to keep an older one from corroborating on its own. A row whose file was
// deleted holds a NULL file_id and is never read.
func organizeMovedFolderTx(ctx context.Context, tx *sql.Tx, fileID int64, priorFolder, destFolder string) (bool, error) {
	if fileID == 0 {
		return false, nil
	}
	var src, dst []byte
	err := tx.QueryRowContext(ctx,
		`SELECT src, dst FROM organize_journal WHERE file_id=? AND state='committed'
		 ORDER BY created_at DESC, id DESC LIMIT 1`, fileID).Scan(&src, &dst)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return identity.MatchKey(filepath.Dir(string(src))) == priorFolder &&
		identity.MatchKey(filepath.Dir(string(dst))) == destFolder, nil
}

// foldDrainedReleaseGroupTx folds the old album's release group together with the
// destination's when the carry left it with no albums. A retag re-keys the group too,
// so without this the ghost the album carry just cleared would reappear one level up,
// holding the group's own stars, art, curation, mbid, and enrichment marker.
//
// Which of the two survives mirrors the album rung. A destination group a scan just
// created for the new key holds nothing but the album that carried onto it, so the old
// group survives, keeps its pid and attachments, and adopts the new identity, the way
// an in-place rename would. A destination that carries attachments or albums of its own
// survives instead and the drained group folds into it.
func foldDrainedReleaseGroupTx(ctx context.Context, tx *sql.Tx, prior, dest *rekeyAlbum, affected *affectedRollups) error {
	if !prior.rgID.Valid || !dest.rgID.Valid || prior.rgID.Int64 == dest.rgID.Int64 {
		return nil
	}
	var albums int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM album WHERE release_group_id=?", prior.rgID.Int64).Scan(&albums); err != nil {
		return err
	}
	if albums != 0 {
		return nil
	}
	priorGroup, err := loadRekeyGroupTx(ctx, tx, prior.rgID.Int64)
	if err != nil || priorGroup == nil {
		return err
	}
	destGroup, err := loadRekeyGroupTx(ctx, tx, dest.rgID.Int64)
	if err != nil || destGroup == nil {
		return err
	}
	// The album rung already refused a carry across conflicting ids, so nothing reaches
	// here with two of them. The fold checks anyway rather than trust its caller.
	if mbidsConflict(priorGroup.mbid, destGroup.mbid) {
		return nil
	}
	fresh, err := freshReleaseGroupTx(ctx, tx, destGroup.id)
	if err != nil {
		return err
	}
	if !fresh {
		_, err := mergeEntityTx(ctx, tx, model.MergeReleaseGroup, "release_group",
			model.PID(destGroup.pid), model.PID(priorGroup.pid))
		return err
	}
	// The merge repoints the carried album back onto the old group and frees the fresh
	// row's unique match_key, which the rewrite below then takes.
	if _, err := mergeEntityTx(ctx, tx, model.MergeReleaseGroup, "release_group",
		model.PID(priorGroup.pid), model.PID(destGroup.pid)); err != nil {
		return err
	}
	// The rewrite below flips the survivor's primary artist after the merge already
	// recomputed rollups, so mark both artists for the caller's rollup maintenance or
	// their release_group_count goes stale. No delta for the rewrite: mergeEntityTx
	// already wrote the survivor's OpUpdate, and a consumer refetches after the
	// transaction commits.
	if priorGroup.artistID.Valid {
		affected.artists[priorGroup.artistID.Int64] = true
	}
	if destGroup.artistID.Valid {
		affected.artists[destGroup.artistID.Int64] = true
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE release_group SET match_key=?, title=?, primary_artist_id=? WHERE id=?",
		destGroup.key, destGroup.title, destGroup.artistID, priorGroup.id); err != nil {
		return err
	}
	return refreshEntitySortKeyTx(ctx, tx, model.MergeReleaseGroup, "release_group", priorGroup.id)
}

// freshReleaseGroupTx reports whether a release group reads as fresh: nothing attached
// to it, and no album but the one the carry moved onto it. That is usually a row this
// scan just made for the new key, though an established group that never gained
// attachments and holds a single album classifies the same way. The attachments are the
// three albumHasAttachmentsTx reads, and enrichment markers are left out there for the
// same reason.
func freshReleaseGroupTx(ctx context.Context, tx *sql.Tx, rgID int64) (bool, error) {
	var attached int
	if err := tx.QueryRowContext(ctx, `SELECT
		  (SELECT COUNT(*) FROM art_map WHERE entity_type='release_group' AND entity_id=?1)
		+ (SELECT COUNT(*) FROM entity_curation WHERE entity_type='release_group' AND entity_id=?1)
		+ (SELECT COUNT(*) FROM entity_play_state WHERE entity_type='release_group' AND entity_id=?1)`,
		rgID).Scan(&attached); err != nil {
		return false, err
	}
	if attached > 0 {
		return false, nil
	}
	var albums int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM album WHERE release_group_id=?", rgID).Scan(&albums); err != nil {
		return false, err
	}
	return albums == 1, nil
}

// mbidsConflict reports whether two rows carry different MusicBrainz ids. One id and
// one blank is not a conflict, since the typo-fix rule lets a lone id ride a rename.
func mbidsConflict(a, b sql.NullString) bool {
	x, y := strings.TrimSpace(a.String), strings.TrimSpace(b.String)
	return x != "" && y != "" && x != y
}

// releaseGroupMBIDConflictTx reports whether the two albums' groups carry different
// MusicBrainz ids. One album without a group, or both under the same one, can hold no
// conflict and reads nothing.
func releaseGroupMBIDConflictTx(ctx context.Context, tx *sql.Tx, priorRG, destRG sql.NullInt64) (bool, error) {
	if !priorRG.Valid || !destRG.Valid || priorRG.Int64 == destRG.Int64 {
		return false, nil
	}
	var a, b sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT mbid FROM release_group WHERE id=?", priorRG.Int64).Scan(&a); err != nil {
		return false, err
	}
	if err := tx.QueryRowContext(ctx,
		"SELECT mbid FROM release_group WHERE id=?", destRG.Int64).Scan(&b); err != nil {
		return false, err
	}
	return mbidsConflict(a, b), nil
}

// heuristicAlbumKeyPrefix marks an album key derived from tags and folder rather than a
// MusicBrainz release id. See identity.AlbumKey.
const heuristicAlbumKeyPrefix = "al:"

// albumKeyFolder returns the folder component of a heuristic album key, which is its
// last unit-separated segment.
func albumKeyFolder(key string) string {
	i := strings.LastIndexByte(key, 0x1f)
	if i < 0 {
		return ""
	}
	return key[i+1:]
}

// loadRekeyAlbumTx reads one album's re-key state, returning nil when the row is gone.
func loadRekeyAlbumTx(ctx context.Context, tx *sql.Tx, albumID int64) (*rekeyAlbum, error) {
	a := &rekeyAlbum{id: albumID}
	err := tx.QueryRowContext(ctx,
		"SELECT pid, match_key, title, mbid, year, release_group_id FROM album WHERE id=?", albumID).
		Scan(&a.pid, &a.key, &a.title, &a.mbid, &a.year, &a.rgID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// loadRekeyGroupTx reads one release group's fold state, returning nil when the row is
// gone.
func loadRekeyGroupTx(ctx context.Context, tx *sql.Tx, rgID int64) (*rekeyGroup, error) {
	g := &rekeyGroup{id: rgID}
	err := tx.QueryRowContext(ctx,
		"SELECT pid, match_key, title, mbid, primary_artist_id FROM release_group WHERE id=?", rgID).
		Scan(&g.pid, &g.key, &g.title, &g.mbid, &g.artistID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

// albumHasAttachmentsTx reports whether an album carries anything a re-key must not
// displace: mapped art, curation rows (locks included), or a user's play state. Those
// three are the user-visible attachments, and they are polymorphic with no foreign key,
// so nothing else moves them for us.
//
// Enrichment markers are deliberately not consulted. mergeEntityTx unions or refreshes
// them either way, and a marker alone is not worth keeping a row alive for.
func albumHasAttachmentsTx(ctx context.Context, tx *sql.Tx, albumID int64) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT
		  (SELECT COUNT(*) FROM art_map WHERE entity_type='album' AND entity_id=?1)
		+ (SELECT COUNT(*) FROM entity_curation WHERE entity_type='album' AND entity_id=?1)
		+ (SELECT COUNT(*) FROM entity_play_state WHERE entity_type='album' AND entity_id=?1)`,
		albumID).Scan(&n)
	return n > 0, err
}
