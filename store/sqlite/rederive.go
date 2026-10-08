package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// A scan put rewrites an existing item's columns from the file, so whatever a lock the
// scan honours does not protect is re-derived. Everything hanging off a re-derived field
// has to follow the column: the entity links, the search row, the item delta, and the
// provenance row, which would otherwise describe a value the item no longer holds.

// scanFields are the item columns a track scan put writes. artist_sort has no edit
// surface of its own, but a put that changes only it still changes the item.
var scanFields = []string{
	"title", "artist", "artist_sort", "album", "album_artist", "composer", "composer_sort",
	"comment", "genre", "year", "track_no", "track_total", "disc_no", "disc_total", "bpm",
	"isrc", "mbid", "compilation",
}

// scanFieldValue reads one scanFields column off the values a put writes, as the string
// the comparison uses.
func scanFieldValue(field, title string, tr model.Track) string {
	switch field {
	case "title":
		return title
	case "artist":
		return tr.Artist
	case "artist_sort":
		return tr.ArtistSort
	case "album":
		return tr.Album
	case "album_artist":
		return tr.AlbumArtist
	case "composer":
		return tr.Composer
	case "composer_sort":
		return tr.ComposerSort
	case "comment":
		return tr.Comment
	case "genre":
		return tr.Genre
	case "year":
		return strconv.Itoa(tr.Year)
	case "track_no":
		return strconv.Itoa(tr.TrackNo)
	case "track_total":
		return strconv.Itoa(tr.TrackTotal)
	case "disc_no":
		return strconv.Itoa(tr.DiscNo)
	case "disc_total":
		return strconv.Itoa(tr.DiscTotal)
	case "bpm":
		return strconv.Itoa(tr.BPM)
	case "isrc":
		return tr.ISRC
	case "mbid":
		return tr.MBID
	case "compilation":
		return strconv.FormatBool(tr.Compilation)
	}
	return ""
}

// rederivable is a provenance row a scan put can re-derive: the value it records and
// where that value came from.
type rederivable struct{ value, source string }

// rederivableRowsTx returns the provenance rows on an item whose recorded value a scan
// put holds against the file, by field: every scalar row carrying a value, less the locked
// ones when the scan honours locks. A lock-only row (source tag) and a row recording no
// value (the enrichment genre fill, whose genres are many) state nothing a file could
// contradict, though a re-derived column still retires them. Custom-tag rows are
// syncItemTagsTx's.
func rederivableRowsTx(ctx context.Context, tx *sql.Tx, itemID int64, preserveLocks bool) (map[string]rederivable, error) {
	rows, err := tx.QueryContext(ctx, `SELECT field, value, source FROM field_provenance
		WHERE item_id = ? AND source <> 'tag' AND value IS NOT NULL AND field NOT LIKE 'tag.%'
		  AND (locked = 0 OR ?)`,
		itemID, !preserveLocks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]rederivable{}
	for rows.Next() {
		var field string
		var r rederivable
		if err := rows.Scan(&field, &r.value, &r.source); err != nil {
			return nil, err
		}
		out[field] = r
	}
	return out, rows.Err()
}

