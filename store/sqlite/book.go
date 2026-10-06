package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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

		// One resolution of the book key serves the relink, the overlay and the item write,
		// so an ambiguous identifier is looked up and logged once.
		key, err := resolveItemTx(ctx, tx, s.log, model.KindBook, in.Item.IdentityKey,
			bookAdoptKey{author: in.Book.Author, title: in.Item.Title})
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		fileID, filePID, err := s.resolveScannedFile(ctx, tx, in.LibraryID, in.File, key.accept(), now, res)
		if err != nil {
			return err
		}
		res.FilePID = filePID

		// A track whose file this is, now read as a book, becomes one in place.
		rekinded := false
		if key.id == 0 {
			id, err := rekindItemForFileTx(ctx, tx, fileID, model.KindBook, in.Item.IdentityKey, in.PreserveLocks && !in.KindForced, now)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if id != 0 {
				key, rekinded = &resolvedItem{id: id}, true
				res.MetadataChanged = true
			}
		}

		// Replace the file's sidecar observations (prunes a since-deleted .cue/.lrc so it
		// does not force a full re-hash forever).
		if err := replaceFileAuxTx(ctx, tx, fileID, in.AuxObservations); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// This scan's own diagnostics, plus the derived-under-current-rules stamp (see
		// PutScannedTrack: the stamp is scan-only and must not live in the helper).
		if err := replaceFileDiagnosticsTx(ctx, tx, fileID, model.OriginScan, in.Diagnostics); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := stampDiagVersionTx(ctx, tx, fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		fileTitle, fileBook := in.Item.Title, in.Book

		// Overlay locked book fields, and the fills the part says nothing about, onto the
		// scanned values before any writer runs (overlayStoredBookTx).
		prior, err := overlayStoredBookTx(ctx, tx, s.log, fileID, &in.Book, &in.Item, in.Derived, in.PreserveLocks, key)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Read before upsertItem returns the book to present.
		wasMissing := false
		if prior != nil {
			if wasMissing, err = itemMissingTx(ctx, tx, prior.itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// A rebuild adopts the file's WAXBIN_ITEM_PID stamp (organize stamps books too) to
		// restore the book's original identity; identity stays essence-first, so a taken or
		// invalid hint falls back to a fresh PID. Parts of one book share the stamp: the
		// first to create the item adopts it, the rest join it by book key.
		itemID, itemPID, created, stateChanged, priorTitle, err := upsertItem(ctx, tx, s.log, in.Item, bookAdoptKey{author: in.Book.Author, title: in.Item.Title}, now, in.PreferredItemPID, key)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		res.ItemPID, res.ItemCreated = itemPID, created

		affected := newAffectedRollups()

		position, err := keptPositionTx(ctx, tx, itemID, fileID, in)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Attach this file as a part FIRST, so its role (the first part is the
		// representative 'primary', the rest are 'part', and a copy of a part on disk is
		// an 'alternate') is known before deciding whether it owns the book's metadata,
		// and detach it from any other item.
		dep, err := departingTx(ctx, tx, fileID, in.File.EssenceHash, itemID, in.PreserveLocks)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// A track turned into a book has one part, so every alternate it kept copies or
		// encodes that part and sits at its position.
		if rekinded {
			if _, err := tx.ExecContext(ctx, "UPDATE item_file SET position = ? WHERE item_id = ? AND role = 'alternate'",
				position, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		link, err := linkBookFile(ctx, tx, itemID, fileID, position, in.File, in.LibraryID, wasMissing)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		role := link.role
		res.Joined = link.changed
		res.Promoted = append(res.Promoted, link.promoted...)
		if link.demoted {
			if err := refreshCopyDiagnosticsTx(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		// A part another item brought in moves the places from where it lands on, so they
		// keep their audio, a place where a part starts being that part's. One landing at
		// the end moves nothing: a place at the old end has heard the book, and the new
		// part comes next. The places the other item held come across with the fold below,
		// each inside its own part, and the book stays finished only for a listener who
		// finished that item too.
		if link.changed && role != alternateRole && !link.replaced && dep.arrived(itemID) {
			span, ok, err := partSpanTx(ctx, tx, itemID, fileID)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if ok && !span.last {
				if err := shiftPositionsTx(ctx, tx, itemID, span.offset, span.length); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
			if err := unfinishForArrivalTx(ctx, tx, itemID, dep.sources(itemID), now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		// A surviving item lost a file (e.g. a multi-file book whose part was retagged
		// into another book): it keeps a primary and its rollups and total are refreshed.
		promoted, folded, err := reconcileOrphansTx(ctx, tx, link.orphans, dep, affected)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		res.Promoted, res.Folded = append(res.Promoted, promoted...), folded
		// A book a file the folder rule kept alone makes up takes the key the file's tags
		// give it now (a retitled book with no ALBUM, one named for a renamed folder), so a
		// book found later under the old key is a book of its own.
		if in.Adopted && in.OwnKey != "" && role == bookPrimaryRole {
			rekeyed, err := rekeyOwnBookTx(ctx, tx, itemID, in.OwnKey, now)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			res.MetadataChanged = res.MetadataChanged || rekeyed
		}

		// upsertItem rewrote the title from this part, but the primary part owns the book's
		// metadata, so any other part puts the title back, as does a file the folder rule
		// brought in, whose tags name no book, unless it is the book's only file. On the
		// primary a title the put re-derives settles its provenance here; the other fields
		// settle below.
		ownsMeta := created || role == bookPrimaryRole
		ownsTitle := created || (ownsMeta && !in.Adopted)
		if ownsMeta && !ownsTitle {
			parts, err := queryInt64sTx(ctx, tx, "SELECT file_id FROM item_file WHERE item_id = ? AND role IN ('primary', 'part')", itemID)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			ownsTitle = len(parts) == 1
		}
		title := in.Item.Title
		switch {
		case !ownsTitle && priorTitle != in.Item.Title:
			if _, err := tx.ExecContext(ctx, "UPDATE playable_item SET title=?, sort_key=? WHERE id=?",
				priorTitle, model.SortKey(priorTitle), itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			title = priorTitle
		case ownsTitle && !created:
			var rows map[string]rederivable
			if known := prior.priorFor(itemID); known != nil {
				rows = known.rederivable(in.PreserveLocks)
			} else if rows, err = rederivableRowsTx(ctx, tx, itemID, in.PreserveLocks); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if r, ok := rows["title"]; priorTitle != in.Item.Title || (ok && r.value != in.Item.Title) {
				if _, err := retireProvenanceTx(ctx, tx, itemID, []string{"title"}, in.PreserveLocks); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				res.MetadataChanged = true
				// The retire rewrote the title's row, which the overlay's read still holds.
				prior = nil
			}
		}

		// A copy of a part still on disk adds nothing to the book: no metadata, chapters,
		// cover or acquisition, only its own file row and the alternate edge.
		if role == alternateRole {
			return s.attachBookCopyTx(ctx, tx, in, fileID, filePID, itemID, itemPID, link, stateChanged, fileTitle, title, fileBook, affected, res, now)
		}

		// Custom tags are owned by the primary part, like the book's other metadata.
		// Overlay them onto item_tag every scan (idempotent, honoring per-key locks),
		// before upsertBook so its FTS rebuild picks up the tag values.
		tagsChanged := false
		var tagsReplaced []string
		if ownsMeta {
			c, r, err := syncItemTagsTx(ctx, tx, itemID, in.CustomTags, in.PreserveLocks)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			tagsChanged, tagsReplaced = c, r
		}

		// Only the primary part writes the book's metadata/series/contributors/genres/FTS,
		// on a real change or when its tags disagree with the catalog. Gating the metadata
		// on the primary makes one part the owner, so an asymmetrically-tagged later part
		// can't clobber the book's narrator/series/etc. by scan order. The genres are still
		// collected for every changed part so the genre rollup's summed-across-parts
		// duration reflects a newly attached part.
		changed := created || res.FileCreated || res.ContentChanged || res.Relinked || link.changed || rekinded
		// The fields the primary part's put re-derives, judged before upsertBook replaces
		// them. Any read of the primary re-derives, a forced rescan of unchanged bytes
		// included, the way a track's put does.
		var rederived []string
		var prov map[string]rederivable
		if ownsMeta && !created {
			if rederived, prov, err = rederivedBookTx(ctx, tx, itemID, in.Book, in.PreserveLocks, prior); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		rewrite := ownsMeta && (changed || len(rederived) > 0)
		if changed || rewrite {
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if rewrite {
			if err := upsertBook(ctx, tx, itemID, in.Book, affected); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := settleRederivedBookTx(ctx, tx, itemID, rederived, prov, in.PreserveLocks); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			res.MetadataChanged = res.MetadataChanged || len(rederived) > 0
		}
		if !rewrite && (tagsChanged || res.MetadataChanged) {
			// upsertBook (which rebuilds the FTS row) did not run, so refresh the search row
			// directly or a custom-tag change on the primary part, or a re-derived title,
			// would not be searchable until the next audio change. tagsChanged is only ever
			// set for the primary part.
			if err := rebuildItemSearchFTSTx(ctx, tx, itemID, string(model.KindBook)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
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
		if ownsMeta {
			c, err := attachArtRespectingLockTx(ctx, tx, itemID, in.CoverArt, in.PreserveLocks)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			artChanged = c
		}
		// A book's .cue-sourced chapters are the sidecar analogue of a track's lyrics:
		// both change without the audio bytes changing, so both must reach the scanner's
		// counters rather than reporting as no change at all.
		res.SidecarsChanged = artChanged || chaptersChanged

		// Origin evidence from this part's own tags, recorded only when the book has no
		// acquisition row yet. acquisition is item-level while tags are file-level, so
		// for a multi-file book whichever part is scanned first supplies the row.
		acqAdded, err := insertAcquisitionIfAbsentTx(ctx, tx, itemID, in.Acquisition, in.PreserveLocks)
		if err != nil {
			return err
		}
		if err := settleOwedByScanTx(ctx, tx, fileID, itemID, scanSettle{
			isBook: true, fileTitle: fileTitle, title: title, fileBook: fileBook,
			bookRederived: rewrite, preserveLocks: in.PreserveLocks, derived: in.Derived,
			cover: in.CoverArt, acquisitionRecorded: acqAdded,
			fileTags: in.CustomTags, tagsReplaced: tagsReplaced,
		}); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		kindLocked := false
		if in.LockKind {
			if kindLocked, err = lockKindTx(ctx, tx, itemID, now); err != nil {
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
		if created || res.ContentChanged || res.FileCreated || res.Relinked || link.changed || res.MetadataChanged || chaptersChanged || stateChanged || artChanged || acqAdded || tagsChanged || kindLocked {
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

// resolveScannedFile finds-or-creates the file row for a scanned audio file,
// preserving the pid on a path match (rescan/retag) or an essence match whose old
// path is gone (a move). The book and virtual-track paths share it. Unlike resolveFile
// it returns no prior-essence hash, because neither caller does the in-place identity
// preservation PutScannedTrack runs when an essence-algorithm change leaves the bytes
// untouched. A book keys on its book key (ASIN/ISBN/title), so it is
// essence-independent. A virtual track's key embeds the file essence, so an
// essence-algorithm change re-keys the whole set and forks the tracks, the way a
// whole-file track without an MBID would if PutScannedTrack did not preserve it in
// place. That is a known limitation, unreachable before 1.0 (the essence version is
// frozen); a real re-encode changes the bytes and reconciles normally.
func (s *Store) resolveScannedFile(ctx context.Context, tx *sql.Tx, libraryID int64, file model.File, accept map[int64]bool, now int64, res *model.ScanItemResult) (int64, model.PID, error) {
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
		relink, err := fileByEssenceGoneTx(ctx, tx, file.EssenceHash, file.ContentHash, libraryID, accept)
		if err != nil {
			return 0, "", err
		}
		if relink != nil {
			res.Relinked = true
			res.RelinkedFrom = string(relink.Path)
			res.ContentChanged = relink.ContentHash != file.ContentHash
			if err := updateFileRow(ctx, tx, relink.ID, file, now); err != nil {
				return 0, "", err
			}
			if err := renameCopyDetailsTx(ctx, tx, relink.ID, relink.DisplayPath, file.DisplayPath); err != nil {
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
		 series_seq_sort, year, publisher, asin, isbn, isbn_key, edition, abridged, description, genre, mbid,
		 track_total)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET
			subtitle=excluded.subtitle, author=excluded.author, author_sort=excluded.author_sort,
			author_id=excluded.author_id, narrator=excluded.narrator, series_id=excluded.series_id,
			series_seq=excluded.series_seq, series_seq_sort=excluded.series_seq_sort, year=excluded.year,
			publisher=excluded.publisher, asin=excluded.asin, isbn=excluded.isbn,
			isbn_key=excluded.isbn_key, edition=excluded.edition,
			abridged=excluded.abridged, description=excluded.description, genre=excluded.genre, mbid=excluded.mbid,
			track_total=excluded.track_total`,
		itemID, b.Subtitle, author, authorSort, nullInt64(authorID), b.Narrator, nullInt64(seriesID),
		b.SeriesSeq, model.SortKey(b.SeriesSeq), nullInt(b.Year), b.Publisher, b.ASIN, b.ISBN,
		identity.ISBNKey(b.ISBN), b.Edition, nullBool(b.Abridged), b.Description, b.Genre,
		nullStr(b.MBID), nullInt(b.TrackTotal)); err != nil {
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

// bookLink is what linkBookFile did: the role the file holds in the book, whether the
// edge changed, the items the detach left behind, the part an alternate copies or encodes,
// whether a part became an alternate (the file took the place of a gone part, or of a
// lesser encoding, or yielded its own to a better one), whether the file took another
// file's place in the book's timeline, and an alternate it promoted in its own place.
type bookLink struct {
	role     string
	changed  bool
	orphans  []int64
	twin     *bookTwin
	encoding *bookTwin
	demoted  bool
	replaced bool
	promoted []model.PromotedFile
}

// bookTwin is a part of a book on another file with the same audio as an arriving file.
type bookTwin struct {
	fileID   int64
	role     string
	position int
	path     []byte
	display  string
}

// linkBookFile attaches fileID to the book at the given part position and detaches
// it from any other item (a file belongs to exactly one item). An existing part keeps
// its role. A file that holds no part of the book and has the audio of one of its parts
// is a copy, an alternate at that part's position, even while the part's path is missing:
// mid-walk the part may have moved to a folder not reached yet, so a copy takes a gone
// part's place only once reconciliation has marked the book missing (bookMissing), the
// part staying as an alternate. A part whose audio the book's primary or a lower file id
// also holds is a copy cataloged as a part of its own before copies became alternates,
// and folds into one now.
//
// Another encoding of the part at its position (model.OtherEncoding) is that part's alternate
// unless it ranks ahead of it the way a track's encodings rank (outranksPrimaryTx); then
// it takes the part's place and the part becomes its alternate, the replacement leaving
// the book's timeline as it was. A part read again yields its place the same way to a
// better encoding the book holds on disk (outrankingAlternateTx), so the walk order does
// not decide which one is the part. A part off disk is replaced only once the book is
// missing, since it may have moved.
func linkBookFile(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, position int, f model.File, libraryID int64, bookMissing bool) (*bookLink, error) {
	essence := f.EssenceHash
	prev, err := queryInt64sTx(ctx, tx,
		"SELECT DISTINCT item_id FROM item_file WHERE file_id = ? AND item_id <> ?", fileID, bookItemID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_file WHERE file_id = ? AND item_id <> ?", fileID, bookItemID); err != nil {
		return nil, err
	}
	link := &bookLink{orphans: prev, changed: len(prev) > 0}

	var curRole string
	curPos := -1
	err = tx.QueryRowContext(ctx,
		"SELECT role, position FROM item_file WHERE item_id = ? AND file_id = ? LIMIT 1", bookItemID, fileID).Scan(&curRole, &curPos)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	role := curRole
	if role != bookPrimaryRole {
		var twin *bookTwin
		if essence != "" {
			if twin, err = bookTwinTx(ctx, tx, bookItemID, fileID, essence); err != nil {
				return nil, err
			}
		}
		switch {
		case twin != nil && role != bookPartRole && bookMissing && !pathExists(twin.path):
			if err := demoteBookPartTx(ctx, tx, bookItemID, twin.fileID); err != nil {
				return nil, err
			}
			role, position, link.demoted, link.replaced = twin.role, twin.position, true, true
		case twin != nil && (role != bookPartRole || twin.role == bookPrimaryRole || twin.fileID < fileID):
			role, position, link.twin = alternateRole, twin.position, twin
		case role == alternateRole:
			// Another encoding of a part (a track's encoding alternate the item kept when
			// it turned into a book): it copies no part's audio, and stays a copy.
			position = curPos
		case role != bookPartRole:
			role = ""
		}
	}
	switch {
	case role == "":
		enc, encRole, err := partEncodingTx(ctx, tx, bookItemID, fileID, position, f)
		if err != nil {
			return nil, err
		}
		if enc == nil {
			break
		}
		take := bookMissing
		if pathExists(enc.path) {
			if take, err = outranksPrimaryTx(ctx, tx, libraryID, f, enc); err != nil {
				return nil, err
			}
		}
		if !take {
			role, link.encoding = alternateRole, &bookTwin{fileID: enc.fileID, role: encRole, position: position, path: enc.path, display: enc.display}
			break
		}
		if err := demoteBookPartTx(ctx, tx, bookItemID, enc.fileID); err != nil {
			return nil, err
		}
		role, link.demoted, link.replaced = encRole, true, true
	case role == bookPrimaryRole || role == bookPartRole:
		better, err := outrankingAlternateTx(ctx, tx, bookItemID, libraryID, f, func(c altCandidate) bool {
			return c.position == position && model.OtherEncoding(f, c.file())
		})
		if err != nil || better == nil {
			if err != nil {
				return nil, err
			}
			break
		}
		if _, err := tx.ExecContext(ctx, "UPDATE item_file SET role = ? WHERE item_id = ? AND file_id = ?",
			role, bookItemID, better.fileID); err != nil {
			return nil, err
		}
		if err := unstampTx(ctx, tx, better.fileID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM chapter WHERE book_item_id = ? AND file_id = ?", bookItemID, fileID); err != nil {
			return nil, err
		}
		link.promoted = append(link.promoted, model.PromotedFile{FilePID: better.pid, LibraryID: better.libraryID, Path: better.path})
		role, link.demoted = alternateRole, true
		link.encoding = &bookTwin{fileID: better.fileID, role: role, position: position, path: better.path, display: better.display}
	}
	if role == "" {
		var hasPrimary int
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM item_file WHERE item_id = ? AND role = 'primary')", bookItemID).Scan(&hasPrimary); err != nil {
			return nil, err
		}
		role = bookPrimaryRole
		if hasPrimary == 1 {
			role = bookPartRole
		}
	}
	link.role = role
	if role == curRole && position == curPos {
		return link, nil
	}
	link.changed = true
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_file WHERE item_id = ? AND file_id = ?", bookItemID, fileID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO item_file(item_id, file_id, role, position) VALUES (?,?,?,?)",
		bookItemID, fileID, role, position); err != nil {
		return nil, err
	}
	return link, nil
}

// keptPositionTx is the position a book part takes: the one the scan read, with the disc
// or the place the file does not state (PutScannedBookInput.DiscUnstated, PlaceUnstated)
// kept from the position the catalog holds for the file, in this book first.
func keptPositionTx(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, in model.PutScannedBookInput) (int, error) {
	if !in.DiscUnstated && !in.PlaceUnstated {
		return in.Position, nil
	}
	var stored int
	err := tx.QueryRowContext(ctx, `SELECT position FROM item_file WHERE file_id = ?
		ORDER BY item_id = ? DESC, role = 'alternate' LIMIT 1`, fileID, bookItemID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return in.Position, nil
	}
	if err != nil {
		return 0, err
	}
	disc, place := model.SplitPartPosition(in.Position)
	storedDisc, storedPlace := model.SplitPartPosition(stored)
	if in.DiscUnstated {
		disc = storedDisc
	}
	if in.PlaceUnstated {
		place = storedPlace
	}
	return model.PartPosition(disc, place), nil
}

// rekeyOwnBookTx gives a book of one part the key its file's own tags give it, unless
// another book holds that key or it says less than the book's own (bookKeyRank).
func rekeyOwnBookTx(ctx context.Context, tx *sql.Tx, itemID int64, own string, now int64) (bool, error) {
	var cur string
	var parts int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(identity_key, ''),
		(SELECT COUNT(*) FROM item_file WHERE item_id = ? AND role IN ('primary', 'part'))
		FROM playable_item WHERE id = ?`, itemID, itemID).Scan(&cur, &parts); err != nil {
		return false, err
	}
	if parts != 1 || own == cur || bookKeyRank(own) < bookKeyRank(cur) {
		return false, nil
	}
	var held bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM playable_item
		WHERE kind = 'book' AND identity_key = ? AND id <> ?)`, own, itemID).Scan(&held); err != nil || held {
		return false, err
	}
	_, err := tx.ExecContext(ctx, "UPDATE playable_item SET identity_key = ?, updated_at = ? WHERE id = ?", own, now, itemID)
	return err == nil, err
}

// bookKeyRank orders book keys by what they say: an identifier, then a title with an
// author, then a title alone, then an essence fallback.
func bookKeyRank(key string) int {
	switch {
	case strings.HasPrefix(key, "asin:"), strings.HasPrefix(key, "isbn:"):
		return 3
	case strings.HasPrefix(key, "book:"):
		if author, _, _ := strings.Cut(strings.TrimPrefix(key, "book:"), "\x1f"); author != "" {
			return 2
		}
		return 1
	}
	return 0
}

// bookTwinTx returns the book's part on another file with the given essence, the primary
// first and then the lowest file id, or nil.
func bookTwinTx(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, essence string) (*bookTwin, error) {
	var tw bookTwin
	err := tx.QueryRowContext(ctx, `SELECT f.id, itf.role, itf.position, f.path, f.display_path
		FROM item_file itf JOIN file f ON f.id = itf.file_id
		WHERE itf.item_id = ? AND itf.file_id <> ? AND itf.role IN ('primary', 'part') AND f.essence_hash = ?
		ORDER BY itf.role = 'primary' DESC, f.id LIMIT 1`, bookItemID, fileID, essence).Scan(&tw.fileID, &tw.role, &tw.position, &tw.path, &tw.display)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tw, nil
}

// demoteBookPartTx makes a book's part on fileID an alternate, dropping the chapters it
// held as a part.
func demoteBookPartTx(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64) error {
	if _, err := tx.ExecContext(ctx, "UPDATE item_file SET role = 'alternate' WHERE item_id = ? AND file_id = ?",
		bookItemID, fileID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM chapter WHERE book_item_id = ? AND file_id = ?", bookItemID, fileID)
	return err
}

// partEncodingTx returns the book's part at position that f is another encoding of
// (model.OtherEncoding), primary first, with its role, or nil.
func partEncodingTx(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, position int, f model.File) (*standingPrimary, string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT f.id, f.path, f.display_path, COALESCE(f.essence_hash, ''), itf.role,
			COALESCE(f.codec, ''), COALESCE(f.bitrate, 0), COALESCE(f.sample_rate, 0), COALESCE(f.bit_depth, 0),
			COALESCE(f.duration_ms, 0), l.read_only
		FROM item_file itf JOIN file f ON f.id = itf.file_id JOIN library l ON l.id = f.library_id
		WHERE itf.item_id = ? AND itf.file_id <> ? AND itf.position = ? AND itf.role IN ('primary', 'part')
		ORDER BY itf.role = 'primary' DESC, f.id`, bookItemID, fileID, position)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var sp standingPrimary
		var role string
		q := &sp.quality
		if err := rows.Scan(&sp.fileID, &sp.path, &sp.display, &sp.essence, &role,
			&q.Codec, &q.Bitrate, &q.SampleRate, &q.BitDepth, &q.DurationMS, &sp.readOnly); err != nil {
			return nil, "", err
		}
		q.EssenceHash = sp.essence
		if model.OtherEncoding(f, *q) {
			return &sp, role, nil
		}
	}
	return nil, "", rows.Err()
}

// attachBookCopyTx is PutScannedBook's copy branch: the file is an alternate of the part
// it copies, so the book's metadata, chapters, cover and acquisition are left alone. Any
// chapters a former part edge stored for the file go, and the owed rows its tags pay are
// settled against the stored book. The book emits an update when the edge changed or
// the put brought it back from missing (stateChanged).
func (s *Store) attachBookCopyTx(ctx context.Context, tx *sql.Tx, in model.PutScannedBookInput, fileID int64, filePID model.PID,
	itemID int64, itemPID model.PID, link *bookLink, stateChanged bool, fileTitle, title string, fileBook model.Book, affected *affectedRollups, res *model.ScanItemResult, now int64) error {
	const op = "store.PutScannedBook"
	res.AttachedAsCopy, res.Joined = true, link.changed
	d := model.FileDiagnostic{Code: model.DiagDuplicateCopy, Severity: model.SeverityInfo}
	if link.twin != nil {
		d.Detail = link.twin.display
	} else if link.encoding != nil {
		d.Code, d.Detail = model.DiagAlternateEncoding, link.encoding.display
	} else if primary, err := primaryFileTx(ctx, tx, itemID); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	} else if primary != nil {
		d = copyDiagnostic(in.File.EssenceHash, primary)
	}
	if err := upsertFileDiagnosticTx(ctx, tx, fileID, model.OriginScan, "", d, now); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM chapter WHERE book_item_id = ? AND file_id = ?", itemID, fileID); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if link.changed {
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := refreshBookDuration(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if !affected.empty() {
		if err := maintainRollupsTx(ctx, tx, affected, now); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if err := settleOwedByScanTx(ctx, tx, fileID, itemID, scanSettle{
		isBook: true, fileTitle: fileTitle, title: title, fileBook: fileBook,
		preserveLocks: in.PreserveLocks, derived: in.Derived, cover: in.CoverArt, fileTags: in.CustomTags,
	}); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if res.FileCreated || res.ContentChanged || res.Relinked {
		if err := appendChange(ctx, tx, "file", filePID, opFor(res.FileCreated)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if link.changed || stateChanged {
		if err := appendChange(ctx, tx, "item", itemPID, model.OpUpdate); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return nil
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
	FROM item_file itf JOIN file f ON f.id = itf.file_id
	WHERE itf.item_id = %s AND itf.role IN ('primary', 'part'))`

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

// refreshAllBookDurations recomputes every book's total from its current parts. The
// per-write refreshes keep the column current; this whole-catalog pass is the repair
// for drift `db verify` reports, and it is the only way back for a book that lost its
// last part before the detach path shed the total, since a rescan cannot help when
// the file it would re-read is gone.
func refreshAllBookDurations(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE book SET total_duration_ms = "+fmt.Sprintf(bookEffectiveDurationSum, "book.item_id"))
	return err
}

// chapterSourceRank orders chapter sources by precedence (lower wins). A remote
// podcast:chapters JSON is richest and outranks embedded chapters (the documented
// episode contract); for books (which never carry podcast_url) embedded chapters are
// authoritative over an external .cue, and a synthesized single chapter ranks below
// a real source. One ordering serves both kinds because their source sets are
// disjoint (books: embedded/cue/synthetic; episodes: podcast_url).
// These literals are not model.ProvenanceSource: they name a derivation method, which
// is what the ranking is over, so unifying the two vocabularies would break it.
func chapterSourceRank(source string) int {
	switch source {
	case "user":
		// A user-curated chapter list is authoritative over any derived source.
		return -1
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
// intact. It serves the sources that arrive without a scan of the audio: a podcast's
// chapter URL and a user's own chapters.
func syncChaptersForFileSource(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, source string, chapters []model.Chapter) (bool, error) {
	return syncChapters(ctx, tx, bookItemID, fileID, source, chapters, true)
}

// syncChapters replaces a file's chapters with the desired set tagged with source,
// reporting whether it changed anything. scopeToSource limits the replace (and the
// no-op comparison) to rows of that source; otherwise it replaces every source's
// rows for the file. It no-ops (no write, no change) when the stored rows already
// match, so a no-op rescan stays change_log-silent.
func syncChapters(ctx context.Context, tx *sql.Tx, bookItemID, fileID int64, source string, chapters []model.Chapter, scopeToSource bool) (bool, error) {
	chapters = normalizeChapterStarts(chapters)
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
	} else {
		// An all-source scan replace never touches user-curated chapters, so a
		// `scan --force` cannot discard them (they win on read via chapterSourceRank).
		del += " AND source <> 'user'"
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

// normalizeChapterStarts puts one file's chapters in start order and collapses any
// that share a start, keeping the last of them. The read path spans each chapter to
// the next start, so an earlier chapter at the same instant would have no span, and
// at offset 0 it would read back as open-ended. Start-only sources make the collision
// real (an ASF marker inside the preroll lands at 0 beside the first real one), and
// SetItemChapters refuses such a list, so the stored rows must never hold one. A
// curated list arrives already ordered and distinct, so this leaves it as it is.
func normalizeChapterStarts(chs []model.Chapter) []model.Chapter {
	out := slices.Clone(chs)
	slices.SortStableFunc(out, func(a, b model.Chapter) int { return cmp.Compare(a.FileStartMS, b.FileStartMS) })
	n := 0
	for _, c := range out {
		if n > 0 && out[n-1].FileStartMS == c.FileStartMS {
			out[n-1] = c
			continue
		}
		out[n] = c
		n++
	}
	out = out[:n]
	for i := range out {
		out[i].Position = i
	}
	return out
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
	} else {
		// Mirror the all-source replace, which never deletes user chapters, so the
		// no-op comparison ignores them too (a scan stays silent when only its own
		// derived sources are unchanged).
		q += " AND source <> 'user'"
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

// syncBookSearchFTS rebuilds a book's search row (bookSearchRowTx) from the values
// the caller just stored, author being the display the book row holds.
func syncBookSearchFTS(ctx context.Context, tx *sql.Tx, itemID int64, b model.Book, author string) error {
	r, err := bookSearchRowTx(ctx, tx, itemID, b, author)
	if err != nil {
		return err
	}
	return writeSearchRowTx(ctx, tx, itemID, r)
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
	fileID     int64
	path       []byte
	sortKey    string
	role       string // the item_file edge role: primary or part
	libraryPID model.PID
	virtual    bool // the edge plays a window of the file
}

// bookParts returns a book's parts in reading order, leaving out alternates. It is the
// single source for both the chapter timeline (bookChapters) and organize (ItemFiles).
// Parts order by the stored part position, then a numeric-aware key over the rel
// path, so an unnumbered set ("p2", "p10") sorts naturally rather than
// lexicographically (which would place "p10" before "p2" and corrupt the timeline).
func (s *Store) bookParts(ctx context.Context, bookItemID int64) ([]bookPart, error) {
	return bookPartsQ(ctx, s.read, bookItemID)
}

// bookPartsQ is bookParts over an explicit queryer, so the user-chapter write
// path can read the parts inside its own transaction (the same reading order the
// read timeline uses).
func bookPartsQ(ctx context.Context, q queryer, bookItemID int64) ([]bookPart, error) {
	const op = "store.bookParts"
	rows, err := q.QueryContext(ctx,
		`SELECT f.id, f.pid, f.path, f.display_path, itf.position, COALESCE(f.duration_ms, 0), f.rel_path, itf.role,
			(SELECT l.pid FROM library l WHERE l.id = f.library_id), itf.start_frames IS NOT NULL
		 FROM item_file itf JOIN file f ON f.id = itf.file_id
		 WHERE itf.item_id = ? AND itf.role IN ('primary', 'part')`, bookItemID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []bookPart
	for rows.Next() {
		var p bookPart
		var rel []byte
		if err := rows.Scan(&p.fileID, &p.FilePID, &p.path, &p.DisplayPath, &p.Position, &p.DurationMS, &rel, &p.role, &p.libraryPID, &p.virtual); err != nil {
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
	// The query has no ORDER BY, and folding can give two part names one key, so the
	// file id breaks the last tie rather than leaving reading order to row order.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		if out[i].sortKey != out[j].sortKey {
			return out[i].sortKey < out[j].sortKey
		}
		return out[i].fileID < out[j].fileID
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
	// one source (podcast_url chapters beside embedded ones, say), so pick the single
	// highest-precedence source per file: embedded beats cue.
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
	// User chapters are authored against the whole book timeline (SetItemChapters
	// splits one flat list across the parts), so their presence on any part
	// suppresses the derived sources book-wide: a part the user's list leaves
	// uncovered stays empty rather than falling back to its scanned chapters,
	// which would interleave two navigations. Derived sources keep per-file
	// precedence, since different parts can legitimately carry different sources
	// (one part embedded, another cue).
	userCurated := false
	for _, srcs := range bySource {
		if len(srcs["user"]) > 0 {
			userCurated = true
			break
		}
	}
	byFile := make(map[int64][]model.Chapter, len(bySource))
	// derivedFloor is each file's preferred derived source's furthest chapter
	// offset, kept when user curation suppresses those rows from the output: the
	// timeline must still advance past the content the derived chapters prove
	// exists (a part with an unknown file duration would otherwise contribute
	// nothing), or every later part's chapters would shift away from the
	// timeline the curation was authored and split against.
	var derivedFloor map[int64]int64
	if userCurated {
		derivedFloor = make(map[int64]int64, len(bySource))
	}
	for fid, srcs := range bySource {
		if !userCurated {
			byFile[fid] = preferredChapters(srcs)
			continue
		}
		byFile[fid] = srcs["user"]
		derived := make(map[string][]model.Chapter, len(srcs))
		for src, chs := range srcs {
			if src != "user" {
				derived[src] = chs
			}
		}
		var ext int64
		for _, c := range preferredChapters(derived) {
			if c.FileEndMS > ext {
				ext = c.FileEndMS
			}
			if c.FileStartMS > ext {
				ext = c.FileStartMS
			}
		}
		derivedFloor[fid] = ext
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
		// db verify use). Under user curation the suppressed derived chapters still
		// floor the advance (derivedFloor), matching the timeline splitBookChapters
		// mapped the curation against.
		eff := part.DurationMS
		if maxEnd > eff {
			eff = maxEnd
		}
		if f := derivedFloor[part.fileID]; f > eff {
			eff = f
		}
		cum += eff
	}
	// Fill open-ended chapters from the next chapter's start, and the last from the
	// total book duration, so each chapter has a concrete [start, end) span. Starts
	// are distinct within a file (normalizeChapterStarts), so the span is never empty.
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

// ItemFiles returns every file backing an item: its parts in the same natural reading
// order the chapter timeline uses (one row for a track or single-file book, every part
// for a multi-file book), so organize moves a book's parts in order, then its
// alternates by position.
//
// Each ref carries its item_file edge role alongside the position. The two are
// independent: the primary is whichever part was attached first, or the
// lowest-positioned survivor after a primary is detached, so it is not reading-order
// part one and cannot be inferred from Position. A caller that needs to know which
// part the primary-file reads (LoadPeaks, LoudnessByItem) answered for has to read
// Role.
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
			FilePID: p.FilePID, Path: p.path, DisplayPath: p.DisplayPath, Position: p.Position, Role: p.role,
			LibraryPID: p.libraryPID, Virtual: p.virtual,
		}
	}
	rows, err := s.read.QueryContext(ctx, `SELECT f.pid, f.path, f.display_path, itf.position, l.pid, itf.start_frames IS NOT NULL
		FROM item_file itf JOIN file f ON f.id = itf.file_id JOIN library l ON l.id = f.library_id
		WHERE itf.item_id = ? AND itf.role = 'alternate' ORDER BY itf.position, f.id`, itemID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	for rows.Next() {
		ref := model.ItemFileRef{Role: alternateRole}
		if err := rows.Scan(&ref.FilePID, &ref.Path, &ref.DisplayPath, &ref.Position, &ref.LibraryPID, &ref.Virtual); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
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

// BookByKey returns the book a library holds under an identity key, its primary file in
// the library, with its parts in reading order (no alternates), or nil when it holds none.
func (s *Store) BookByKey(ctx context.Context, libraryID int64, key string) (*model.ItemView, []model.ItemFileRef, error) {
	const op = "store.BookByKey"
	var pid string
	err := s.read.QueryRowContext(ctx, `SELECT pi.pid FROM playable_item pi
		JOIN item_file itf ON itf.item_id = pi.id AND itf.role = 'primary'
		JOIN file f ON f.id = itf.file_id
		WHERE pi.kind = 'book' AND pi.identity_key = ? AND f.library_id = ? LIMIT 1`, key, libraryID).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	view, err := s.ItemByPID(ctx, model.PID(pid))
	if err != nil {
		return nil, nil, err
	}
	files, err := s.ItemFiles(ctx, model.PID(pid))
	if err != nil {
		return nil, nil, err
	}
	return view, slices.DeleteFunc(files, func(f model.ItemFileRef) bool { return f.Role == alternateRole }), nil
}
