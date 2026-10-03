package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// alternateRole is the item_file role of a file attached to an item without owning its
// metadata: the same audio as the item's primary, or another encoding of its recording.
const (
	primaryRole   = "primary"
	alternateRole = "alternate"
)

// standingPrimary is an item's primary file, with the columns the copy decision reads.
type standingPrimary struct {
	fileID   int64
	path     []byte
	display  string
	essence  string
	quality  model.File
	readOnly bool
}

// primaryFileTx returns an item's primary file, or nil when it has none.
func primaryFileTx(ctx context.Context, tx *sql.Tx, itemID int64) (*standingPrimary, error) {
	var sp standingPrimary
	var essence, codec sql.NullString
	var bitrate, rate, depth sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT f.id, f.path, f.display_path, f.essence_hash, f.codec,
			f.bitrate, f.sample_rate, f.bit_depth, l.read_only
		FROM item_file itf JOIN file f ON f.id = itf.file_id JOIN library l ON l.id = f.library_id
		WHERE itf.item_id = ? AND itf.role = 'primary'`, itemID).
		Scan(&sp.fileID, &sp.path, &sp.display, &essence, &codec, &bitrate, &rate, &depth, &sp.readOnly)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sp.essence = essence.String
	sp.quality = model.File{Codec: codec.String, Bitrate: int(bitrate.Int64),
		SampleRate: int(rate.Int64), BitDepth: int(depth.Int64)}
	return &sp, nil
}

// arrivalTakesOverTx reports whether an arriving file takes an item over from its primary.
// A primary missing from disk is replaced only once reconciliation has marked the item
// missing: until then it may be a file moved to a folder the walk has not reached. One on
// disk yields only to another encoding that outranks it (outranksPrimaryTx).
func arrivalTakesOverTx(ctx context.Context, tx *sql.Tx, itemID, libraryID int64, f model.File, primary *standingPrimary) (bool, error) {
	if !pathExists(primary.path) {
		return itemMissingTx(ctx, tx, itemID)
	}
	return outranksPrimaryTx(ctx, tx, libraryID, f, primary)
}

// outranksPrimaryTx reports whether an arriving file outranks an item's primary: only
// another encoding (different audio), ranking ahead of it by a writable library before a
// read-only one and then quality (model.CompareQuality), the order promotion uses. A tie
// keeps the primary.
func outranksPrimaryTx(ctx context.Context, tx *sql.Tx, libraryID int64, f model.File, primary *standingPrimary) (bool, error) {
	if f.EssenceHash == primary.essence {
		return false, nil
	}
	var readOnly bool
	if err := tx.QueryRowContext(ctx, "SELECT read_only FROM library WHERE id = ?", libraryID).Scan(&readOnly); err != nil {
		return false, err
	}
	if readOnly != primary.readOnly {
		return !readOnly, nil
	}
	return model.CompareQuality(f, primary.quality) < 0, nil
}

// outrankingAlternateTx returns the item's best alternate on disk holding another encoding
// that outranks f, a file now read as the item's primary or a part, or nil; also, when set,
// narrows the alternates (a book's to the encodings of f's part). Such an alternate was
// read while the primary was away (moved to a folder the walk had not reached), so it
// waited as an alternate where the other walk order would have made it the primary.
func outrankingAlternateTx(ctx context.Context, tx *sql.Tx, itemID, libraryID int64, f model.File, also func(altCandidate) bool) (*altCandidate, error) {
	var readOnly bool
	if err := tx.QueryRowContext(ctx, "SELECT read_only FROM library WHERE id = ?", libraryID).Scan(&readOnly); err != nil {
		return nil, err
	}
	return bestAlternateTx(ctx, tx, itemID, func(c altCandidate) bool {
		if c.start.Valid || c.essence == f.EssenceHash || (also != nil && !also(c)) {
			return false
		}
		if c.readOnly != readOnly {
			return !c.readOnly
		}
		return model.CompareQuality(c.quality, f) < 0
	}, onDisk)
}

// itemMissingTx reports whether an item is in the missing state.
func itemMissingTx(ctx context.Context, tx *sql.Tx, itemID int64) (bool, error) {
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT state FROM playable_item WHERE id = ?", itemID).Scan(&state); err != nil {
		return false, err
	}
	return state == string(model.StateMissing), nil
}

// reviveItemTx returns a missing item to present, for a file of it found on disk, and
// reports whether it did. The caller emits the item update.
func reviveItemTx(ctx context.Context, tx *sql.Tx, itemID, now int64) (bool, error) {
	r, err := tx.ExecContext(ctx, "UPDATE playable_item SET state = ?, updated_at = ? WHERE id = ? AND state = ?",
		string(model.StatePresent), now, itemID, string(model.StateMissing))
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// markUnreachableMissingTx marks a present item missing when none of its files is on
// disk, files in gone counting as gone without a stat, and reports whether it did, having
// emitted the item update. An item whose primary could only be replaced by a file under an
// absent root, or by a gone one, is playable from nothing until that file is read again.
func markUnreachableMissingTx(ctx context.Context, tx *sql.Tx, itemID int64, gone map[int64]bool) (bool, error) {
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT state FROM playable_item WHERE id = ?", itemID).Scan(&state); err != nil {
		return false, err
	}
	if state != string(model.StatePresent) {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT f.id, f.path FROM item_file itf JOIN file f ON f.id = itf.file_id
		WHERE itf.item_id = ?`, itemID)
	if err != nil {
		return false, err
	}
	type fileAt struct {
		id   int64
		path []byte
	}
	var files []fileAt
	for rows.Next() {
		var f fileAt
		if err := rows.Scan(&f.id, &f.path); err != nil {
			rows.Close()
			return false, err
		}
		files = append(files, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(files) == 0 {
		return false, err
	}
	for _, f := range files {
		if !gone[f.id] && pathExists(f.path) {
			return false, nil
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE playable_item SET state = ?, updated_at = ? WHERE id = ?",
		string(model.StateMissing), nowNS(), itemID); err != nil {
		return false, err
	}
	return true, appendItemUpdateTx(ctx, tx, itemID)
}

// reach says where a file stands for promotion: on disk, under a library root that is
// absent (an unplugged drive that may come back), or gone from a root that is present.
type reach int

const (
	onDisk reach = iota
	rootAbsent
	goneFromRoot
)

// reachOf classifies a file, statting each library root once per roots map: a promotion
// looks at one item's alternates, which mostly share a root or two.
func reachOf(path, root []byte, roots map[string]bool) reach {
	if pathExists(path) {
		return onDisk
	}
	up, seen := roots[string(root)]
	if !seen {
		info, err := os.Stat(pathx.Long(string(root)))
		up = err == nil && info.IsDir()
		roots[string(root)] = up
	}
	if !up {
		return rootAbsent
	}
	return goneFromRoot
}

// fileByEssenceGoneTx returns the row a moved file relinks to: a file in the library with
// the same essence whose path is gone from disk, one holding the same bytes first, then the
// lowest id. A row still on disk under its own spelling is a copy, never a relink target;
// one whose path resolves but whose spelling the folders no longer list (fsx.Lister) is the
// file renamed between two spellings of its name outside WaxBin, which NTFS and APFS
// resolve either way, and it relinks like any move. A row must back nothing
// or one of the items in accept, the items the arriving file would join (a rip's row
// backs a track per window, and a moved rip whose sheet changed one window still takes
// its own row): relinking another item's row would hand the file over to it, or detach and
// delete that item, so the arriving file gets a row of its own and reconciliation settles
// the gone one. A file whose key no item holds yet (retagged and moved in one pass) takes
// the only gone row of its audio, the row a retag in place and then a move would relink.
func fileByEssenceGoneTx(ctx context.Context, tx *sql.Tx, essence, content string, libraryID int64, accept map[int64]bool) (*model.File, error) {
	rows, err := tx.QueryContext(ctx, fileSelect+` WHERE essence_hash = ? AND library_id = ?
		ORDER BY content_hash = ? DESC, id`, essence, libraryID, content)
	if err != nil {
		return nil, err
	}
	var matches []*model.File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		matches = append(matches, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}
	root, err := libraryRootTx(ctx, tx, libraryID)
	if err != nil {
		return nil, err
	}
	lister := fsx.NewLister()
	var gone []*model.File
	for _, f := range matches {
		if pathExists(f.Path) && lister.Spelled(root, string(f.Path)) {
			continue
		}
		gone = append(gone, f)
		items, err := queryInt64sTx(ctx, tx, "SELECT DISTINCT item_id FROM item_file WHERE file_id = ?", f.ID)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 || slices.ContainsFunc(items, func(id int64) bool { return accept[id] }) {
			return f, nil
		}
	}
	if len(accept) == 0 && len(gone) == 1 {
		return gone[0], nil
	}
	return nil, nil
}

// libraryRootTx reads a library's root inside a transaction.
func libraryRootTx(ctx context.Context, tx *sql.Tx, libraryID int64) (string, error) {
	var root []byte
	if err := tx.QueryRowContext(ctx, "SELECT root FROM library WHERE id = ?", libraryID).Scan(&root); err != nil {
		return "", err
	}
	return string(root), nil
}

// resolvedItem is an item key resolution made earlier in the same transaction, so the
// steps after it neither repeat the lookup nor log a book's ambiguous identifier again:
// the item's id, 0 when the key named no item.
type resolvedItem struct{ id int64 }

// resolveItemTx resolves an item key once for a put (existingItemIDByIdentityTx).
func resolveItemTx(ctx context.Context, tx *sql.Tx, log logger, kind model.Kind, key string, adopt bookAdoptKey) (*resolvedItem, error) {
	id, _, err := existingItemIDByIdentityTx(ctx, tx, log, kind, key, adopt)
	if err != nil {
		return nil, err
	}
	return &resolvedItem{id: id}, nil
}

// accept is the accept set for a relink (fileByEssenceGoneTx): the resolved item, if any.
func (r *resolvedItem) accept() map[int64]bool {
	if r == nil || r.id == 0 {
		return nil
	}
	return map[int64]bool{r.id: true}
}

// linkAlternateFile attaches fileID to itemID as an alternate, detaching it from every
// other item the way linkPrimaryFile does. It reports whether the edge changed and the
// items the detach left behind. An existing alternate edge keeps its position.
func linkAlternateFile(ctx context.Context, tx *sql.Tx, itemID, fileID int64) (bool, []int64, error) {
	var others, held int
	if err := tx.QueryRowContext(ctx, `SELECT
			COUNT(*) FILTER (WHERE NOT (item_id = ? AND role = 'alternate')),
			COUNT(*) FILTER (WHERE item_id = ? AND role = 'alternate')
		FROM item_file WHERE file_id = ?`, itemID, itemID, fileID).Scan(&others, &held); err != nil {
		return false, nil, err
	}
	if others == 0 && held == 1 {
		return false, nil, nil
	}
	prev, err := queryInt64sTx(ctx, tx,
		"SELECT DISTINCT item_id FROM item_file WHERE file_id = ? AND item_id <> ?", fileID, itemID)
	if err != nil {
		return false, nil, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_file WHERE file_id = ?", fileID); err != nil {
		return false, nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO item_file(item_id, file_id, role, position)
		SELECT ?, ?, 'alternate', COALESCE(MAX(position), 0) + 1 FROM item_file WHERE item_id = ?`,
		itemID, fileID, itemID); err != nil {
		return false, nil, err
	}
	return true, prev, nil
}

// demoteToAlternateTx turns an item's primary edge on fileID into an alternate, for a
// better encoding taking the item over.
func demoteToAlternateTx(ctx context.Context, tx *sql.Tx, itemID, fileID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE item_file SET role = 'alternate',
			position = (SELECT COALESCE(MAX(position), 0) + 1 FROM item_file WHERE item_id = ?)
		WHERE item_id = ? AND file_id = ? AND role = 'primary'`, itemID, itemID, fileID)
	return err
}

// copyDiagnostic is the scan diagnostic an alternate carries: duplicate_copy for the
// primary's audio, alternate_encoding for another encoding, naming the primary's path.
func copyDiagnostic(essence string, primary *standingPrimary) model.FileDiagnostic {
	code := model.DiagAlternateEncoding
	if essence == primary.essence {
		code = model.DiagDuplicateCopy
	}
	return model.FileDiagnostic{Code: code, Severity: model.SeverityInfo, Detail: primary.display}
}

// refreshCopyDiagnosticsTx rewrites the copy diagnostic of every whole-file alternate of
// an item after its primary or parts changed, and drops any from the parts themselves. A
// copy of a part names that part (for a book, not necessarily the primary); another
// encoding names the book's part at its position, or the primary.
func refreshCopyDiagnosticsTx(ctx context.Context, tx *sql.Tx, itemID int64) error {
	primary, err := primaryFileTx(ctx, tx, itemID)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.id, itf.role, itf.position, f.display_path, COALESCE(f.essence_hash, ''),
			`+lostFileCols+`, pi.kind = 'book'
		FROM item_file itf JOIN file f ON f.id = itf.file_id JOIN playable_item pi ON pi.id = itf.item_id
		WHERE itf.item_id = ? AND itf.start_frames IS NULL
		ORDER BY itf.role = 'alternate', itf.position, f.id`, itemID)
	if err != nil {
		return err
	}
	type edge struct {
		fileID   int64
		role     string
		position int
		display  string
		lost     lostEdge
	}
	var edges, parts []edge
	book := false
	for rows.Next() {
		var e edge
		if err := rows.Scan(append([]any{&e.fileID, &e.role, &e.position, &e.display, &e.lost.file.EssenceHash},
			append(e.lost.fileFields(), &book)...)...); err != nil {
			rows.Close()
			return err
		}
		if e.role != alternateRole {
			parts = append(parts, e)
		}
		edges = append(edges, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := nowNS()
	for _, e := range edges {
		if err := dropCopyDiagnosticsTx(ctx, tx, e.fileID); err != nil {
			return err
		}
		if e.role != alternateRole || primary == nil {
			continue
		}
		d := copyDiagnostic(e.lost.file.EssenceHash, primary)
		for _, p := range parts {
			if e.lost.file.EssenceHash != "" && p.lost.file.EssenceHash == e.lost.file.EssenceHash {
				d.Code, d.Detail = model.DiagDuplicateCopy, p.display
				break
			}
			if book && p.position == e.position && model.OtherEncoding(p.lost.file, e.lost.file) {
				d.Detail = p.display
			}
		}
		if err := upsertFileDiagnosticTx(ctx, tx, e.fileID, model.OriginScan, "", d, now); err != nil {
			return err
		}
	}
	return nil
}

// renameCopyDetailsTx re-points the copy diagnostics that name a moved file, on the
// alternates of the items it holds a primary or part edge on, from its old display path
// to its new one.
func renameCopyDetailsTx(ctx context.Context, tx *sql.Tx, fileID int64, from, to string) error {
	if from == to {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE file_diagnostic SET detail = ?
		WHERE origin = ? AND code IN (?, ?) AND detail = ? AND file_id IN (
			SELECT a.file_id FROM item_file p JOIN item_file a ON a.item_id = p.item_id AND a.role = 'alternate'
			WHERE p.file_id = ? AND p.role IN ('primary', 'part'))`,
		model.CapDetail(to), string(model.OriginScan), string(model.DiagDuplicateCopy), string(model.DiagAlternateEncoding),
		model.CapDetail(from), fileID)
	return err
}

func dropCopyDiagnosticsTx(ctx context.Context, tx *sql.Tx, fileID int64) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM file_diagnostic WHERE file_id = ? AND origin = ? AND code IN (?, ?)",
		fileID, string(model.OriginScan), string(model.DiagDuplicateCopy), string(model.DiagAlternateEncoding))
	return err
}

