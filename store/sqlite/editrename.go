package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file holds the batch-level rename pre-pass inside the edit transaction. It
// detects "every member of an entity moves at once to one new key" and rewrites the
// entity chain in place, so the entity keeps its id, pid, curation, art, play state,
// and enrichment marker instead of ghosting while a fresh row takes its members. On a
// key collision (the rename lands on a key another row already owns) the old entity
// auto-merges into the incumbent through mergeEntityTx: incumbent survives with its
// pid, locked-wins curation, survivor-wins art, star fold, loser OpDelete. Every
// condition failure falls back to today's split-and-ghost behavior. The pre-pass
// runs in chain order, artist stage, then release-group stage, then per-album
// groups; the first two are planned at batch level because one artist or release
// group can back several of the batch's albums. It writes entity rows only, never
// track rows, provenance, or item deltas; the merge branches write what
// mergeEntityTx always wrote.
//
// Books participate too. A book's author is an artist entity like a track credit, so
// an author edit is planned in the same artist stage and covered by the same
// all-references-move checks; the series above the book gets its own stage after it.
// A series carries no art, curation, or play state, so its rename preserves the pid
// and the deltas hanging off it and nothing else, and a taken series key merges into
// the incumbent the way the artist and release-group stages do.
//
// Durability: the rename is DB-only. It survives an unforced rescan (the entity block
// is gated on content change) and a forced rescan when the edited fields are locked
// (the CLI default); an unlocked forced rescan re-derives from tags and re-forks, the
// same contract as any DB edit. Tag write-back closes that gap.
//
// Stale identifiers: an enrichment-filled album.mbid or release_group.mbid column
// survives the rename untouched, which is right for the typo-fix premise. A genuine
// re-identification clears the entity mbid through `waxbin entity`, which is a
// different gesture with a different reach: it takes back the enrichment marker as
// well and re-keys the chain to the heuristic form a scan computes, so the members
// follow the row instead of staying pinned to the disowned id (entityedit.go).
//
// Swaps: when A renames onto B's key while B renames away in the same batch, A's
// group merges into B first and B's group then fails the all-members check (its count
// now includes A's members) and splits, so a swap can produce a merge-then-split, not
// just a plain split for the second group.
//
// A fallback that drains its album is not left to ghost. The per-item resolve behind the
// pre-pass runs the scan-side re-key reconciliation (scanreconcile.go), and an edit moves
// no file, so both keys name the folder the item still sits in and the drained row
// carries onto the new one with its pid and attachments. The swap's second group is not
// one of those: the merge left it holding the first group's members, so it never drains.

// renameMember is one pre-pass participant: the batch entry, the entity chain it
// currently sits on, and the chain its overlaid edit implies.
type renameMember struct {
	entry                                     editEntry
	curArtistID, curAlbumArtistID, curAlbumID int64
	tr                                        model.Track // loaded track with the participating fields overlaid
	anchor                                    string      // overlaid release-group anchor credit (the raw string)
	newRGKey, newAlbumKey                     string
	hasPath                                   bool // the item has a primary file, so the folder-keyed album key is real
	editsArtist                               bool
	creditNames                               []string // the overlaid performing credit's artists
	creditPrimary                             string   // first of creditNames ("" for a cleared credit)
	anchorPrimary                             string   // primary of the overlaid anchor credit
	preAnchorPrimary                          string   // primary of the anchor before the overlay
}

// buildRenameMember loads one participant's scan-equivalent state (loadTrackForEditTx,
// after the mbid carryover and derived-credit reconstruction) and derives the chain
// keys its overlaid edit implies. Overlaying only the participating fields through
// applyTrackEdit keeps parse and split semantics identical to the apply loop, so a bad
// year aborts here with the same error the apply would give.
func buildRenameMember(ctx context.Context, tx *sql.Tx, e editEntry, op string) (*renameMember, error) {
	cur, _, filePath, err := loadTrackForEditTx(ctx, tx, e.itemID)
	if err != nil {
		return nil, err
	}
	m := &renameMember{entry: e, tr: cur}
	var artistID, albumArtistID, albumID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT artist_id, album_artist_id, album_id FROM track WHERE item_id=?", e.itemID).
		Scan(&artistID, &albumArtistID, &albumID); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	m.curArtistID, m.curAlbumArtistID, m.curAlbumID = artistID.Int64, albumArtistID.Int64, albumID.Int64
	m.hasPath = len(filePath) > 0
	preAnchor, _, _ := albumChainKeys(cur, filePath)
	if pres, _ := creditNames(preAnchor, nil, nil); len(pres) > 0 {
		m.preAnchorPrimary = pres[0]
	}
	for _, f := range e.fields {
		if !editKeyFields[f] {
			continue
		}
		if err := applyTrackEdit(&m.tr, f, e.norm[f], op); err != nil {
			return nil, err
		}
		if f == "artist" && len(e.credits) > 0 {
			// The credit path already split its names; the joined display the apply
			// stores cannot be split back into them.
			m.tr.Artists = e.credits
		}
	}
	m.anchor, m.newRGKey, m.newAlbumKey = albumChainKeys(m.tr, filePath)

	// The credit primaries the artist stage compares, through the same creditNames
	// the apply loop's resolution uses. The edit path never carries file-stated
	// MusicBrainz artist ids (the track table does not persist them), so passing no
	// ids matches the apply loop exactly.
	m.editsArtist = slices.Contains(e.fields, "artist")
	m.creditNames, _ = creditNames(m.tr.Artist, m.tr.Artists, nil)
	if len(m.creditNames) > 0 {
		m.creditPrimary = m.creditNames[0]
	}
	if anchors, _ := creditNames(m.anchor, nil, nil); len(anchors) > 0 {
		m.anchorPrimary = anchors[0]
	}
	return m, nil
}

