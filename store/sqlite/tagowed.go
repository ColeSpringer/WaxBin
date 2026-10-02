package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// noteOwedItemTx records, inside the edit that changed them, that an item's files lag the
// catalog on keys (model.DiagTagWriteOwed). An episode, whose tags are its feed's, and a
// file several items share or one carrying a cue window are left out: no write-back can
// ever pay those.
func noteOwedItemTx(ctx context.Context, tx *sql.Tx, itemID int64, kind string, keys []string) error {
	if len(keys) == 0 || (kind != string(model.KindTrack) && kind != string(model.KindBook)) {
		return nil
	}
	ids, err := queryInt64sTx(ctx, tx, `SELECT f.id FROM item_file itf JOIN file f ON f.id = itf.file_id
		WHERE itf.item_id = ? AND NOT `+fileSharedOrVirtualExpr, itemID)
	if err != nil {
		return err
	}
	return noteOwedTx(ctx, tx, ids, keys)
}

// noteOwedMembersTx is noteOwedItemTx over the member files an entity's values are
// written to (entityMemberFilesFrom).
func noteOwedMembersTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, entityID int64, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	ids, err := queryInt64sTx(ctx, tx, "SELECT DISTINCT f.id "+entityMemberFilesFrom(et)+
		" AND NOT "+fileSharedOrVirtualExpr, entityID)
	if err != nil {
		return err
	}
	return noteOwedTx(ctx, tx, ids, keys)
}

