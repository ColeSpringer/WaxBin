package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// trackEditFields are the fields editable on a track item.
var trackEditFields = map[string]bool{
	"title": true, "artist": true, "album_artist": true, "album": true,
	"composer": true, "comment": true, "genre": true, "year": true,
	"track_no": true, "disc_no": true,
}

// bookEditFields are the fields editable on a book item.
var bookEditFields = map[string]bool{
	"title": true, "author": true, "narrator": true, "series": true,
	"subtitle": true, "genre": true, "year": true,
}

// editEntityFields are the track fields whose edit re-resolves normalized entities
// and their maintained rollups. genre drives item_genre + the genre rollup, and year
// participates in the album identity key (AlbumKey), so both route through the entity
// path alongside artist/album_artist/album.
var editEntityFields = map[string]bool{
	"artist": true, "album_artist": true, "album": true, "genre": true, "year": true,
}

// editableFieldsForKind returns the editable-field set for an item kind, or nil for
// a kind with no edit path (episodes edit through the podcast subsystem).
func editableFieldsForKind(kind string) map[string]bool {
	switch kind {
	case string(model.KindTrack):
		return trackEditFields
	case string(model.KindBook):
		return bookEditFields
	default:
		return nil
	}
}

// EditItemField edits one metadata field on an item. It is EditItemFields with a
// single-entry map; see there for the full contract.
func (s *Store) EditItemField(ctx context.Context, itemPID model.PID, field, value string, source model.ProvenanceSource, lock, force bool) error {
	return s.EditItemFields(ctx, itemPID, map[string]string{field: value}, source, lock, force)
}

