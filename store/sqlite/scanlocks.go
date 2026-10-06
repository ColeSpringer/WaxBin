package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/identity"
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

// lockedFieldSetTx returns the set of an item's locked fields, a credit's twin included
// (withTwinLocks), draining its cursor before returning so the caller can write to the
// same transaction.
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
	withTwinLocks(out)
	return out, rows.Err()
}

// withTwinLocks marks the twin of each locked spelling locked too: a credit and the field
// it fills share one column (owedTwins), so a lock on either holds both.
func withTwinLocks(locked map[string]bool) {
	for f := range locked {
		if twin, ok := owedTwins[f]; ok {
			locked[twin] = true
		}
	}
}

// lockSpellings returns the spellings whose locks hold field: itself, and its twin when
// it has one.
func lockSpellings(field string) []string {
	if twin, ok := owedTwins[field]; ok {
		return []string{field, twin}
	}
	return []string{field}
}

// existingItemIDByIdentityTx resolves an item's rowid by (kind, identity_key),
// returning ok=false when no such item exists yet (a brand-new item has no locks).
//
// A book falls through to the same identifier adoption upsertItem uses, and has to: this
// runs first, so without it the part about to be adopted misses its lock overlay and a
// forced rescan clobbers the curated fields of the book it is joining.
func existingItemIDByIdentityTx(ctx context.Context, tx *sql.Tx, log logger, kind model.Kind, identityKey string, adopt bookAdoptKey) (int64, bool, error) {
	if identityKey == "" {
		return 0, false, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM playable_item WHERE kind=? AND identity_key=?", string(kind), identityKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) && kind == model.KindBook {
		adopted, aerr := adoptBookItemByIdentTx(ctx, tx, log, identityKey, adopt)
		if aerr != nil {
			return 0, false, aerr
		}
		if adopted != 0 {
			return adopted, true, nil
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// storedRow is one provenance row a scan overlay reads: where the value came from,
// whether it is locked, the value it records (when it records one), and when it was
// written.
type storedRow struct {
	source    string
	locked    bool
	value     sql.NullString
	updatedAt int64
}

// storedRowsTx reads an item's provenance rows by field.
func storedRowsTx(ctx context.Context, tx *sql.Tx, itemID int64) (map[string]storedRow, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT field, source, locked, value, updated_at FROM field_provenance WHERE item_id = ?", itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]storedRow{}
	for rows.Next() {
		var f string
		var r storedRow
		if err := rows.Scan(&f, &r.source, &r.locked, &r.value, &r.updatedAt); err != nil {
			return nil, err
		}
		out[f] = r
	}
	return out, rows.Err()
}

// scanPrior is what a scan overlay read about the existing item a put lands on: its
// provenance rows and, when the overlay loaded it, its stored track or book. The
// re-derive after upsertItem reuses it for that item rather than reading the same rows
// again, which nothing between the two rewrites.
type scanPrior struct {
	itemID int64
	rows   map[string]storedRow
	track  *model.Track
	book   *model.Book
}

// priorFor returns p when it describes itemID, else nil.
func (p *scanPrior) priorFor(itemID int64) *scanPrior {
	if p == nil || p.itemID != itemID {
		return nil
	}
	return p
}

// rederivable filters the rows the way rederivableRowsTx selects them.
func (p *scanPrior) rederivable(preserveLocks bool) map[string]rederivable {
	out := map[string]rederivable{}
	for f, r := range p.rows {
		if r.source == string(model.SourceTag) || !r.value.Valid || strings.HasPrefix(f, "tag.") || (r.locked && preserveLocks) {
			continue
		}
		out[f] = rederivable{value: r.value.String, source: r.source}
	}
	return out
}

// fillSource reports whether a scan keeps a value from this source while the file says
// nothing for the field. What enrichment filled, and what a normalize pass respelled, are
// values the file never carried, so a file silent on the field holds nothing against them.
func fillSource(source string) bool {
	return source == string(model.SourceEnrichment) || source == string(model.SourceNormalize)
}

// partitionStoredRows splits an item's provenance rows into the locked fields a scan
// honours and the fields a scan carries over a silent file, which are the ones a fill
// source wrote and no lock holds.
func partitionStoredRows(rows map[string]storedRow, preserveLocks bool) (locked map[string]bool, fills []string) {
	locked = map[string]bool{}
	for f, r := range rows {
		if r.locked && preserveLocks {
			locked[f] = true
		} else if fillSource(r.source) {
			fills = append(fills, f)
		}
	}
	return locked, fills
}

// trackColumnOf maps a provenance field onto the track column it describes: a credit
// writes the same denormalized column as its scalar twin.
func trackColumnOf(field string) string {
	switch field {
	case model.CreditField(model.RoleArtist):
		return "artist"
	case model.CreditField(model.RoleComposer):
		return "composer"
	}
	return field
}

// overlayStoredTrackTx carries onto the scanned track what a scan must not re-derive from
// the file, before anything is written: the locked fields when the scan honours locks, so
// a curated edit survives `scan --force`, and what enrichment filled or a normalize pass
// respelled wherever the file states nothing for the field. A fill holds nothing against
// the file, so it is carried on every scan, --ignore-locks included, and the file's own
// value replaces it. derived names the fields the file's tags do not state
// (PutScannedTrackInput.Derived). A nonzero fileID reopens the enrichment write-back for a
// fill a settled write put on that file and the file no longer carries. It must run
// before upsertItem, which writes the title. It returns what it read of the existing item,
// nil for a new one. A non-nil known is the caller's resolution of the item's key, used in
// place of resolving it again.
func overlayStoredTrackTx(ctx context.Context, tx *sql.Tx, log logger, fileID int64, tr *model.Track, item *model.PlayableItem, derived []string, preserveLocks bool, known *resolvedItem) (*scanPrior, error) {
	if known == nil {
		var err error
		if known, err = resolveItemTx(ctx, tx, log, item.Kind, item.IdentityKey, bookAdoptKey{}); err != nil {
			return nil, err
		}
	}
	id := known.id
	if id == 0 {
		return nil, nil
	}
	rows, err := storedRowsTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	prior := &scanPrior{itemID: id, rows: rows}
	locked, fills := partitionStoredRows(rows, preserveLocks)
	if len(locked) == 0 && len(fills) == 0 {
		return prior, nil
	}
	cur, curTitle, _, err := loadTrackForEditTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	prior.track = &cur

	if locked["title"] {
		item.Title, item.SortKey = curTitle, model.SortKey(curTitle)
	}
	for f := range locked {
		copyTrackField(tr, cur, trackColumnOf(f))
	}
	// A locked number past the total the file states would read back as "7 of 1", so
	// that total is cleared the way the renumbering edit cleared it (clearStaleTotalsTx).
	if locked["track_no"] && !locked["track_total"] && staleTotal(tr.TrackNo, tr.TrackTotal) {
		tr.TrackTotal = 0
	}
	if locked["disc_no"] && !locked["disc_total"] && staleTotal(tr.DiscNo, tr.DiscTotal) {
		tr.DiscTotal = 0
	}

	// A year or genre filled for an album belongs to that album, so a file retagged onto
	// another one leaves them behind.
	sameAlbum := sameAlbumTags(*tr, cur)
	var carried []string
	for _, f := range fills {
		col := trackColumnOf(f)
		if !trackFillHolds(col, rows, curTitle, cur) || !trackFieldSilent(col, *tr, derived) {
			continue
		}
		if (col == "year" || col == "genre") && !sameAlbum {
			continue
		}
		if col == "title" {
			item.Title, item.SortKey = curTitle, model.SortKey(curTitle)
		} else {
			copyTrackField(tr, cur, col)
		}
		carried = append(carried, f)
	}
	// A total kept beside a number the file now states past it would read "12 of 10".
	if slices.Contains(carried, "track_total") && staleTotal(tr.TrackNo, tr.TrackTotal) {
		tr.TrackTotal = 0
	}
	if slices.Contains(carried, "disc_total") && staleTotal(tr.DiscNo, tr.DiscTotal) {
		tr.DiscTotal = 0
	}
	return prior, reopenLostFillsTx(ctx, tx, fileID, carried, rows)
}

// fillRowsHold reports whether the fill rows behind a column still describe what it
// holds: no row on any of its fields comes from another source (a later edit owns it), and
// a row recording a value reproduces the stored column.
func fillRowsHold(fields []string, rows map[string]storedRow, reproduces func(field, value string) bool) bool {
	for _, f := range fields {
		r, ok := rows[f]
		if !ok {
			continue
		}
		if !fillSource(r.source) {
			return false
		}
		if r.value.Valid && !reproduces(f, r.value.String) {
			return false
		}
	}
	return true
}

// trackFillHolds is fillRowsHold for a track column, the credit row behind the artist or
// composer display included.
func trackFillHolds(col string, rows map[string]storedRow, curTitle string, cur model.Track) bool {
	fields := []string{col}
	switch col {
	case "artist":
		fields = append(fields, model.CreditField(model.RoleArtist))
	case "composer":
		fields = append(fields, model.CreditField(model.RoleComposer))
	}
	return fillRowsHold(fields, rows, func(f, value string) bool {
		_, ok := rowReproduces(f, value, curTitle, cur)
		return ok
	})
}

// trackFieldSilent reports whether a scanned file states nothing for a column: the value
// is empty for its type, or a display fallback guessed it rather than the tags stating it.
func trackFieldSilent(col string, tr model.Track, derived []string) bool {
	if slices.Contains(derived, col) {
		return true
	}
	switch col {
	case "title":
		return false
	case "artist":
		return tr.Artist == "" && len(tr.Artists) == 0
	case "genre":
		return tr.Genre == "" && len(tr.Genres) == 0
	case "year", "track_no", "track_total", "disc_no", "disc_total", "bpm":
		return scanFieldValue(col, "", tr) == "0"
	case "compilation":
		return !tr.Compilation
	}
	return scanFieldValue(col, "", tr) == ""
}

// sameAlbumTags reports whether a scanned track names the album its stored columns do,
// by the album title and album artist, or the artist when neither has an album artist.
func sameAlbumTags(tr, cur model.Track) bool {
	if identity.MatchKey(tr.Album) != identity.MatchKey(cur.Album) {
		return false
	}
	if tr.AlbumArtist != "" || cur.AlbumArtist != "" {
		return identity.MatchKey(tr.AlbumArtist) == identity.MatchKey(cur.AlbumArtist)
	}
	return identity.MatchKey(tr.Artist) == identity.MatchKey(cur.Artist)
}

// reopenLostFillsTx owes a file the enrichment write again when a scan carried a fill
// over it that a settled write had put there: the file has lost the value. A file whose
// write reported a value it could not store is left settled, since a retry would only
// lose it again.
func reopenLostFillsTx(ctx context.Context, tx *sql.Tx, fileID int64, carried []string, rows map[string]storedRow) error {
	if fileID == 0 {
		return nil
	}
	var oldest int64
	for _, f := range carried {
		r := rows[f]
		if r.source != string(model.SourceEnrichment) || r.locked {
			continue
		}
		if oldest == 0 || r.updatedAt < oldest {
			oldest = r.updatedAt
		}
	}
	if oldest == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE file SET enrich_settled_at = 0
		WHERE id = ? AND enrich_settled_at >= ?
		  AND NOT EXISTS (SELECT 1 FROM file_diagnostic WHERE file_id = ? AND origin = ? AND code = ?)`,
		fileID, oldest, fileID, string(model.OriginEnrichment), string(model.DiagTagWriteLost))
	return err
}

// copyTrackField copies one field's stored value from cur onto the scanned track. The
// artist carries its sort and its split list, since a credit set as ["Jay-Z", "Alicia
// Keys"] stores "Jay-Z, Alicia Keys" and the splitter does not split on a comma, so
// re-deriving the list would collapse two artists into one. The composer carries its
// sort the same way, and the genre its list. A field the track has no column for is
// ignored.
func copyTrackField(tr *model.Track, cur model.Track, field string) {
	switch field {
	case "artist":
		tr.Artist, tr.ArtistSort, tr.Artists = cur.Artist, cur.ArtistSort, cur.Artists
	case "album_artist":
		tr.AlbumArtist = cur.AlbumArtist
	case "album":
		tr.Album = cur.Album
	case "composer":
		tr.Composer, tr.ComposerSort = cur.Composer, cur.ComposerSort
	case "composer_sort":
		tr.ComposerSort = cur.ComposerSort
	case "comment":
		tr.Comment = cur.Comment
	case "genre":
		tr.Genre, tr.Genres = cur.Genre, cur.Genres
	case "year":
		tr.Year = cur.Year
	case "track_no":
		tr.TrackNo = cur.TrackNo
	case "disc_no":
		tr.DiscNo = cur.DiscNo
	case "track_total":
		tr.TrackTotal = cur.TrackTotal
	case "disc_total":
		tr.DiscTotal = cur.DiscTotal
	case "bpm":
		tr.BPM = cur.BPM
	case "isrc":
		tr.ISRC = cur.ISRC
	case "mbid":
		tr.MBID = cur.MBID
	case "compilation":
		tr.Compilation = cur.Compilation
	}
}

// bookColumnOf maps a provenance field onto the book value it describes: a credit
// writes the same display as its scalar twin.
func bookColumnOf(field string) string {
	switch field {
	case model.CreditField(model.RoleAuthor):
		return "author"
	case model.CreditField(model.RoleNarrator):
		return "narrator"
	}
	return field
}

// overlayStoredBookTx is overlayStoredTrackTx for a book. Author and narrator carry their
// split lists, so upsertBook re-resolves the same contributor entities: a book rescan of
// the primary part rebuilds every contributor role from the scanned lists, which a
// locked role must overlay, whether it was locked as a scalar or a credit, and translator
// and editor have only their credit locks. It must run before upsertItem and upsertBook.
// known is the caller's resolution of the book's key, as for overlayStoredTrackTx.
func overlayStoredBookTx(ctx context.Context, tx *sql.Tx, log logger, fileID int64, b *model.Book, item *model.PlayableItem, derived []string, preserveLocks bool, known *resolvedItem) (*scanPrior, error) {
	if known == nil {
		var err error
		if known, err = resolveItemTx(ctx, tx, log, item.Kind, item.IdentityKey, bookAdoptKey{author: b.Author, title: item.Title}); err != nil {
			return nil, err
		}
	}
	id := known.id
	if id == 0 {
		return nil, nil
	}
	rows, err := storedRowsTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	prior := &scanPrior{itemID: id, rows: rows}
	locked, fills := partitionStoredRows(rows, preserveLocks)
	if len(locked) == 0 && len(fills) == 0 {
		return prior, nil
	}
	cur, curTitle, err := loadBookForEditTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	prior.book = &cur

	if locked["title"] {
		item.Title, item.SortKey = curTitle, model.SortKey(curTitle)
	}
	for f := range locked {
		copyBookField(b, cur, bookColumnOf(f))
	}

	var carried []string
	for _, f := range fills {
		col := bookColumnOf(f)
		if !bookFillHolds(col, rows, cur) || !bookFieldSilent(col, *b, derived) {
			continue
		}
		if col == "title" {
			item.Title, item.SortKey = curTitle, model.SortKey(curTitle)
		} else {
			copyBookField(b, cur, col)
		}
		carried = append(carried, f)
	}
	return prior, reopenLostFillsTx(ctx, tx, fileID, carried, rows)
}

// bookFillHolds is trackFillHolds for a book. A title row records no value to reproduce.
func bookFillHolds(col string, rows map[string]storedRow, cur model.Book) bool {
	fields := []string{col}
	switch col {
	case "author":
		fields = append(fields, model.CreditField(model.RoleAuthor))
	case "narrator":
		fields = append(fields, model.CreditField(model.RoleNarrator))
	}
	return fillRowsHold(fields, rows, func(f, value string) bool {
		if f == "title" {
			return true
		}
		_, ok := bookRowReproduces(f, value, cur)
		return ok
	})
}

// bookFieldSilent is trackFieldSilent for a book.
func bookFieldSilent(col string, b model.Book, derived []string) bool {
	if slices.Contains(derived, col) {
		return true
	}
	switch col {
	case "title":
		return false
	case "author":
		return b.Author == "" && len(b.Authors) == 0
	case "narrator":
		return b.Narrator == "" && len(b.Narrators) == 0
	case "genre":
		return b.Genre == "" && len(b.Genres) == 0
	case model.CreditField(model.RoleTranslator):
		return len(b.Translators) == 0
	case model.CreditField(model.RoleEditor):
		return len(b.Editors) == 0
	case "year":
		return b.Year == 0
	}
	return bookFieldValue(col, b, false) == ""
}

// copyBookField copies one field's stored value from cur onto the scanned book, the
// author and narrator with their split lists. A field the book has no column or credit
// list for is ignored.
func copyBookField(b *model.Book, cur model.Book, field string) {
	switch field {
	case "author":
		b.Authors, b.Author, b.AuthorSort = cur.Authors, cur.Author, cur.AuthorSort
	case "author_sort":
		b.AuthorSort = cur.AuthorSort
	case "narrator":
		b.Narrators, b.Narrator = cur.Narrators, cur.Narrator
	case model.CreditField(model.RoleTranslator):
		b.Translators = cur.Translators
	case model.CreditField(model.RoleEditor):
		b.Editors = cur.Editors
	case "series":
		b.Series = cur.Series
	case "subtitle":
		b.Subtitle = cur.Subtitle
	case "genre":
		b.Genre, b.Genres = cur.Genre, cur.Genres
	case "year":
		b.Year = cur.Year
	case "publisher":
		b.Publisher = cur.Publisher
	case "asin":
		b.ASIN = cur.ASIN
	case "isbn":
		b.ISBN = cur.ISBN
	case "edition":
		b.Edition = cur.Edition
	case "description":
		b.Description = cur.Description
	case "mbid":
		b.MBID = cur.MBID
	}
}
