package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file holds the structured-curation edit APIs: user-set lyrics, chapters, and
// artwork. Each records a lock in the item-scoped field_provenance table (the field
// names "lyrics", "chapters", "art"), so a scan or enrichment pass preserves the
// curated artifact. The scalar edit path never touches these; they have their own
// shapes (structured lyrics, a chapter list, raw image bytes).

// SetItemLyrics replaces an item's lyrics with a curated set, recording the lock the
// caller asked for on the "lyrics" field so a later scan/enrichment does not overwrite
// it. Passing nil (or empty) lyrics clears the row. A locked lyrics is refused with
// CodeLocked unless force is set.
//
// The lyrics carry their own attribution on the *model.Lyrics: an unstamped set is
// recorded as a user edit, and one that names an origin keeps it, so an embedder that
// fetched the words itself is not reported as having typed them. A caller that read the
// lyrics, edited the text, and passes the same struct back is therefore stamping the
// origin it read; clear Source to record a hand edit.
func (s *Store) SetItemLyrics(ctx context.Context, itemPID model.PID, ly *model.Lyrics, lock model.LockChange, force bool) error {
	const op = "store.SetItemLyrics"
	if err := checkLockChange(lock, op); err != nil {
		return err
	}
	// Up front so a caller error is CodeInvalid rather than the CodeIO the transaction
	// wraps putLyricsTx's refusal in.
	if ly.HasContent() {
		attr := model.Attribution{Source: ly.Source, Provider: ly.Provider}.OrUser()
		if err := checkAttribution(attr, attr.ValidForLyrics, op); err != nil {
			return err
		}
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !curatableFieldForKind(kind, "lyrics") {
			return waxerr.New(waxerr.CodeInvalid, op, "lyrics are not editable on a "+kind+" item")
		}
		if !force {
			locked, err := fieldLockedTx(ctx, tx, itemID, "lyrics")
			if err != nil {
				return err
			}
			if locked {
				return waxerr.New(waxerr.CodeLocked, op, "lyrics are locked (use force to override)")
			}
		}
		// The curation write is authoritative (preserveLock=false). An unnamed origin
		// defaults to a user edit; a named one is kept, and putLyricsTx refuses an
		// unpaired or unknown source rather than storing a half-answer.
		want := &model.Lyrics{}
		if ly != nil {
			cp := *ly
			attr := model.Attribution{Source: cp.Source, Provider: cp.Provider}.OrUser()
			cp.Source, cp.Provider = attr.Source, attr.Provider
			want = &cp
		}
		if _, err := putLyricsTx(ctx, tx, itemID, want, false); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := setCurationLockTx(ctx, tx, itemID, "lyrics", lyricsAttribution(want), lock); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
}

// SetItemChapters replaces a book's user-curated chapters (source "user", which
// wins on read over any derived source; on a multi-file book the user rows
// suppress derived chapters on every part). Passing an empty list clears the
// user chapters, falling back to the scanned ones. It records a lock on the
// "chapters" field. A user chapter list survives a `scan --force` through the
// source precedence (the scan replace never touches user rows), not the lock.
// The lock is a curation marker for consumers. A locked chapters set is refused
// with CodeLocked unless force is set.
//
// The input is a flat book-timeline list: StartMS/EndMS are offsets from the
// start of the whole book, exactly what Chapters returns. Starts must be
// strictly increasing and non-negative; an explicit end must be past its start.
// A zero end is open (the read fills it from the next chapter's start, across
// part boundaries, so a spanning chapter round-trips exactly). The list is split
// into per-part rows against the same cumulative part timeline the read builds;
// every part is written, so a re-set clears user rows in parts the new list no
// longer covers. Legacy shape, single-file books only: a list whose
// StartMS/EndMS are all zero with any File* offset set is read as file-relative
// offsets, which mean the same thing there.
func (s *Store) SetItemChapters(ctx context.Context, itemPID model.PID, chapters []model.Chapter, lock model.LockChange, force bool) error {
	const op = "store.SetItemChapters"
	if err := checkLockChange(lock, op); err != nil {
		return err
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !curatableFieldForKind(kind, "chapters") {
			return waxerr.New(waxerr.CodeInvalid, op, "chapters are not editable on a "+kind+" item")
		}
		if !force {
			locked, err := fieldLockedTx(ctx, tx, itemID, "chapters")
			if err != nil {
				return err
			}
			if locked {
				return waxerr.New(waxerr.CodeLocked, op, "chapters are locked (use force to override)")
			}
		}
		parts, err := bookPartsQ(ctx, tx, itemID)
		if err != nil {
			return err
		}
		if len(parts) == 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "item has no backing file")
		}
		perPart, err := splitBookChapters(ctx, tx, itemID, parts, chapters, op)
		if err != nil {
			return err
		}
		// Every part is written, empty slices included: a re-set must clear the
		// user rows of parts the new list leaves uncovered, and a clear loops
		// them all.
		for i := range parts {
			if _, err := syncChaptersForFileSource(ctx, tx, itemID, parts[i].fileID, "user", perPart[i]); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		// User chapters can extend the effective duration past the parts' own length.
		if err := refreshBookDuration(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := setCurationLockTx(ctx, tx, itemID, "chapters", model.Attribution{Source: model.SourceUser}, lock); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
}

// splitBookChapters validates a book-timeline chapter list and splits it into
// one per-part slice (index-aligned with parts), inverting the timeline
// bookChapters builds. Each chapter becomes a single row in the part its start
// falls in; a start at or past the total attaches to the last part (the
// single-file precedent; refreshBookDuration absorbs the extension).
//
// Part boundaries are the cumulative effective durations over the derived
// state: max(file duration, the preferred derived source's furthest chapter
// extent) per part, the same per-part advance bookChapters used for the
// timeline the user read before curating, and keeps using after (the
// derivedFloor there). Using the derived extent also keeps the mapping
// independent of the user rows being replaced. (A file whose embedded chapter
// extents overrun its real duration shifts this timeline; that corrupt-metadata
// edge is accepted, not solved.)
//
// End handling per chapter: a zero end stays open. An end equal to the next
// chapter's start (or, for the last chapter, the book total) is contiguous and
// is stored open too, so the read reconstructs it exactly even when it crosses a
// part boundary; a last-chapter end equal to the total therefore means "runs to
// the end of the book" and follows the total if a rescan changes it. An
// explicit non-contiguous end stores file-relative when it stays inside the
// starting part, and clamps to that part's end when it would cross into the
// next one (a continuation row there would render as a phantom duplicate
// chapter). The last part never clamps, so an end past the book total extends
// the book like the single-file path always has.
func splitBookChapters(ctx context.Context, tx *sql.Tx, itemID int64, parts []bookPart, chapters []model.Chapter, op string) ([][]model.Chapter, error) {
	chs := make([]model.Chapter, len(chapters))
	copy(chs, chapters)

	// Legacy input shape: file-relative offsets with the timeline fields unset.
	// Only a single-file book sniffs for it, since that is the only shape the
	// old API accepted; the two coordinate systems are identical there, so the
	// conversion cannot move anything. A multi-file book reads the timeline
	// fields strictly, so stale File* offsets on a round-tripped chapter can
	// never be mistaken for input.
	if len(parts) == 1 {
		allTimelineZero, anyFileOffset := true, false
		for i := range chs {
			if chs[i].StartMS != 0 || chs[i].EndMS != 0 {
				allTimelineZero = false
			}
			if chs[i].FileStartMS > 0 || chs[i].FileEndMS > 0 {
				anyFileOffset = true
			}
		}
		if allTimelineZero && anyFileOffset {
			for i := range chs {
				chs[i].StartMS, chs[i].EndMS = chs[i].FileStartMS, chs[i].FileEndMS
			}
		}
	}

	prev := int64(-1)
	for i := range chs {
		c := chs[i]
		if c.StartMS < 0 {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "chapter start cannot be negative")
		}
		if c.StartMS <= prev {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "chapter starts must be strictly increasing")
		}
		if c.EndMS != 0 && c.EndMS <= c.StartMS {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "chapter end must be past its start")
		}
		prev = c.StartMS
	}

	extents, err := nonUserChapterExtentsTx(ctx, tx, itemID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// starts[i] is part i's book-timeline offset; total is the timeline length.
	starts := make([]int64, len(parts))
	var total int64
	for i := range parts {
		starts[i] = total
		eff := parts[i].DurationMS
		if ext := extents[parts[i].fileID]; ext > eff {
			eff = ext
		}
		total += eff
	}

	perPart := make([][]model.Chapter, len(parts))
	for i := range chs {
		c := chs[i]
		// The part whose window holds the start: the last one starting at or
		// before it (a start at or past the total lands in the last part).
		p := len(parts) - 1
		for ; p > 0; p-- {
			if starts[p] <= c.StartMS {
				break
			}
		}
		partEnd := total
		if p+1 < len(parts) {
			partEnd = starts[p+1]
		}

		fileStart := c.StartMS - starts[p]
		var fileEnd int64
		contiguous := (i+1 < len(chs) && c.EndMS == chs[i+1].StartMS) ||
			(i+1 == len(chs) && c.EndMS == total)
		switch {
		case c.EndMS == 0 || contiguous:
			fileEnd = 0
		case c.EndMS <= partEnd || p == len(parts)-1:
			fileEnd = c.EndMS - starts[p]
		default:
			fileEnd = partEnd - starts[p]
		}
		perPart[p] = append(perPart[p], model.Chapter{
			Position:    len(perPart[p]),
			Title:       c.Title,
			FileStartMS: fileStart,
			FileEndMS:   fileEnd,
		})
	}
	return perPart, nil
}

