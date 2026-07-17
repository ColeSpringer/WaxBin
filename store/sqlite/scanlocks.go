package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
)

// This file holds the scan-side lock preservation: when the user has locked a field
// (a curated edit), a re-derive-from-disk scan (`scan --force`) must not overwrite
// it. Rather than making every downstream writer (upsertItem/upsertTrack/upsertBook/
// resolveAndLinkEntities/syncItemGenres/resolveContributors) individually
// lock-aware, the scanned model is OVERLAID with the item's current locked-field
// values before those writers run. A locked identity field then re-resolves to the
// same entity (the denormalized column, its FK, and the genre links stay in sync),
// which is the delicate case the writers would otherwise get wrong.
//
// The lookup is gated on PreserveLocks (off only for `scan --force --ignore-locks`)
// and reads the field_provenance_locked partial index, which covers only locked rows.
// An unlocked item therefore costs an empty index probe, and a catalog with no locks
// costs nothing.

// lockedFieldSetTx returns the set of an item's locked fields, draining its cursor
// before returning so the caller can write to the same transaction.
func lockedFieldSetTx(ctx context.Context, tx *sql.Tx, itemID int64) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT field FROM field_provenance WHERE item_id=? AND locked=1", itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out map[string]bool
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[field] = true
	}
	return out, rows.Err()
}

// existingItemIDByIdentityTx resolves an item's rowid by (kind, identity_key),
// returning ok=false when no such item exists yet (a brand-new item has no locks).
func existingItemIDByIdentityTx(ctx context.Context, tx *sql.Tx, kind model.Kind, identityKey string) (int64, bool, error) {
	if identityKey == "" {
		return 0, false, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE kind=? AND identity_key=?", string(kind), identityKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// preserveLockedTrackFieldsTx overlays an existing track item's current locked-field
// values onto the scanned track and item before they are written, so a forced
// rescan cannot clobber a curated edit. It is a no-op for a new item or one with no
// locked fields. It must run before upsertItem (which writes the title).
func preserveLockedTrackFieldsTx(ctx context.Context, tx *sql.Tx, tr *model.Track, item *model.PlayableItem) error {
	id, ok, err := existingItemIDByIdentityTx(ctx, tx, item.Kind, item.IdentityKey)
	if err != nil || !ok {
		return err
	}
	locked, err := lockedFieldSetTx(ctx, tx, id)
	if err != nil || len(locked) == 0 {
		return err
	}
	cur, curTitle, _, err := loadTrackForEditTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if locked["title"] {
		item.Title = curTitle
		item.SortKey = model.SortKey(curTitle)
	}
	if locked["artist"] {
		tr.Artist, tr.ArtistSort = cur.Artist, cur.ArtistSort
	}
	if locked["album_artist"] {
		tr.AlbumArtist = cur.AlbumArtist
	}
	if locked["album"] {
		tr.Album = cur.Album
	}
	// composer is set either as a scalar edit ("composer") or via the credit API
	// ("credit.composer"), which also writes the track.composer denorm. A track rescan
	// re-derives that denorm from disk (item_contributor is untouched, since only books
	// rebuild contributors on scan), so a locked credit.composer must preserve the
	// denorm too, or show and the credit list would diverge.
	if locked["composer"] || locked["credit.composer"] {
		tr.Composer = cur.Composer
	}
	if locked["comment"] {
		tr.Comment = cur.Comment
	}
	if locked["genre"] {
		tr.Genre, tr.Genres = cur.Genre, cur.Genres
	}
	if locked["year"] {
		tr.Year = cur.Year
	}
	if locked["track_no"] {
		tr.TrackNo = cur.TrackNo
	}
	if locked["disc_no"] {
		tr.DiscNo = cur.DiscNo
	}
	if locked["isrc"] {
		tr.ISRC = cur.ISRC
	}
	if locked["mbid"] {
		tr.MBID = cur.MBID
	}
	if locked["compilation"] {
		tr.Compilation = cur.Compilation
	}
	return nil
}

// preserveLockedBookFieldsTx overlays an existing book item's current locked-field
// values onto the scanned book and item before they are written. Author and narrator
// preserve their split lists so upsertBook re-resolves the same contributor entities.
// It must run before upsertItem/upsertBook.
func preserveLockedBookFieldsTx(ctx context.Context, tx *sql.Tx, b *model.Book, item *model.PlayableItem) error {
	id, ok, err := existingItemIDByIdentityTx(ctx, tx, item.Kind, item.IdentityKey)
	if err != nil || !ok {
		return err
	}
	locked, err := lockedFieldSetTx(ctx, tx, id)
	if err != nil || len(locked) == 0 {
		return err
	}
	cur, curTitle, err := loadBookForEditTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if locked["title"] {
		item.Title = curTitle
		item.SortKey = model.SortKey(curTitle)
	}
	// A book rescan of the primary part re-runs upsertBook -> resolveContributors, which
	// DELETEs every item_contributor role and rebuilds from the scanned book's lists. So
	// a locked contributor role must overlay its current list, whether it was locked as
	// a scalar ("author"/"narrator") or via the credit API ("credit.<role>"). Translator
	// and editor have no scalar field, so only the credit lock preserves them; without
	// this they would vanish entirely on a content-changed rescan.
	if locked["author"] || locked["credit.author"] {
		b.Authors, b.Author, b.AuthorSort = cur.Authors, cur.Author, cur.AuthorSort
	}
	if locked["narrator"] || locked["credit.narrator"] {
		b.Narrators, b.Narrator = cur.Narrators, cur.Narrator
	}
	if locked["credit.translator"] {
		b.Translators = cur.Translators
	}
	if locked["credit.editor"] {
		b.Editors = cur.Editors
	}
	if locked["series"] {
		b.Series = cur.Series
	}
	if locked["subtitle"] {
		b.Subtitle = cur.Subtitle
	}
	if locked["genre"] {
		b.Genre, b.Genres = cur.Genre, cur.Genres
	}
	if locked["year"] {
		b.Year = cur.Year
	}
	if locked["publisher"] {
		b.Publisher = cur.Publisher
	}
	if locked["asin"] {
		b.ASIN = cur.ASIN
	}
	if locked["isbn"] {
		b.ISBN = cur.ISBN
	}
	if locked["edition"] {
		b.Edition = cur.Edition
	}
	if locked["description"] {
		b.Description = cur.Description
	}
	if locked["mbid"] {
		b.MBID = cur.MBID
	}
	return nil
}