// bookRenameMember is one book pre-pass participant: the batch entry, the author and
// series entities it currently sits on, and the overlaid book its edit implies.
type bookRenameMember struct {
	entry                    editEntry
	curAuthorID, curSeriesID int64
	b                        model.Book // loaded book with the participating fields overlaid
	authorPrimary            string     // primary of the overlaid author credit
	editsAuthor, editsSeries bool
	newSeries                string // the overlaid series name
}

// buildBookRenameMember loads one book participant through loadBookForEditTx, the same
// reader the apply loop uses, and overlays the participating fields through
// applyBookEdit so the split semantics match what the apply will resolve. Every credit
// field in the batch is overlaid, not just the key-bearing author: the fold-back guard
// reads them, and a batch can hand one name from the author to the narrator.
func buildBookRenameMember(ctx context.Context, tx *sql.Tx, e editEntry, op string) (*bookRenameMember, error) {
	cur, _, err := loadBookForEditTx(ctx, tx, e.itemID)
	if err != nil {
		return nil, err
	}
	m := &bookRenameMember{entry: e, b: cur}
	var authorID, seriesID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT author_id, series_id FROM book WHERE item_id=?", e.itemID).
		Scan(&authorID, &seriesID); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	m.curAuthorID, m.curSeriesID = authorID.Int64, seriesID.Int64
	for _, f := range e.fields {
		if !bookKeyFields[f] && !bookCreditFields[f] {
			continue
		}
		if err := applyBookEdit(&m.b, f, e.norm[f], op); err != nil {
			return nil, err
		}
		if f == "author" && len(e.credits) > 0 {
			// See buildRenameMember: the credit path's list is authoritative.
			m.b.Authors = e.credits
		}
	}
	m.editsAuthor = slices.Contains(e.fields, "author")
	m.editsSeries = slices.Contains(e.fields, "series")
	if len(m.b.Authors) > 0 {
		m.authorPrimary = m.b.Authors[0]
	}
	m.newSeries = m.b.Series
	return m, nil
}

// overlaidCredits returns every artist name the overlaid book credits, so the fold-back
// guard sees a name the batch moves from one credit to another.
func (m *bookRenameMember) overlaidCredits() []string {
	out := make([]string, 0, len(m.b.Authors)+len(m.b.Narrators))
	out = append(out, m.b.Authors...)
	return append(out, m.b.Narrators...)
}

// renameEntitiesForEditsTx runs the pre-pass over the batch: it builds the
// participants (track entries editing a chain-key field, book entries editing an
// author or series), groups the tracks by their current album, and renames or merges
// each whole-set group's chain. Groups execute in ascending album id with fresh
// per-group reads, so an earlier group's rename or merge is visible to later ones; a
// cross-rename swap inside one batch therefore falls back to split for the second
// group (documented above, not optimized).
func renameEntitiesForEditsTx(ctx context.Context, tx *sql.Tx, log logger, entries []editEntry, affected *affectedRollups, op string) error {
	var members []*renameMember
	var books []*bookRenameMember
	for _, e := range entries {
		switch e.kind {
		case string(model.KindTrack):
			if !slices.ContainsFunc(e.fields, func(f string) bool { return editKeyFields[f] }) {
				continue
			}
			m, err := buildRenameMember(ctx, tx, e, op)
			if err != nil {
				return err
			}
			members = append(members, m)
		case string(model.KindBook):
			if !slices.ContainsFunc(e.fields, func(f string) bool { return bookKeyFields[f] }) {
				continue
			}
			m, err := buildBookRenameMember(ctx, tx, e, op)
			if err != nil {
				return err
			}
			books = append(books, m)
		}
	}
	if len(members) == 0 && len(books) == 0 {
		return nil
	}

	groups := map[int64][]*renameMember{}
	for _, m := range members {
		if m.curAlbumID != 0 {
			groups[m.curAlbumID] = append(groups[m.curAlbumID], m)
		}
	}
	// Chain order artist -> release_group -> album: the artist stage is planned with
	// the album groups and executes first, then the release-group stage (batch-level,
	// since one group can back several of the batch's albums), then the album groups.
	// The series stage follows the artist stage, the book chain's own second rung.
	if err := renameArtistsForEditsTx(ctx, tx, members, books, nil, groups, affected, op); err != nil {
		return err
	}
	if err := renameSeriesForEditsTx(ctx, tx, books, op); err != nil {
		return err
	}
	if err := renameReleaseGroupsForEditsTx(ctx, tx, groups, affected, op); err != nil {
		return err
	}
	ids := make([]int64, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := renameAlbumChainTx(ctx, tx, log, id, groups[id], affected, op); err != nil {
			return err
		}
	}
	return nil
}

// creditRenameMember is one contributor-role participant in the artist stage: a batch
// credit entry on a role that backs no entity-key column of its own. It carries the
// artist the role credits now and the single name the entry moves it to. That is a pair
// only when exactly one artist holds the role and the entry names exactly one
// replacement: several holders collapsing onto one name is not a rename, and the query
// behind the holders has no order, so nothing could say which of them the pair meant.
type creditRenameMember struct {
	itemID  int64
	role    model.ContributorRole
	priorID int64    // the artist the role credits now, 0 when no pair can form
	target  string   // the name the pair moves it to
	names   []string // every name the entry credits, which the fold-back guard reads
}

// buildCreditRenameMember reads the artists a contributor role credits now and forms
// the entry's rename pair under the cardinality rule above.
func buildCreditRenameMember(ctx context.Context, tx *sql.Tx, e creditEntry, op string) (*creditRenameMember, error) {
	m := &creditRenameMember{itemID: e.itemID, role: e.role, names: e.clean}
	prior, err := contributorArtistIDsForRole(ctx, tx, e.itemID, e.role)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if len(prior) == 1 && len(e.clean) == 1 {
		m.priorID, m.target = prior[0], e.clean[0]
	}
	return m, nil
}

