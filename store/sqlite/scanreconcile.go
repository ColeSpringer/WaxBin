package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/colespringer/waxbin/model"
)

// This file holds the album re-key reconciliation the scan path needs. A heuristic
// album key embeds the folder, so moving a release on disk and rescanning it keys every
// member onto a fresh album row while the old row drains to zero tracks and ghosts with
// its pid, curation, art, and stars until a manual GC destroys it. A scan resolves one
// file at a time and has no batch to check all-members against, the way the edit-path
// rename pre-pass does, so the detection lives at the write that re-keys:
// reconcileAlbumRekeyTx runs inside the same transaction, just after the track's album
// FK moves, and does its work on the file whose move empties the old row. That also
// catches a latent ghost, since an organize without retag only rewrites file paths and
// the re-key waits for a later forced or content-changing scan.
//
// What carries: when the destination row is bare the old row survives, keeps its id and
// pid, and adopts the new key and display columns, which is the in-place rename
// analogue. When the destination already carries attachments of its own it survives
// instead and the drained row folds into it through mergeEntityTx: locked-wins curation,
// survivor-wins art per role, star fold, mbid union, loser OpDelete. Either way what the
// old row held reaches a live album. The release group and artist rows are untouched,
// since their keys carry no folder and a move never re-keys them.
//
// What falls through to the ghost, silently and on purpose:
//
//   - a title or artist retag, which re-keys the release group too, so the two rows sit
//     under different groups and nothing ties them together
//   - a partial move, where the old row still holds members. The reconciliation waits
//     for the file that drains it, so a half-moved release stays split until the rest
//     arrives, and stays split for good if it never does.
//   - an album holding duplicate copies of a file, which never relinks: the essence
//     match is deliberately exactly-one, so a copied file scans as a new file and the
//     old row keeps a member
//   - an album whose members scatter across several folders, where the destination of
//     the last re-keyed file takes the carry. It is one live album rather than a ghost,
//     which is what matters; picking a better winner is not worth the machinery for a
//     layout a user made by hand.
//   - two editions of one release group trading folders in a single scan, where each
//     drain reconciles onto whatever row the other left behind. The attachments stay on
//     live albums, but which edition ends up wearing which is the order the files were
//     walked in.
//
// A year retag carries, because the year is part of the album key and not the group's.
// An external retag that drains the whole album is the scan equivalent of a whole-set
// year edit, which the edit pre-pass already renames in place.

// rekeyAlbum is the state of one side of a candidate re-key, read before any write so
// the merge branch can adopt values from a row it is about to delete.
type rekeyAlbum struct {
	id    int64
	pid   string
	key   string
	title string
	year  sql.NullInt64
	rgID  sql.NullInt64
}

// reconcileAlbumRekeyTx carries an album's identity and attachments onto the row a scan
// just re-keyed its members to. It takes the album the track sat on before the FK update
// and the one it sits on after; any guard failure returns nil and leaves the old row to
// ghost exactly as it does today.
func reconcileAlbumRekeyTx(ctx context.Context, tx *sql.Tx, priorAlbumID, newAlbumID int64, affected *affectedRollups) error {
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
	// Only a heuristic pair can be a folder re-key; an mbid-keyed album carries no
	// folder segment and survives a move untouched. The shared release group is the
	// evidence that the two rows are one release, which is also what keeps a title or
	// artist retag out: those re-key the group, so the rows land under different ones.
	if !strings.HasPrefix(prior.key, heuristicAlbumKeyPrefix) ||
		!strings.HasPrefix(dest.key, heuristicAlbumKeyPrefix) {
		return nil
	}
	if !prior.rgID.Valid || !dest.rgID.Valid || prior.rgID.Int64 != dest.rgID.Int64 {
		return nil
	}
	// An empty folder segment is a key no scan of a real file computes, so moving the
	// old row onto it would strand its attachments on the next resolve.
	if albumKeyFolder(dest.key) == "" {
		return nil
	}

	attached, err := albumHasAttachmentsTx(ctx, tx, dest.id)
	if err != nil {
		return err
	}
	// Both rows sit under the same release group, which the guard above established, so
	// either branch dirties that one rollup.
	affected.rgs[dest.rgID.Int64] = true

	if attached {
		// The destination is an album a user already curated, so it keeps its identity
		// and absorbs the drained row. This is also the answer to files moving into a
		// folder that already holds a release: without it the old row's curation would
		// be dropped on the floor rather than merged.
		if _, err := mergeEntityTx(ctx, tx, model.MergeAlbum, "album",
			model.PID(dest.pid), model.PID(prior.pid)); err != nil {
			return err
		}
		return nil
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
	if _, err := tx.ExecContext(ctx,
		"UPDATE album SET match_key=?, title=?, year=? WHERE id=?",
		dest.key, dest.title, dest.year, prior.id); err != nil {
		return err
	}
	if err := refreshEntitySortKeyTx(ctx, tx, model.MergeAlbum, "album", prior.id); err != nil {
		return err
	}
	// No delta here: mergeEntityTx already wrote the survivor's OpUpdate, and a consumer
	// refetches after the transaction commits, so it reads the rewritten key.
	return nil
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
		"SELECT pid, match_key, title, year, release_group_id FROM album WHERE id=?", albumID).
		Scan(&a.pid, &a.key, &a.title, &a.year, &a.rgID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
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