// nonUserChapterExtentsTx returns, per backing file, the furthest chapter offset
// of the file's preferred derived (non-user) source. It mirrors bookChapters'
// single-source-per-file choice, not a max across sources: the split must map
// against the exact timeline the read displayed, and when a file briefly carries
// two derived sources only the preferred one's chapters advanced that timeline.
// The cursor closes on return, freeing the tx connection for the caller's writes.
func nonUserChapterExtentsTx(ctx context.Context, tx *sql.Tx, itemID int64) (map[int64]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT file_id, source, MAX(MAX(start_ms, end_ms)) FROM chapter
		 WHERE book_item_id = ? AND source <> 'user' GROUP BY file_id, source`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	extents := map[int64]int64{}
	bestRank := map[int64]int{}
	for rows.Next() {
		var fid, ext int64
		var source string
		if err := rows.Scan(&fid, &source, &ext); err != nil {
			return nil, err
		}
		rank := chapterSourceRank(source)
		if cur, ok := bestRank[fid]; ok && rank >= cur {
			continue
		}
		bestRank[fid] = rank
		extents[fid] = ext
	}
	return extents, rows.Err()
}

// SetItemArt sets (or, with empty bytes, clears) one artwork role on a track/book
// item from raw image bytes, attributed to attr (an unstated origin is a user edit).
// An empty role means the front cover. Every role carries a lock: the front's is the
// item's "art" field, which a scan's re-derive also answers to, and an auxiliary
// role's is its own "art.<role>" field. The caller's lock instruction is recorded on
// whichever of those the role owns, and that same lock is what refuses the next set
// with CodeLocked unless force is given. force skips the check for this one write and
// nothing else: it does not rewrite the lock, so a forced set leaves an existing one
// standing unless the caller asked for a change. A clear deletes only the named role,
// leaving the item's other slots intact, and records the lock the caller asked for, so
// a cleared slot can be held empty.
//
// format is the caller's own name for the picture, used only when the bytes cannot name
// themselves (see probeArtImage). It is ignored on a clear.
func (s *Store) SetItemArt(ctx context.Context, itemPID model.PID, role model.ArtRole, raw []byte, format string, attr model.Attribution, lock model.LockChange, force bool) error {
	const op = "store.SetItemArt"
	if role == "" {
		role = model.ArtRoleFront
	}
	if !role.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown art role: "+string(role))
	}
	if err := checkLockChange(lock, op); err != nil {
		return err
	}
	// Up front, not left to the inner write: a clear carries no image for the art_map
	// writer to check, and the attribution still reaches the lock row. Checking here also
	// keeps a caller error CodeInvalid rather than the CodeIO the transaction wraps it in.
	attr = attr.OrUser()
	if err := checkAttribution(attr, attr.ValidForArt, op); err != nil {
		return err
	}
	var img *model.ArtImage
	if len(raw) > 0 {
		i, err := probeArtImage(raw, format, attr)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeInvalid, op, err)
		}
		img = i
	}
	lockField := artRoleLockField(role)
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !curatableFieldForKind(kind, lockField) {
			return waxerr.New(waxerr.CodeInvalid, op, "art is not editable on a "+kind+" item")
		}
		if !force {
			locked, err := artRoleLockedTx(ctx, tx, model.ArtTrack, itemID, role)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if locked {
				return waxerr.New(waxerr.CodeLocked, op, artLockedMessage(model.ArtTrack, itemPID, role))
			}
		}
		// One path for set and clear: replace this role's mapping (a nil image just
		// deletes it). A cleared role's orphaned source becomes GC-able.
		if err := setEntityArtRoleTx(ctx, tx, "track", itemID, string(role), img); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if lock != model.LockUnchanged {
			if err := setCurationLockTx(ctx, tx, itemID, lockField, attr, lock); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
}

// SetEntityArt sets a durable image on a non-item entity (an album, artist, release
// group, genre, podcast, or playlist) under one role, from raw image bytes. An empty
// role means the front cover. This is what makes album art durable: a set album cover
// is a real art_map row that ResolveArt prefers over the read-derived track cover,
// and that GCArt and the derived-data checks already treat as live. Empty bytes
// clear the role. The role must be in the closed model.ArtRole vocabulary; an
// arbitrary string used to be accepted (and stored) here, and closing it is a
// deliberate tightening so a typo cannot mint an unreachable slot.
//
// The cover is attributed to attr, so an embedder that fetched the picture itself is
// recorded as having fetched it rather than as a hand-set cover; an unstated origin is a
// user edit.
//
// The lock story mirrors SetItemArt's, per role: the caller's lock instruction is
// recorded on the role's own field and that same lock refuses the next set with
// CodeLocked unless force is given, which skips the check for this one write alone and
// leaves the lock as it stands. So a chosen front cover survives an enrich --force and
// a podcast feed's next image-URL change, and a chosen back cover survives the
// enrichment pass that would otherwise fill an empty slot. Locking a cleared role is
// how a user stops a provider filling it, in any role.
//
// A locked front cover does not refuse a hand-set auxiliary image. The whole-entity
// "art" lock is the gate the automatic writers answer to, not a refusal of the user's
// own hand, and it never refused one; narrowing it now would break flows that set
// aux art on an entity whose cover is deliberately pinned.
//
// The lock is recorded in whichever table governs the entity type (artLockIsItemScoped),
// so a track cover set through here and one set through SetItemArt share one lock, and
// the scan, enrichment, and feed-sync guards all read the lock they were written.
//
// format carries the same meaning it does on SetItemArt: the caller's own name for a
// picture the bytes cannot name, ignored on a clear.
func (s *Store) SetEntityArt(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, role model.ArtRole, raw []byte, format string, attr model.Attribution, lock model.LockChange, force bool) error {
	const op = "store.SetEntityArt"
	if !entityType.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(entityType))
	}
	if role == "" {
		role = model.ArtRoleFront
	}
	if !role.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown art role: "+string(role))
	}
	if err := checkLockChange(lock, op); err != nil {
		return err
	}
	// See SetItemArt: the clear path has no image to carry the check, and a caller error
	// belongs outside the transaction that would reclassify it as I/O.
	attr = attr.OrUser()
	if err := checkAttribution(attr, attr.ValidForArt, op); err != nil {
		return err
	}
	var img *model.ArtImage
	if len(raw) > 0 {
		i, err := probeArtImage(raw, format, attr)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeInvalid, op, err)
		}
		img = i
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		entityID, err := artEntityIDTx(ctx, tx, entityType, entityPID, op)
		if err != nil {
			return err
		}
		if !force {
			locked, err := artRoleLockedTx(ctx, tx, entityType, entityID, role)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if locked {
				return waxerr.New(waxerr.CodeLocked, op, artLockedMessage(entityType, entityPID, role))
			}
		}
		if err := setEntityArtRoleTx(ctx, tx, string(entityType), entityID, string(role), img); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if lock != model.LockUnchanged {
			if err := setArtLockTx(ctx, tx, entityType, entityID, artRoleLockField(role), attr, lock); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		// The aux backfill's marker predates any vacancy this write opens on a release
		// group. Clearing an auxiliary slot opens that one role when the write leaves it
		// fillable; the default clear locks the slot behind it, and then nothing opened.
		//
		// A front-role write carrying LockOff clears the marker on the instruction rather
		// than on a transition: that lock is the whole-entity "art" field, so asking for
		// it off frees every role, and this side does not check whether one was standing
		// first. A --no-lock set on an already-unlocked group therefore clears it too,
		// which costs one re-ask. SetArtLock reaches the same rule from the other side,
		// but only past its idempotency return, so it clears on the transition alone.
		if entityType == model.ArtReleaseGroup || entityType == model.ArtArtist {
			whole := artRoleLockField(role) == "art"
			drop := whole && lock == model.LockOff
			// A cleared role opens a vacancy the marker says was already asked about. At
			// the release-group rung only an auxiliary role can, since that predicate
			// never consults the front; the artist one does, so its front counts too, and
			// without this a cleared artist front with the lock left alone is never
			// backfilled again short of a forced run.
			if img == nil && (!whole || entityType == model.ArtArtist) {
				blocked, err := artFillBlockedTx(ctx, tx, entityType, entityID, role)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				drop = drop || !blocked
			}
			if drop {
				if err := deleteArtBackfillMarkerTx(ctx, tx, entityType, entityID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
		}
		// A track/episode entity is a playable_item; emit an item delta for it, else an
		// entity delta so a consumer re-resolves the (now durable) cover.
		if entityType == model.ArtTrack || entityType == model.ArtEpisode {
			return appendChange(ctx, tx, "item", entityPID, model.OpUpdate)
		}
		return appendChange(ctx, tx, string(entityType), entityPID, model.OpUpdate)
	})
}

// ArtLocked reports whether an art entity's slot in one role is pinned against the
// automatic writers: the effective lock, the entity's whole "art" field or the role's
// own "art.<role>" one, which is the same lock ArtRoles reports for that slot. An empty
// role means the front cover, where the two are one field. It is the read a caller needs
// to explain a cover that resolves to nothing, and on an entity with no art at all the
// lock is otherwise only visible through ArtRoles.
//
// A user's own art set is not gated on this. SetEntityArt and SetItemArt still consult
// the role's own lock alone, so a locked reading here does not always mean the next
// hand-set image in that role will be refused.
func (s *Store) ArtLocked(ctx context.Context, entityType model.ArtEntity, pid model.PID, role model.ArtRole) (bool, error) {
	const op = "store.ArtLocked"
	if !entityType.Valid() {
		return false, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(entityType))
	}
	if role == "" {
		role = model.ArtRoleFront
	}
	if !role.Valid() {
		return false, waxerr.New(waxerr.CodeInvalid, op, "unknown art role: "+string(role))
	}
	entityID, err := artEntityIDTx(ctx, s.read, entityType, pid, op)
	if err != nil {
		return false, err
	}
	locked, err := artFillBlockedTx(ctx, s.read, entityType, entityID, role)
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return locked, nil
}

// SetArtLock sets or clears an art entity's lock in one role without touching the
// image itself, which is the one thing SetEntityArt cannot express: that one always
// writes the slot too, so unlocking through it means re-supplying the image.
// Unlocking is the way out of a cleared-and-locked slot, the state that refuses
// every later `art set` in that role.
//
// An empty role means the front cover, whose lock is the plain "art" field and also
// the whole-entity gate on enrichment's fills; every other role locks its own slot
// alone. Callers at an input boundary refuse an explicitly spelled "front" and point
// at the plain form, so there is one way to say it.
//
// It inherits SetEntityArt's behavior exactly otherwise: the lock lands in whichever
// table governs the entity type (artLockIsItemScoped), so a track's shares one home
// with `lock <pid> art`, and the change delta has the same shape. Like SetEntityArt,
// and unlike SetItemArt, it does not run the curatableFieldForKind check; that is
// deliberate, not an oversight, since the art lock is keyed by art entity type rather
// than by item kind.
func (s *Store) SetArtLock(ctx context.Context, entityType model.ArtEntity, pid model.PID, role model.ArtRole, lock bool) error {
	const op = "store.SetArtLock"
	if !entityType.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(entityType))
	}
	if role == "" {
		role = model.ArtRoleFront
	}
	if !role.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown art role: "+string(role))
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		entityID, err := artEntityIDTx(ctx, tx, entityType, pid, op)
		if err != nil {
			return err
		}
		// Idempotent, the way LockField/UnlockField already are: unlocking an entity
		// that was never locked writes nothing, so publishing an update delta for it
		// would send every ChangesSince tailer to re-fetch for no change.
		cur, err := artRoleLockedTx(ctx, tx, entityType, entityID, role)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if cur == lock {
			return nil
		}
		// The lock is the write here, not an instruction accompanying one, so it carries the
		// only attribution there is: a user asking for it.
		if err := setArtLockTx(ctx, tx, entityType, entityID, artRoleLockField(role),
			model.Attribution{Source: model.SourceUser}, model.LockOf(lock)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// The aux backfill's marker records that this group's vacancies were asked about
		// as of then, and an unlock changes that picture. Releasing the whole "art" lock
		// clears it outright, since any role may now be fillable and over-clearing costs
		// one re-ask; releasing a single role clears it only when that role really ended
		// up open, which a standing whole lock prevents.
		if !lock && (entityType == model.ArtReleaseGroup || entityType == model.ArtArtist) {
			drop := artRoleLockField(role) == "art"
			if !drop {
				blocked, err := artFillBlockedTx(ctx, tx, entityType, entityID, role)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				drop = !blocked
			}
			if drop {
				if err := deleteArtBackfillMarkerTx(ctx, tx, entityType, entityID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
		}
		if entityType == model.ArtTrack || entityType == model.ArtEpisode {
			return appendChange(ctx, tx, "item", pid, model.OpUpdate)
		}
		return appendChange(ctx, tx, string(entityType), pid, model.OpUpdate)
	})
}

// setEntityArtRoleTx replaces one (entity, role) art mapping, storing the source, its
// provenance, and leaving the entity's other roles intact. A nil image clears the role.
// It is where an unstorable image is refused rather than stored, and where an
// undescribed one is completed from its own bytes. It replaces the row unconditionally,
// so a deliberate set always re-attributes; the automatic ingests come through
// attachEntityArtTxChanged, which re-points only on a differing hash and re-attributes
// in place otherwise. Those two are the only writers of an art_map row's attribution,
// and both run it through checkArtImage first.
func setEntityArtRoleTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, role string, img *model.ArtImage) error {
	img = storableArt(img)
	if err := checkArtImage(img, entityType, role); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM art_map WHERE entity_type=? AND entity_id=? AND role=?", entityType, entityID, role); err != nil {
		return err
	}
	if img == nil {
		return nil
	}
	if err := insertArtSourceTx(ctx, tx, img); err != nil {
		return err
	}
	// A plain INSERT, not OR IGNORE: the DELETE above already removed the only primary
	// key this can collide with, so the IGNORE has nothing left to absorb except a
	// genuine failure (a missing art_source under foreign_keys=ON, a rejected value).
	// Swallowing one of those would delete the old mapping and store no new one, and
	// report success.
	_, err := tx.ExecContext(ctx,
		`INSERT INTO art_map(entity_type, entity_id, source_hash, role, source, provider, source_url, updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		entityType, entityID, img.Hash, role, string(img.Source), img.Provider, img.SourceURL, nowNS())
	return err
}