func noteOwedTx(ctx context.Context, tx *sql.Tx, fileIDs []int64, keys []string) error {
	now := nowNS()
	for _, id := range fileIDs {
		for _, k := range keys {
			if err := upsertFileDiagnosticTx(ctx, tx, id, model.OriginEdit, "", model.FileDiagnostic{
				Code: model.DiagTagWriteOwed, Severity: model.SeverityInfo, TagKey: k,
				Detail: k + " was edited in the catalog and not written to the file",
			}, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// SettleTagWriteOwed clears a file's owed rows for fields, once a write-back landed them.
func (s *Store) SettleTagWriteOwed(ctx context.Context, filePID model.PID, fields []string) error {
	const op = "store.SettleTagWriteOwed"
	if len(fields) == 0 {
		return nil
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		fileID, err := idByPIDTx(ctx, tx, "file", filePID, op)
		if err != nil {
			return err
		}
		if err := settleTagWriteOwedTx(ctx, tx, fileID, fields); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
}

func settleTagWriteOwedTx(ctx context.Context, tx *sql.Tx, fileID int64, fields []string) error {
	return deleteOwedTx(ctx, tx, "file_id = ?", fileID, withOwedTwins(fields))
}

// owedTwins pairs the scalar spelling of a column with its credit's, which share the tags
// a write-back writes, so one landing pays an owed row about the other. A scan settles them
// the same way, as one column.
var owedTwins = func() map[string]string {
	twins := map[string]string{}
	for col, role := range map[string]model.ContributorRole{
		"artist": model.RoleArtist, "composer": model.RoleComposer,
		"author": model.RoleAuthor, "narrator": model.RoleNarrator,
	} {
		twins[col] = model.CreditField(role)
		twins[model.CreditField(role)] = col
	}
	return twins
}()

// withOwedTwins adds the other spelling of each field that has one.
func withOwedTwins(fields []string) []string {
	out := slices.Clone(fields)
	for _, f := range fields {
		if twin, ok := owedTwins[f]; ok && !slices.Contains(out, twin) {
			out = append(out, twin)
		}
	}
	return out
}

// deleteOwedTx deletes the owed rows naming fields on the files where picks them.
func deleteOwedTx(ctx context.Context, tx *sql.Tx, where string, arg int64, fields []string) error {
	if len(fields) == 0 {
		return nil
	}
	args := make([]any, 0, len(fields)+3)
	args = append(args, arg, string(model.OriginEdit), string(model.DiagTagWriteOwed))
	for _, f := range fields {
		args = append(args, f)
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM file_diagnostic WHERE `+where+`
		AND origin = ? AND code = ? AND tag_key IN `+placeholders(len(fields)), args...)
	return err
}

// owedFieldsTx lists the fields a file's owed rows name.
func owedFieldsTx(ctx context.Context, tx *sql.Tx, fileID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT tag_key FROM file_diagnostic
		WHERE file_id = ? AND origin = ? AND code = ?`, fileID, string(model.OriginEdit), string(model.DiagTagWriteOwed))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// scanSettle is what a scan put knows that pays owed rows: what the file says, from
// before the lock overlay, and what the put did with it.
type scanSettle struct {
	isBook              bool
	fileTitle, title    string
	fileTrack, track    model.Track         // a track's values before and after the overlay
	fileBook            model.Book          // a book part's values before the overlay
	bookRederived       bool                // upsertBook rewrote the book's unlocked fields
	preserveLocks       bool                // a locked field kept its catalog value
	derived             []string            // fields the file's tags do not state (PutScannedTrackInput.Derived)
	cover               *model.ArtImage     // the scanned cover; the file's own when Source is tag
	acquisitionRecorded bool                // the put recorded an acquisition from the tags
	fileTags            map[string][]string // the file's custom tags
	tagsReplaced        []string            // the custom tag keys the put took from this file
}

// settleOwedByScanTx clears the owed rows a scan put pays. A row goes from this file once
// the catalog holds what the file says, and from every file of the item once the put
// re-derived the value from disk, since the edit the rows were about is then gone.
//
// An item field or credit goes from this file when the file's value is the catalog's
// after the put (an unlocked track field is, once the put re-derived it rather than kept
// an enrichment fill over a silent file), and a book's from every part when upsertBook
// re-derived it unlocked from the primary part. A locked value the
// file's tags do not state stays owed even when a fallback reproduces it. The cover goes
// everywhere once the item's front came from disk, and from this file when its embedded
// picture is the catalog's; an acquisition goes when the put recorded one from the tags,
// which undoes a clear; an album or release-group id goes when the file names the one the
// item now sits under, which a re-resolving scan restores after a clear or a detach. A
// custom tag goes from this file when the file's values are the catalog's, and from
// every file once the put replaced the catalog's values with the file's. An entity's
// identifiers and sort and an album's cover are never re-derived by a scan, so only the
// write-back that lands them pays those; an item that moves off the entity drops them
// (dropLeftEntityOwedTx).
func settleOwedByScanTx(ctx context.Context, tx *sql.Tx, fileID, itemID int64, s scanSettle) error {
	owed, err := owedFieldsTx(ctx, tx, fileID)
	if err != nil || len(owed) == 0 {
		return err
	}
	var here, everywhere []string
	var locked map[string]bool
	lockedFields := func() (map[string]bool, error) {
		if locked == nil && s.preserveLocks {
			m, err := lockedFieldSetTx(ctx, tx, itemID)
			if err != nil {
				return nil, err
			}
			if m == nil {
				m = map[string]bool{}
			}
			locked = m
		}
		return locked, nil
	}
	// lockedFallback reports a column whose value only a lock and a fallback agree on:
	// the file's tags do not state it, so agreeing with the catalog pays nothing.
	lockedFallback := func(col string) (bool, error) {
		if !slices.Contains(s.derived, col) {
			return false, nil
		}
		l, err := lockedFields()
		return l[col] || (col == "artist" && l[model.CreditField(model.RoleArtist)]), err
	}
	var stored *model.Book
	// The item's custom tags and the file's, by canonical key, read on the first owed tag.
	var catalogTags, fileTags map[string][]string
	for _, f := range owed {
		switch {
		case f == "art":
			paidAll, paidHere, err := owedCoverPaidTx(ctx, tx, itemID, s.cover)
			if err != nil {
				return err
			}
			if paidAll {
				everywhere = append(everywhere, f)
			} else if paidHere {
				here = append(here, f)
			}
		case f == "acquisition":
			if s.acquisitionRecorded {
				everywhere = append(everywhere, f)
			}
		case f == "album.mbid" || f == "release_group.mbid":
			album, group, err := itemReleaseIDsTx(ctx, tx, itemID)
			if err != nil {
				return err
			}
			if (f == "album.mbid" && strings.EqualFold(album, s.fileTrack.MBReleaseID)) ||
				(f == "release_group.mbid" && strings.EqualFold(group, s.fileTrack.MBReleaseGroupID)) {
				here = append(here, f)
			}
		case strings.HasPrefix(f, "album.") || strings.HasPrefix(f, "artist.") || strings.HasPrefix(f, "release_group."):
			// An entity's value ("album.label", "album.art"): a scan never re-derives it.
		case strings.HasPrefix(f, "tag."):
			key, _ := model.CutTagPrefix(f)
			if slices.Contains(s.tagsReplaced, key) {
				everywhere = append(everywhere, f)
				continue
			}
			if catalogTags == nil {
				if catalogTags, err = loadItemTagsTx(ctx, tx, itemID); err != nil {
					return err
				}
				fileTags = canonicalTags(s.fileTags)
			}
			if slices.Equal(fileTags[key], catalogTags[key]) {
				here = append(here, f)
			}
		case f == "title":
			held, err := lockedFallback(f)
			if err != nil {
				return err
			}
			if s.fileTitle == s.title && !held {
				here = append(here, f)
			}
		case s.isBook:
			col := bookOwedColumn(f)
			if col == "" {
				continue
			}
			// The file is held against the book the catalog holds, since a part that does
			// not own the book's metadata never wrote it, and a fill the put kept over a
			// silent file was not re-derived from it.
			if stored == nil {
				b, _, err := loadBookForEditTx(ctx, tx, itemID)
				if err != nil {
					return err
				}
				stored = &b
			}
			if !bookFileAgrees(col, s.fileBook, *stored) {
				continue
			}
			if s.bookRederived {
				l, err := lockedFields()
				if err != nil {
					return err
				}
				if !l[f] {
					everywhere = append(everywhere, f)
					continue
				}
			}
			here = append(here, f)
		default:
			col := f
			switch f {
			case model.CreditField(model.RoleArtist):
				col = "artist"
			case model.CreditField(model.RoleComposer):
				col = "composer"
			}
			if !slices.Contains(scanFields, col) || scanFieldValue(col, s.fileTitle, s.fileTrack) != scanFieldValue(col, s.title, s.track) {
				continue
			}
			held, err := lockedFallback(col)
			if err != nil {
				return err
			}
			if !held {
				here = append(here, f)
			}
		}
	}
	if err := deleteOwedTx(ctx, tx, "file_id = ?", fileID, here); err != nil {
		return err
	}
	return deleteOwedTx(ctx, tx, "file_id IN (SELECT file_id FROM item_file WHERE item_id = ?)", itemID, everywhere)
}

// canonicalTags returns a file's custom tags under their canonical keys, cleaned as the
// catalog stores them.
func canonicalTags(tags map[string][]string) map[string][]string {
	out := make(map[string][]string, len(tags))
	for k, vs := range tags {
		if canon, ok := model.CanonicalTagKey(k); ok {
			if clean := model.CleanTagValues(vs); len(clean) > 0 {
				out[canon] = clean
			}
		}
	}
	return out
}

// bookOwedColumn maps an owed key on a book to the bookScanFields value it describes, the
// author and narrator credits onto the displays they write, or "" for none.
func bookOwedColumn(f string) string {
	switch f {
	case model.CreditField(model.RoleAuthor):
		return "author"
	case model.CreditField(model.RoleNarrator):
		return "narrator"
	}
	if slices.Contains(bookScanFields, f) {
		return f
	}
	return ""
}

// bookFileAgrees reports whether a part's file states the value the catalog holds for col.
// An entity-backed name compares folded, since the entity keeps its own spelling.
func bookFileAgrees(col string, file, stored model.Book) bool {
	switch col {
	case "series":
		return identity.MatchKey(file.Series) == identity.MatchKey(stored.Series)
	case "credit.translator":
		return foldedNamesEqual(file.Translators, stored.Translators)
	case "credit.editor":
		return foldedNamesEqual(file.Editors, stored.Editors)
	}
	return bookFieldValue(col, file, false) == bookFieldValue(col, stored, true)
}

// owedCoverPaidTx reports whether a put paid an owed cover: on every file once the item's
// front came from disk (a tag or a sidecar, so the curated cover is gone), or on this
// one when its own embedded picture is the front the catalog holds. A cleared front and
// a file with no embedded picture agree too.
func owedCoverPaidTx(ctx context.Context, tx *sql.Tx, itemID int64, scanned *model.ArtImage) (all, here bool, err error) {
	var hash, source string
	err = tx.QueryRowContext(ctx, `SELECT source_hash, source FROM art_map
		WHERE entity_type = 'track' AND entity_id = ? AND role = 'front'`, itemID).Scan(&hash, &source)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, false, err
	}
	if source == string(model.SourceTag) || source == string(model.SourceSidecar) {
		return true, false, nil
	}
	embedded := ""
	if scanned != nil && scanned.Source == model.SourceTag {
		embedded = storableArt(scanned).Hash
	}
	return false, embedded == hash, nil
}

// itemReleaseIDsTx returns the MusicBrainz ids of the album and release group a track
// sits under, empty where there is none.
func itemReleaseIDsTx(ctx context.Context, tx *sql.Tx, itemID int64) (album, group string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(al.mbid, ''), COALESCE(rg.mbid, '') FROM track t
		LEFT JOIN album al ON al.id = t.album_id
		LEFT JOIN release_group rg ON rg.id = al.release_group_id
		WHERE t.item_id = ?`, itemID).Scan(&album, &group)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return album, group, err
}