// renameArtistsForCreditsTx runs the artist stage of the pre-pass over a batch of
// credit entries. The chain stages are deliberately absent: they derive keys from the
// overlaid names and would fire whatever the artist stage decided, the credit surfaces
// re-resolve nothing behind them, so a moved release-group or album key would name a
// credit the columns underneath it no longer spell, and duplicate members per track
// would inflate a group's apparent size in the RG stage's coverage count. The album
// group is still collected for a track, since the artist coverage check reads it to
// decide whether a release group's own credit moved.
//
// Only the two roles backing an entity-key column build a chain member (a track's
// performing credit, a book's author), and an item holds at most one entry for each, so
// no item ever writes two values into the stage's per-item maps. Every other role
// becomes a creditRenameMember, whose target is keyed by the (item, role) pair instead.
func renameArtistsForCreditsTx(ctx context.Context, tx *sql.Tx, entries []creditEntry, affected *affectedRollups, op string) error {
	// The display an overlay would store for every role the batch rewrites, per item, so
	// a book member sees the credits the batch leaves behind rather than the ones it is
	// about to replace.
	byItemRole := map[int64]map[string]string{}
	for _, e := range entries {
		if byItemRole[e.itemID] == nil {
			byItemRole[e.itemID] = map[string]string{}
		}
		// Joined with the separator the overlay's own split understands: applyBookEdit
		// runs the value back through identity.SplitCredits, which never splits on a
		// comma, so ", " would hand the member one bogus name made of two people.
		byItemRole[e.itemID][string(e.role)] = strings.Join(e.clean, "; ")
	}

	var members []*renameMember
	var books []*bookRenameMember
	var credits []*creditRenameMember
	for _, e := range entries {
		renameField, keyed := creditRenameField(e.role)
		if !keyed {
			m, err := buildCreditRenameMember(ctx, tx, e, op)
			if err != nil {
				return err
			}
			credits = append(credits, m)
			continue
		}
		entry := editEntry{
			pid: e.pid, itemID: e.itemID, kind: e.kind,
			fields:  []string{renameField},
			norm:    map[string]string{renameField: strings.Join(e.clean, ", ")},
			credits: e.clean,
		}
		if e.kind == string(model.KindBook) {
			// A book credit role and the field a scan reads it back from share a name,
			// and bookCreditFields is exactly the pair applyBookEdit can overlay; a
			// translator or editor entry has no field to ride along on.
			for role, v := range byItemRole[e.itemID] {
				if role != string(e.role) && bookCreditFields[role] {
					entry.fields = append(entry.fields, role)
					entry.norm[role] = v
				}
			}
			// The riders come out of a map, so the sort pins their order; only the
			// narrator can ride today, but a wider bookCreditFields would otherwise
			// hand this member a map-ordered list.
			slices.Sort(entry.fields)
			m, err := buildBookRenameMember(ctx, tx, entry, op)
			if err != nil {
				return err
			}
			books = append(books, m)
			continue
		}
		m, err := buildRenameMember(ctx, tx, entry, op)
		if err != nil {
			return err
		}
		members = append(members, m)
	}
	if len(members) == 0 && len(books) == 0 && len(credits) == 0 {
		return nil
	}

	groups := map[int64][]*renameMember{}
	for _, m := range members {
		if m.curAlbumID != 0 {
			groups[m.curAlbumID] = append(groups[m.curAlbumID], m)
		}
	}
	return renameArtistsForEditsTx(ctx, tx, members, books, credits, groups, affected, op)
}