// checkArtImage refuses an image an art_map row cannot hold: an attribution outside the
// column's vocabulary or without its pairing, and a content address the store could not
// derive. A nil image is a clear and passes. The address branch is unreachable while
// storableArt hashes every carrier with bytes, and stands as the guard against a future
// path that hands an image over some other way.
func checkArtImage(img *model.ArtImage, entityType, role string) error {
	if img == nil {
		return nil
	}
	const op = "store.setEntityArtRole"
	if err := checkAttribution(img.Attribution, img.Attribution.ValidForArt, op); err != nil {
		return err
	}
	if img.Hash == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "art image has no content address: "+entityType+" "+role)
	}
	return nil
}

// probeArtImage builds a storable art image from raw bytes: its content hash always,
// and its format/dimensions when the bytes decode (or its magic-sniffed format for an
// exotic AVIF/HEIC cover), falling back to the format the caller named for a picture
// nothing here recognizes. The fallback never overrides a decoded format: naming png
// over JPEG bytes stores jpeg. The refusal narrows to its true meaning, that neither
// the caller nor the bytes could name this picture at all, which is still more likely
// the wrong file than an exotic one.
//
// format is normalized here rather than taken on trust. NormalizeFormat is idempotent,
// so the Library facade normalizing first costs this nothing, and a precondition no
// caller could be held to is worse than a second call.
func probeArtImage(raw []byte, format string, attr model.Attribution) (*model.ArtImage, error) {
	info := art.Describe(raw)
	if info.Format == "" {
		info.Format = art.NormalizeFormat(format)
	}
	if info.Format == "" {
		return nil, errors.New("unrecognized image data")
	}
	return &model.ArtImage{
		Data: raw, Hash: info.Hash, Format: info.Format, Width: info.Width, Height: info.Height,
		Attribution: attr,
	}, nil
}