// attachCopyTx is PutScannedTrack's copy branch: the file joins an item whose primary it
// does not take over, as an alternate. None of the item's writers run, since the
// primary's tags own the item; the file keeps its own row, sidecar observations and
// diagnostics, and the owed rows its tags already pay are settled against the catalog.
// A missing item the file belongs to is present again, its primary being on disk. The
// item emits one update when the edge is new or it came back.
func (s *Store) attachCopyTx(ctx context.Context, tx *sql.Tx, in model.PutScannedTrackInput, fileID int64, filePID model.PID,
	itemID int64, primary *standingPrimary, fileTitle string, fileTrack model.Track, res *model.ScanItemResult, now int64) error {
	const op = "store.PutScannedTrack"
	var itemPID model.PID
	if err := tx.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE id = ?", itemID).Scan(&itemPID); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	res.ItemPID, res.AttachedAsCopy = itemPID, true

	dep, err := departingTx(ctx, tx, fileID, in.File.EssenceHash, itemID, in.PreserveLocks)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	changed, orphans, err := linkAlternateFile(ctx, tx, itemID, fileID)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	res.Joined = changed
	revived, err := reviveItemTx(ctx, tx, itemID, now)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	affected := newAffectedRollups()
	promoted, folded, err := reconcileOrphansTx(ctx, tx, orphans, dep, affected)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	res.Promoted = append(res.Promoted, promoted...)
	res.Folded = append(res.Folded, folded...)
	if changed {
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if !affected.empty() {
		if err := maintainRollupsTx(ctx, tx, affected, now); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if err := upsertFileDiagnosticTx(ctx, tx, fileID, model.OriginScan, "", copyDiagnostic(in.File.EssenceHash, primary), now); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	cur, curTitle, _, err := loadTrackForEditTx(ctx, tx, itemID)
	if err != nil {
		return err
	}
	if err := settleOwedByScanTx(ctx, tx, fileID, itemID, scanSettle{
		fileTitle: fileTitle, title: curTitle, fileTrack: fileTrack, track: cur,
		preserveLocks: in.PreserveLocks, derived: in.Derived, cover: in.CoverArt, fileTags: in.CustomTags,
	}); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	if res.FileCreated || res.ContentChanged || res.Relinked {
		if err := appendChange(ctx, tx, "file", filePID, opFor(res.FileCreated)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if changed || revived {
		if err := appendChange(ctx, tx, "item", itemPID, model.OpUpdate); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return nil
}

// reconcileOrphansTx settles the items a link detached a file from. One that still has a
// file puts an alternate in the place the file left (promoteLostTx: a copy of a book's
// part fills its gap) or otherwise keeps a primary, its rollups and book total are
// recomputed, and it emits an update, marked missing when none of its files is on disk;
// one left with none folds into the item the file joined (foldItemIntoTx) and is deleted.
// It returns the files promoted and the pids of the items folded.
func reconcileOrphansTx(ctx context.Context, tx *sql.Tx, orphans []int64, dep departure, affected *affectedRollups) ([]model.PromotedFile, []model.PID, error) {
	var promoted []model.PromotedFile
	var folded []model.PID
	for _, oid := range orphans {
		has, err := itemHasAnyFile(ctx, tx, oid)
		if err != nil {
			return nil, nil, err
		}
		if err := affected.collect(ctx, tx, oid); err != nil {
			return nil, nil, err
		}
		if has {
			p, err := promoteLostTx(ctx, tx, oid, dep.books[oid], dep.lost[oid])
			filled := p != nil
			if err == nil && p == nil {
				p, err = ensurePrimary(ctx, tx, oid)
			}
			if err != nil {
				return nil, nil, err
			}
			if p != nil {
				promoted = append(promoted, *p)
			}
			// A part that left a book takes its places on the timeline along, unless a copy
			// or another encoding of it took its place.
			if span, ok := dep.spans[oid]; ok && !filled {
				if err := moveDepartedPositionsTx(ctx, tx, oid, dep.into, dep.file, span); err != nil {
					return nil, nil, err
				}
			}
			if err := refreshBookDuration(ctx, tx, oid); err != nil {
				return nil, nil, err
			}
			marked, err := markUnreachableMissingTx(ctx, tx, oid, nil)
			if err != nil {
				return nil, nil, err
			}
			if !marked {
				if err := appendItemUpdateTx(ctx, tx, oid); err != nil {
					return nil, nil, err
				}
			}
			continue
		}
		fold := dep.into != 0 && !dep.lost[oid].start.Valid
		if fold {
			if err := foldItemIntoTx(ctx, tx, oid, dep.into, dep.file, dep.preserveLocks); err != nil {
				return nil, nil, err
			}
		}
		opid, err := deleteItemCascade(ctx, tx, oid)
		if err != nil {
			return nil, nil, err
		}
		if fold {
			folded = append(folded, opid)
		}
		if err := appendChange(ctx, tx, "item", opid, model.OpDelete); err != nil {
			return nil, nil, err
		}
	}
	return promoted, folded, nil
}

// appendItemUpdateTx appends an item update delta by item id.
func appendItemUpdateTx(ctx context.Context, tx *sql.Tx, itemID int64) error {
	var pid model.PID
	if err := tx.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE id = ?", itemID).Scan(&pid); err != nil {
		return err
	}
	return appendChange(ctx, tx, "item", pid, model.OpUpdate)
}

// lostEdge is the edge a file held on an item when it left the item: its role,
// position and window, and the file as model.OtherEncoding reads it.
type lostEdge struct {
	role       string
	position   int
	start, end sql.NullInt64
	file       model.File
}

// lostFileCols reads what lostEdge.file holds from the file alias f.
const lostFileCols = `COALESCE(f.codec, ''), COALESCE(f.sample_rate, 0), COALESCE(f.bit_depth, 0), COALESCE(f.duration_ms, 0)`

func (e *lostEdge) fileFields() []any {
	return []any{&e.file.Codec, &e.file.SampleRate, &e.file.BitDepth, &e.file.DurationMS}
}

// altCandidate is an alternate edge an item can promote.
type altCandidate struct {
	fileID, libraryID int64
	pid               model.PID
	path              []byte
	display           string
	reach             reach
	readOnly          bool
	essence           string
	quality           model.File
	position          int
	start, end        sql.NullInt64
}

// file is the candidate's file as model.OtherEncoding reads it.
func (c altCandidate) file() model.File {
	f := c.quality
	f.EssenceHash = c.essence
	return f
}

// bestAlternateTx returns the alternate an item promotes, among those that keep and reach
// no further than worst: a file on disk first, then one under an absent root, then a gone
// one, and within each a file in a writable library first, then the better encoding,
// then position and file id. It returns nil when none qualifies.
func bestAlternateTx(ctx context.Context, tx *sql.Tx, itemID int64, keep func(altCandidate) bool, worst reach) (*altCandidate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT f.id, f.pid, f.path, f.display_path, f.library_id, l.root, l.read_only,
			COALESCE(f.essence_hash, ''), COALESCE(f.codec, ''), COALESCE(f.bitrate, 0), COALESCE(f.sample_rate, 0),
			COALESCE(f.bit_depth, 0), COALESCE(f.duration_ms, 0), itf.position, itf.start_frames, itf.end_frames
		FROM item_file itf JOIN file f ON f.id = itf.file_id JOIN library l ON l.id = f.library_id
		WHERE itf.item_id = ? AND itf.role = 'alternate'`, itemID)
	if err != nil {
		return nil, err
	}
	var cands []altCandidate
	roots := map[string]bool{}
	for rows.Next() {
		var c altCandidate
		var root []byte
		if err := rows.Scan(&c.fileID, &c.pid, &c.path, &c.display, &c.libraryID, &root, &c.readOnly, &c.essence,
			&c.quality.Codec, &c.quality.Bitrate, &c.quality.SampleRate, &c.quality.BitDepth, &c.quality.DurationMS,
			&c.position, &c.start, &c.end); err != nil {
			rows.Close()
			return nil, err
		}
		if keep != nil && !keep(c) {
			continue
		}
		if c.reach = reachOf(c.path, root, roots); c.reach <= worst {
			cands = append(cands, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(cands) == 0 {
		return nil, err
	}
	slices.SortStableFunc(cands, func(a, b altCandidate) int {
		if c := cmp.Compare(a.reach, b.reach); c != 0 {
			return c
		}
		if a.readOnly != b.readOnly {
			if a.readOnly {
				return 1
			}
			return -1
		}
		if c := model.CompareQuality(a.quality, b.quality); c != 0 {
			return c
		}
		if c := cmp.Compare(a.position, b.position); c != 0 {
			return c
		}
		return cmp.Compare(a.fileID, b.fileID)
	})
	return &cands[0], nil
}

// promoteTx gives an alternate the role and position of the edge it replaces and
// re-diagnoses the item's copies against it: a whole-file item through
// refreshCopyDiagnosticsTx, a rip track file by file, since a rip file can be one track's
// primary and another's alternate.
func promoteTx(ctx context.Context, tx *sql.Tx, itemID int64, c *altCandidate, role string, position int) (*model.PromotedFile, error) {
	if _, err := tx.ExecContext(ctx, "UPDATE item_file SET role = ?, position = ? WHERE item_id = ? AND file_id = ? AND role = 'alternate'",
		role, position, itemID, c.fileID); err != nil {
		return nil, err
	}
	if err := unstampTx(ctx, tx, c.fileID); err != nil {
		return nil, err
	}
	if c.start.Valid {
		files, err := queryInt64sTx(ctx, tx, "SELECT DISTINCT file_id FROM item_file WHERE item_id = ?", itemID)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if err := refreshRipCopyDiagnosticTx(ctx, tx, f); err != nil {
				return nil, err
			}
		}
	} else if err := refreshCopyDiagnosticsTx(ctx, tx, itemID); err != nil {
		return nil, err
	}
	return &model.PromotedFile{FilePID: c.pid, LibraryID: c.libraryID, Path: c.path}, nil
}

// refreshRipCopyDiagnosticTx sets a rip file's copy diagnostic from its edges: while it
// backs a track as an alternate it names that track's primary, and otherwise it has none.
func refreshRipCopyDiagnosticTx(ctx context.Context, tx *sql.Tx, fileID int64) error {
	if err := dropCopyDiagnosticsTx(ctx, tx, fileID); err != nil {
		return err
	}
	var essence string
	var itemID int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(f.essence_hash, ''), itf.item_id
		FROM item_file itf JOIN file f ON f.id = itf.file_id
		WHERE itf.file_id = ? AND itf.role = 'alternate' ORDER BY itf.item_id LIMIT 1`, fileID).Scan(&essence, &itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	primary, err := primaryFileTx(ctx, tx, itemID)
	if err != nil || primary == nil {
		return err
	}
	return upsertFileDiagnosticTx(ctx, tx, fileID, model.OriginScan, "", copyDiagnostic(essence, primary), nowNS())
}

// promoteLostTx gives an item that lost a primary or part edge an alternate in its place.
// A track's primary goes to its best alternate of any encoding; a rip track's goes only
// to an alternate over the same window, and a book's part only to a copy of the lost
// part, since another part's copy cannot stand in for it. Only a file on disk takes a
// place here: one under an absent root or gone would leave the item unplayable while
// it read as present (ensurePrimary is the backstop that may promote one). It returns
// nil when nothing qualifies.
func promoteLostTx(ctx context.Context, tx *sql.Tx, itemID int64, book bool, lost lostEdge) (*model.PromotedFile, error) {
	var keep func(altCandidate) bool
	switch {
	case lost.role == alternateRole:
		return nil, nil
	case lost.start.Valid:
		keep = func(c altCandidate) bool { return c.start == lost.start && c.end == lost.end }
	case book:
		// A copy of the part, or another encoding of it at its position (model.OtherEncoding).
		keep = func(c altCandidate) bool {
			return (lost.file.EssenceHash != "" && c.essence == lost.file.EssenceHash) ||
				(c.position == lost.position && model.OtherEncoding(lost.file, c.file()))
		}
	case lost.role != primaryRole:
		return nil, nil
	}
	c, err := bestAlternateTx(ctx, tx, itemID, keep, onDisk)
	if err != nil || c == nil {
		return nil, err
	}
	return promoteTx(ctx, tx, itemID, c, lost.role, lost.position)
}

// ensurePrimary makes sure an item that still has files keeps exactly one 'primary'
// edge, promoting one when it has none, and returns the file it promoted (nil when it
// promoted nothing). A book promotes its lowest-positioned part; any other item, or a
// book left with only alternates, promotes its best alternate (bestAlternateTx), one
// under an absent root or gone when nothing better is left, the caller then marking the
// item missing (markUnreachableMissingTx). This
// matters for a multi-file book whose primary part was re-keyed into another item, and
// for a track whose primary was re-encoded away from its copies.
func ensurePrimary(ctx context.Context, tx *sql.Tx, itemID int64) (*model.PromotedFile, error) {
	var hasPrimary int
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM item_file WHERE item_id = ? AND role = 'primary')", itemID).Scan(&hasPrimary); err != nil {
		return nil, err
	}
	if hasPrimary == 1 {
		return nil, nil
	}
	var p model.PromotedFile
	var fileID int64
	err := tx.QueryRowContext(ctx, `SELECT f.id, f.pid, f.library_id, f.path FROM item_file itf JOIN file f ON f.id = itf.file_id
		WHERE itf.item_id = ? AND itf.role = 'part' ORDER BY itf.position, f.id LIMIT 1`, itemID).
		Scan(&fileID, &p.FilePID, &p.LibraryID, &p.Path)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx,
			"UPDATE item_file SET role = 'primary' WHERE item_id = ? AND file_id = ? AND role = 'part'", itemID, fileID); err != nil {
			return nil, err
		}
		if err := unstampTx(ctx, tx, fileID); err != nil {
			return nil, err
		}
		return &p, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	c, err := bestAlternateTx(ctx, tx, itemID, nil, goneFromRoot)
	if err != nil || c == nil {
		return nil, err
	}
	return promoteTx(ctx, tx, itemID, c, primaryRole, 0)
}

// unstampTx clears a promoted file's mtime stamp (no file has an mtime of -1 ns), so the
// next scan reads it in full rather than fast-pathing it. The caller's re-read of the
// file can be cut short by a cancel or a crash, and its item still owes it that read.
func unstampTx(ctx context.Context, tx *sql.Tx, fileID int64) error {
	_, err := tx.ExecContext(ctx, "UPDATE file SET mtime_ns = -1 WHERE id = ?", fileID)
	return err
}

// itemLostEdgesTx returns the edges a file holds, by item, with the item's kind, read
// before the file leaves.
func itemLostEdgesTx(ctx context.Context, tx *sql.Tx, fileID int64, essence string) (map[int64]lostEdge, map[int64]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT itf.item_id, itf.role, itf.position, itf.start_frames, itf.end_frames, pi.kind,
			`+lostFileCols+`
		FROM item_file itf JOIN playable_item pi ON pi.id = itf.item_id JOIN file f ON f.id = itf.file_id
		WHERE itf.file_id = ?`, fileID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	edges, books := map[int64]lostEdge{}, map[int64]bool{}
	for rows.Next() {
		var id int64
		var kind string
		e := lostEdge{file: model.File{EssenceHash: essence}}
		if err := rows.Scan(append([]any{&id, &e.role, &e.position, &e.start, &e.end, &kind}, e.fileFields()...)...); err != nil {
			return nil, nil, err
		}
		// A primary or part edge wins over a stray alternate edge of the same pair.
		if prev, ok := edges[id]; !ok || prev.role == alternateRole {
			edges[id] = e
		}
		books[id] = kind == string(model.KindBook)
	}
	return edges, books, rows.Err()
}

// FileOwner returns a file's edge as an ItemFileRef and the item it backs, preferring the
// item it is the primary of (the rule the trash journal records), for deleting the file
// on its own. A file backing no item returns an empty item pid; an unknown pid is
// CodeNotFound.
func (s *Store) FileOwner(ctx context.Context, filePID model.PID) (model.PID, model.ItemFileRef, error) {
	const op = "store.FileOwner"
	var item model.PID
	ref := model.ItemFileRef{FilePID: filePID}
	err := s.read.QueryRowContext(ctx, `SELECT f.path, f.display_path, COALESCE(pi.pid, ''), COALESCE(itf.role, ''),
			COALESCE(itf.position, 0)
		FROM file f LEFT JOIN item_file itf ON itf.file_id = f.id LEFT JOIN playable_item pi ON pi.id = itf.item_id
		WHERE f.pid = ? ORDER BY itf.role = 'primary' DESC, itf.item_id LIMIT 1`, string(filePID)).
		Scan(&ref.Path, &ref.DisplayPath, &item, &ref.Role, &ref.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ref, waxerr.New(waxerr.CodeNotFound, op, "no such file: "+string(filePID))
	}
	if err != nil {
		return "", ref, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return item, ref, nil
}

// ItemsWithCopies returns every item holding an alternate file, in item order, with all
// its files: parts first in position order, then alternates. An alternate holding the
// audio of one of the item's parts is the same audio, and otherwise another encoding.
func (s *Store) ItemsWithCopies(ctx context.Context) ([]model.ItemCopies, error) {
	const op = "store.ItemsWithCopies"
	rows, err := s.read.QueryContext(ctx, `SELECT pi.id, pi.pid, pi.kind, pi.title,
			COALESCE(NULLIF(t.artist, ''), bk.author, ''),
			f.pid, l.pid, f.display_path, f.size, itf.role, COALESCE(f.essence_hash, ''),
			COALESCE(f.codec, ''), COALESCE(f.bitrate, 0), COALESCE(f.sample_rate, 0), COALESCE(f.bit_depth, 0)
		FROM playable_item pi
		JOIN item_file itf ON itf.item_id = pi.id
		JOIN file f ON f.id = itf.file_id
		JOIN library l ON l.id = f.library_id
		LEFT JOIN track t ON t.item_id = pi.id
		LEFT JOIN book bk ON bk.item_id = pi.id
		WHERE pi.id IN (SELECT item_id FROM item_file WHERE role = 'alternate')
		ORDER BY pi.id, itf.role = 'alternate', itf.position, f.id`)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.ItemCopies
	var lastID int64
	var partAudio map[string]bool
	for rows.Next() {
		var id int64
		var it model.ItemCopies
		var f model.CopyFile
		var essence string
		if err := rows.Scan(&id, &it.ItemPID, &it.Kind, &it.Title, &it.Artist,
			&f.FilePID, &f.LibraryPID, &f.DisplayPath, &f.Size, &f.Role, &essence,
			&f.Codec, &f.Bitrate, &f.SampleRate, &f.BitDepth); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if len(out) == 0 || id != lastID {
			out = append(out, it)
			lastID, partAudio = id, map[string]bool{}
		}
		if f.Role == alternateRole {
			f.Reason = model.CopyOtherEncoding
			if partAudio[essence] {
				f.Reason = model.CopySameAudio
			}
		} else {
			partAudio[essence] = true
		}
		cur := &out[len(out)-1]
		cur.Files = append(cur.Files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}