// renameReleaseGroupsForEditsTx is the release-group stage of the pre-pass, planned
// at batch level because one release group can back several of the batch's album
// groups (a multi-disc set is one album row per folder, all under one group), and a
// per-group walk would have the first album fork the group off to a fresh pid before
// the second could show the whole set is moving. A release group is rewritten in
// place (or auto-merged into the incumbent on a key collision) only when every album
// under it is a batch group whose every member moves at once to one uniform new RG
// key; a fully covered group whose key does not move (a case-only fold, or an
// mbid-keyed group) still gets its display title refreshed when every member edited
// the title to one value, since that title is the facet display and the MusicBrainz
// search text. Partial coverage falls through to renameAlbumChainTx's per-album
// fallback, which moves the covered album under a found-or-created group and leaves
// the old one with its remaining albums.
func renameReleaseGroupsForEditsTx(ctx context.Context, tx *sql.Tx, groups map[int64][]*renameMember, affected *affectedRollups, op string) error {
	// Qualify each album group for RG purposes: the batch covers every track of the
	// album and every member computes one non-empty new RG key. Album-key uniformity
	// is deliberately not required: the year is an album-key segment but not an RG
	// one, so a batch that renames a whole set while re-dating one member still moves
	// the group in place and only the album splits.
	type rgIntent struct {
		newRGKey    string
		titleEdited bool
		newTitle    string
	}
	byRG := map[int64][]rgIntent{}
	for albumID, group := range groups {
		var total int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM track WHERE album_id=?", albumID).Scan(&total); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if total != len(group) {
			continue
		}
		uniform, key := uniformValue(group, func(m *renameMember) string { return m.newRGKey })
		if !uniform || key == "" {
			continue
		}
		var rgID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			"SELECT release_group_id FROM album WHERE id=?", albumID).Scan(&rgID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !rgID.Valid {
			continue
		}
		titleEdited, newTitle := uniformEditedValue(group, "album", func(m *renameMember) string { return m.tr.Album })
		byRG[rgID.Int64] = append(byRG[rgID.Int64], rgIntent{key, titleEdited, newTitle})
	}

	ids := make([]int64, 0, len(byRG))
	for id := range byRG {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, rgID := range ids {
		intents := byRG[rgID]
		newKey := intents[0].newRGKey
		agree := true
		for _, it := range intents[1:] {
			if it.newRGKey != newKey {
				agree = false
				break
			}
		}
		if !agree {
			continue
		}
		// Full coverage: every album under the group is one of the qualified groups.
		var rgAlbums int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM album WHERE release_group_id=?", rgID).Scan(&rgAlbums); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if rgAlbums != len(intents) {
			continue
		}
		var curKey, pid, curTitle string
		err := tx.QueryRowContext(ctx,
			"SELECT match_key, pid, title FROM release_group WHERE id=?", rgID).Scan(&curKey, &pid, &curTitle)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		titleEdited := true
		newTitle := intents[0].newTitle
		for _, it := range intents {
			if !it.titleEdited || it.newTitle != newTitle {
				titleEdited = false
				break
			}
		}
		if newKey == curKey {
			// The key does not move (a case-only fold, or an mbid-keyed group):
			// refresh the display title alone. No marker delete here, matching the
			// artist step: a rename that folds to the same key is not new search
			// evidence.
			if titleEdited && newTitle != curTitle {
				if _, err := tx.ExecContext(ctx,
					"UPDATE release_group SET title=? WHERE id=?", newTitle, rgID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if err := refreshEntitySortKeyTx(ctx, tx, model.MergeReleaseGroup, "release_group", rgID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if err := appendChange(ctx, tx, "release_group", model.PID(pid), model.OpUpdate); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
			continue
		}
		var incPID string
		err = tx.QueryRowContext(ctx,
			"SELECT pid FROM release_group WHERE match_key=?", newKey).Scan(&incPID)
		switch {
		case err == nil:
			// Taken: auto-merge into the incumbent, which repoints every album.
			if _, err := mergeEntityTx(ctx, tx, model.MergeReleaseGroup, "release_group",
				model.PID(incPID), model.PID(pid)); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			// Free: rewrite the row in place. The unmatched enrichment marker is
			// deleted so the rename re-queues an entity that never matched: RG
			// resolution text-searches by title plus artist, and a key move is new
			// evidence (the in-tree precedent is fillAlbumIdentifiersTx clearing the
			// unmatched album marker). Matched markers stay, they record real writes.
			// primary_artist_id is left alone; the per-item loop's on-hit adoption
			// corrects it when the credit moved.
			rgTitle := curTitle
			if titleEdited {
				rgTitle = newTitle
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE release_group SET title=?, match_key=? WHERE id=?", rgTitle, newKey, rgID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := refreshEntitySortKeyTx(ctx, tx, model.MergeReleaseGroup, "release_group", rgID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "release_group", model.PID(pid), model.OpUpdate); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := clearUnmatchedEntityMarkerTx(ctx, tx, model.EnrichReleaseGroupType, rgID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		default:
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return nil
}

// renameArtistsForEditsTx is the artist stage of the pre-pass. An old artist entity
// renames in place (or auto-merges into an incumbent) only when every reference to it
// moves at once to one uniform target primary name: the pairs come from the artist
// edits (via artist_id), from anchor moves (via album_artist_id where the overlaid
// anchor primary differs from the entity's spelling), from book author edits (via
// book.author_id), and from a credit batch's contributor-role entries (via the artist
// the role credits now), and the coverage checks below require every reference the
// catalog holds to be part of the move. Any failure falls back to today's
// split-and-ghost behavior for that artist.
func renameArtistsForEditsTx(ctx context.Context, tx *sql.Tx, members []*renameMember, books []*bookRenameMember, credits []*creditRenameMember, groups map[int64][]*renameMember, affected *affectedRollups, op string) error {
	type artistRow struct {
		name, matchKey, pid string
	}
	cache := map[int64]artistRow{}
	artistOf := func(id int64) (artistRow, error) {
		if r, ok := cache[id]; ok {
			return r, nil
		}
		var r artistRow
		err := tx.QueryRowContext(ctx,
			"SELECT name, match_key, pid FROM artist WHERE id=?", id).Scan(&r.name, &r.matchKey, &r.pid)
		if err == nil {
			cache[id] = r
		}
		return r, err
	}

	// One uniform target name per old artist; a conflicting pair blocks the artist.
	targets := map[int64]string{}
	blocked := map[int64]bool{}
	addPair := func(id int64, n string) {
		if id == 0 {
			return
		}
		if cur, ok := targets[id]; ok {
			if cur != n {
				blocked[id] = true
			}
			return
		}
		targets[id] = n
	}
	for _, m := range members {
		if m.editsArtist {
			addPair(m.curArtistID, m.creditPrimary)
		}
		// An anchor pair fires only when the edit actually moved the anchor, never
		// on drift between the denormalized column and the entity's own spelling: a
		// merge leaves columns spelling the loser's name, and without this gate an
		// unrelated whole-set edit (a year change) would rename the surviving entity
		// back to the column value through the vacuously-passing coverage checks.
		if m.curAlbumArtistID != 0 && m.anchorPrimary != m.preAnchorPrimary {
			addPair(m.curAlbumArtistID, m.anchorPrimary)
		}
	}
	for _, m := range books {
		if m.editsAuthor {
			addPair(m.curAuthorID, m.authorPrimary)
		}
	}
	for _, m := range credits {
		addPair(m.priorID, m.target)
	}
	if len(targets) == 0 {
		return nil
	}

	// Per-item targets for the coverage checks, and each album group's current RG
	// with its uniform anchor target for the release_group reference check.
	editArtistTarget := map[int64]string{}
	editArtistNames := map[int64][]string{}
	anchorTarget := map[int64]string{}
	for _, m := range members {
		anchorTarget[m.entry.itemID] = m.anchorPrimary
		if m.editsArtist {
			editArtistTarget[m.entry.itemID] = m.creditPrimary
			editArtistNames[m.entry.itemID] = m.creditNames
		}
	}
	bookAuthorTarget := map[int64]string{}
	bookAuthorNames := map[int64][]string{}
	for _, m := range books {
		if m.editsAuthor {
			bookAuthorTarget[m.entry.itemID] = m.authorPrimary
			bookAuthorNames[m.entry.itemID] = m.b.Authors
		}
	}
	// Contributor-role targets are keyed by the pair, since an item can be in the batch
	// under two roles. Only a formed pair covers: an entry the cardinality rule refused
	// names no single successor, so its credit rows block instead.
	creditTarget := map[itemRoleKey]string{}
	for _, m := range credits {
		if m.priorID != 0 {
			creditTarget[itemRoleKey{m.itemID, string(m.role)}] = m.target
		}
	}
	// A release group can back several album groups, so the anchors accumulate with
	// an explicit veto: a non-uniform group, or two groups disagreeing, blocks the
	// whole RG rather than letting map iteration order pick a survivor.
	rgAnchors := map[int64]string{}
	rgBlocked := map[int64]bool{}
	for albumID, group := range groups {
		var rgID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			"SELECT release_group_id FROM album WHERE id=?", albumID).Scan(&rgID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !rgID.Valid {
			continue
		}
		uniform, n := uniformValue(group, func(m *renameMember) string { return m.anchorPrimary })
		if !uniform {
			rgBlocked[rgID.Int64] = true
			continue
		}
		if cur, ok := rgAnchors[rgID.Int64]; ok && cur != n {
			rgBlocked[rgID.Int64] = true
			continue
		}
		rgAnchors[rgID.Int64] = n
	}
	for id := range rgBlocked {
		delete(rgAnchors, id)
	}

	scope := artistRenameScope{
		members: members, books: books, credits: credits,
		editArtistTarget: editArtistTarget, editArtistNames: editArtistNames,
		anchorTarget:     anchorTarget,
		bookAuthorTarget: bookAuthorTarget, bookAuthorNames: bookAuthorNames,
		creditTarget: creditTarget, rgAnchors: rgAnchors,
	}
	ids := make([]int64, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		n := targets[id]
		if blocked[id] || identity.MatchKey(n) == "" {
			continue
		}
		r, err := artistOf(id)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		ok, err := artistRenameCoveredTx(ctx, tx, id, n, r.matchKey, scope)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !ok {
			continue
		}
		if err := renameArtistTx(ctx, tx, id, r.name, r.matchKey, r.pid, n, affected, op); err != nil {
			return err
		}
	}
	return nil
}

// artistRenameScope is the batch view the artist coverage checks read: the
// participants and the per-item target names their overlaid edits imply.
type artistRenameScope struct {
	members          []*renameMember
	books            []*bookRenameMember
	credits          []*creditRenameMember
	editArtistTarget map[int64]string       // item -> overlaid performing-credit primary
	editArtistNames  map[int64][]string     // item -> every name the overlaid performing credit holds
	anchorTarget     map[int64]string       // item -> overlaid release-group anchor primary
	bookAuthorTarget map[int64]string       // book item -> overlaid author primary
	bookAuthorNames  map[int64][]string     // book item -> every name the overlaid author credit holds
	creditTarget     map[itemRoleKey]string // (item, contributor role) -> the name the pair moves it to
	rgAnchors        map[int64]string       // release group -> its groups' uniform anchor target
}

// artistRenameCoveredTx runs the all-references-move checks for one artist rename
// candidate. Every failure means split, never an error.
func artistRenameCoveredTx(ctx context.Context, tx *sql.Tx, id int64, n, curKey string, sc artistRenameScope) (bool, error) {
	// A new credit that still names this artist keeps it referenced while its
	// curation would move to the new spelling (the "Beta feat. Alpha" case), so a
	// genuine key move requires no member folding a name back onto it, on either the
	// track or the book side. A same-key respelling deliberately keeps every
	// reference.
	if identity.MatchKey(n) != curKey {
		for _, m := range sc.members {
			for _, nm := range m.creditNames {
				if identity.MatchKey(nm) == curKey {
					return false, nil
				}
			}
		}
		for _, m := range sc.books {
			for _, nm := range m.overlaidCredits() {
				if identity.MatchKey(nm) == curKey {
					return false, nil
				}
			}
		}
		for _, m := range sc.credits {
			for _, nm := range m.names {
				if identity.MatchKey(nm) == curKey {
					return false, nil
				}
			}
		}
	}
	// Every performing-credit reference is a participant editing artist to n.
	trackRefs, err := queryInt64sTx(ctx, tx, "SELECT item_id FROM track WHERE artist_id=?", id)
	if err != nil {
		return false, err
	}
	for _, itemID := range trackRefs {
		if sc.editArtistTarget[itemID] != n {
			return false, nil
		}
	}
	// Every anchor reference is a participant whose overlaid anchor primary is n
	// (an unedited album_artist naming the old spelling blocks here).
	anchorRefs, err := queryInt64sTx(ctx, tx, "SELECT item_id FROM track WHERE album_artist_id=?", id)
	if err != nil {
		return false, err
	}
	for _, itemID := range anchorRefs {
		if t, ok := sc.anchorTarget[itemID]; !ok || t != n {
			return false, nil
		}
	}
	// Every book whose primary author is this artist is a participant editing author
	// to n. A book outside the batch, or one editing only its series, blocks.
	bookRefs, err := queryInt64sTx(ctx, tx, "SELECT item_id FROM book WHERE author_id=?", id)
	if err != nil {
		return false, err
	}
	for _, itemID := range bookRefs {
		if t, ok := sc.bookAuthorTarget[itemID]; !ok || t != n {
			return false, nil
		}
	}
	// Contributor rows: a credit covers only when the batch rewrites that exact slot to
	// this name. A performing credit on a batch track and an author credit on a batch book
	// are rewritten as a whole field, so the reference moves wherever the artist sits in
	// the new list, primary or not; the check is membership in the overlaid names. Every
	// other role needs a credit-batch entry moving that (item, role) pair, and there the
	// compare is against the pair's single target: membership alone would let a credit row
	// belonging to an item renaming elsewhere pass, and the file behind it still spells the
	// old name, so the next content-changing rescan would fork it straight back while the
	// renamed entity kept curation describing nobody. A credit on an item outside the batch
	// always blocks.
	credits, err := contributorRefsTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	for _, c := range credits {
		var covered bool
		switch model.ContributorRole(c.role) {
		case model.RoleArtist:
			covered = slices.Contains(sc.editArtistNames[c.itemID], n)
		case model.RoleAuthor:
			covered = slices.Contains(sc.bookAuthorNames[c.itemID], n)
		default:
			t, ok := sc.creditTarget[itemRoleKey{c.itemID, c.role}]
			covered = ok && t == n
		}
		if !covered {
			return false, nil
		}
	}
	// Every release group pointing at this artist must be one of this batch's album
	// groups whose anchor target is n: a renamed-in-place RG keeps pointing at the
	// artist, which then spells n, while a merged or re-resolved one gets repointed
	// by the merge or the per-item on-hit adoption.
	rgRefs, err := queryInt64sTx(ctx, tx, "SELECT id FROM release_group WHERE primary_artist_id=?", id)
	if err != nil {
		return false, err
	}
	for _, rgID := range rgRefs {
		if t, ok := sc.rgAnchors[rgID]; !ok || t != n {
			return false, nil
		}
	}
	return true, nil
}

// contributorRef is one credit row naming an artist: the item it sits on and the role
// it was credited under.
type contributorRef struct {
	itemID int64
	role   string
}

// contributorRefsTx collects an artist's credit rows, draining and closing its cursor
// before returning so the caller can keep querying the same single-connection
// transaction.
func contributorRefsTx(ctx context.Context, tx *sql.Tx, artistID int64) ([]contributorRef, error) {
	rows, err := tx.QueryContext(ctx, "SELECT item_id, role FROM item_contributor WHERE artist_id=?", artistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contributorRef
	for rows.Next() {
		var c contributorRef
		if err := rows.Scan(&c.itemID, &c.role); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// renameArtistTx rewrites one covered artist onto its target name: a same-key
// respelling refreshes the display, a free key renames in place with the old spelling
// preserved as an alias (mirroring repointArtist), and a taken key auto-merges into
// the incumbent, which keeps its own spelling (consistent with resolveArtist's on-hit
// behavior; merge already aliases the loser's name and repoints every reference). The
// mbid column stays untouched on an in-place rename; preserving identity is the
// point. Only the key-move branch deletes the artist's unmatched enrichment marker,
// the same rule as the RG step: artist resolution text-searches by name, and MatchKey
// folds exactly what that search is insensitive to, so a same-key respelling is by
// construction not new evidence and must not re-queue a rate-limited lookup.
func renameArtistTx(ctx context.Context, tx *sql.Tx, id int64, curName, curKey, pid, n string, affected *affectedRollups, op string) error {
	newKey := identity.MatchKey(n)
	if newKey == curKey {
		if n == curName {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE artist SET name=? WHERE id=?", n, id); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return finishArtistRenameTx(ctx, tx, id, pid, false, affected, op)
	}
	var incPID string
	err := tx.QueryRowContext(ctx, "SELECT pid FROM artist WHERE match_key=?", newKey).Scan(&incPID)
	switch {
	case err == nil:
		_, err := mergeEntityTx(ctx, tx, model.MergeArtist, "artist", model.PID(incPID), model.PID(pid))
		return err
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO artist_alias(artist_id, name, sort_key, is_primary) VALUES (?,?,?,0)",
			id, curName, model.SortKey(curName)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE artist SET name=?, match_key=? WHERE id=?", n, newKey, id); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return finishArtistRenameTx(ctx, tx, id, pid, true, affected, op)
	default:
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
}

// finishArtistRenameTx is the shared tail of the two name-writing branches: sort
// refresh, one artist OpUpdate, and a rollup refresh. requeue deletes the unmatched
// enrichment marker; only a key move sets it (see renameArtistTx).
func finishArtistRenameTx(ctx context.Context, tx *sql.Tx, id int64, pid string, requeue bool, affected *affectedRollups, op string) error {
	if err := refreshEntitySortKeyTx(ctx, tx, model.MergeArtist, "artist", id); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := appendChange(ctx, tx, "artist", model.PID(pid), model.OpUpdate); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if requeue {
		if err := clearUnmatchedEntityMarkerTx(ctx, tx, model.EnrichArtistType, id); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	affected.artists[id] = true
	return nil
}

// renameSeriesForEditsTx is the series stage of the pre-pass. A series is rewritten in
// place only when every book on it is a batch participant editing series to one
// uniform new name and no batch member moves onto the name it is leaving; anything else
// falls back to the split the per-item resolveSeries performs. There is nothing to carry
// but the pid and the deltas that reference it: a series row holds no art, curation,
// play state, or enrichment marker. A taken key folds the old row into the incumbent
// through the series merge primitive.
func renameSeriesForEditsTx(ctx context.Context, tx *sql.Tx, books []*bookRenameMember, op string) error {
	// One uniform target name per old series; a conflicting pair blocks it.
	targets := map[int64]string{}
	blocked := map[int64]bool{}
	seriesTarget := map[int64]string{}
	for _, m := range books {
		if !m.editsSeries {
			continue
		}
		seriesTarget[m.entry.itemID] = m.newSeries
		if m.curSeriesID == 0 {
			continue
		}
		if cur, ok := targets[m.curSeriesID]; ok {
			if cur != m.newSeries {
				blocked[m.curSeriesID] = true
			}
			continue
		}
		targets[m.curSeriesID] = m.newSeries
	}

	ids := make([]int64, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		n := targets[id]
		// An empty target clears the series off its books, which un-links rather than
		// renames, so the old row stays where it is and drains.
		if blocked[id] || identity.MatchKey(n) == "" {
			continue
		}
		refs, err := queryInt64sTx(ctx, tx, "SELECT item_id FROM book WHERE series_id=?", id)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		covered := true
		for _, itemID := range refs {
			if t, ok := seriesTarget[itemID]; !ok || t != n {
				covered = false
				break
			}
		}
		if !covered {
			continue
		}
		kept, err := seriesNameKeptByBatchTx(ctx, tx, id, n, books)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if kept {
			continue
		}
		if err := renameSeriesTx(ctx, tx, id, n, op); err != nil {
			return err
		}
	}
	return nil
}

// seriesNameKeptByBatchTx reports whether a batch member's overlaid series folds back
// onto the row's current key, which would leave the name in use while the rename carried
// the row's identity away to the new one. It is the series twin of the artist stage's
// fold-back guard; a same-key respelling keeps every reference by design and is exempt.
func seriesNameKeptByBatchTx(ctx context.Context, tx *sql.Tx, id int64, n string, books []*bookRenameMember) (bool, error) {
	var curKey string
	err := tx.QueryRowContext(ctx, "SELECT match_key FROM series WHERE id=?", id).Scan(&curKey)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if identity.MatchKey(n) == curKey {
		return false, nil
	}
	for _, m := range books {
		if identity.MatchKey(m.newSeries) == curKey {
			return true, nil
		}
	}
	return false, nil
}

// renameSeriesTx rewrites one covered series onto its target name: a same-key
// respelling refreshes the display, a free key renames in place, and a taken key
// auto-merges into the incumbent, the same three branches renameArtistTx has. Either
// write emits one series OpUpdate; the merge branch emits its own deltas instead, the
// loser's being a delete.
func renameSeriesTx(ctx context.Context, tx *sql.Tx, id int64, n, op string) error {
	var curName, curKey, pid string
	err := tx.QueryRowContext(ctx,
		"SELECT name, match_key, pid FROM series WHERE id=?", id).Scan(&curName, &curKey, &pid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	newKey := identity.MatchKey(n)
	if newKey == curKey {
		if n == curName {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE series SET name=?, sort_key=? WHERE id=?", n, model.SortKey(n), id); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	} else {
		var incPID string
		err := tx.QueryRowContext(ctx, "SELECT pid FROM series WHERE match_key=?", newKey).Scan(&incPID)
		switch {
		case err == nil:
			// Taken: fold this row into the incumbent, which re-points every book and
			// emits the loser's delete. Returning here rather than falling through is
			// what keeps the tail from also emitting an update for a deleted pid.
			_, err := mergeEntityTx(ctx, tx, model.MergeSeries, "series", model.PID(incPID), model.PID(pid))
			return err
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx,
				"UPDATE series SET name=?, sort_key=?, match_key=? WHERE id=?",
				n, model.SortKey(n), newKey, id); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		default:
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if err := appendChange(ctx, tx, "series", model.PID(pid), model.OpUpdate); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return nil
}

// queryInt64sTx collects one integer column, draining and closing its cursor before
// returning so the caller can keep using the same single-connection transaction.
func queryInt64sTx(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// uniformValue reports whether one projection agrees across a group, and that value.
func uniformValue[T comparable](group []*renameMember, proj func(*renameMember) T) (bool, T) {
	want := proj(group[0])
	for _, m := range group[1:] {
		if proj(m) != want {
			var zero T
			return false, zero
		}
	}
	return true, want
}

// renameAlbumChainTx renames or merges one group's release_group and album when the
// batch moves every member at once to one uniform new key. Any condition failure
// returns nil, and the per-item apply loop afterwards splits the members off exactly
// as it does today; on success that loop is transparent, since it computes the same
// keys through the same helpers and hits the renamed or merged rows.
func renameAlbumChainTx(ctx context.Context, tx *sql.Tx, log logger, albumID int64, group []*renameMember, affected *affectedRollups, op string) error {
	// All-members: the batch must cover every track the album has.
	var total int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM track WHERE album_id=?", albumID).Scan(&total); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if total != len(group) {
		return nil
	}
	// Uniformity: one shared target key. An empty key means the edit un-groups the
	// members (a cleared title, or a cleared anchor through the not-grouped guard),
	// and the old entities ghost as today. An mbid-keyed member cannot reach the
	// empty case: the loader's key carryover keys the chain before the title or
	// anchor is consulted, so clearing those leaves the member grouped, matching a
	// scan of a file whose MusicBrainz ids outlive its text tags.
	newAlbumKey := group[0].newAlbumKey
	for _, m := range group[1:] {
		if m.newAlbumKey != newAlbumKey {
			return nil
		}
	}
	if newAlbumKey == "" {
		return nil
	}

	var curKey, curPID, curTitle string
	var curYear, curRGID sql.NullInt64
	err := tx.QueryRowContext(ctx,
		"SELECT match_key, pid, title, year, release_group_id FROM album WHERE id=?", albumID).
		Scan(&curKey, &curPID, &curTitle, &curYear, &curRGID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !curRGID.Valid) {
		return nil // no chain resolution ever leaves; fall back
	}
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	var curRGKey, curRGTitle string
	err = tx.QueryRowContext(ctx,
		"SELECT match_key, title FROM release_group WHERE id=?", curRGID.Int64).
		Scan(&curRGKey, &curRGTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	// The display values a rename may carry: a column is written only when every
	// member edited its field to one identical value, so a case-divergent batch that
	// still folds to one key keeps the current display.
	titleEdited, newTitle := uniformEditedValue(group, "album", func(m *renameMember) string { return m.tr.Album })
	yearEdited, newYear := uniformEditedValue(group, "year", func(m *renameMember) int { return m.tr.Year })

	// Release-group fallback. The batch-level RG stage already moved (or merged) a
	// fully covered group in place, after which the fresh read above sees the new
	// key and this block is skipped. Reaching it with a uniform differing key means
	// another album under the old group stays behind, so this album alone moves
	// under the found-or-created new group and the old one keeps its other albums
	// (no ghost). primaryArtistID 0: the artist row for a new anchor may not exist
	// yet, so the row inserts with a NULL primary and the per-item on-hit adoption
	// fills it and maintains both rollups; artists are never resolved inside the
	// pre-pass.
	newRGKey := group[0].newRGKey
	rgUniform := true
	for _, m := range group[1:] {
		if m.newRGKey != newRGKey {
			rgUniform = false
			break
		}
	}
	oldRGID := curRGID.Int64
	finalRGID := oldRGID
	if rgUniform && newRGKey != "" && newRGKey != curRGKey {
		rgTitle := curRGTitle
		if titleEdited {
			rgTitle = newTitle
		}
		newID, err := resolveReleaseGroup(ctx, tx, log, newRGKey, rgTitle, 0, group[0].tr.MBReleaseGroupID, affected)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// resolveAlbum on a hit never repoints the FK, so move it explicitly, and
		// say so in the change log: the album's group membership is readable state
		// (entity info serves the group pid), so a consumer needs a delta to refetch.
		if _, err := tx.ExecContext(ctx,
			"UPDATE album SET release_group_id=? WHERE id=?", newID, albumID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := appendChange(ctx, tx, "album", model.PID(curPID), model.OpUpdate); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		finalRGID = newID
	}

	// Album step.
	if newAlbumKey == curKey {
		// Display refresh: covers case-only renames and mbid-keyed albums. Album
		// markers are not cleared here (or on any album rename): the release match
		// consults identifiers and the RG's mbid, never the album title or year, so a
		// rename is not new evidence at that level.
		var wrote bool
		if titleEdited && newTitle != curTitle {
			if _, err := tx.ExecContext(ctx, "UPDATE album SET title=? WHERE id=?", newTitle, albumID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			wrote = true
		}
		if yearEdited && int64(newYear) != curYear.Int64 {
			if _, err := tx.ExecContext(ctx, "UPDATE album SET year=? WHERE id=?", nullInt(newYear), albumID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			wrote = true
		}
		if wrote {
			if err := refreshEntitySortKeyTx(ctx, tx, model.MergeAlbum, "album", albumID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "album", model.PID(curPID), model.OpUpdate); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
	} else {
		// An archived member has no primary file, so the folder segment of its
		// computed key is empty and no scan of the restored files would ever compute
		// it; moving the row (or, through a merge, its attachments) onto such a key
		// would orphan them on the next re-resolve, so the group falls back to the
		// split the per-item loop performs. The same-key branch above is unaffected,
		// since an mbid key carries no folder segment.
		for _, m := range group {
			if !m.hasPath {
				return nil
			}
		}
		var incID int64
		var incPID string
		err := tx.QueryRowContext(ctx,
			"SELECT id, pid FROM album WHERE match_key=?", newAlbumKey).Scan(&incID, &incPID)
		switch {
		case err == nil:
			// Taken: auto-merge into the incumbent, which repoints the members; the
			// per-item loop then resolves onto it by the shared key. Collisions are
			// only ever heuristic against heuristic, since an mbid-keyed album's new
			// key is its own unchanged key under the loader's mbid carryover.
			if _, err := mergeEntityTx(ctx, tx, model.MergeAlbum, "album",
				model.PID(incPID), model.PID(curPID)); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			// Free: rewrite the row in place; columns not backed by an edited field
			// keep their current values.
			title, year := curTitle, curYear.Int64
			if titleEdited {
				title = newTitle
			}
			if yearEdited {
				year = int64(newYear)
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE album SET match_key=?, title=?, year=? WHERE id=?",
				newAlbumKey, title, nullInt(int(year)), albumID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := refreshEntitySortKeyTx(ctx, tx, model.MergeAlbum, "album", albumID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "album", model.PID(curPID), model.OpUpdate); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		default:
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}

	// The chain ids that still exist land in the shared affected set (the merge
	// branches maintained their own rollups and per-item deltas already; the edited
	// members then also emit their item OpUpdate from applyItemEditTx, and duplicate
	// rows in the append-only change_log are harmless).
	for _, rgID := range []int64{oldRGID, finalRGID} {
		if err := addRGChainToAffected(ctx, tx, rgID, affected); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return nil
}

// uniformEditedValue reports whether every member of a group edited one field to the
// same overlaid value, and that value's projection.
func uniformEditedValue[T comparable](group []*renameMember, field string, proj func(*renameMember) T) (bool, T) {
	var zero T
	for _, m := range group {
		if !slices.Contains(m.entry.fields, field) {
			return false, zero
		}
	}
	want := proj(group[0])
	for _, m := range group[1:] {
		if proj(m) != want {
			return false, zero
		}
	}
	return true, want
}

// addRGChainToAffected marks a release group and its primary artist for a rollup
// recompute, tolerating an id a merge already deleted.
func addRGChainToAffected(ctx context.Context, tx *sql.Tx, rgID int64, affected *affectedRollups) error {
	var primary sql.NullInt64
	err := tx.QueryRowContext(ctx,
		"SELECT primary_artist_id FROM release_group WHERE id=?", rgID).Scan(&primary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	affected.rgs[rgID] = true
	if primary.Valid {
		affected.artists[primary.Int64] = true
	}
	return nil
}
