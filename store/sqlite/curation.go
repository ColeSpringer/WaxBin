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

// SetItemChapters replaces a single-file book's user-curated chapters (source "user",
// which wins on read over any derived source). Passing an empty list clears the user
// chapters, falling back to the scanned ones. It records a lock on the "chapters"
// field. A user chapter list survives a `scan --force` through the source precedence
// (the scan replace never touches user rows), not the lock. The lock is a curation
// marker for consumers. A locked chapters set is refused with CodeLocked unless force
// is set.
//
// A multi-file book is refused (CodeUnsupported): its chapters span parts with
// per-file offsets, so writing this one flat list to the primary file only would leave
// the other parts on their scanned chapters, a silently half-curated book. Curating a
// multi-file book's chapters needs a per-part API, which is deferred.
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
		if n, err := itemFileCountTx(ctx, tx, itemID); err != nil {
			return err
		} else if n > 1 {
			return waxerr.New(waxerr.CodeUnsupported, op, "user chapters are only supported for single-file books")
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
		fileID, err := primaryFileIDTx(ctx, tx, itemID)
		if err != nil {
			return err
		}
		if _, err := syncChaptersForFileSource(ctx, tx, itemID, fileID, "user", chapters); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// User chapters can extend the effective duration past the file's own length.
		if err := refreshBookDuration(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := setCurationLockTx(ctx, tx, itemID, "chapters", lock); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
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
// group, genre, or podcast) under one role, from raw image bytes. An empty role
// means the front cover. This is what makes album art durable: a set album cover
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

// itemFileCountTx returns how many files back an item (1 for a track or single-file
// book, N for a multi-file book).
func itemFileCountTx(ctx context.Context, tx *sql.Tx, itemID int64) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM item_file WHERE item_id=?", itemID).Scan(&n); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.SetItemChapters", err)
	}
	return n, nil
}

// primaryFileIDTx returns an item's primary backing file id, or CodeNotFound when it
// has none (an archived item).
func primaryFileIDTx(ctx context.Context, tx *sql.Tx, itemID int64) (int64, error) {
	var fileID int64
	err := tx.QueryRowContext(ctx,
		"SELECT file_id FROM item_file WHERE item_id=? AND role='primary'", itemID).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, "store.SetItemChapters", "item has no primary file")
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.SetItemChapters", err)
	}
	return fileID, nil
}

// artEntityIDTx resolves an art entity's pid to the internal id its art_map rows use:
// the playable_item id for a track/episode, else the row id in the entity's own table.
func artEntityIDTx(ctx context.Context, tx *sql.Tx, entityType model.ArtEntity, pid model.PID, op string) (int64, error) {
	var table string
	switch entityType {
	case model.ArtTrack, model.ArtEpisode:
		table = "playable_item"
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