// rederivedTrackTx works out which fields a track put re-derives over an existing item,
// reading the stored columns before upsertTrack replaces them. It returns those fields
// and the provenance rows they were judged against.
func rederivedTrackTx(ctx context.Context, tx *sql.Tx, itemID int64, priorTitle, title string, tr model.Track, preserveLocks bool, known *scanPrior) ([]string, map[string]rederivable, error) {
	known = known.priorFor(itemID)
	var prior model.Track
	if known != nil && known.track != nil {
		prior = *known.track
	} else {
		cols, err := readTrackColumnsTx(ctx, tx, itemID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		prior = cols
	}
	var rows map[string]rederivable
	if known != nil {
		rows = known.rederivable(preserveLocks)
	} else {
		var err error
		if rows, err = rederivableRowsTx(ctx, tx, itemID, preserveLocks); err != nil {
			return nil, nil, err
		}
	}
	return rederivedFields(priorTitle, prior, rows, title, tr), rows, nil
}

// rederivedFields lists the columns a put re-derives: each one whose stored value the put
// replaces, and each one a provenance row describes with a value that does not reproduce
// what the put writes (a stale row an older scan left behind).
func rederivedFields(priorTitle string, prior model.Track, rows map[string]rederivable, title string, tr model.Track) []string {
	set := map[string]bool{}
	for _, f := range scanFields {
		if scanFieldValue(f, priorTitle, prior) != scanFieldValue(f, title, tr) {
			set[f] = true
		}
	}
	for field, row := range rows {
		if col, ok := rowReproduces(field, row.value, title, tr); col != "" && !ok {
			set[col] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// rowReproduces maps a provenance row onto the column it describes and reports whether
// its value, applied the way an edit applies it, yields what the put writes. A credit row
// holds the "; "-joined list, which syncCreditDenormTx writes to the column as the artist
// display or verbatim as the composer. col is empty for a row no track column carries.
func rowReproduces(field, value, title string, tr model.Track) (col string, ok bool) {
	switch field {
	case "title":
		return "title", value == title
	case model.CreditField(model.RoleArtist):
		return "artist", strings.Join(strings.Split(value, "; "), ", ") == tr.Artist
	case model.CreditField(model.RoleComposer):
		return "composer", value == tr.Composer
	}
	if !trackEditFields[field] {
		return "", false
	}
	edited := tr
	if err := applyTrackEdit(&edited, field, value, ""); err != nil {
		return field, false
	}
	return field, scanFieldValue(field, title, edited) == scanFieldValue(field, title, tr)
}

// settleRederivedTx retires the provenance of every re-derived field, the credit rows
// that denormalize into one included, and reports whether one of them held an enrichment
// value. A composer credit re-derived away takes its contributor rows with it, since a
// track's composer list only ever comes from that credit.
func settleRederivedTx(ctx context.Context, tx *sql.Tx, itemID int64, fields []string, rows map[string]rederivable, preserveLocks bool, affected *affectedRollups) (bool, error) {
	if len(fields) == 0 {
		return false, nil
	}
	targets := make([]string, 0, len(fields)+2)
	for _, f := range fields {
		targets = append(targets, f)
		switch f {
		case "artist":
			targets = append(targets, model.CreditField(model.RoleArtist))
		case "composer":
			targets = append(targets, model.CreditField(model.RoleComposer))
		}
	}
	enrichment, err := retireProvenanceTx(ctx, tx, itemID, targets, preserveLocks)
	if err != nil {
		return false, err
	}
	if _, credited := rows[model.CreditField(model.RoleComposer)]; !credited || !slices.Contains(fields, "composer") {
		return enrichment, nil
	}
	prior, err := contributorArtistIDsForRole(ctx, tx, itemID, model.RoleComposer)
	if err != nil {
		return false, err
	}
	for _, aid := range prior {
		affected.artists[aid] = true
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM item_contributor WHERE item_id = ? AND role = ?",
		itemID, string(model.RoleComposer))
	return enrichment, err
}

// dropMootEnrichmentDriftTx clears the enrichment write-back's diagnostics on a file once
// a scan has re-derived away the values a failed write was about and the file owes
// nothing more: no enrichment value newer than its settle stamp is left on the item, and
// no album label is due (the two halves enrichedTagSelect owes a file by). A pass with
// nothing left to write clears them the same way.
func dropMootEnrichmentDriftTx(ctx context.Context, tx *sql.Tx, fileID, itemID int64) error {
	var owed bool
	err := tx.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM field_provenance fp WHERE fp.item_id = ? AND fp.source = 'enrichment'
		        AND fp.locked = 0 AND fp.updated_at > f.enrich_settled_at)
		OR EXISTS (SELECT 1 FROM track t JOIN album al ON al.id = t.album_id
		           JOIN entity_curation lab ON `+enrichmentLabelRowJoin+`
		           WHERE t.item_id = ? AND COALESCE(al.label, '') <> '' AND lab.updated_at > f.enrich_settled_at)
		FROM file f WHERE f.id = ?`, itemID, itemID, fileID).Scan(&owed)
	if err != nil || owed {
		return err
	}
	return replaceFileDiagnosticsTx(ctx, tx, fileID, model.OriginEnrichment, nil)
}

// retireProvenanceTx drops the provenance rows of fields a scan re-derived, and reports
// whether one of them held an enrichment value. An unlocked row goes. Under --ignore-locks
// a locked row keeps its lock and becomes a lock-only row, since the curated value it
// recorded is gone from the column.
func retireProvenanceTx(ctx context.Context, tx *sql.Tx, itemID int64, fields []string, preserveLocks bool) (bool, error) {
	args := make([]any, 0, len(fields)+2)
	args = append(args, itemID)
	for _, f := range fields {
		args = append(args, f)
	}
	in := placeholders(len(fields))
	var enrichment bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM field_provenance
		WHERE item_id = ? AND source = 'enrichment' AND (locked = 0 OR ?) AND field IN `+in+`)`,
		append([]any{itemID, !preserveLocks}, args[1:]...)...).Scan(&enrichment); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM field_provenance WHERE item_id = ? AND locked = 0 AND field IN "+in, args...); err != nil {
		return false, err
	}
	if preserveLocks {
		return enrichment, nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE field_provenance SET source = 'tag', value = NULL, provider = NULL,
		updated_at = ? WHERE item_id = ? AND locked = 1 AND source <> 'tag' AND field IN `+in,
		append([]any{nowNS()}, args...)...)
	return enrichment, err
}

// bookScanFields are the book values upsertBook rewrites from a primary part whose bytes
// changed. The title is upsertItem's, settled on every put. The two credits with no
// column of their own compare as their name lists.
var bookScanFields = []string{
	"author", "author_sort", "narrator", "series", "subtitle", "genre", "year", "publisher",
	"asin", "isbn", "edition", "description", "mbid", "credit.translator", "credit.editor",
}

// bookEntityFields are the bookScanFields a stored book reads back through an entity. The
// stored spelling is the entity's (the first one seen, or a merge survivor's) rather than
// the tag's, so a difference in it is no edit, and only a provenance row whose value the
// file no longer carries says the catalog departed from the file.
var bookEntityFields = map[string]bool{"series": true, "credit.translator": true, "credit.editor": true}

// bookFieldValue reads one bookScanFields value off b. A stored book (loadBookForEditTx)
// carries its columns as written; a scanned one is derived the way upsertBook derives
// what it writes, the author display from the split authors.
func bookFieldValue(field string, b model.Book, stored bool) string {
	switch field {
	case "author":
		if stored {
			return b.Author
		}
		return bookAuthorDisplay(b)
	case "author_sort":
		return b.AuthorSort
	case "narrator":
		return b.Narrator
	case "series":
		return b.Series
	case "subtitle":
		return b.Subtitle
	case "genre":
		return b.Genre
	case "year":
		return strconv.Itoa(b.Year)
	case "publisher":
		return b.Publisher
	case "asin":
		return b.ASIN
	case "isbn":
		return b.ISBN
	case "edition":
		return b.Edition
	case "description":
		return b.Description
	case "mbid":
		return b.MBID
	case "credit.translator":
		return strings.Join(b.Translators, "; ")
	case "credit.editor":
		return strings.Join(b.Editors, "; ")
	}
	return ""
}

// rederivedBookTx is rederivedTrackTx for a book's primary part, read before upsertBook
// replaces the book row and its contributors.
func rederivedBookTx(ctx context.Context, tx *sql.Tx, itemID int64, b model.Book, preserveLocks bool, known *scanPrior) ([]string, map[string]rederivable, error) {
	known = known.priorFor(itemID)
	var prior model.Book
	if known != nil && known.book != nil {
		prior = *known.book
	} else {
		stored, _, err := loadBookForEditTx(ctx, tx, itemID)
		if waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		prior = stored
	}
	var rows map[string]rederivable
	if known != nil {
		rows = known.rederivable(preserveLocks)
	} else {
		var err error
		if rows, err = rederivableRowsTx(ctx, tx, itemID, preserveLocks); err != nil {
			return nil, nil, err
		}
	}
	set := map[string]bool{}
	for _, f := range bookScanFields {
		if !bookEntityFields[f] && bookFieldValue(f, prior, true) != bookFieldValue(f, b, false) {
			set[f] = true
		}
	}
	for field, row := range rows {
		if col, ok := bookRowReproduces(field, row.value, b); col != "" && !ok {
			set[col] = true
		}
	}
	return slices.Sorted(maps.Keys(set)), rows, nil
}

// bookRowReproduces is rowReproduces for a book. A credit row's "; " list becomes the
// author or narrator display the way syncCreditDenormTx writes it.
func bookRowReproduces(field, value string, b model.Book) (col string, ok bool) {
	switch field {
	case model.CreditField(model.RoleAuthor):
		return "author", strings.Join(strings.Split(value, "; "), ", ") == bookFieldValue("author", b, false)
	case model.CreditField(model.RoleNarrator):
		return "narrator", strings.Join(strings.Split(value, "; "), ", ") == b.Narrator
	case model.CreditField(model.RoleTranslator), model.CreditField(model.RoleEditor):
		return field, value == bookFieldValue(field, b, false)
	}
	if field == "title" || !bookEditFields[field] {
		return "", false
	}
	edited := b
	if err := applyBookEdit(&edited, field, value, ""); err != nil {
		return field, false
	}
	return field, bookFieldValue(field, edited, false) == bookFieldValue(field, b, false)
}

// settleRederivedBookTx retires the provenance of every book field the put re-derived,
// the credit rows behind the author and narrator displays included, and when one held an
// enrichment value clears the write-back's moot drift on every part, since the book's
// values ride each part's tags.
func settleRederivedBookTx(ctx context.Context, tx *sql.Tx, itemID int64, fields []string, rows map[string]rederivable, preserveLocks bool) error {
	if len(fields) == 0 {
		return nil
	}
	targets := make([]string, 0, len(fields)+2)
	for _, f := range fields {
		targets = append(targets, f)
		switch f {
		case "author":
			targets = append(targets, model.CreditField(model.RoleAuthor))
		case "narrator":
			targets = append(targets, model.CreditField(model.RoleNarrator))
		}
	}
	enrichment, err := retireProvenanceTx(ctx, tx, itemID, targets, preserveLocks)
	if err != nil || !enrichment {
		return err
	}
	parts, err := queryInt64sTx(ctx, tx, "SELECT file_id FROM item_file WHERE item_id = ?", itemID)
	if err != nil {
		return err
	}
	for _, fileID := range parts {
		if err := dropMootEnrichmentDriftTx(ctx, tx, fileID, itemID); err != nil {
			return err
		}
	}
	return nil
}
