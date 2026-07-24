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

// SetItemLyrics replaces an item's lyrics with a user-curated set (source "user"),
// recording a lock on the "lyrics" field by default so a later scan/enrichment does
// not overwrite it. Passing nil (or empty) lyrics clears the row. A locked lyrics is
// refused with CodeLocked unless force is set.
func (s *Store) SetItemLyrics(ctx context.Context, itemPID model.PID, ly *model.Lyrics, lock, force bool) error {
	const op = "store.SetItemLyrics"
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
		// The user write is authoritative (preserveLock=false); source is stamped "user".
		want := &model.Lyrics{}
		if ly != nil {
			cp := *ly
			cp.Source = string(model.SourceUser)
			want = &cp
		}
		if _, err := putLyricsTx(ctx, tx, itemID, want, false); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := setCurationLockTx(ctx, tx, itemID, "lyrics", lock); err != nil {
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
func (s *Store) SetItemChapters(ctx context.Context, itemPID model.PID, chapters []model.Chapter, lock, force bool) error {
	const op = "store.SetItemChapters"
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
		if err := setCurationLockTx(ctx, tx, itemID, "chapters", lock); err != nil {
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
// item from raw image bytes. An empty role means the front cover. The "art"
// field's lock story applies to the front role only: a lock is recorded (by
// default) and enforced there, because front is the slot a scan re-derives from
// the file/directory and the lock exists to protect against exactly that; the
// other roles have no scan producer to guard against, so the lock and force
// flags are ignored for them. A locked front cover is refused with CodeLocked
// unless force is set. A clear deletes only the named role, leaving the item's
// other slots intact.
func (s *Store) SetItemArt(ctx context.Context, itemPID model.PID, role model.ArtRole, raw []byte, lock, force bool) error {
	const op = "store.SetItemArt"
	if role == "" {
		role = model.ArtRoleFront
	}
	if !role.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown art role: "+string(role))
	}
	var img *model.ArtImage
	if len(raw) > 0 {
		i, err := probeArtImage(raw)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeInvalid, op, err)
		}
		img = i
	}
	front := role == model.ArtRoleFront
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !curatableFieldForKind(kind, "art") {
			return waxerr.New(waxerr.CodeInvalid, op, "art is not editable on a "+kind+" item")
		}
		if front && !force {
			locked, err := fieldLockedTx(ctx, tx, itemID, "art")
			if err != nil {
				return err
			}
			if locked {
				return waxerr.New(waxerr.CodeLocked, op, "art is locked (use force to override)")
			}
		}
		// One path for set and clear: replace this role's mapping (a nil image just
		// deletes it). A cleared role's orphaned source becomes GC-able.
		if err := setEntityArtRoleTx(ctx, tx, "track", itemID, string(role), img); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if front {
			if err := setCurationLockTx(ctx, tx, itemID, "art", lock); err != nil {
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
// deliberate tightening so a typo cannot mint an unreachable slot. Entity art
// takes no lock/force: the lock system is item-scoped (field_provenance), so a
// non-item entity has nothing to lock; enrichment fills entity covers only when
// empty.
func (s *Store) SetEntityArt(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, role model.ArtRole, raw []byte) error {
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
	var img *model.ArtImage
	if len(raw) > 0 {
		i, err := probeArtImage(raw)
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
		if err := setEntityArtRoleTx(ctx, tx, string(entityType), entityID, string(role), img); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// A track/episode entity is a playable_item; emit an item delta for it, else an
		// entity delta so a consumer re-resolves the (now durable) cover.
		if entityType == model.ArtTrack || entityType == model.ArtEpisode {
			return appendChange(ctx, tx, "item", entityPID, model.OpUpdate)
		}
		return appendChange(ctx, tx, string(entityType), entityPID, model.OpUpdate)
	})
}

// setEntityArtRoleTx replaces one (entity, role) art mapping, storing the source and
// leaving the entity's other roles intact. A nil image clears the role.
func setEntityArtRoleTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, role string, img *model.ArtImage) error {
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
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO art_map(entity_type, entity_id, source_hash, role)
		 VALUES (?,?,?,?)`, entityType, entityID, img.Hash, role)
	return err
}

// probeArtImage builds a storable art image from raw bytes: its content hash always,
// and its format/dimensions when the bytes decode (or its magic-sniffed format for an
// exotic AVIF/HEIC cover). Undecodable bytes with no recognizable magic are rejected.
func probeArtImage(raw []byte) (*model.ArtImage, error) {
	img := &model.ArtImage{Data: raw, Hash: art.Hash(raw)}
	format, w, h, err := art.Probe(raw)
	if err != nil {
		if f, ok := art.SniffExotic(raw); ok {
			img.Format = f
			return img, nil
		}
		return nil, errors.New("unrecognized image data")
	}
	img.Format, img.Width, img.Height = format, w, h
	return img, nil
}

// setCurationLockTx upserts the lock bit on a curation field's provenance row
// (source "user"), or drops a pure-lock row when unlocking, keeping the table sparse.
func setCurationLockTx(ctx context.Context, tx *sql.Tx, itemID int64, field string, lock bool) error {
	if lock {
		_, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, updated_at)
			VALUES (?,?,?,1,?)
			ON CONFLICT(item_id, field) DO UPDATE SET source=excluded.source, locked=1, updated_at=excluded.updated_at`,
			itemID, field, string(model.SourceUser), nowNS())
		return err
	}
	_, err := tx.ExecContext(ctx,
		"DELETE FROM field_provenance WHERE item_id=? AND field=? AND (value IS NULL OR value='')", itemID, field)
	return err
}

// artEntityIDTx resolves an art entity's pid to the internal id its art_map rows use:
// the playable_item id for a track/episode, else the row id in the entity's own table.
// The track and episode slots share playable_item, so the row's kind has to match the
// requested slot (itemArtSlotExpr is the read side of the same rule). Without that
// check an episode cover set on a track's pid would store a map row no resolver ever
// consults, and GC would keep it alive because the id is real.
func artEntityIDTx(ctx context.Context, tx *sql.Tx, entityType model.ArtEntity, pid model.PID, op string) (int64, error) {
	var table string
	switch entityType {
	case model.ArtTrack, model.ArtEpisode:
		return itemIDForArtSlotTx(ctx, tx, entityType, pid, op)
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
	err := tx.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE pid=?", string(pid)).Scan(&id)
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
func itemIDForArtSlotTx(ctx context.Context, tx *sql.Tx, slot model.ArtEntity, pid model.PID, op string) (int64, error) {
	var id int64
	var kind string
	err := tx.QueryRowContext(ctx, "SELECT id, kind FROM playable_item WHERE pid=?", string(pid)).Scan(&id, &kind)
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

// attachArtRespectingLockTx maps an item's scanned cover unless the "art" field is
// locked and preserveLock is set (a scan/enrich pass must not overwrite a user cover).
func attachArtRespectingLockTx(ctx context.Context, tx *sql.Tx, itemID int64, img *model.ArtImage, preserveLock bool) (bool, error) {
	if preserveLock && img != nil && len(img.Data) > 0 {
		locked, err := fieldLockedTx(ctx, tx, itemID, "art")
		if err != nil {
			return false, err
		}
		if locked {
			return false, nil
		}
	}
	return attachArtTxChanged(ctx, tx, itemID, img)
}
