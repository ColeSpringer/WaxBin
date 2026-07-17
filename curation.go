package waxbin

import (
	"context"

	"github.com/colespringer/waxbin/model"
)

// This file exposes the structured-curation edit APIs (lyrics, chapters, artwork) on
// the Library facade. They are catalog-only in this phase: a set records user
// provenance and, by default, locks the artifact so a scan/enrichment pass preserves
// it. Embedding the edit into the on-disk file (an .lrc sidecar, an embedded picture)
// is a later, opt-in write-back concern.

// SetLyrics replaces an item's lyrics with a user-curated set, locking the "lyrics"
// field by default. Passing nil clears the lyrics.
func (l *Library) SetLyrics(ctx context.Context, itemPID model.PID, ly *model.Lyrics, lock, force bool) error {
	return l.store.SetItemLyrics(ctx, itemPID, ly, lock, force)
}

// SetChapters replaces a book's user-curated chapters (which win on read over the
// scanned ones), locking the "chapters" field by default. An empty list clears them.
func (l *Library) SetChapters(ctx context.Context, itemPID model.PID, chapters []model.Chapter, lock, force bool) error {
	return l.store.SetItemChapters(ctx, itemPID, chapters, lock, force)
}

// SetItemArt sets (or, with empty bytes, clears) a track/book item's cover from raw
// image bytes, locking the "art" field by default.
func (l *Library) SetItemArt(ctx context.Context, itemPID model.PID, raw []byte, lock, force bool) error {
	return l.store.SetItemArt(ctx, itemPID, raw, lock, force)
}

// SetEntityArt sets a durable cover on a non-item entity (album, artist, release
// group, genre, or podcast) under the given role (default "front"). This makes album
// art durable: ResolveArt prefers it over the read-derived track cover. Entity art
// takes no lock/force (the lock system is item-scoped).
func (l *Library) SetEntityArt(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, role string, raw []byte) error {
	return l.store.SetEntityArt(ctx, entityType, entityPID, role, raw)
}
