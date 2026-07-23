package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
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
		if existing != nil {
			// No-op when nothing changed, so re-opening a library each session
			// doesn't emit a spurious change_log delta.
			if existing.DisplayRoot == lib.DisplayRoot && existing.Mode == lib.Mode &&
				existing.MediaType() == lib.MediaType() && existing.Profile == lib.Profile {
				out = existing
				return nil
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE library SET display_root=?, mode=?, media=?, profile=? WHERE id=?",
				lib.DisplayRoot, string(lib.Mode), media, lib.Profile, existing.ID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			existing.DisplayRoot, existing.Mode, existing.Media, existing.Profile =
				lib.DisplayRoot, lib.Mode, model.MediaType(media), lib.Profile
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

// LibraryByRoot looks up a library by its raw root bytes.
func (s *Store) LibraryByRoot(ctx context.Context, root []byte) (*model.Library, error) {
	lib, err := libraryByRootDB(ctx, s.read, root)
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
	rows, err := s.read.QueryContext(ctx, librarySelect+" ORDER BY id")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.Libraries", err)
	}
	defer rows.Close()
	var out []*model.Library
	for rows.Next() {
		lib, err := scanLibrary(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.Libraries", err)
		}
		out = append(out, lib)
	}
	return out, rows.Err()
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
		err := s.read.QueryRowContext(ctx, "SELECT id FROM library WHERE pid = ?", string(pid)).Scan(&id)
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
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()

		fileID, filePID, priorEssence, err := s.resolveFile(ctx, tx, in, now, res)
		if err != nil {
			return err
		}
		res.FilePID = filePID

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
		}

		// Overlay the item's locked fields onto the scanned values before any writer
		// runs, so a curated edit survives a re-derive-from-disk `scan --force`. Runs
		// after the essence-algorithm re-key above so it resolves the (possibly re-keyed)
		// existing item. Off only for `scan --force --ignore-locks`.
		if in.PreserveLocks {
			if err := preserveLockedTrackFieldsTx(ctx, tx, &in.Track, &in.Item); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		itemID, itemPID, created, stateChanged, err := upsertItem(ctx, tx, in.Item, now, in.PreferredItemPID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		res.ItemPID, res.ItemCreated = itemPID, created

		if err := upsertTrack(ctx, tx, itemID, in.Track); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Overlay the file's custom tags onto item_tag, honoring per-key locks. Runs
		// before the entity block so the FTS rebuild there picks up the tag values, and
		// every scan (like lyrics/art) so an added custom tag is caught without an audio
		// change.
		tagsChanged, err := syncItemTagsTx(ctx, tx, itemID, in.CustomTags, in.PreserveLocks)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Track the entities whose maintained rollups this write touches, then
		// recompute only those rows inside this transaction.
		affected := newAffectedRollups()

		// Resolve normalized entities and refresh the item's FTS row when the scan
		// actually changed catalog inputs. A byte-identical rescan skips this work
		// and emits no entity-side deltas.
		entitiesResolved := created || res.FileCreated || res.ContentChanged || res.Relinked
		if entitiesResolved {
			// The entities the item leaves (on a retag) lose the track, so collect
			// them before relinking, and the entities it joins after.
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := resolveAndLinkEntities(ctx, tx, itemID, in.Track, in.File.Path); err != nil {
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
		lyricsChanged, err := putLyricsTx(ctx, tx, itemID, in.Lyrics, in.PreserveLocks)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		artChanged, err := attachArtRespectingLockTx(ctx, tx, itemID, in.CoverArt, in.PreserveLocks)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Surfaced to the caller, not just used for the delta below: a sidecar-only
		// change leaves the audio bytes (and so ContentChanged) untouched, so without
		// this the scanner's counters would all read zero for it.
		res.SidecarsChanged = lyricsChanged || artChanged

		// Origin evidence carried by the file's own tags, recorded only when the item
		// has no acquisition row yet (an event-recorded origin always wins).
		acqAdded, err := insertAcquisitionIfAbsentTx(ctx, tx, itemID, in.Acquisition)
		if err != nil {
			return err
		}

		// Re-home the file onto this item, detaching it from any prior item (the
		// case when an in-place essence change re-keys the file to a new identity).
		orphans, err := linkPrimaryFile(ctx, tx, itemID, fileID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, oid := range orphans {
			has, err := itemHasAnyFile(ctx, tx, oid)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if has {
				// A surviving item (a multi-file book) that just lost a file must keep a
				// primary (or it reads back headless) AND have its rollups recomputed,
				// since its summed duration/genre rollup shrank with the detached part.
				if err := affected.collect(ctx, tx, oid); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if err := ensurePrimary(ctx, tx, oid); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if err := refreshBookDuration(ctx, tx, oid); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				continue
			}
			// The orphaned item's entities lose it; collect them before the delete.
			if err := affected.collect(ctx, tx, oid); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			opid, err := deleteItemCascade(ctx, tx, oid)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "item", opid, model.OpDelete); err != nil {
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

		// Emit change_log rows only for real changes, so a no-op rescan is silent
		// (essence-first change detection) and delta consumers don't re-process.
		if res.FileCreated || res.ContentChanged || res.Relinked {
			if err := appendChange(ctx, tx, "file", filePID, opFor(res.FileCreated)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		// Emit an item delta on create, a content change, a state transition (a restored
		// file flipping missing -> present), a lyrics/cover-only change, OR a newly
		// attributed origin, so a delta consumer never serves stale metadata/art after
		// any real change. acqAdded is true only when a row was actually inserted, so a
		// rescan of an already-attributed item stays silent.
		if created || res.ContentChanged || stateChanged || lyricsChanged || artChanged || acqAdded || tagsChanged {
			if err := appendChange(ctx, tx, "item", itemPID, opFor(created)); err != nil {
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
// (rescan/retag) or an essence match (re-link after a move). For a path match it
// also returns the file's prior essence hash, so the caller can detect an
// essence-algorithm change over unchanged bytes and preserve item identity.
func (s *Store) resolveFile(ctx context.Context, tx *sql.Tx, in model.PutScannedTrackInput, now int64, res *model.ScanItemResult) (int64, model.PID, string, error) {
	if existing, err := fileByPathTx(ctx, tx, in.File.Path); err != nil {
		return 0, "", "", err
	} else if existing != nil {
		res.ContentChanged = existing.ContentHash != in.File.ContentHash
		if err := updateFileRow(ctx, tx, existing.ID, in.File, now); err != nil {
			return 0, "", "", err
		}
		return existing.ID, existing.PID, existing.EssenceHash, nil
	}

	// No path match: re-link an existing row with identical essence only when
	// that row's file is gone from disk (a genuine move). If the old path still
	// exists, this is a duplicate copy, not a relocation. Give it its own file
	// row so both copies stay cataloged.
	if in.File.EssenceHash != "" {
		relink, err := fileByEssenceSingleTx(ctx, tx, in.File.EssenceHash, in.LibraryID)
		if err != nil {
			return 0, "", "", err
		}
		if relink != nil && !pathExists(relink.Path) {
			res.Relinked = true
			res.ContentChanged = relink.ContentHash != in.File.ContentHash
			if err := updateFileRow(ctx, tx, relink.ID, in.File, now); err != nil {
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

// preserveItemIdentityForFile re-keys the item backing fileID to newKey. It is
// used only when a file's bytes are unchanged but a new essence algorithm
// produced a different digest, letting the same audio keep its item identity. It
// is a no-op when there is no backing item, the key is already current, or another
// item already owns newKey.
func preserveItemIdentityForFile(ctx context.Context, tx *sql.Tx, fileID int64, kind model.Kind, newKey string) error {
	if newKey == "" {
		return nil
	}
	var itemID int64
	var curKey sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT pi.id, pi.identity_key FROM item_file itf
		 JOIN playable_item pi ON pi.id = itf.item_id
		 WHERE itf.file_id = ? AND itf.role = 'primary'`, fileID).Scan(&itemID, &curKey)
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
	const op = "store.QueryItems"
	fm, ok := fieldMapFor(q.Entity)
	if !ok {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unsupported query entity: "+string(q.Entity))
	}
	c, err := query.Compile(q, fm)
	if err != nil {
		return nil, err
	}
	userJoin, leadArgs, err := s.userStateJoin(ctx, c, userPID, op)
	if err != nil {
		return nil, err
	}

	megabytes := c.LimitMode == query.LimitMegabytes
	budget := megabytes || c.LimitMode == query.LimitMinutes

	var sb strings.Builder
	if megabytes {
		// The megabytes budget needs the item's total byte cost, which is not an
		// ItemView column. Widen only this statement's SELECT (the budget scan
		// appends the matching dest explicitly) rather than touching the shared
		// itemViewCols/itemViewDests pair every other reader scans. The cost sums
		// all backing files, not just the primary: a multi-file book transfers
		// every part, so pricing only part one would overflow a device budget.
		sb.WriteString("SELECT " + itemViewCols +
			", (SELECT COALESCE(SUM(szf.size),0) FROM item_file szif JOIN file szf ON szf.id = szif.file_id WHERE szif.item_id = pi.id)" +
			itemJoins)
	} else {
		sb.WriteString(itemSelect)
	}
	sb.WriteString(userJoin)
	// leadArgs carries the join's user id (or is empty) and precedes the query args.
	args := append(leadArgs, c.Args...)
	where := andWhere(c.Where, entityPredicate(q.Entity))
	if where != "" {
		sb.WriteString(" WHERE ")
		sb.WriteString(where)
	}
	// Random mode compiles with no Sorts, and a budget mode with empty Sorts and a
	// non-zero seed shuffles the fill order; both order by the deterministic
	// wb_shuffle hash. Browse has to inline its seed as a literal so the identical
	// expression can repeat in SELECT, ORDER BY, and the keyset WHERE, but here
	// the expression appears only in the ORDER BY, so the seed binds as a normal
	// positional arg (after the WHERE args, before LIMIT/OFFSET, matching clause
	// order). Everything else keeps the query's own order, defaulting to the
	// canonical sort_key order.
	switch {
	case c.LimitMode == query.LimitRandom, budget && c.OrderBy == "" && c.LimitSeed != 0:
		seed := c.LimitSeed
		if seed == 0 {
			seed = time.Now().UnixNano() // a fresh draw per evaluation
		}
		sb.WriteString(" ORDER BY wb_shuffle(?, pi.pid), pi.pid")
		args = append(args, seed)
	case c.OrderBy != "":
		sb.WriteString(" ORDER BY ")
		sb.WriteString(c.OrderBy)
		sb.WriteString(", pi.pid")
	default:
		sb.WriteString(" ORDER BY pi.sort_key, pi.pid")
	}
	if budget {
		// A budget mode caps by accumulation below, never by SQL LIMIT; only the
		// offset stays in SQL, skipping rows before any budget accounting.
		if c.Offset > 0 {
			sb.WriteString(" LIMIT -1 OFFSET ?") // SQLite requires a LIMIT before OFFSET
			args = append(args, c.Offset)
		}
	} else {
		if c.Limit > 0 {
			sb.WriteString(" LIMIT ?")
			args = append(args, c.Limit)
		} else if c.Offset > 0 {
			sb.WriteString(" LIMIT -1") // SQLite requires a LIMIT before OFFSET
		}
		if c.Offset > 0 {
			sb.WriteString(" OFFSET ?")
			args = append(args, c.Offset)
		}
	}

	rows, err := s.read.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()

	if budget {
		return s.scanBudgetItems(rows, c, megabytes, op)
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
	if err := s.read.QueryRowContext(ctx, stmt, args...).Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return n, nil
}

// ItemByPID returns a single item view by public id.
func (s *Store) ItemByPID(ctx context.Context, pid model.PID) (*model.ItemView, error) {
	const op = "store.ItemByPID"
	row := s.read.QueryRowContext(ctx, itemSelect+" WHERE pi.pid = ?", string(pid))
	v, err := scanItemView(row)
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
		rows, err := s.read.QueryContext(ctx, itemSelect+" WHERE pi.pid IN "+placeholders(len(chunk)), args...)
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
	f, err := fileByPathDB(ctx, s.read, path)
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
	row := s.read.QueryRowContext(ctx, fileSelect+" WHERE pid = ?", string(pid))
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
	rows, err := s.read.QueryContext(ctx, `SELECT pi.pid, f.codec, f.bitrate, f.sample_rate, f.bit_depth
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
	row := s.read.QueryRowContext(ctx, fileSelect+" WHERE essence_hash = ? LIMIT 1", essence)
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, "store.FileByEssence", "no file with that essence")
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.FileByEssence", err)
	}
	return f, nil
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

// CommitMove updates the file's path columns, marks the journal row
// 'committed', and logs the change in one transaction.
func (s *Store) CommitMove(ctx context.Context, journalPID model.PID, in model.RelocateInput) error {
	const op = "store.CommitMove"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		fileID, err := fileIDByPID(ctx, tx, in.FilePID, op)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE file SET path=?, display_path=?, rel_path=?, last_seen=? WHERE id=?",
			in.NewPath, in.NewDisplayPath, in.NewRelPath, nowNS(), fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE organize_journal SET state='committed' WHERE pid=?", string(journalPID)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "file", in.FilePID, model.OpUpdate)
	})
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

func fileIDByPID(ctx context.Context, tx *sql.Tx, pid model.PID, op string) (int64, error) {
	var fileID int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM file WHERE pid = ?", string(pid)).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such file: "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return fileID, nil
}

// ChangesSince returns change_log rows after seq (capped per call).
func (s *Store) ChangesSince(ctx context.Context, seq int64) ([]model.Change, error) {
	const op = "store.ChangesSince"
	rows, err := s.read.QueryContext(ctx,
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
	return out, rows.Err()
}

// LatestChangeSeq returns the highest change_log seq (0 if empty).
func (s *Store) LatestChangeSeq(ctx context.Context) (int64, error) {
	var seq int64
	if err := s.read.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) FROM change_log").Scan(&seq); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.LatestChangeSeq", err)
	}
	return seq, nil
}

func appendChange(ctx context.Context, tx *sql.Tx, entityType string, pid model.PID, op model.ChangeOp) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO change_log(ts, entity_type, entity_pid, op) VALUES (?,?,?,?)",
		nowNS(), entityType, string(pid), string(op))
	return err
}

func opFor(created bool) model.ChangeOp {
	if created {
		return model.OpCreate
	}
	return model.OpUpdate
}

// pathExists reports whether the file at the given raw path is still present on
// disk. It distinguishes a move (old path gone) from a duplicate copy (old path
// still present) when deciding whether to re-link by essence, and backs
// organize-journal recovery, so a Windows long path must be probed with the
// extended-length prefix or a present file would read as absent (mis-classifying a
// move, or rolling back a completed move during recovery).
func pathExists(path []byte) bool {
	_, err := os.Stat(pathx.Long(string(path)))
	return err == nil
}