// EditItemFields applies metadata-field edits to a track or book item in one
// transaction. It writes the denormalized subtype columns, re-resolves the affected
// normalized entities (a track's artist, release group, and album, or a book's
// contributors and series) and their rollups, and rebuilds the FTS row. Each edited
// field gets a provenance row recording the source, the curated value, and a null
// provider, and when lock is set the field is locked. One item change delta is
// emitted at the end.
//
// The edit is DB-only. On-disk tags are left alone; the facade's opt-in write-back
// handles those. A locked field is refused with CodeLocked unless force is set. A
// field that does not apply to the item's kind, such as artist on a book or author on
// a track, returns CodeInvalid, and an episode returns CodeUnsupported.
//
// Re-resolution can leave an entity with no items behind it. That ghost keeps a zero
// rollup row instead of being deleted here, which db verify reads as clean (its
// rollup query LEFT JOINs from the entity, so every entity has a row). The standing
// orphan-GC pass removes it later, so the edit needs no in-transaction GC.
func (s *Store) EditItemFields(ctx context.Context, itemPID model.PID, edits map[string]string, source model.ProvenanceSource, lock, force bool) error {
	const op = "store.EditItemFields"
	if len(edits) == 0 {
		return waxerr.New(waxerr.CodeInvalid, op, "no fields to edit")
	}
	if !source.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "invalid provenance source: "+string(source))
	}
	// Validate every field name up front so a bad field rejects the whole edit before
	// any write. Iterate in a stable order so the edit is deterministic regardless of
	// Go's map ordering.
	fields := make([]string, 0, len(edits))
	for f := range edits {
		if !model.IsMetadataField(f) {
			return waxerr.New(waxerr.CodeInvalid, op, "not an editable metadata field: "+f)
		}
		fields = append(fields, f)
	}
	sort.Strings(fields)

	// Trim surrounding whitespace from every value once, up front, so the denormalized
	// column, the resolved entity (whose match key trims), and the recorded provenance
	// all store the same value regardless of how the caller spaced it. This is the
	// storage source of truth for every caller (the CLI and library embedders alike);
	// the facade mirrors it when writing on-disk tags.
	norm := make(map[string]string, len(edits))
	for _, f := range fields {
		norm[f] = strings.TrimSpace(edits[f])
	}

	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		allowed := editableFieldsForKind(kind)
		if allowed == nil {
			return waxerr.New(waxerr.CodeUnsupported, op, "metadata editing is not supported for a "+kind+" item")
		}
		for _, f := range fields {
			if !allowed[f] {
				return waxerr.New(waxerr.CodeInvalid, op, "field "+f+" is not editable on a "+kind+" item")
			}
		}

		// Honor locks unless force: reject the whole edit if any target field is
		// locked, so a partial edit never lands.
		if !force {
			for _, f := range fields {
				locked, err := fieldLockedTx(ctx, tx, itemID, f)
				if err != nil {
					return err
				}
				if locked {
					return waxerr.New(waxerr.CodeLocked, op, "field is locked (use force to override): "+f)
				}
			}
		}

		switch kind {
		case string(model.KindTrack):
			if err := editTrackFieldsTx(ctx, tx, itemID, fields, norm, op); err != nil {
				return err
			}
		case string(model.KindBook):
			if err := editBookFieldsTx(ctx, tx, itemID, fields, norm, op); err != nil {
				return err
			}
		}

		// Record a provenance row for every edited field with the source, the curated
		// value, and a null provider. A user or organize edit has no external provider;
		// enrichment is what fills provider later. This runs in the same transaction as
		// the column writes, so the whole edit commits or rolls back together.
		now := nowNS()
		for _, f := range fields {
			if err := upsertEditProvenanceTx(ctx, tx, itemID, f, source, norm[f], lock, now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
}

// editTrackFieldsTx applies the edits to a track item. It mutates the loaded track,
// updates the title on playable_item, and when an entity field changed re-resolves
// the entities and their rollups and rebuilds the FTS row.
func editTrackFieldsTx(ctx context.Context, tx *sql.Tx, itemID int64, fields []string, edits map[string]string, op string) error {
	tr, title, filePath, err := loadTrackForEditTx(ctx, tx, itemID)
	if err != nil {
		return err
	}

	var touchTitle, touchTrack, touchEntities bool
	newTitle := title
	for _, f := range fields {
		if f == "title" {
			newTitle = edits[f]
			touchTitle = true
			continue
		}
		if err := applyTrackEdit(&tr, f, edits[f], op); err != nil {
			return err
		}
		touchTrack = true
		if editEntityFields[f] {
			touchEntities = true
		}
	}

	// Title first, so the FTS rebuild below (which reads the item's title from
	// playable_item) picks up the new value.
	if touchTitle {
		if _, err := tx.ExecContext(ctx,
			"UPDATE playable_item SET title=?, sort_key=?, updated_at=? WHERE id=?",
			newTitle, model.SortKey(newTitle), nowNS(), itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if touchTrack {
		if err := upsertTrack(ctx, tx, itemID, tr); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}

	switch {
	case touchEntities:
		// Collect the entities the item belongs to now, re-resolve its FKs and genres
		// from the mutated track (this also rebuilds FTS), collect the entities it belongs
		// to after, and recompute the rollups for that combined set only.
		affected := newAffectedRollups()
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := resolveAndLinkEntities(ctx, tx, itemID, tr, filePath); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, nowNS()); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
	case touchTitle:
		// A title-only edit still needs its FTS row rebuilt, since title is the heaviest
		// search field. The entity branch above already does this when it runs.
		if err := syncSearchFTS(ctx, tx, itemID, tr); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return nil
}

// editBookFieldsTx applies the edits to a book item. It mutates the loaded book,
// updates the title on playable_item, and re-upserts the book, which re-resolves the
// contributor artists and series, rebuilds the item genres and FTS row, and refreshes
// the touched entities' rollups. Reusing upsertBook, the scanner's own book writer,
// means a user edit resolves identity the same way a scan does.
func editBookFieldsTx(ctx context.Context, tx *sql.Tx, itemID int64, fields []string, edits map[string]string, op string) error {
	b, title, err := loadBookForEditTx(ctx, tx, itemID)
	if err != nil {
		return err
	}

	var touchTitle, touchBook bool
	newTitle := title
	for _, f := range fields {
		if f == "title" {
			newTitle = edits[f]
			touchTitle = true
			continue
		}
		if err := applyBookEdit(&b, f, edits[f], op); err != nil {
			return err
		}
		touchBook = true
	}

	if touchTitle {
		if _, err := tx.ExecContext(ctx,
			"UPDATE playable_item SET title=?, sort_key=?, updated_at=? WHERE id=?",
			newTitle, model.SortKey(newTitle), nowNS(), itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}

	switch {
	case touchBook:
		// upsertBook re-resolves the series and contributors, adding the touched artists
		// to affected, then rewrites the book row, syncs item genres, and rebuilds the
		// book FTS row from the title updated above. Collecting genres before and after
		// keeps the genre rollup current.
		affected := newAffectedRollups()
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := upsertBook(ctx, tx, itemID, b, affected); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, nowNS()); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
	case touchTitle:
		if err := syncBookSearchFTS(ctx, tx, itemID, b, bookAuthorDisplay(b)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return nil
}

// applyTrackEdit mutates one field of tr in place, parsing the numeric fields. An
// empty numeric value clears the field (nullInt stores 0 as NULL). It never handles
// title, which lives on playable_item and is applied by the caller.
func applyTrackEdit(tr *model.Track, field, value, op string) error {
	switch field {
	case "artist":
		tr.Artist = value
		tr.ArtistSort = model.SortKey(value)
	case "album_artist":
		tr.AlbumArtist = value
	case "album":
		tr.Album = value
	case "composer":
		tr.Composer = value
	case "comment":
		tr.Comment = value
	case "genre":
		tr.Genre = value
		tr.Genres = identity.SplitGenres(value)
	case "year":
		n, err := parseIntField(value, "year", op)
		if err != nil {
			return err
		}
		tr.Year = n
	case "track_no":
		n, err := parseIntField(value, "track_no", op)
		if err != nil {
			return err
		}
		tr.TrackNo = n
	case "disc_no":
		n, err := parseIntField(value, "disc_no", op)
		if err != nil {
			return err
		}
		tr.DiscNo = n
	default:
		// Unreachable: the caller validated field against trackEditFields and split off
		// title, so every remaining case is handled above.
		return waxerr.New(waxerr.CodeInvalid, op, "unhandled track field: "+field)
	}
	return nil
}

// applyBookEdit mutates one field of b in place. An author or narrator value splits
// into contributor entities the same way the scanner splits a credit. Clearing the
// author sort lets upsertBook recompute it from the new author. It never handles
// title, which lives on playable_item and is applied by the caller.
func applyBookEdit(b *model.Book, field, value, op string) error {
	switch field {
	case "author":
		b.Authors = identity.SplitCredits(value)
		b.Author = value
		b.AuthorSort = "" // recomputed from the new author display by upsertBook
	case "narrator":
		b.Narrators = identity.SplitCredits(value)
		b.Narrator = strings.Join(b.Narrators, ", ")
	case "series":
		b.Series = value
	case "subtitle":
		b.Subtitle = value
	case "genre":
		b.Genre = value
		b.Genres = identity.SplitGenres(value)
	case "year":
		n, err := parseIntField(value, "year", op)
		if err != nil {
			return err
		}
		b.Year = n
	default:
		// Unreachable: the caller validated field against bookEditFields and split off
		// title.
		return waxerr.New(waxerr.CodeInvalid, op, "unhandled book field: "+field)
	}
	return nil
}

// bookAuthorDisplay derives a book's author display from its split authors (matching
// upsertBook), falling back to the raw author when no split authors are present.
func bookAuthorDisplay(b model.Book) string {
	if a := strings.Join(b.Authors, ", "); a != "" {
		return a
	}
	return b.Author
}

// parseIntField parses an integer field value, treating an empty string as 0 (a
// clear). A non-numeric value is a usage error.
func parseIntField(value, field, op string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "field "+field+" must be an integer: "+value)
	}
	// year, track_no, and disc_no are all non-negative. A negative value is a usage
	// error rather than a clear. An empty string clears the field, and 0 stores as NULL.
	if n < 0 {
		return 0, waxerr.New(waxerr.CodeInvalid, op, "field "+field+" cannot be negative: "+value)
	}
	return n, nil
}

// loadTrackForEditTx reads a track item's current denormalized columns, title, and
// primary-file path so an edit can change one field and re-resolve. Genres come from
// the item_genre links by display name rather than from re-splitting the joined genre
// column, so an edit that does not touch genre re-applies the same set it read. The
// file path anchors the folder-keyed AlbumKey, which keeps the album identity stable
// across re-resolution. The path is nil when the item has no primary file.
func loadTrackForEditTx(ctx context.Context, tx *sql.Tx, itemID int64) (model.Track, string, []byte, error) {
	tr := model.Track{ItemID: itemID}
	var trackNo, trackTotal, discNo, discTotal, year sql.NullInt64
	var compilation int
	var mbid sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT artist, artist_sort, album, album_artist, composer,
		comment, track_no, track_total, disc_no, disc_total, year, genre, compilation, isrc, mbid
		FROM track WHERE item_id = ?`, itemID).Scan(
		&tr.Artist, &tr.ArtistSort, &tr.Album, &tr.AlbumArtist, &tr.Composer,
		&tr.Comment, &trackNo, &trackTotal, &discNo, &discTotal, &year, &tr.Genre, &compilation, &tr.ISRC, &mbid)
	if errors.Is(err, sql.ErrNoRows) {
		return tr, "", nil, waxerr.New(waxerr.CodeNotFound, "store.EditItemFields", "item has no track row")
	}
	if err != nil {
		return tr, "", nil, waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
	}
	tr.TrackNo, tr.TrackTotal = int(trackNo.Int64), int(trackTotal.Int64)
	tr.DiscNo, tr.DiscTotal = int(discNo.Int64), int(discTotal.Int64)
	tr.Year = int(year.Int64)
	tr.Compilation = compilation != 0
	tr.MBID = mbid.String

	genres, err := currentItemGenresTx(ctx, tx, itemID)
	if err != nil {
		return tr, "", nil, waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
	}
	tr.Genres = genres

	var title string
	if err := tx.QueryRowContext(ctx, "SELECT title FROM playable_item WHERE id = ?", itemID).Scan(&title); err != nil {
		return tr, "", nil, waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
	}

	// The primary file's path anchors the folder-keyed album identity. A missing primary
	// (an archived item) is fine here. Re-resolution then keys the album by folder ".",
	// the same as a fully rootless scan would.
	var path []byte
	err = tx.QueryRowContext(ctx, `SELECT f.path FROM item_file itf JOIN file f ON f.id = itf.file_id
		WHERE itf.item_id = ? AND itf.role = 'primary'`, itemID).Scan(&path)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return tr, "", nil, waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
	}
	return tr, title, path, nil
}

// loadBookForEditTx reads a book item's current columns, contributor role-lists,
// genres, and title, rebuilding the full model.Book so an edit can change one field
// and re-upsert with everything else preserved. Genres and contributors come from
// their link tables rather than being re-derived, so an edit that touches neither
// re-applies the same sets it read.
func loadBookForEditTx(ctx context.Context, tx *sql.Tx, itemID int64) (model.Book, string, error) {
	b := model.Book{ItemID: itemID}
	var seriesID, year, abridged sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT subtitle, author, author_sort, narrator, series_id,
		series_seq, year, publisher, asin, isbn, edition, abridged, description, genre
		FROM book WHERE item_id = ?`, itemID).Scan(
		&b.Subtitle, &b.Author, &b.AuthorSort, &b.Narrator, &seriesID,
		&b.SeriesSeq, &year, &b.Publisher, &b.ASIN, &b.ISBN, &b.Edition, &abridged, &b.Description, &b.Genre)
	if errors.Is(err, sql.ErrNoRows) {
		return b, "", waxerr.New(waxerr.CodeNotFound, "store.EditItemFields", "item has no book row")
	}
	if err != nil {
		return b, "", waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
	}
	b.Year = int(year.Int64)
	if abridged.Valid {
		v := abridged.Int64 != 0
		b.Abridged = &v
	}
	if seriesID.Valid {
		if err := tx.QueryRowContext(ctx, "SELECT name FROM series WHERE id = ?", seriesID.Int64).Scan(&b.Series); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return b, "", waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
		}
	}

	if err := loadBookContributorsTx(ctx, tx, itemID, &b); err != nil {
		return b, "", waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
	}

	genres, err := currentItemGenresTx(ctx, tx, itemID)
	if err != nil {
		return b, "", waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
	}
	b.Genres = genres

	var title string
	if err := tx.QueryRowContext(ctx, "SELECT title FROM playable_item WHERE id = ?", itemID).Scan(&title); err != nil {
		return b, "", waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
	}
	return b, title, nil
}

// loadBookContributorsTx fills a book's Authors/Narrators/Translators/Editors lists
// from the item_contributor relation in credited order, draining and closing its
// cursor before returning so the caller can write to the same transaction.
func loadBookContributorsTx(ctx context.Context, tx *sql.Tx, itemID int64, b *model.Book) error {
	rows, err := tx.QueryContext(ctx, `SELECT a.name, ic.role FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id WHERE ic.item_id = ? ORDER BY ic.role, ic.position`, itemID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, role string
		if err := rows.Scan(&name, &role); err != nil {
			return err
		}
		switch model.ContributorRole(role) {
		case model.RoleAuthor:
			b.Authors = append(b.Authors, name)
		case model.RoleNarrator:
			b.Narrators = append(b.Narrators, name)
		case model.RoleTranslator:
			b.Translators = append(b.Translators, name)
		case model.RoleEditor:
			b.Editors = append(b.Editors, name)
		}
	}
	return rows.Err()
}

// currentItemGenresTx returns an item's genre display names in id order, draining
// and closing its cursor before returning so the caller can safely write to the same
// single-connection transaction afterward.
func currentItemGenresTx(ctx context.Context, tx *sql.Tx, itemID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT g.name FROM item_genre ig
		JOIN genre g ON g.id = ig.genre_id WHERE ig.item_id = ? ORDER BY g.id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// upsertEditProvenanceTx writes an edit's provenance row with the source, the curated
// value, the lock bit, and a null provider. The lock bit follows the caller's choice.
// An edit auto-locks by default, and an unlocked edit still records source=user. The
// insert names the provider column that a later enrichment pass fills, so enrichment
// only populates provider and never has to reshape this statement.
func upsertEditProvenanceTx(ctx context.Context, tx *sql.Tx, itemID int64, field string, source model.ProvenanceSource, value string, lock bool, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, value, provider, updated_at)
		VALUES (?,?,?,?,?,NULL,?)
		ON CONFLICT(item_id, field) DO UPDATE SET
			source=excluded.source, locked=excluded.locked, value=excluded.value,
			provider=excluded.provider, updated_at=excluded.updated_at`,
		itemID, field, string(source), boolInt(lock), value, now)
	return err
}

// itemIDKindByPIDTx resolves an item pid to its rowid and kind inside a transaction.
func itemIDKindByPIDTx(ctx context.Context, tx *sql.Tx, pid model.PID, op string) (int64, string, error) {
	var id int64
	var kind string
	err := tx.QueryRowContext(ctx, "SELECT id, kind FROM playable_item WHERE pid = ?", string(pid)).Scan(&id, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(pid))
	}
	if err != nil {
		return 0, "", waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, kind, nil
}

// FileSharedOrVirtual reports whether a file backs more than one item, or its edge to
// any item carries a start/end offset window. Either case makes the file's on-disk
// tags unsafe to rewrite for one item, since the tags belong to the whole file and a
// per-item edit would clobber the siblings that share it. The facade checks this
// before a tag write-back. A file with no edges (an orphan) is not shared.
func (s *Store) FileSharedOrVirtual(ctx context.Context, filePID model.PID) (bool, error) {
	const op = "store.FileSharedOrVirtual"
	var distinctItems, hasOffsets int
	err := s.read.QueryRowContext(ctx, `SELECT
		COUNT(DISTINCT itf.item_id),
		COALESCE(MAX(CASE WHEN itf.start_ms IS NOT NULL OR itf.end_ms IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM item_file itf JOIN file f ON f.id = itf.file_id
		WHERE f.pid = ?`, string(filePID)).Scan(&distinctItems, &hasOffsets)
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return distinctItems > 1 || hasOffsets == 1, nil
}