// setCurationLockTx upserts the lock bit on a curation field's provenance row, carrying
// the attribution of the write that asked for the lock, or drops a pure-lock row when
// unlocking, keeping the table sparse. LockUnchanged writes nothing at all, so a write
// that formed no lock intent leaves the stored one exactly as it stands.
//
// The row is not inert: with no artifact attached, ArtRoles reports the "art" row's
// source as the front cover's, so a lock recorded here under an invented source would
// be reported as fact.
func setCurationLockTx(ctx context.Context, tx *sql.Tx, itemID int64, field string, attr model.Attribution, lock model.LockChange) error {
	switch lock {
	case model.LockUnchanged:
		return nil
	case model.LockOn:
		_, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, provider, locked, updated_at)
			VALUES (?,?,?,?,1,?)
			ON CONFLICT(item_id, field) DO UPDATE SET
				source=excluded.source, provider=excluded.provider, locked=1, updated_at=excluded.updated_at`,
			itemID, field, string(attr.Source), nullStr(attr.Provider), nowNS())
		return err
	}
	_, err := tx.ExecContext(ctx,
		"DELETE FROM field_provenance WHERE item_id=? AND field=? AND (value IS NULL OR value='')", itemID, field)
	return err
}

// lyricsAttribution is the attribution a lyrics set records on its lock row: the words'
// own origin, so a locked lyric with no row of its own still reports where it came from.
func lyricsAttribution(ly *model.Lyrics) model.Attribution {
	if !ly.HasContent() {
		return model.Attribution{Source: model.SourceUser}
	}
	return model.Attribution{Source: ly.Source, Provider: ly.Provider}
}

// artLockIsItemScoped reports whether an art entity's art locks live in the
// item-scoped field_provenance table rather than in entity_curation. It governs the
// plain "art" field and every "art.<role>" alike, so a role's lock is never split from
// the front lock beside it. It exists because the lock has to sit where the writer that
// must respect it already looks, and the two art writers look in different places: a
// track or book item's cover is re-derived by the scan through
// attachArtRespectingLockTx, which reads field_provenance, while every other entity's
// cover is written by enrichment or a feed sync, which read entity_curation.
//
// An episode is deliberately not item-scoped even though it is a playable_item: "art"
// is not a curatable field for the episode kind (staticCurationFieldKinds), so a
// field_provenance row for it would be the junk row that whitelist exists to prevent.
// Its lock lives in entity_curation and AttachEpisodeFile consults it there.
func artLockIsItemScoped(entityType model.ArtEntity) bool {
	return entityType == model.ArtTrack
}

// artRoleLockField names the curation field carrying one art role's lock. The front
// cover has no field of its own: its lock is the plain "art" field and nothing else,
// which is also the whole-entity gate on enrichment's fills, so there is one lock and
// one home for it (model.CutArtRolePrefix says why an "art.front" spelling would
// mislead). Every other role gets its own "art.<role>" row, the same namespacing
// credit.<role> and tag.<KEY> use. An empty role means front, matching
// model.ParseArtRole.
func artRoleLockField(role model.ArtRole) string {
	if role == "" || role == model.ArtRoleFront {
		return "art"
	}
	return "art." + string(role)
}

// artLock is one art curation lock row, from whichever table governs the entity. The
// attribution and UpdatedAt matter when the lock has no artifact behind it, since they
// are then the only attribution ArtRoles has to report. A missing row reads as the zero
// value, which is an unlocked slot.
type artLock struct {
	Locked bool
	model.Attribution
	UpdatedAt int64
}

// artLockTx reads one art curation field from whichever table governs the entity type
// (artLockIsItemScoped). It is the single art-lock reader every gate goes through, so a
// track's lock cannot be written to one table and read from the other. It reads the
// provider alongside the source, because a lock row can carry the attribution of the
// write that created it and reporting the source without it would reintroduce the
// half-answer on exactly the read that makes a coverless lock visible.
func artLockTx(ctx context.Context, q queryer, entityType model.ArtEntity, entityID int64, field string) (artLock, error) {
	var (
		out    artLock
		locked int
		source string
		err    error
	)
	if artLockIsItemScoped(entityType) {
		err = q.QueryRowContext(ctx,
			"SELECT locked, source, COALESCE(provider,''), updated_at FROM field_provenance WHERE item_id=? AND field=?",
			entityID, field).Scan(&locked, &source, &out.Provider, &out.UpdatedAt)
	} else {
		err = q.QueryRowContext(ctx,
			"SELECT locked, source, COALESCE(provider,''), updated_at FROM entity_curation WHERE entity_type=? AND entity_id=? AND field=?",
			string(entityType), entityID, field).Scan(&locked, &source, &out.Provider, &out.UpdatedAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return artLock{}, nil
	}
	if err != nil {
		return artLock{}, err
	}
	out.Locked, out.Source = locked == 1, model.ProvenanceSource(source)
	return out, nil
}

// artRoleLockedTx reports whether one role's own lock stands. It is the gate a user's
// deliberate `art set` consults: the front role reads the plain "art" lock, an
// auxiliary role reads its own "art.<role>" row and nothing else. The whole-entity
// lock is not folded in here on purpose, since it never refused a hand-set aux image
// and starting now would break flows that rely on it.
func artRoleLockedTx(ctx context.Context, q queryer, entityType model.ArtEntity, entityID int64, role model.ArtRole) (bool, error) {
	lock, err := artLockTx(ctx, q, entityType, entityID, artRoleLockField(role))
	return lock.Locked, err
}

// artFillBlockedTx reports whether an automatic writer must leave one role alone: a
// scan re-deriving a front cover, an enrichment pass, a podcast feed sync. Those widen
// where a user's own hand does not, so they answer to both locks, the whole-entity
// "art" and the role's own "art.<role>". For the front role the two are the same field
// and this is one read.
//
// fillEntityAuxArtTx applies the same rule over a batch of roles instead of calling
// this per role, so that it can read the whole-entity lock once and skip the batch
// outright when it stands. ArtLocked reads through here too: it reports what the
// automatic writers will do rather than what a hand set will meet.
func artFillBlockedTx(ctx context.Context, q queryer, entityType model.ArtEntity, entityID int64, role model.ArtRole) (bool, error) {
	whole, err := artLockTx(ctx, q, entityType, entityID, "art")
	if err != nil || whole.Locked {
		return whole.Locked, err
	}
	if artRoleLockField(role) == "art" {
		return false, nil
	}
	return artRoleLockedTx(ctx, q, entityType, entityID, role)
}

// artLockedMessage is the refusal a locked art slot gets from both set paths. It names
// `art unlock` rather than only --force: releasing the lock is the fix when the slot
// was cleared and locked, and --force alone would overwrite under a lock the user may
// still want. The --type is spelled out because it defaults to track, and a non-front
// role names itself because its lock is its own.
func artLockedMessage(entityType model.ArtEntity, pid model.PID, role model.ArtRole) string {
	msg := "art is locked: waxbin art unlock " + string(pid) + " --type " + string(entityType)
	if artRoleLockField(role) != "art" {
		msg += " --role " + string(role)
	}
	return msg + " to release it, or --force to override this one write"
}

// setArtLockTx records one art curation lock in whichever table governs the entity
// type, dropping a pure-lock row on unlock so both tables stay sparse and writing
// nothing at all for LockUnchanged.
func setArtLockTx(ctx context.Context, tx *sql.Tx, entityType model.ArtEntity, entityID int64, field string, attr model.Attribution, lock model.LockChange) error {
	if artLockIsItemScoped(entityType) {
		return setCurationLockTx(ctx, tx, entityID, field, attr, lock)
	}
	return setEntityCurationLockTx(ctx, tx, string(entityType), entityID, field, attr, lock)
}

// artEntityIDTx resolves an art entity's pid to the internal id its art_map rows use:
// the playable_item id for a track/episode, else the row id in the entity's own table.
// The track and episode slots share playable_item, so the row's kind has to match the
// requested slot (itemArtSlotExpr is the read side of the same rule). Without that
// check an episode cover set on a track's pid would store a map row no resolver ever
// consults, and GC would keep it alive because the id is real. It takes a queryer so
// the ArtLocked read shares one resolver with the writers.
func artEntityIDTx(ctx context.Context, q queryer, entityType model.ArtEntity, pid model.PID, op string) (int64, error) {
	var table string
	switch entityType {
	case model.ArtTrack, model.ArtEpisode:
		return itemIDForArtSlotTx(ctx, q, entityType, pid, op)
	case model.ArtAlbum:
		table = "album"
	case model.ArtReleaseGroup:
		table = "release_group"
	case model.ArtArtist:
		table = "artist"
	case model.ArtGenre:
		table = "genre"
	case model.ArtPodcast:
		table = "podcast"
	case model.ArtPlaylist:
		table = "playlist"
	default:
		return 0, waxerr.New(waxerr.CodeInvalid, op, "unsupported art entity type: "+string(entityType))
	}
	var id int64
	err := q.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE pid=?", string(pid)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such "+string(entityType)+": "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, nil
}

// itemIDForArtSlotTx resolves a playable item's pid for the track or episode art slot,
// rejecting a pid whose kind belongs to the other slot. An episode's cover lives under
// the episode slot and a track's or book's under the track slot, so a mismatch would
// write art nothing reads back.
func itemIDForArtSlotTx(ctx context.Context, q queryer, slot model.ArtEntity, pid model.PID, op string) (int64, error) {
	var id int64
	var kind string
	err := q.QueryRowContext(ctx, "SELECT id, kind FROM playable_item WHERE pid=?", string(pid)).Scan(&id, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such "+string(slot)+": "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	isEpisode := model.Kind(kind) == model.KindEpisode
	if isEpisode != (slot == model.ArtEpisode) {
		return 0, waxerr.New(waxerr.CodeInvalid, op,
			"item "+string(pid)+" is a "+kind+", which does not carry "+string(slot)+" art")
	}
	return id, nil
}

// attachArtRespectingLockTx maps an item's scanned front cover unless the "art" field
// is locked and preserveLock is set (a scan/enrich pass must not overwrite a user
// cover). It goes through artFillBlockedTx, the one gate every automatic art writer
// shares, so a track's lock is read from the table it is written to.
func attachArtRespectingLockTx(ctx context.Context, tx *sql.Tx, itemID int64, img *model.ArtImage, preserveLock bool) (bool, error) {
	if preserveLock && img != nil && len(img.Data) > 0 {
		locked, err := artFillBlockedTx(ctx, tx, model.ArtTrack, itemID, model.ArtRoleFront)
		if err != nil {
			return false, err
		}
		if locked {
			return false, nil
		}
	}
	return attachArtTxChanged(ctx, tx, itemID, img)
}
