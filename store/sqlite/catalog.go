package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// Compile-time assertion that Store satisfies the catalog port.
var _ model.Catalog = (*Store)(nil)

// EnsureLibrary upserts a library by root, preserving pid/created_at on an
// existing row and refreshing its mode/profile/display.
//
// The root is matched byte-exact first and then by the platform's path rule
// (libraryByRootDB), so on Windows a re-registered C:\Music and c:\Music are one
// library. A row found by that fold keeps the root and display_root it was first
// registered with: the stored spelling is the prefix every file.path was built from
// and the one LoadScopedFileIndex ranges over, so re-spelling it in place would hide
// every file under it from the next scan. Use RelocateLibraryRoot to actually move a
// root; it rewrites the file paths too. Only the policy fields (mode/media/profile)
// refresh on that branch. A mode other than in-place drops the folder fallback, which
// only an in-place library takes.
func (s *Store) EnsureLibrary(ctx context.Context, lib *model.Library) (*model.Library, error) {
	const op = "store.EnsureLibrary"
	var out *model.Library
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()
		existing, err := libraryByRootTx(ctx, tx, lib.Root)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		media := string(lib.MediaType()) // "" normalizes to mixed
		if existing != nil && !bytes.Equal(existing.Root, lib.Root) {
			// Matched by case fold, so the caller spelled the root differently. The
			// stored spelling stays, which means display_root does too; only the policy
			// fields can change here. This branch needs its own no-op guard because the
			// one below compares DisplayRoot, which differs by definition on a fold
			// match, so reusing it would re-spell the row and append a delta on every
			// open.
			if existing.Mode == lib.Mode && existing.MediaType() == lib.MediaType() &&
				existing.Profile == lib.Profile {
				out = existing
				return nil
			}
			fallback := existing.FolderFallback && lib.Mode == model.ModeInPlace
			if _, err := tx.ExecContext(ctx,
				"UPDATE library SET mode=?, media=?, profile=?, folder_fallback=? WHERE id=?",
				string(lib.Mode), media, lib.Profile, fallback, existing.ID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			existing.Mode, existing.Media, existing.Profile = lib.Mode, model.MediaType(media), lib.Profile
			existing.FolderFallback = fallback
			out = existing
			return appendChange(ctx, tx, "library", existing.PID, model.OpUpdate)
		}
		if existing != nil {
			// No-op when nothing changed, so re-opening a library each session
			// doesn't emit a spurious change_log delta.
			if existing.DisplayRoot == lib.DisplayRoot && existing.Mode == lib.Mode &&
				existing.MediaType() == lib.MediaType() && existing.Profile == lib.Profile {
				out = existing
				return nil
			}
			fallback := existing.FolderFallback && lib.Mode == model.ModeInPlace
			if _, err := tx.ExecContext(ctx,
				"UPDATE library SET display_root=?, mode=?, media=?, profile=?, folder_fallback=? WHERE id=?",
				lib.DisplayRoot, string(lib.Mode), media, lib.Profile, fallback, existing.ID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			existing.DisplayRoot, existing.Mode, existing.Media, existing.Profile =
				lib.DisplayRoot, lib.Mode, model.MediaType(media), lib.Profile
			existing.FolderFallback = fallback
			out = existing
			return appendChange(ctx, tx, "library", existing.PID, model.OpUpdate)
		}
		pid := model.NewPID()
		r, err := tx.ExecContext(ctx,
			"INSERT INTO library(pid, root, display_root, mode, media, profile, created_at) VALUES (?,?,?,?,?,?,?)",
			string(pid), lib.Root, lib.DisplayRoot, string(lib.Mode), media, lib.Profile, now)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		id, _ := r.LastInsertId()
		out = &model.Library{ID: id, PID: pid, Root: lib.Root, DisplayRoot: lib.DisplayRoot,
			Mode: lib.Mode, Media: model.MediaType(media), Profile: lib.Profile, CreatedAt: now}
		return appendChange(ctx, tx, "library", pid, model.OpCreate)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LibraryByRoot looks up a library by root: byte-exact first, then by the platform's
// path rule where it folds case (libraryByRootDB), so a re-cased Windows root finds
// the library it names rather than reporting no such root.
func (s *Store) LibraryByRoot(ctx context.Context, root []byte) (*model.Library, error) {
	lib, err := libraryByRootDB(ctx, s.rdb(), root)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.LibraryByRoot", err)
	}
	if lib == nil {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.LibraryByRoot", "no such library root")
	}
	return lib, nil
}

// Libraries lists all registered libraries.
func (s *Store) Libraries(ctx context.Context) ([]*model.Library, error) {
	out, err := librariesDB(ctx, s.rdb())
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.Libraries", err)
	}
	return out, nil
}

// SetLibraryReadOnly sets a library's read-only flag and returns the library. A change
// appends a library update delta and a repeat appends nothing. The internal podcast
// library is refused, since its files are the podcast engine's to manage.
func (s *Store) SetLibraryReadOnly(ctx context.Context, pid model.PID, readOnly bool) (*model.Library, error) {
	return s.setLibraryFlag(ctx, "store.SetLibraryReadOnly", pid, "read_only", readOnly,
		func(l *model.Library) string {
			if l.Mode == model.ModePodcast {
				return "the internal podcast library cannot be made read-only"
			}
			return ""
		},
		func(l *model.Library) *bool { return &l.ReadOnly })
}

// SetLibraryFolderFallback turns a library's folder fallback on or off (see
// model.Library.FolderFallback) and returns the library, with SetLibraryReadOnly's
// delta rule. Only an in-place library takes it: a managed library's folders are
// organize's rendering of the catalog (placeholders like "Unknown Artist" included), so
// they could only echo it back, and an episode's names come from its feed.
func (s *Store) SetLibraryFolderFallback(ctx context.Context, pid model.PID, on bool) (*model.Library, error) {
	return s.setLibraryFlag(ctx, "store.SetLibraryFolderFallback", pid, "folder_fallback", on,
		func(l *model.Library) string {
			if on && l.Mode != model.ModeInPlace {
				return "only an in-place library takes a folder fallback; a " + string(l.Mode) + " library's folders are WaxBin's own"
			}
			return ""
		},
		func(l *model.Library) *bool { return &l.FolderFallback })
}

// setLibraryFlag sets one boolean library column, named by the caller, and the field
// flag reads it through. refuse names why the library cannot take the change, or "".
func (s *Store) setLibraryFlag(ctx context.Context, op string, pid model.PID, column string, on bool, refuse func(*model.Library) string, flag func(*model.Library) *bool) (*model.Library, error) {
	var out *model.Library
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		lib, err := scanLibrary(tx.QueryRowContext(ctx, librarySelect+" WHERE pid = ?", string(pid)))
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "no such library: "+string(pid))
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if reason := refuse(lib); reason != "" {
			return waxerr.New(waxerr.CodeInvalid, op, reason)
		}
		out = lib
		if *flag(lib) == on {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE library SET "+column+" = ? WHERE id = ?", on, lib.ID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		*flag(lib) = on
		return appendChange(ctx, tx, "library", pid, model.OpUpdate)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// dropLibraryTx drops a library's active trash entries, giving each one's item an update
// (it can no longer be restored, which a tailer learns as DeleteTrashRow tells it), then
// the library row with its delta, and returns how many entries went. Restored entries go
// with the row.
func dropLibraryTx(ctx context.Context, tx *sql.Tx, libID int64, libPID model.PID) (int, error) {
	items, err := queryInt64sTx(ctx, tx, `SELECT DISTINCT pi.id FROM trash tr JOIN playable_item pi ON pi.pid = tr.item_pid
		WHERE tr.library_id = ? AND tr.restored_at IS NULL`, libID)
	if err != nil {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, "DELETE FROM trash WHERE library_id = ? AND restored_at IS NULL", libID)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	for _, id := range items {
		if err := appendItemUpdateTx(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM library WHERE id = ?", libID); err != nil {
		return 0, err
	}
	return int(n), appendChange(ctx, tx, "library", libPID, model.OpDelete)
}

// removeBatch is how many files a library removal detaches per transaction.
const removeBatch = 500

// RemoveLibrary takes a library out of the catalog and touches nothing on disk. Each file
// row under it is detached as a trash detach does it: an alternate in another library
// takes the place of a primary or part, and an item left with no file is archived, its
// pid, play state and list entries kept, so adding the root back and scanning re-links
// it. The library's trash journal goes too, leaving its trashed files where they lie,
// then the row with one library delta. The files go removeBatch at a time, each batch its
// own transaction, beat (when set) hearing the files detached so far and the total after
// each; a cancel stops between batches with the library still registered, and calling
// again finishes the job. The promoted files are returned for the caller to re-read. The
// internal podcast library is refused.
func (s *Store) RemoveLibrary(ctx context.Context, libPID model.PID, beat func(done, total int) error) (*model.RemoveRootReport, []model.PromotedFile, error) {
	return s.removeLibrary(ctx, libPID, removeBatch, beat)
}

func (s *Store) removeLibrary(ctx context.Context, libPID model.PID, batch int, beat func(done, total int) error) (*model.RemoveRootReport, []model.PromotedFile, error) {
	const op = "store.RemoveLibrary"
	lib, err := scanLibrary(s.rdb().QueryRowContext(ctx, librarySelect+" WHERE pid = ?", string(libPID)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, waxerr.New(waxerr.CodeNotFound, op, "no such library: "+string(libPID))
	}
	if err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if lib.Mode == model.ModePodcast {
		return nil, nil, waxerr.New(waxerr.CodeInvalid, op, "the internal podcast library follows the podcasts dir config and cannot be removed")
	}
	rep := &model.RemoveRootReport{Root: lib.DisplayRoot}
	var promoted []model.PromotedFile
	detached := map[model.PID]bool{}
	// The promoted files are handed back however the run ends, less any it went on to
	// detach, which re-reading would put back.
	settled := func() []model.PromotedFile {
		var out []model.PromotedFile
		for _, p := range promoted {
			if !detached[p.FilePID] {
				out = append(out, p)
			}
		}
		return out
	}
	for finished := false; !finished; {
		if err := ctx.Err(); err != nil {
			return rep, settled(), waxerr.FromContext(op, err, waxerr.CodeIO)
		}
		b := newDetachBatch()
		dropped, left := 0, 0
		err := s.writeTx(ctx, func(tx *sql.Tx) error {
			ids, err := queryInt64sTx(ctx, tx, "SELECT id FROM file WHERE library_id = ? ORDER BY id LIMIT ?", lib.ID, batch)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			for _, id := range ids {
				if _, err := b.detachTx(ctx, tx, id); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
			if err := b.settleTx(ctx, tx); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// The batch that empties the library deletes it in the same transaction, so no
			// file can land in between and go with the row unsettled. Until then the count
			// left keeps the progress true when a file lands meanwhile.
			if len(ids) == batch {
				return waxerr.Wrap(waxerr.CodeIO, op,
					tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM file WHERE library_id = ?", lib.ID).Scan(&left))
			}
			finished = true
			n, err := dropLibraryTx(ctx, tx, lib.ID, libPID)
			dropped = n
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		})
		if err != nil {
			return rep, settled(), err
		}
		rep.FilesDetached += len(b.files)
		rep.ItemsArchived += b.archived
		rep.TrashRowsDropped += dropped
		promoted = append(promoted, b.promoted...)
		for _, pid := range b.files {
			detached[pid] = true
		}
		if beat != nil && len(b.files) > 0 {
			if err := beat(rep.FilesDetached, rep.FilesDetached+left); err != nil {
				return rep, settled(), err
			}
		}
	}
	return rep, settled(), nil
}

// LibraryReadOnly reads one library's read-only flag by rowid, for the write loops'
// check just before each write.
func (s *Store) LibraryReadOnly(ctx context.Context, id int64) (bool, error) {
	const op = "store.LibraryReadOnly"
	var ro bool
	err := s.rdb().QueryRowContext(ctx, "SELECT read_only FROM library WHERE id = ?", id).Scan(&ro)
	if errors.Is(err, sql.ErrNoRows) {
		return false, waxerr.New(waxerr.CodeNotFound, op, fmt.Sprintf("no such library: %d", id))
	}
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return ro, nil
}

// libraryIDsByPIDs resolves library pids to rowids, in input order. An unknown
// pid is CodeNotFound rather than a silently narrower scope, the same treatment
// userStateJoin gives a bad user pid. A nil or empty input returns nil. The
// per-pid lookup is fine at this table's size (a handful of roots).
func (s *Store) libraryIDsByPIDs(ctx context.Context, pids []model.PID, op string) ([]int64, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	out := make([]int64, 0, len(pids))
	for _, pid := range pids {
		var id int64
		err := s.rdb().QueryRowContext(ctx, "SELECT id FROM library WHERE pid = ?", string(pid)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, waxerr.New(waxerr.CodeNotFound, op, "no such library: "+string(pid))
		}
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, id)
	}
	return out, nil
}

// PutScannedTrack persists one scanned track atomically: resolve/insert the
// file (preserving pid on a path or essence match), resolve/insert the logical
// item by (kind, identity_key), upsert the track subtype, and link them, writing
// the matching change_log rows. The store owns all pid assignment.
func (s *Store) PutScannedTrack(ctx context.Context, in model.PutScannedTrackInput) (*model.ScanItemResult, error) {
	const op = "store.PutScannedTrack"
	res := &model.ScanItemResult{}
	cover, err := s.examineArt(ctx, in.CoverArt)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()

		// One resolution of the item key serves the relink, the overlay and the item
		// write, unless the essence re-key below moves a key.
		key, err := resolveItemTx(ctx, tx, s.log, in.Item.Kind, in.Item.IdentityKey, bookAdoptKey{})
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		fileID, filePID, priorEssence, err := s.resolveFile(ctx, tx, in, key.accept(), now, res)
		if err != nil {
			return err
		}
		res.FilePID = filePID

		// A book whose sole file this is, now read as a track, becomes one in place.
		rekinded := false
		if key.id == 0 {
			id, err := rekindItemForFileTx(ctx, tx, fileID, model.KindTrack, in.Item.IdentityKey, in.PreserveLocks && !in.KindForced, now)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if id != 0 {
				key, rekinded = &resolvedItem{id: id}, true
			}
		}

		// Record this file's sidecar observations (replacing the prior set) so the next
		// scan can stat-compare them and re-parse only a changed sidecar, and so a
		// since-deleted sidecar's observation is pruned rather than forcing a full
		// re-hash forever.
		if err := replaceFileAuxTx(ctx, tx, fileID, in.AuxObservations); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Replace this scan's own diagnostics and record that they were derived under
		// the current rule set. The stamp lives here, at the scan call site, rather than
		// inside the shared helper: an organize-origin write must never mark a
		// never-scanned file as derived.
		if err := replaceFileDiagnosticsTx(ctx, tx, fileID, model.OriginScan, in.Diagnostics); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := stampDiagVersionTx(ctx, tx, fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Unchanged bytes with a different essence hash mean the essence algorithm
		// changed. A real re-encode would change content_hash too. Re-key the item
		// in place so its pid, play_state, and provenance survive the upgrade.
		if !res.ContentChanged && priorEssence != "" && priorEssence != in.File.EssenceHash {
			if err := preserveItemIdentityForFile(ctx, tx, fileID, in.Item.Kind, in.Item.IdentityKey); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			key = nil
		}

		// What the file itself says, kept from before the lock overlay so the owed rows
		// can be checked against it.
		fileTitle, fileTrack := in.Item.Title, in.Track

		// Overlay the item's locked fields, and the fills the file says nothing about,
		// onto the scanned values before any writer runs (overlayStoredTrackTx). Runs
		// after the essence-algorithm re-key above so it resolves the (possibly re-keyed)
		// existing item.
		prior, err := overlayStoredTrackTx(ctx, tx, s.log, fileID, &in.Track, &in.Item, in.Derived, in.PreserveLocks, key)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// An item's primary on another file is re-pointed by a put only as
		// arrivalTakesOverTx allows: the same audio arriving again is a copy, another
		// encoding of the recording takes the item over by outranking a primary still on
		// disk, and whichever file loses becomes an alternate. A primary the walk finds
		// missing may have moved to a folder it has not reached yet, so it is replaced
		// only once reconciliation has marked the item missing.
		demoted := false
		if prior != nil {
			primary, err := primaryFileTx(ctx, tx, prior.itemID)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// The primary itself, read again, yields to a better encoding the item holds on
			// disk, so the end state does not depend on which file the walk reached first.
			if primary != nil && primary.fileID == fileID {
				better, err := outrankingAlternateTx(ctx, tx, prior.itemID, in.LibraryID, in.File, nil)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if better != nil {
					if err := demoteToAlternateTx(ctx, tx, prior.itemID, fileID); err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
					p, err := promoteTx(ctx, tx, prior.itemID, better, primaryRole, 0)
					if err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
					res.Promoted = append(res.Promoted, *p)
					if err := appendItemUpdateTx(ctx, tx, prior.itemID); err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
					if primary, err = primaryFileTx(ctx, tx, prior.itemID); err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
					return s.attachCopyTx(ctx, tx, in, fileID, filePID, prior.itemID, primary, fileTitle, fileTrack, res, now, cover)
				}
			}
			if primary != nil && primary.fileID != fileID {
				better, err := arrivalTakesOverTx(ctx, tx, prior.itemID, in.LibraryID, in.File, primary)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if !better {
					return s.attachCopyTx(ctx, tx, in, fileID, filePID, prior.itemID, primary, fileTitle, fileTrack, res, now, cover)
				}
				if err := demoteToAlternateTx(ctx, tx, prior.itemID, primary.fileID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				demoted = true
			}
		}

		known := &resolvedItem{}
		if prior != nil {
			known.id = prior.itemID
		}
		itemID, itemPID, created, stateChanged, priorTitle, err := upsertItem(ctx, tx, s.log, in.Item, in.Track.Year, bookAdoptKey{}, now, in.PreferredItemPID, known)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		res.ItemPID, res.ItemCreated = itemPID, created

		// The fields this put re-derives over an existing item, judged before upsertTrack
		// replaces the columns they are compared against.
		var rederived []string
		var prov map[string]rederivable
		if !created {
			rederived, prov, err = rederivedTrackTx(ctx, tx, itemID, priorTitle, in.Item.Title, in.Track, in.PreserveLocks, prior)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		res.MetadataChanged = len(rederived) > 0 || rekinded

		if err := upsertTrack(ctx, tx, itemID, in.Track); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Overlay the file's custom tags onto item_tag, honoring per-key locks. Runs
		// before the entity block so the FTS rebuild there picks up the tag values, and
		// every scan (like lyrics/art) so an added custom tag is caught without an audio
		// change.
		tagsChanged, tagsReplaced, err := syncItemTagsTx(ctx, tx, itemID, in.CustomTags, in.PreserveLocks)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Track the entities whose maintained rollups this write touches, then
		// recompute only those rows inside this transaction.
		affected := newAffectedRollups()

		retiredEnrichment, err := settleRederivedTx(ctx, tx, itemID, rederived, prov, in.PreserveLocks, affected)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Resolve normalized entities and refresh the item's FTS row when the scan
		// actually changed catalog inputs. A byte-identical rescan skips this work
		// and emits no entity-side deltas.
		entitiesResolved := created || res.FileCreated || res.ContentChanged || res.Relinked || res.MetadataChanged || rekinded
		if entitiesResolved {
			// The entities the item leaves (on a retag) lose the track, so collect
			// them before relinking, and the entities it joins after.
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := resolveAndLinkEntities(ctx, tx, s.log, itemID, in.Track, in.File.Path, res.RelinkedFrom, fileID, affected); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		} else if tagsChanged {
			// A custom-tag change with unchanged audio bytes did not re-resolve entities
			// (which is what rebuilds the FTS row), so refresh the search row directly or the
			// new tag values would not be searchable until the next audio change.
			if err := syncSearchFTS(ctx, tx, itemID, in.Track); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// Lyrics and cover art are re-evaluated on every scan, outside the
		// audio-change gate, so an added or edited .lrc sidecar or directory cover
		// image is picked up even when the audio bytes are unchanged. Both writes are
		// idempotent (they compare against the stored value and do nothing when it is
		// unchanged), so a no-op rescan stays silent. They run after entity resolution
		// so a freshly resolved album_id is available to map art onto; an unchanged
		// rescan reuses the album_id persisted by a prior scan. Their changed flags feed
		// the item delta below so a lyrics/cover-only change is not silent to consumers.
		lyricsChanged, err := putLyricsTx(ctx, tx, itemID, in.Lyrics, in.PreserveLocks, true)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		artChanged, err := attachArtRespectingLockTx(ctx, tx, itemID, cover, in.PreserveLocks)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Surfaced to the caller, not just used for the delta below: a sidecar-only
		// change leaves the audio bytes (and so ContentChanged) untouched, so without
		// this the scanner's counters would all read zero for it.
		res.SidecarsChanged = lyricsChanged || artChanged

		// Origin evidence carried by the file's own tags, recorded only when the item
		// has no acquisition row yet (an event-recorded origin always wins).
		acqAdded, err := insertAcquisitionIfAbsentTx(ctx, tx, itemID, in.Acquisition, in.PreserveLocks)
		if err != nil {
			return err
		}

		// Re-home the file onto this item as its primary, detaching it from any prior
		// item (an in-place essence change re-keys the file to a new identity, or a copy
		// takes over an item whose primary is gone).
		dep, err := departingTx(ctx, tx, fileID, in.File.EssenceHash, itemID, in.PreserveLocks)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if created {
			dep.intoPID = itemPID
		}
		orphans, err := linkPrimaryFile(ctx, tx, itemID, fileID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		var took model.PID
		if res.Promoted, res.Folded, took, err = reconcileOrphansTx(ctx, tx, orphans, dep, affected); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		handed := carryHandedPID(res, &itemPID, took)
		if demoted {
			if err := refreshCopyDiagnosticsTx(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// Recompute touched rollups from the final base tables, keeping browse
		// counts current without a whole-catalog rebuild.
		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if err := settleOwedByScanTx(ctx, tx, fileID, itemID, scanSettle{
			fileTitle: fileTitle, title: in.Item.Title, fileTrack: fileTrack, track: in.Track,
			preserveLocks: in.PreserveLocks, derived: in.Derived,
			cover: cover, acquisitionRecorded: acqAdded,
			fileTags: in.CustomTags, tagsReplaced: tagsReplaced,
		}); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Read after entity resolution, which settles the album the label half asks about.
		if retiredEnrichment {
			if err := dropMootEnrichmentDriftTx(ctx, tx, fileID, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		kindLocked := false
		if in.LockKind {
			if kindLocked, err = lockKindTx(ctx, tx, itemID, now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// Emit change_log rows only for real changes, so a no-op rescan is silent
		// (essence-first change detection) and delta consumers don't re-process.
		if res.FileCreated || res.ContentChanged || res.Relinked {
			if err := appendChange(ctx, tx, "file", filePID, opFor(res.FileCreated)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		// Emit an item delta on create, a content change, a re-derived field, a state
		// transition (a restored file flipping missing -> present), a lyrics/cover-only
		// change, OR a newly attributed origin, so a delta consumer never serves stale
		// metadata/art after any real change. acqAdded is true only when a row was actually
		// inserted, so a rescan of an already-attributed item stays silent.
		if created || res.ContentChanged || res.MetadataChanged || stateChanged || lyricsChanged || artChanged || acqAdded || tagsChanged || demoted || kindLocked {
			if err := appendChange(ctx, tx, "item", itemPID, putItemOp(created, handed)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// resolveFile finds-or-creates the file row, preserving the pid on a path match
// (rescan/retag) or a match on a gone row of the same essence (re-link after a move).
// For a path match it also returns the file's prior essence hash, so the caller can
// detect an essence-algorithm change over unchanged bytes and preserve item identity.
func (s *Store) resolveFile(ctx context.Context, tx *sql.Tx, in model.PutScannedTrackInput, accept map[int64]bool, now int64, res *model.ScanItemResult) (int64, model.PID, string, error) {
	if existing, err := fileByPathTx(ctx, tx, in.File.Path); err != nil {
		return 0, "", "", err
	} else if existing != nil {
		res.ContentChanged = existing.ContentHash != in.File.ContentHash
		if err := updateFileRow(ctx, tx, existing.ID, in.File, now); err != nil {
			return 0, "", "", err
		}
		return existing.ID, existing.PID, existing.EssenceHash, nil
	}

	// No path match: re-link a row with identical essence only when that row's file
	// is gone from disk (a genuine move). A copy whose original is still on disk gets
	// its own row, which the caller attaches to the item as an alternate.
	if in.File.EssenceHash != "" {
		relink, err := fileByEssenceGoneTx(ctx, tx, in.File.EssenceHash, in.File.ContentHash, in.LibraryID, accept)
		if err != nil {
			return 0, "", "", err
		}
		if relink != nil {
			res.Relinked = true
			res.RelinkedFrom = string(relink.Path)
			res.ContentChanged = relink.ContentHash != in.File.ContentHash
			if err := updateFileRow(ctx, tx, relink.ID, in.File, now); err != nil {
				return 0, "", "", err
			}
			if err := renameCopyDetailsTx(ctx, tx, relink.ID, relink.DisplayPath, in.File.DisplayPath); err != nil {
				return 0, "", "", err
			}
			return relink.ID, relink.PID, relink.EssenceHash, nil
		}
	}

	res.FileCreated = true
	pid := model.NewPID()
	id, err := insertFileRow(ctx, tx, in.LibraryID, pid, in.File, now)
	if err != nil {
		return 0, "", "", err
	}
	return id, pid, "", nil
}

// preserveItemIdentityForFile re-keys the item of kind backing fileID to newKey. It is
// used only when a file's bytes are unchanged but a new essence algorithm
// produced a different digest, letting the same audio keep its item identity. It
// is a no-op when there is no backing item of that kind (a book part read as a track
// leaves the book's key alone), the key is already current, or another item already
// owns newKey. An alternate re-keys its item too: a copy re-read before
// its primary would otherwise fork a new item that the primary then joins, leaving
// the original item with no file. A windowed edge never does, since a rip's track is a
// window of the file and not the whole file the new key names.
func preserveItemIdentityForFile(ctx context.Context, tx *sql.Tx, fileID int64, kind model.Kind, newKey string) error {
	if newKey == "" {
		return nil
	}
	var itemID int64
	var curKey sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT pi.id, pi.identity_key FROM item_file itf
		 JOIN playable_item pi ON pi.id = itf.item_id
		 WHERE itf.file_id = ? AND itf.role IN ('primary', 'alternate') AND itf.start_frames IS NULL AND pi.kind = ?`,
		fileID, string(kind)).Scan(&itemID, &curKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if curKey.String == newKey {
		return nil
	}
	// Do not collide with a different item that already owns newKey; the normal
	// upsert/orphan path handles that real dedup case.
	var other int64
	switch err := tx.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE kind = ? AND identity_key = ? AND id <> ?",
		string(kind), newKey, itemID).Scan(&other); {
	case err == nil:
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE playable_item SET identity_key = ? WHERE id = ?", newKey, itemID)
	return err
}

// budgetScanCeiling bounds how many rows a budget-mode (minutes/megabytes)
// evaluation scans before giving up on filling the budget, so a pathological
// catalog full of zero-duration or zero-size rows (which are skipped, not
// admitted) cannot turn "an hour of music" into a full-table crawl.
const budgetScanCeiling = 50_000

// QueryItems compiles q against the item field whitelist and returns the matching
// item views. If q references a per-user field such as starred, rating, or
// play_count, it evaluates against userPID's play_state, and an empty userPID means
// the default user. A query with no user-state field is not scoped by user, but a
// non-empty userPID is still checked to exist so a typo does not pass silently.
//
// A non-count LimitMode reinterprets Limit: random draws Limit rows by a seeded
// shuffle (LimitSeed pins the order; 0 draws a fresh order per call), and
// minutes/megabytes accumulate rows in order until adding the next row would
// exceed the budget, at which point that row is excluded and the scan stops. A
// row with no measurable cost (an unknown duration, a fileless item) cannot
// participate in a budget fill and is skipped, so "an hour of music" can never
// admit an unbounded run of unpriceable rows as free. Budget modes honor Sorts;
// with empty Sorts a non-zero LimitSeed fills in shuffle order ("a random hour
// of music"), and seed 0 fills in the canonical sort_key order. Offset stays in
// SQL for every mode, so it skips rows before any budget accumulation. A
// megabytes budget prices an item at the sum of all its backing files, every
// part of a multi-file book included (pricing only the primary would overflow a
// device budget). For a virtual track carved from a shared single-file rip that
// means the whole rip file's size once per included track, over-counting that
// under-fills the budget rather than overflowing it, which is the safe way to
// be wrong.
func (s *Store) QueryItems(ctx context.Context, q query.Query, userPID model.PID) ([]*model.ItemView, error) {
	return s.queryItems(ctx, q, userPID, false)
}

// QueryItemsByPrimary is QueryItems with the library field reading each item's primary
// file alone, for an operation on an item's own files (organize, a delete by query): an
// item another library holds only a copy of is not that library's to move or delete.
func (s *Store) QueryItemsByPrimary(ctx context.Context, q query.Query, userPID model.PID) ([]*model.ItemView, error) {
	return s.queryItems(ctx, q, userPID, true)
}

func (s *Store) queryItems(ctx context.Context, q query.Query, userPID model.PID, byPrimary bool) ([]*model.ItemView, error) {
	const op = "store.QueryItems"
	stmt, args, c, err := s.queryItemsStmt(ctx, q, userPID, byPrimary, op)
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb().QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()

	if c.LimitMode == query.LimitMegabytes || c.LimitMode == query.LimitMinutes {
		return s.scanBudgetItems(rows, c, c.LimitMode == query.LimitMegabytes, op)
	}

	var out []*model.ItemView
	for rows.Next() {
		v, err := scanItemView(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// queryItemsStmt assembles queryItems' statement and bind args, split out so the plan
// tests EXPLAIN the real statement.
func (s *Store) queryItemsStmt(ctx context.Context, q query.Query, userPID model.PID, byPrimary bool, op string) (string, []any, *query.Compiled, error) {
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return "", nil, nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	if byPrimary {
		fm = primaryLibraryFields{fm}
	}
	c, err := query.Compile(q, fm)
	if err != nil {
		return "", nil, nil, err
	}
	userJoin, leadArgs, err := s.userStateJoin(ctx, c, userPID, op)
	if err != nil {
		return "", nil, nil, err
	}

	megabytes := c.LimitMode == query.LimitMegabytes
	budget := megabytes || c.LimitMode == query.LimitMinutes
	// Random mode compiles with no Sorts, and a budget mode with empty Sorts and a
	// non-zero seed shuffles the fill order; both order by the deterministic wb_shuffle
	// hash. No index holds that order, so the rows are sorted by id and hash in a
	// subquery and the item view is read for the rows the caller takes, as browse's
	// random list does. The seed is an int64 formatted here, so inlining it is safe.
	// Everything else keeps the query's own order, defaulting to the canonical
	// sort_key order.
	shuffled := c.LimitMode == query.LimitRandom || budget && c.OrderBy == "" && c.LimitSeed != 0

	cols := itemViewCols
	if megabytes {
		// The megabytes budget needs the item's total byte cost, which is not an
		// ItemView column. Widen only this statement's SELECT (the budget scan
		// appends the matching dest explicitly) rather than touching the shared
		// itemViewCols/itemViewDests pair every other reader scans. The cost sums
		// all parts, not just the primary: a multi-file book transfers every part,
		// so pricing only part one would overflow a device budget. An alternate is
		// another copy of the same audio, which a device holds once.
		cols += ", (SELECT COALESCE(SUM(szf.size),0) FROM item_file szif JOIN file szf ON szf.id = szif.file_id" +
			" WHERE szif.item_id = pi.id AND szif.role IN ('primary', 'part'))"
	}
	where := andWhere(c.Where, entityPredicate(q.Entity))
	// leadArgs carries the join's user id (or is empty) and precedes the query args.
	args := append(leadArgs, c.Args...)

	var sb strings.Builder
	if shuffled {
		seed := c.LimitSeed
		if seed == 0 {
			seed = time.Now().UnixNano() // a fresh draw per evaluation
		}
		from := itemJoins
		if where == "" && userJoin == "" {
			from = " FROM playable_item pi" // see browseStmt
		}
		fmt.Fprintf(&sb, "SELECT %s FROM (SELECT pi.id, pi.pid, wb_shuffle(%d, pi.pid) AS ord%s", cols, seed, from)
	} else {
		sb.WriteString("SELECT " + cols + itemJoins)
	}
	sb.WriteString(userJoin)
	if where != "" {
		sb.WriteString(" WHERE ")
		sb.WriteString(where)
	}
	switch {
	case shuffled:
		sb.WriteString(" ORDER BY ord, pi.pid")
	case c.OrderBy != "":
		sb.WriteString(" ORDER BY ")
		sb.WriteString(c.OrderBy)
		sb.WriteString(", pi.pid")
	default:
		sb.WriteString(" ORDER BY pi.sort_key, pi.pid")
	}
	// A budget mode caps by accumulation below, never by SQL LIMIT; only the offset
	// stays in SQL, skipping rows before any budget accounting. A shuffle's subquery
	// carries a LIMIT whatever the mode, since SQLite folds a subquery without one into
	// the outer join and sorts the whole view again.
	switch {
	case c.Limit > 0 && !budget:
		sb.WriteString(" LIMIT ?")
		args = append(args, c.Limit)
	case c.Offset > 0 || shuffled:
		sb.WriteString(" LIMIT -1") // SQLite requires a LIMIT before OFFSET
	}
	if c.Offset > 0 {
		sb.WriteString(" OFFSET ?")
		args = append(args, c.Offset)
	}
	if shuffled {
		sb.WriteString(") w JOIN playable_item pi ON pi.id = w.id" + itemSubJoins + " ORDER BY w.ord, w.pid")
	}
	return sb.String(), args, c, nil
}

// scanBudgetItems fills a minutes/megabytes budget from ordered rows: each row's
// cost is its effective duration (minutes) or the summed size of all its backing
// files (megabytes, the extra trailing SELECT column), and the first row that
// would overflow the budget is excluded and ends the scan. The order is
// authoritative, so there is no best-fit skipping. A row with no measurable cost
// (unknown duration, fileless item) is skipped rather than admitted: it cannot
// be priced against the budget, and admitting it free would let an unanalyzed
// run swamp "an hour of music". Rows are scanned with explicit dests so the
// count matches the possibly-widened SELECT; the shared scanItemView would
// mismatch it.
func (s *Store) scanBudgetItems(rows *sql.Rows, c *query.Compiled, megabytes bool, op string) ([]*model.ItemView, error) {
	unit := int64(60_000) // minutes -> milliseconds of playtime
	if megabytes {
		unit = 1_000_000 // SI megabytes (10^6 bytes)
	}
	budgetLeft := int64(c.Limit) * unit
	if budgetLeft/unit != int64(c.Limit) {
		// An absurd Limit overflowed the multiply; saturate rather than let a
		// wrapped-negative budget silently return nothing (a budget that large
		// means "everything fits").
		budgetLeft = math.MaxInt64
	}
	var out []*model.ItemView
	scanned := 0
	for rows.Next() {
		if scanned >= budgetScanCeiling {
			// The scan-work guard fired: the catalog fed this many rows without
			// filling the budget. The rows accumulated so far are returned (they
			// legitimately fit), and the warning keeps the truncation observable.
			// A returned flag would push a signature change through the Catalog
			// port and every caller for a case only a degenerate catalog hits.
			s.log.Warn("budget limit-mode scan hit the row ceiling; result truncated",
				"mode", string(c.LimitMode), "ceiling", budgetScanCeiling, "returned", len(out))
			break
		}
		scanned++
		var v model.ItemView
		var n itemViewNulls
		var size int64
		dests := itemViewDests(&v, &n)
		if megabytes {
			dests = append(dests, &size)
		}
		if err := rows.Scan(dests...); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		n.apply(&v)
		cost := v.DurationMS
		if megabytes {
			cost = size
		}
		if cost <= 0 {
			continue // no measurable cost, so the row cannot join the fill
		}
		if cost > budgetLeft {
			break // the overflowing row is excluded and the fill ends
		}
		budgetLeft -= cost
		out = append(out, &v)
	}
	// Stop the statement promptly on an early break instead of draining the rest
	// of the (unlimited) result set; the caller's deferred Close is then a no-op.
	if err := rows.Close(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, rows.Err()
}

// CountItems returns the number of items matching q, ignoring limit, offset, and
// limit mode: a count is over the full match set, answering "how many would a
// random 25 draw from". userPID scopes any per-user field the same way QueryItems does.
// The user join is on play_state's primary key, so it matches at most one row per
// item and COUNT(*) stays exact.
func (s *Store) CountItems(ctx context.Context, q query.Query, userPID model.PID) (int, error) {
	const op = "store.CountItems"
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	c, err := query.Compile(q, fm)
	if err != nil {
		return 0, err
	}
	userJoin, leadArgs, err := s.userStateJoin(ctx, c, userPID, op)
	if err != nil {
		return 0, err
	}
	stmt := itemCountSelect + userJoin
	if where := andWhere(c.Where, entityPredicate(q.Entity)); where != "" {
		stmt += " WHERE " + where
	}
	// leadArgs (the join user id, or empty) precedes the WHERE args.
	args := append(leadArgs, c.Args...)
	var n int
	if err := s.rdb().QueryRowContext(ctx, stmt, args...).Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return n, nil
}

// ItemByPID returns a single item view by public id.
func (s *Store) ItemByPID(ctx context.Context, pid model.PID) (*model.ItemView, error) {
	const op = "store.ItemByPID"
	v, err := scanItemView(s.rstmts.queryRowContext(ctx, s.rdb(), itemSelect+" WHERE pi.pid = ?", string(pid)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return v, nil
}

// ItemsByPIDs returns item views for the given pids in input order, skipping any
// pid with no matching item and collapsing a repeated pid to its first position.
// The lookup is chunked to stay well under SQLite's bound-parameter limit, so a
// pid array longer than idBatchSize spans multiple SELECTs and is NOT an atomic
// snapshot: a concurrent write between chunks can produce a mixed view. A
// UI-feeding read can tolerate that; a caller that needs a consistent snapshot
// cannot.
func (s *Store) ItemsByPIDs(ctx context.Context, pids []model.PID) ([]*model.ItemView, error) {
	const op = "store.ItemsByPIDs"
	if len(pids) == 0 {
		return nil, nil
	}
	unique := uniquePIDs(pids)
	byPID := make(map[model.PID]*model.ItemView, len(unique))
	err := chunkSlice(unique, idBatchSize, func(chunk []model.PID) error {
		args := make([]any, len(chunk))
		for i, pid := range chunk {
			args[i] = string(pid)
		}
		rows, err := s.rdb().QueryContext(ctx, itemSelect+" WHERE pi.pid IN "+placeholders(len(chunk)), args...)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanItemView(rows)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			byPID[v.PID] = v
		}
		if err := rows.Err(); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]*model.ItemView, 0, len(unique))
	for _, pid := range unique {
		if v, ok := byPID[pid]; ok {
			out = append(out, v)
		}
	}
	return out, nil
}

// FileByPath returns the file at the given raw path, or CodeNotFound.
func (s *Store) FileByPath(ctx context.Context, path []byte) (*model.File, error) {
	f, err := fileByPathDB(ctx, s.rdb(), path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.FileByPath", err)
	}
	if f == nil {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.FileByPath", "no such file")
	}
	return f, nil
}

// FileByPID returns a file (with its quality fields) by public id, or CodeNotFound.
func (s *Store) FileByPID(ctx context.Context, pid model.PID) (*model.File, error) {
	row := s.rdb().QueryRowContext(ctx, fileSelect+" WHERE pid = ?", string(pid))
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.FileByPID", "no file with that pid")
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.FileByPID", err)
	}
	return f, nil
}

// FileQualitiesByItem returns the primary backing file's quality (codec, bitrate,
// sample rate, bit depth) for every present track/book item, keyed by item PID, in
// one query. A catalog-wide quality scan (the upgrade policy) loads all quality up
// front with this instead of one file lookup per item.
func (s *Store) FileQualitiesByItem(ctx context.Context) (map[model.PID]model.File, error) {
	const op = "store.FileQualitiesByItem"
	rows, err := s.rdb().QueryContext(ctx, `SELECT pi.pid, f.codec, f.bitrate, f.sample_rate, f.bit_depth
		FROM playable_item pi
		JOIN item_file if2 ON if2.item_id = pi.id AND if2.role = 'primary'
		JOIN file f ON f.id = if2.file_id
		WHERE pi.kind IN ('track','book') AND pi.state = 'present'`)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	out := make(map[model.PID]model.File)
	for rows.Next() {
		var pid string
		var codec sql.NullString
		var bitrate, sampleRate, bitDepth sql.NullInt64
		if err := rows.Scan(&pid, &codec, &bitrate, &sampleRate, &bitDepth); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out[model.PID(pid)] = model.File{
			Codec: codec.String, Bitrate: int(bitrate.Int64),
			SampleRate: int(sampleRate.Int64), BitDepth: int(bitDepth.Int64),
		}
	}
	return out, rows.Err()
}

// FileByEssence returns a file by essence hash (first match), or CodeNotFound.
func (s *Store) FileByEssence(ctx context.Context, essence string) (*model.File, error) {
	row := s.rdb().QueryRowContext(ctx, fileSelect+" WHERE essence_hash = ? LIMIT 1", essence)
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.FileByEssence", "no file with that essence")
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.FileByEssence", err)
	}
	return f, nil
}

// AdoptItemPID gives the item at from the pid to, unless to is not a valid pid or another
// item holds it, and reports whether it did. The rebuild calls it for an item it made this
// run under a fresh pid, before anything outside the catalog could know that pid, when a
// stamped file joins it; the change log reads as the fresh pid going and the stamped one
// arriving.
func (s *Store) AdoptItemPID(ctx context.Context, from, to model.PID) (bool, error) {
	const op = "store.AdoptItemPID"
	if !to.Valid() {
		return false, nil
	}
	adopted := false
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var err error
		if adopted, err = handPIDTx(ctx, tx, from, to, true); err != nil || !adopted {
			return err
		}
		return appendChange(ctx, tx, "item", to, model.OpCreate)
	})
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return adopted, nil
}

// PlanMove records a 'planned' organize_journal row before the on-disk move,
// returning its journal pid.
func (s *Store) PlanMove(ctx context.Context, in model.RelocateInput) (model.PID, error) {
	const op = "store.PlanMove"
	jpid := model.NewPID()
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		fileID, err := fileIDByPID(ctx, tx, in.FilePID, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO organize_journal(pid, job_pid, file_id, src, dst, state, created_at) VALUES (?,?,?,?,?,'planned',?)",
			string(jpid), string(in.JobPID), fileID, in.SrcPath, in.NewPath, nowNS()); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return jpid, nil
}

// CommitMove updates the file's path columns, marks the journal row 'committed', and
// logs the change in one transaction. A move onto the path the catalog already holds,
// as when a folder above the file was respelled and carried it there (RespellFolder),
// commits the journal row and logs no change, since the file's path has not moved.
func (s *Store) CommitMove(ctx context.Context, journalPID model.PID, in model.RelocateInput) error {
	const op = "store.CommitMove"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		fileID, err := fileIDByPID(ctx, tx, in.FilePID, op)
		if err != nil {
			return err
		}
		if err := commitMoveTx(ctx, tx, fileID, string(journalPID), in); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
}

// commitMoveTx points a file row at its journaled move's destination, marks the journal
// row committed and logs the file's change, for CommitMove and for recovery. A move onto
// the path the row already holds logs nothing.
func commitMoveTx(ctx context.Context, tx *sql.Tx, fileID int64, journalPID string, in model.RelocateInput) error {
	var path, rel []byte
	var from string
	if err := tx.QueryRowContext(ctx, "SELECT path, display_path, rel_path FROM file WHERE id = ?", fileID).Scan(&path, &from, &rel); err != nil {
		return err
	}
	changed := !bytes.Equal(path, in.NewPath) || from != in.NewDisplayPath || !bytes.Equal(rel, in.NewRelPath)
	if _, err := tx.ExecContext(ctx,
		"UPDATE file SET path=?, display_path=?, rel_path=?, last_seen=? WHERE id=?",
		in.NewPath, in.NewDisplayPath, in.NewRelPath, nowNS(), fileID); err != nil {
		return err
	}
	if err := renameCopyDetailsTx(ctx, tx, fileID, from, in.NewDisplayPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE organize_journal SET state='committed' WHERE pid=?", journalPID); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return appendChange(ctx, tx, "file", in.FilePID, model.OpUpdate)
}

// AbortMove marks a planned move 'rolled_back' after an on-disk move failed.
func (s *Store) AbortMove(ctx context.Context, journalPID model.PID) error {
	const op = "store.AbortMove"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE organize_journal SET state='rolled_back' WHERE pid=?", string(journalPID)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
}

// fileIDByPID resolves a file pid to its rowid inside a write transaction, or
// CodeNotFound. It keeps the concrete *sql.Tx rather than widening to queryer: every
// caller resolves the pid and then mutates that row, and the parameter type is what
// makes it a compile error to resolve against the read pool and mutate in a
// transaction the resolution never joined. A read-only caller wants fileIDByPIDRead.
func fileIDByPID(ctx context.Context, tx *sql.Tx, pid model.PID, op string) (int64, error) {
	return fileIDByPIDRead(ctx, tx, pid, op)
}

// fileIDByPIDRead resolves a file pid to its rowid over any read surface, for callers
// that only read (a diagnostic scope, say). It holds the single copy of the SELECT
// both entry points share, following itemIDByPIDRead.
func fileIDByPIDRead(ctx context.Context, q queryer, pid model.PID, op string) (int64, error) {
	var fileID int64
	err := q.QueryRowContext(ctx, "SELECT id FROM file WHERE pid = ?", string(pid)).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such file: "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return fileID, nil
}

// ChangesSince returns change_log rows after seq (capped per call). A seq the feed can
// no longer serve is CodeNotFound, so the consumer reloads in full and resumes from
// LatestChangeSeq: one past the head (the catalog was replaced by a restore or a
// rebuild) and one behind the oldest retained row (PruneChangeLog removed rows the
// consumer never read).
func (s *Store) ChangesSince(ctx context.Context, seq int64) ([]model.Change, error) {
	const op = "store.ChangesSince"
	rows, err := s.rdb().QueryContext(ctx,
		"SELECT seq, ts, entity_type, entity_pid, op FROM change_log WHERE seq > ? ORDER BY seq LIMIT 1000", seq)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.Change
	for rows.Next() {
		var c model.Change
		var pid string
		if err := rows.Scan(&c.Seq, &c.TS, &c.EntityType, &pid, &c.Op); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		c.EntityPID = model.PID(pid)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if len(out) > 0 && seq > 0 && out[0].Seq > seq+1 {
		// Pruning removes a prefix, so the rows after seq are gone only when seq sits
		// below the oldest one left.
		var oldest int64
		if err := s.rdb().QueryRowContext(ctx, "SELECT MIN(seq) FROM change_log").Scan(&oldest); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if seq+1 < oldest {
			return nil, waxerr.New(waxerr.CodeNotFound, op, fmt.Sprintf(
				"change cursor %d is behind the oldest retained change %d; the feed was pruned past it, so reload and resume from its latest seq",
				seq, oldest))
		}
	}
	if len(out) == 0 && seq > 0 {
		head, err := s.LatestChangeSeq(ctx)
		if err != nil {
			return nil, err
		}
		if seq > head {
			return nil, waxerr.New(waxerr.CodeNotFound, op, fmt.Sprintf(
				"change cursor %d is past the feed's head %d; the catalog was replaced, so reload it and resume from its latest seq",
				seq, head))
		}
	}
	return out, nil
}

// LatestChangeSeq returns the highest change_log seq (0 if empty).
func (s *Store) LatestChangeSeq(ctx context.Context) (int64, error) {
	var seq int64
	if err := s.rdb().QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) FROM change_log").Scan(&seq); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.LatestChangeSeq", err)
	}
	return seq, nil
}

// NoteReopened appends the model.ChangeCatalog row and returns its seq. A maintenance
// reopen that found the catalog replaced calls it as its last write.
func (s *Store) NoteReopened(ctx context.Context) (int64, error) {
	const op = "store.NoteReopened"
	var seq int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, appendChangeSQL, nowNS(), model.ChangeCatalog, "", string(model.OpUpdate))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		seq, err = res.LastInsertId()
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
	return seq, err
}

// appendChangeSQL is the one change_log insert. A bulk writer prepares it once
// (see sortKeyPatch.flush) rather than re-parsing it per row.
const appendChangeSQL = "INSERT INTO change_log(ts, entity_type, entity_pid, op) VALUES (?,?,?,?)"

func appendChange(ctx context.Context, tx *sql.Tx, entityType string, pid model.PID, op model.ChangeOp) error {
	_, err := tx.ExecContext(ctx, appendChangeSQL, nowNS(), entityType, string(pid), string(op))
	return err
}

// carryHandedPID makes a put's new item carry on under took, the pid a rip's opening track
// handed it (reconcileOrphansTx), so the put reports and logs it as that item changed
// rather than one created, a track's and a book's put alike. It reports whether there was
// one.
func carryHandedPID(res *model.ScanItemResult, itemPID *model.PID, took model.PID) bool {
	if took == "" {
		return false
	}
	*itemPID = took
	res.ItemPID, res.ItemCreated, res.MetadataChanged = took, false, true
	return true
}

// putItemOp is the change a put logs for its item: a create for one it made, unless a pid
// was handed to it (carryHandedPID), and an update otherwise.
func putItemOp(created, handed bool) model.ChangeOp {
	return opFor(created && !handed)
}

func opFor(created bool) model.ChangeOp {
	if created {
		return model.OpCreate
	}
	return model.OpUpdate
}

// pathExists reports whether the file at the given raw path can still be reached on
// disk. It distinguishes a move (old path gone) from a copy (old path still present)
// when deciding whether to re-link by essence or attach an alternate, and backs
// organize-journal recovery, so a Windows long path must be probed with the
// extended-length prefix or a present file would read as absent (mis-classifying a
// move, or rolling back a completed move during recovery). A filesystem that folds case
// or Unicode form reaches a file under spellings other than its entry's, so the two
// decisions that must tell a file moved to another spelling from one in place, the relink
// and recovery, also ask fsx.Lister whether the path is listed as spelled; the checks that
// ask only whether a file can be played or is gone keep the plain stat.
func pathExists(path []byte) bool {
	_, err := os.Stat(pathx.Long(string(path)))
	return err == nil
}

// RespellFolder gives the cataloged files below folder from the spelling to, a rename of
// the folder between two spellings of its name having moved them (fsx.Speller), so a scan
// finds each file where the catalog says rather than reading it again. It returns how many
// files it moved.
func (s *Store) RespellFolder(ctx context.Context, from, to string) (int, error) {
	const op = "store.RespellFolder"
	from, to = filepath.Clean(from), filepath.Clean(to)
	lo := []byte(from + string(filepath.Separator))
	type below struct {
		id            int64
		pid           model.PID
		path, display string
		root          string
	}
	n := 0
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT f.id, f.pid, f.path, f.display_path, l.root FROM file f
			JOIN library l ON l.id = f.library_id WHERE f.path >= ? AND f.path < ?`, lo, prefixUpperBound(lo))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		var files []below
		for rows.Next() {
			var b below
			var path, root []byte
			if err := rows.Scan(&b.id, &b.pid, &path, &b.display, &root); err != nil {
				rows.Close()
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			b.path, b.root = string(path), string(root)
			files = append(files, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, f := range files {
			path := to + f.path[len(from):]
			display := path
			if strings.HasPrefix(f.display, from) {
				display = to + f.display[len(from):]
			}
			rel := pathx.RelUnder(f.root, path)
			if _, err := tx.ExecContext(ctx, "UPDATE file SET path=?, display_path=?, rel_path=? WHERE id=?",
				[]byte(path), display, []byte(rel), f.id); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := renameCopyDetailsTx(ctx, tx, f.id, f.display, display); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "file", f.pid, model.OpUpdate); err != nil {
				return err
			}
		}
		n = len(files)
		// The other paths that say where things stand go with the folder, as they do with
		// a root move: the sidecars a scan observed and the places trashed files return
		// to. The organize journal keeps the spelling each move was made under, which an
		// undo compares by pathx.CollisionKey and which lets it respell the folder back.
		span := []any{lo, prefixUpperBound(lo)}
		if err := relocateBlobsTx(ctx, tx, "SELECT rowid, path FROM file_aux_state WHERE path >= ? AND path < ?", span,
			"UPDATE file_aux_state SET path = ? WHERE rowid = ?", 1, from, to); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := relocateTrashTx(ctx, tx, "orig_path >= ? AND orig_path < ?", span, from, from, to); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	})
	return n, err
}
