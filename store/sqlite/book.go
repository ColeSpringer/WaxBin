package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// PutScannedBook persists one scanned audiobook file atomically: resolve/insert
// the file (preserving its pid on a path or essence match), resolve/insert the
// book item by (kind, book key), upsert the book subtype with its series and
// role-tagged contributors, attach this file as a part in reading order, store its
// chapters, and write the matching change_log rows. A book groups many files: each
// part call attaches its file to the same book item rather than replacing the
// prior one, so a multi-file book accumulates its parts across scans.
func (s *Store) PutScannedBook(ctx context.Context, in model.PutScannedBookInput) (*model.ScanItemResult, error) {
	const op = "store.PutScannedBook"
	res := &model.ScanItemResult{}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()

		fileID, filePID, err := s.resolveBookFile(ctx, tx, in.LibraryID, in.File, now, res)
		if err != nil {
			return err
		}
		res.FilePID = filePID

		// Replace the file's sidecar observations (prunes a since-deleted .cue/.lrc so it
		// does not force a full re-hash forever).
		if err := replaceFileAuxTx(ctx, tx, fileID, in.AuxObservations); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// A rebuild adopts the file's WAXBIN_ITEM_PID stamp (organize stamps books too) to
		// restore the book's original identity; identity stays essence-first, so a taken or
		// invalid hint falls back to a fresh PID. Parts of one book share the stamp: the
		// first to create the item adopts it, the rest join it by book key.
		itemID, itemPID, created, stateChanged, err := upsertItem(ctx, tx, in.Item, now, in.PreferredItemPID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		res.ItemPID, res.ItemCreated = itemPID, created

		affected := newAffectedRollups()

		// Attach this file as a part FIRST, so its role (the first part is the
		// representative 'primary', the rest are 'part') is known before deciding
		// whether it owns the book's metadata, and detach it from any other item.
		role, orphans, err := linkBookFile(ctx, tx, itemID, fileID, in.Position)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, oid := range orphans {
			has, err := itemHasAnyFile(ctx, tx, oid)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if has {
				// A surviving item lost a file (e.g. a multi-file book whose part was
				// retagged into another book): keep a primary, recompute its shrunken
				// duration/genre rollup, and refresh its denormalized total duration.
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

		// On a real change, resolve the book's chapters (per file) and, only for the
		// primary part, its metadata/series/contributors/genres/FTS. Gating the
		// metadata on the primary makes one part the owner, so an asymmetrically-tagged
		// later part can't clobber the book's narrator/series/etc. by scan order. The
		// genres are still collected for every changed part so the genre rollup's
		// summed-across-parts duration reflects a newly attached part.
		changed := created || res.FileCreated || res.ContentChanged || res.Relinked
		if changed {
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if created || role == bookPrimaryRole {
				if err := upsertBook(ctx, tx, itemID, in.Book, affected); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if err := affected.collect(ctx, tx, itemID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
		}
		// Chapters sync OUTSIDE the audio-change gate (idempotent): an external .cue can
		// change independently of the audio, and a forced rescan must re-import chapters
		// even when the content is unchanged. It no-ops when the stored chapters already
		// match, so a true no-op rescan stays silent.
		chaptersChanged, err := syncChaptersForFile(ctx, tx, itemID, fileID, in.ChapterSource, in.Chapters)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if changed || chaptersChanged {
			// The book row exists now (the primary created it); refresh its denormalized
			// total duration so a new part or a changed chapter span is reflected.
			if err := refreshBookDuration(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// Cover art comes from the primary part (like the rest of the book's metadata),
		// so a later, differently-covered part can't replace it. It runs every scan of
		// the primary (idempotent) to still catch a directory cover added later. Its
		// changed flag feeds the item delta so a cover-only change is not silent.
		artChanged := false
		if created || role == bookPrimaryRole {
			c, err := attachArtTxChanged(ctx, tx, itemID, in.CoverArt)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			artChanged = c
		}

		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		if res.FileCreated || res.ContentChanged || res.Relinked {
			if err := appendChange(ctx, tx, "file", filePID, opFor(res.FileCreated)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		// A book also changes when a NEW part is attached or a part is re-linked (its
		// part list, chapters, and total duration move), not only when it is created or
		// a part's bytes change. Attaching the second file of a multi-file book has
		// created=false and ContentChanged=false, so without FileCreated/Relinked here a
		// change_log tailer would never refresh the existing book. An externally-changed
		// .cue (chaptersChanged with unchanged audio) also warrants a delta.
		if created || res.ContentChanged || res.FileCreated || res.Relinked || chaptersChanged || stateChanged || artChanged {
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

// resolveBookFile finds-or-creates the file row for a book part, preserving the
// pid on a path match (rescan/retag) or an essence match whose old path is gone (a
// move). It mirrors resolveFile but needs no prior-essence return: a book's
// identity is its book key, not the file essence, so an essence-algorithm change
// over unchanged bytes does not threaten item identity.
func (s *Store) resolveBookFile(ctx context.Context, tx *sql.Tx, libraryID int64, file model.File, now int64, res *model.ScanItemResult) (int64, model.PID, error) {
	if existing, err := fileByPathTx(ctx, tx, file.Path); err != nil {
		return 0, "", err
	} else if existing != nil {
		res.ContentChanged = existing.ContentHash != file.ContentHash
		if err := updateFileRow(ctx, tx, existing.ID, file, now); err != nil {
			return 0, "", err
		}
		return existing.ID, existing.PID, nil
	}
	if file.EssenceHash != "" {
		relink, err := fileByEssenceSingleTx(ctx, tx, file.EssenceHash, libraryID)
		if err != nil {
			return 0, "", err
		}
		if relink != nil && !pathExists(relink.Path) {
			res.Relinked = true
			res.ContentChanged = relink.ContentHash != file.ContentHash
			if err := updateFileRow(ctx, tx, relink.ID, file, now); err != nil {
				return 0, "", err
			}
			return relink.ID, relink.PID, nil
		}
	}
	res.FileCreated = true
	pid := model.NewPID()
	id, err := insertFileRow(ctx, tx, libraryID, pid, file, now)
	if err != nil {
		return 0, "", err
	}
	return id, pid, nil
}

// upsertBook writes the book subtype row, resolving its series and contributor
// artists first so their ids land on the row, then refreshing the item genres and
// the FTS row. Touched author/narrator artists and genres are recorded in affected
// so their rollups stay consistent (an author is an artist with zero tracks, which
// the rollup recompute represents as a zero row).
func upsertBook(ctx context.Context, tx *sql.Tx, itemID int64, b model.Book, affected *affectedRollups) error {
	seriesID, err := resolveSeries(ctx, tx, b.Series)
	if err != nil {
		return err
	}
	authorID, err := resolveContributors(ctx, tx, itemID, b, affected)
	if err != nil {
		return err
	}
	// Derive the stored author display from the split author entities, so the
	// denormalized column lists exactly the authors that were linked as contributors
	// (author_id points at the first of them) rather than the raw combined credit. A
	// book with no split authors falls back to the raw value.
	author := strings.Join(b.Authors, ", ")
	if author == "" {
		author = b.Author
	}
	authorSort := b.AuthorSort
	if authorSort == "" {
		authorSort = model.SortKey(author)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO book
		(item_id, subtitle, author, author_sort, author_id, narrator, series_id, series_seq,
		 series_seq_sort, year, publisher, asin, isbn, edition, abridged, description, genre)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET
			subtitle=excluded.subtitle, author=excluded.author, author_sort=excluded.author_sort,
			author_id=excluded.author_id, narrator=excluded.narrator, series_id=excluded.series_id,
			series_seq=excluded.series_seq, series_seq_sort=excluded.series_seq_sort, year=excluded.year,
			publisher=excluded.publisher, asin=excluded.asin, isbn=excluded.isbn, edition=excluded.edition,
			abridged=excluded.abridged, description=excluded.description, genre=excluded.genre`,
		itemID, b.Subtitle, author, authorSort, nullInt64(authorID), b.Narrator, nullInt64(seriesID),
		b.SeriesSeq, model.SortKey(b.SeriesSeq), nullInt(b.Year), b.Publisher, b.ASIN, b.ISBN,
		b.Edition, nullBool(b.Abridged), b.Description, b.Genre); err != nil {
		return err
	}
	if err := syncItemGenres(ctx, tx, itemID, b.Genres, b.Genre); err != nil {
		return err
	}
	return syncBookSearchFTS(ctx, tx, itemID, b, author)
}

// resolveContributors replaces an item's role-tagged contributors from the book's
// author/narrator/translator/editor lists, creating each person's artist entity.
// It returns the primary (first) author's artist id for the book row. Both the old
// and the new contributor artists are recorded in affected so a retag that swaps an
// author refreshes both rollups.
func resolveContributors(ctx context.Context, tx *sql.Tx, itemID int64, b model.Book, affected *affectedRollups) (int64, error) {
	// Collect the contributors this item currently has, so an author/narrator that
	// the retag drops still has its rollup refreshed. The read fully drains and
	// closes its cursor before the writes below, since an open cursor on this single
	// tx connection would block the DELETE/INSERTs that follow.
	priorArtists, err := contributorArtistIDs(ctx, tx, itemID)
	if err != nil {
		return 0, err
	}
	for _, aid := range priorArtists {
		affected.artists[aid] = true
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM item_contributor WHERE item_id = ?", itemID); err != nil {
		return 0, err
	}

	add := func(names []string, role model.ContributorRole) (int64, error) {
		var firstID int64
		for pos, name := range names {
			aid, err := resolveArtist(ctx, tx, name, "")
			if err != nil {
				return 0, err
			}
			if aid == 0 {
				continue
			}
			affected.artists[aid] = true
			if firstID == 0 {
				firstID = aid
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO item_contributor(item_id, artist_id, role, position) VALUES (?,?,?,?)",
				itemID, aid, string(role), pos); err != nil {
				return 0, err
			}
		}
		return firstID, nil
	}

	authors := b.Authors
	if len(authors) == 0 && b.Author != "" {
		authors = []string{b.Author}
	}
	authorID, err := add(authors, model.RoleAuthor)
	if err != nil {
		return 0, err
	}
	if _, err := add(b.Narrators, model.RoleNarrator); err != nil {
		return 0, err
	}
	if _, err := add(b.Translators, model.RoleTranslator); err != nil {
		return 0, err
	}
	if _, err := add(b.Editors, model.RoleEditor); err != nil {
		return 0, err
	}
	return authorID, nil
}

// contributorArtistIDs returns the artist ids currently credited on an item. It
// drains and closes its cursor before returning, so the caller can safely write to
// the same transaction afterward.
func contributorArtistIDs(ctx context.Context, tx *sql.Tx, itemID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT artist_id FROM item_contributor WHERE item_id = ?", itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var aid int64
		if err := rows.Scan(&aid); err != nil {
			return nil, err
		}
		out = append(out, aid)
	}
	return out, rows.Err()
}

// resolveSeries finds-or-creates a series by its normalized match key, mirroring
// resolveArtist. It returns 0 when the name is blank. A new series is emitted to
// the change_log.
func resolveSeries(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	mk := identity.MatchKey(name)
	if mk == "" {
		return 0, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM series WHERE match_key = ?", mk).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	pid := model.NewPID()
	r, err := tx.ExecContext(ctx,
		"INSERT INTO series(pid, name, sort_key, match_key) VALUES (?,?,?,?)",
		string(pid), name, model.SortKey(name), mk)
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "series", pid, model.OpCreate)
}

// Book part roles in item_file. The first part attached to a book is the
// representative 'primary' (so the shared item view and rollups find a backing
// file); the rest are 'part'.
const (
	bookPrimaryRole = "primary"
	bookPartRole    = "part"
)

// linkBookFile attaches fileID to the book at the given part position and detaches
// it from any other item (a file belongs to exactly one item). It returns the role
// the file holds in the book (primary or part) and the items left holding no file
// after the detach, for the caller to delete. An existing edge keeps its role.
func linkBookFile(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, position int) (string, []int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT DISTINCT item_id FROM item_file WHERE file_id = ? AND item_id <> ?", fileID, bookItemID)
	if err != nil {
		return "", nil, err
	}
	var prev []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", nil, err
		}
		prev = append(prev, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", nil, err
	}
	rows.Close()

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_file WHERE file_id = ? AND item_id <> ?", fileID, bookItemID); err != nil {
		return "", nil, err
	}

	// Keep an existing role for this (book, file) edge; otherwise the first part is
	// the representative primary and the rest are parts.
	var role string
	err = tx.QueryRowContext(ctx,
		"SELECT role FROM item_file WHERE item_id = ? AND file_id = ? LIMIT 1", bookItemID, fileID).Scan(&role)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var hasPrimary int
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM item_file WHERE item_id = ? AND role = 'primary')", bookItemID).Scan(&hasPrimary); err != nil {
			return "", nil, err
		}
		if hasPrimary == 1 {
			role = bookPartRole
		} else {
			role = bookPrimaryRole
		}
	case err != nil:
		return "", nil, err
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_file WHERE item_id = ? AND file_id = ?", bookItemID, fileID); err != nil {
		return "", nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO item_file(item_id, file_id, role, position) VALUES (?,?,?,?)",
		bookItemID, fileID, role, position); err != nil {
		return "", nil, err
	}
	return role, prev, nil
}

// ensurePrimary makes sure an item that still has files keeps exactly one 'primary'
// edge: if its primary was detached (leaving only 'part' edges), it promotes the
// lowest-positioned remaining part. A track (single primary file) never reaches the
// promote path; this matters for a multi-file book whose primary part was re-keyed
// into another item.
func ensurePrimary(ctx context.Context, tx *sql.Tx, itemID int64) error {
	var hasPrimary int
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM item_file WHERE item_id = ? AND role = 'primary')", itemID).Scan(&hasPrimary); err != nil {
		return err
	}
	if hasPrimary == 1 {
		return nil
	}
	var fileID int64
	err := tx.QueryRowContext(ctx,
		"SELECT file_id FROM item_file WHERE item_id = ? ORDER BY position, file_id LIMIT 1", itemID).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no files at all; the caller's fileless path handles deletion
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		"UPDATE item_file SET role = 'primary' WHERE item_id = ? AND file_id = ?", itemID, fileID)
	return err
}

// bookEffectiveDurationSum is the SQL subquery for a book's total running time: the
// sum over its parts of each part's EFFECTIVE duration, the larger of the file's
// own duration and its furthest chapter offset. Using the chapter extent as a floor
// means a part with an unknown file duration but real chapters still contributes,
// and the stored total never falls short of the chapter timeline that bookChapters
// builds with the identical definition. The "%s" is the correlated book item id.
const bookEffectiveDurationSum = `(
	SELECT COALESCE(SUM(MAX(
		COALESCE(f.duration_ms, 0),
		COALESCE((SELECT MAX(MAX(c.start_ms, c.end_ms)) FROM chapter c
		          WHERE c.book_item_id = itf.item_id AND c.file_id = itf.file_id), 0)
	)), 0)
	FROM item_file itf JOIN file f ON f.id = itf.file_id WHERE itf.item_id = %s)`

// refreshBookDuration recomputes a book's denormalized total_duration_ms from its
// current parts (effective durations). It is a no-op for a non-book item (the UPDATE
// matches no book row), so callers on the shared track/book detach paths can call it
// unconditionally.
func refreshBookDuration(ctx context.Context, tx *sql.Tx, itemID int64) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE book SET total_duration_ms = "+fmt.Sprintf(bookEffectiveDurationSum, "book.item_id")+" WHERE item_id = ?",
		itemID)
	return err
}

// chapterSourceRank orders chapter sources by precedence (lower wins). A remote
// podcast:chapters JSON is richest and outranks embedded chapters (the documented
// episode contract); for books (which never carry podcast_url) embedded chapters are
// authoritative over an external .cue, and a synthesized single chapter ranks below
// a real source. One ordering serves both kinds because their source sets are
// disjoint (books: embedded/cue/synthetic; episodes: podcast_url).
func chapterSourceRank(source string) int {
	switch source {
	case "podcast_url":
		return 0
	case "embedded":
		return 1
	case "cue":
		return 2
	case "synthetic":
		return 3
	default:
		return 4
	}
}

// preferredChapters returns the chapters of the single highest-precedence source
// present for a file (embedded over cue), so a file that briefly carries both does
// not read back a merged, doubled chapter list.
func preferredChapters(bySource map[string][]model.Chapter) []model.Chapter {
	best, bestRank := "", 1<<30
	for source := range bySource {
		if r := chapterSourceRank(source); r < bestRank {
			best, bestRank = source, r
		}
	}
	return bySource[best]
}

// syncChaptersForFile is the authoritative book-scan chapter write: it replaces ALL
// of a file's chapters (every source) with the scanned set, tagged with source, so a
// source that no longer applies (a synthetic single chapter superseded by embedded
// chapters, or vice versa) is cleared. It is idempotent, so the book scan can call it
// unconditionally (a forced rescan or an externally-changed .cue re-imports chapters
// even when the audio is unchanged) without churning a true no-op rescan.
func syncChaptersForFile(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, source string, chapters []model.Chapter) (bool, error) {
	if source == "" {
		source = "embedded"
	}
	return syncChapters(ctx, tx, bookItemID, fileID, source, chapters, false)
}

// syncChaptersForFileSource replaces only ONE source's chapters for a file, leaving a
// multi-file book's other parts (and this part's chapters from a richer source)
// intact. It is the fast-path seam that updates .lrc/.cue/podcast chapters without
// re-reading the audio.
func syncChaptersForFileSource(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, source string, chapters []model.Chapter) (bool, error) {
	return syncChapters(ctx, tx, bookItemID, fileID, source, chapters, true)
}

// syncChapters replaces a file's chapters with the desired set tagged with source,
// reporting whether it changed anything. scopeToSource limits the replace (and the
// no-op comparison) to rows of that source; otherwise it replaces every source's
// rows for the file. It no-ops (no write, no change) when the stored rows already
// match, so a no-op rescan stays change_log-silent.
func syncChapters(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, source string, chapters []model.Chapter, scopeToSource bool) (bool, error) {
	if same, err := chaptersInSync(ctx, tx, bookItemID, fileID, source, chapters, scopeToSource); err != nil {
		return false, err
	} else if same {
		return false, nil
	}
	del := "DELETE FROM chapter WHERE book_item_id = ? AND file_id = ?"
	args := []any{bookItemID, fileID}
	if scopeToSource {
		del += " AND source = ?"
		args = append(args, source)
	}
	if _, err := tx.ExecContext(ctx, del, args...); err != nil {
		return false, err
	}
	for _, c := range chapters {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO chapter(book_item_id, file_id, position, title, start_ms, end_ms, source) VALUES (?,?,?,?,?,?,?)",
			bookItemID, fileID, c.Position, c.Title, c.FileStartMS, c.FileEndMS, source); err != nil {
			return false, err
		}
	}
	return true, nil
}

// chaptersInSync reports whether the stored chapters already equal want (count,
// order, title, offsets), all under source. scopeToSource compares only that
// source's rows; otherwise it compares every row for the file and requires each to
// carry source (so an all-source replace no-ops only when nothing at all differs).
func chaptersInSync(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, source string, want []model.Chapter, scopeToSource bool) (bool, error) {
	q := "SELECT position, title, start_ms, end_ms, source FROM chapter WHERE book_item_id = ? AND file_id = ?"
	args := []any{bookItemID, fileID}
	if scopeToSource {
		q += " AND source = ?"
		args = append(args, source)
	}
	q += " ORDER BY position"
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	type have struct {
		c   model.Chapter
		src string
	}
	var stored []have
	for rows.Next() {
		var h have
		if err := rows.Scan(&h.c.Position, &h.c.Title, &h.c.FileStartMS, &h.c.FileEndMS, &h.src); err != nil {
			return false, err
		}
		stored = append(stored, h)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(stored) != len(want) {
		return false, nil
	}
	for i, w := range want {
		h := stored[i]
		if h.src != source || h.c.Position != w.Position || h.c.Title != w.Title ||
			h.c.FileStartMS != w.FileStartMS || h.c.FileEndMS != w.FileEndMS {
			return false, nil
		}
	}
	return true, nil
}

// syncBookSearchFTS rebuilds a book's metadata FTS row (rowid == item id). Title
// carries the heaviest weight; the author sits in the artist column, the series in
// the album column, and the narrator plus genre in the low-weighted extra field.
func syncBookSearchFTS(ctx context.Context, tx *sql.Tx, itemID int64, b model.Book, author string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM search_fts WHERE rowid = ?", itemID); err != nil {
		return err
	}
	var title string
	if err := tx.QueryRowContext(ctx, "SELECT title FROM playable_item WHERE id = ?", itemID).Scan(&title); err != nil {
		return err
	}
	extra := strings.TrimSpace(b.Narrator + " " + b.Genre)
	_, err := tx.ExecContext(ctx,
		"INSERT INTO search_fts(rowid, kind, title, subtitle, artist, album, extra) VALUES (?,?,?,?,?,?,?)",
		itemID, string(model.KindBook), title, b.Subtitle, author, b.Series, extra)
	return err
}

// nullBool renders an optional bool as a nullable INTEGER (NULL when unknown).
func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

// BookByPID returns the full read shape for a book: its item view plus subtitle,
// series placement, contributors, backing parts (in reading order), and chapters
// resolved to book-timeline offsets, with the total summed-across-parts duration.
func (s *Store) BookByPID(ctx context.Context, pid model.PID) (*model.BookDetail, error) {
	const op = "store.BookByPID"
	item, err := s.ItemByPID(ctx, pid)
	if err != nil {
		return nil, err
	}
	if item.Kind != model.KindBook {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "item is not a book: "+string(pid))
	}

	d := &model.BookDetail{Item: item}
	var seriesPID sql.NullString
	var abridged sql.NullInt64
	var bookItemID int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT pi.id, b.subtitle, b.series_seq, b.publisher, b.asin, b.isbn, b.edition, b.abridged,
			b.description, srs.pid
		 FROM playable_item pi JOIN book b ON b.item_id = pi.id
		 LEFT JOIN series srs ON srs.id = b.series_id
		 WHERE pi.pid = ?`, string(pid)).
		Scan(&bookItemID, &d.Subtitle, &d.SeriesSeq, &d.Publisher, &d.ASIN, &d.ISBN, &d.Edition,
			&abridged, &d.Description, &seriesPID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, waxerr.New(waxerr.CodeNotFound, op, "no such book: "+string(pid))
		}
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	d.Series = item.Series
	d.SeriesPID = model.PID(seriesPID.String)
	if abridged.Valid {
		v := abridged.Int64 != 0
		d.Abridged = &v
	}

	contribs, err := s.bookContributors(ctx, bookItemID)
	if err != nil {
		return nil, err
	}
	d.Contributors = contribs
	for _, c := range contribs {
		switch c.Role {
		case model.RoleAuthor:
			d.Authors = append(d.Authors, c.Name)
		case model.RoleNarrator:
			d.Narrators = append(d.Narrators, c.Name)
		case model.RoleTranslator:
			d.Translators = append(d.Translators, c.Name)
		case model.RoleEditor:
			d.Editors = append(d.Editors, c.Name)
		}
	}

	parts, err := s.bookParts(ctx, bookItemID)
	if err != nil {
		return nil, err
	}
	d.Files = make([]model.BookPart, len(parts))
	for i, p := range parts {
		d.Files[i] = p.BookPart
	}

	// The total is the book-timeline length from bookChapters (the sum of effective
	// part durations), so it always covers the chapter span and matches the
	// denormalized book.total_duration_ms used by the list view.
	chapters, total, err := s.bookChapters(ctx, bookItemID, parts)
	if err != nil {
		return nil, err
	}
	d.Chapters = chapters
	d.TotalDurationMS = total
	return d, nil
}

// bookContributors returns a book's role-tagged contributors ordered by role then
// credited position.
func (s *Store) bookContributors(ctx context.Context, bookItemID int64) ([]model.Contributor, error) {
	const op = "store.BookByPID"
	rows, err := s.read.QueryContext(ctx,
		`SELECT a.pid, a.name, ic.role, ic.position
		 FROM item_contributor ic JOIN artist a ON a.id = ic.artist_id
		 WHERE ic.item_id = ? ORDER BY ic.role, ic.position`, bookItemID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.Contributor
	for rows.Next() {
		var c model.Contributor
		var role string
		if err := rows.Scan(&c.ArtistPID, &c.Name, &role, &c.Position); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		c.Role = model.ContributorRole(role)
		out = append(out, c)
	}
	return out, rows.Err()
}

// bookPart pairs the public part shape with internals the reads need (the file's
// rowid for the chapter lookup, its raw path bytes for organize, and a numeric-
// aware sort key over its rel_path), since the model boundary exposes only the
// file pid.
type bookPart struct {
	model.BookPart
	fileID  int64
	path    []byte
	sortKey string
}

// bookParts returns a book's backing files in reading order. It is the single
// source for both the chapter timeline (bookChapters) and organize (ItemFiles).
// Parts order by the stored part position, then a numeric-aware key over the rel
// path, so an unnumbered set ("p2", "p10") sorts naturally rather than
// lexicographically (which would place "p10" before "p2" and corrupt the timeline).
func (s *Store) bookParts(ctx context.Context, bookItemID int64) ([]bookPart, error) {
	const op = "store.bookParts"
	rows, err := s.read.QueryContext(ctx,
		`SELECT f.id, f.pid, f.path, f.display_path, itf.position, COALESCE(f.duration_ms, 0), f.rel_path
		 FROM item_file itf JOIN file f ON f.id = itf.file_id
		 WHERE itf.item_id = ?`, bookItemID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []bookPart
	for rows.Next() {
		var p bookPart
		var rel []byte
		if err := rows.Scan(&p.fileID, &p.FilePID, &p.path, &p.DisplayPath, &p.Position, &p.DurationMS, &rel); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// model.SortKey zero-pads digit runs, so a plain string compare of the keys is
		// numeric-aware ("p0000000002" < "p0000000010").
		p.sortKey = model.SortKey(string(rel))
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].sortKey < out[j].sortKey
	})
	return out, nil
}

// bookChapters resolves a book's chapters into book-timeline order and offsets. It
// walks the parts in reading order, accumulating each part's effective duration, so
// a chapter's stored file-relative offset becomes an offset from the start of the
// whole book. An open (zero) end is filled from the next chapter's start, and the
// final open end from the total duration. It also returns the book-timeline total
// (the accumulated effective durations), so BookByPID's reported total can never
// fall short of the chapter span. CurrentChapter ignores the returned total.
func (s *Store) bookChapters(ctx context.Context, bookItemID int64, parts []bookPart) ([]model.Chapter, int64, error) {
	const op = "store.BookByPID"
	// One query for the whole book's chapters; group them by file in memory, then
	// walk the parts in reading order. Iterating parts (not the chapter rows) is what
	// lets a part with no chapters still advance the cumulative book-timeline offset,
	// and it avoids a per-part round trip on a heavily split book.
	rows, err := s.read.QueryContext(ctx,
		`SELECT file_id, title, start_ms, end_ms, source FROM chapter
		 WHERE book_item_id = ? ORDER BY position, start_ms`, bookItemID)
	if err != nil {
		return nil, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	// Group per file AND per source; a file may briefly carry chapters from more than
	// one source (e.g. the fast-path added .cue chapters beside embedded ones), so
	// pick the single highest-precedence source per file: embedded beats cue.
	bySource := map[int64]map[string][]model.Chapter{}
	for rows.Next() {
		var fid int64
		var source string
		var c model.Chapter
		if err := rows.Scan(&fid, &c.Title, &c.FileStartMS, &c.FileEndMS, &source); err != nil {
			return nil, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if bySource[fid] == nil {
			bySource[fid] = map[string][]model.Chapter{}
		}
		bySource[fid][source] = append(bySource[fid][source], c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	byFile := make(map[int64][]model.Chapter, len(bySource))
	for fid, srcs := range bySource {
		byFile[fid] = preferredChapters(srcs)
	}

	var out []model.Chapter
	var cum int64
	pos := 0
	for _, part := range parts {
		var maxEnd int64 // furthest chapter offset within this part
		for _, c := range byFile[part.fileID] {
			c.FilePID = part.FilePID
			c.Position = pos
			c.StartMS = cum + c.FileStartMS
			if c.FileEndMS > 0 {
				c.EndMS = cum + c.FileEndMS
			}
			if c.FileEndMS > maxEnd {
				maxEnd = c.FileEndMS
			}
			if c.FileStartMS > maxEnd {
				maxEnd = c.FileStartMS
			}
			pos++
			out = append(out, c)
		}
		// Advance the timeline by the part's EFFECTIVE duration: the larger of its
		// file duration and its furthest chapter offset. This both keeps later parts
		// from stacking when a file duration is unknown AND keeps the running total
		// (cum) from falling short of any chapter span, so the reported total and the
		// chapter timeline always agree (the same definition refreshBookDuration and
		// db verify use).
		eff := part.DurationMS
		if maxEnd > eff {
			eff = maxEnd
		}
		cum += eff
	}
	// Fill open-ended chapters from the next chapter's start, and the last from the
	// total book duration, so each chapter has a concrete [start, end) span.
	for i := range out {
		if out[i].EndMS != 0 {
			continue
		}
		if i+1 < len(out) {
			out[i].EndMS = out[i+1].StartMS
		} else {
			out[i].EndMS = cum
		}
	}
	return out, cum, nil
}

// Chapters returns a book's chapters in book-timeline order, the read backing the
// CLI chapter listing and chapter-level resume. CodeNotFound when pid is not a book.
func (s *Store) Chapters(ctx context.Context, pid model.PID) ([]model.Chapter, error) {
	const op = "store.Chapters"
	bookItemID, kind, err := s.itemIDKindByPID(ctx, pid, op)
	if err != nil {
		return nil, err
	}
	if kind != string(model.KindBook) {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "item is not a book: "+string(pid))
	}
	parts, err := s.bookParts(ctx, bookItemID)
	if err != nil {
		return nil, err
	}
	chs, _, err := s.bookChapters(ctx, bookItemID, parts)
	return chs, err
}

// CurrentChapter returns the chapter whose book-timeline span contains positionMS
// (the resume position), or the nearest preceding chapter. It returns nil when the
// book has no chapters. positionMS is clamped into range.
func (s *Store) CurrentChapter(ctx context.Context, pid model.PID, positionMS int64) (*model.Chapter, error) {
	chs, err := s.Chapters(ctx, pid)
	if err != nil {
		return nil, err
	}
	if len(chs) == 0 {
		return nil, nil
	}
	for i := range chs {
		if positionMS < chs[i].EndMS {
			return &chs[i], nil
		}
	}
	return &chs[len(chs)-1], nil
}

// BooksInSeries returns the books of a series in sequence order (the zero-padded
// series_seq_sort, then title), demonstrating decimal/string series ordering.
func (s *Store) BooksInSeries(ctx context.Context, seriesPID model.PID) ([]*model.ItemView, error) {
	const op = "store.BooksInSeries"
	seriesID, err := s.idByPID(ctx, "series", seriesPID, op)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx,
		itemSelect+" WHERE bk.series_id = ? ORDER BY bk.series_seq_sort, pi.sort_key, pi.pid", seriesID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
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

// ItemFiles returns every file backing an item in the same natural reading order
// the chapter timeline uses (one row for a track or single-file book, every part
// for a multi-file book), so organize moves a book's parts in order.
func (s *Store) ItemFiles(ctx context.Context, pid model.PID) ([]model.ItemFileRef, error) {
	const op = "store.ItemFiles"
	itemID, _, err := s.itemIDKindByPID(ctx, pid, op)
	if err != nil {
		return nil, err
	}
	parts, err := s.bookParts(ctx, itemID)
	if err != nil {
		return nil, err
	}
	out := make([]model.ItemFileRef, len(parts))
	for i, p := range parts {
		out[i] = model.ItemFileRef{
			FilePID: p.FilePID, Path: p.path, DisplayPath: p.DisplayPath, Position: p.Position,
		}
	}
	return out, nil
}

// itemIDKindByPID resolves an item pid to its internal id and kind.
func (s *Store) itemIDKindByPID(ctx context.Context, pid model.PID, op string) (int64, string, error) {
	var id int64
	var kind string
	err := s.read.QueryRowContext(ctx, "SELECT id, kind FROM playable_item WHERE pid = ?", string(pid)).Scan(&id, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(pid))
	}
	if err != nil {
		return 0, "", waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, kind, nil
}
