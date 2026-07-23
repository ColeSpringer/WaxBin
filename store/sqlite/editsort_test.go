package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// This file covers the editable sort-name surface: composer_sort on tracks and
// author_sort on books. The contract under test: an explicit edit stores the
// literal value; clearing it reverts to the key derived from the display name; a
// name edit regenerates the derived sort UNLESS the sort is locked; and a locked
// sort survives a preserve-locks rescan.

func trackComposerRow(t *testing.T, st *Store, pid model.PID) (composer, composerSort string) {
	t.Helper()
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT composer, composer_sort FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
		string(pid)).Scan(&composer, &composerSort); err != nil {
		t.Fatalf("read composer row: %v", err)
	}
	return composer, composerSort
}

func bookAuthorRow(t *testing.T, st *Store, pid model.PID) (author, authorSort string) {
	t.Helper()
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT author, author_sort FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?",
		string(pid)).Scan(&author, &authorSort); err != nil {
		t.Fatalf("read author row: %v", err)
	}
	return author, authorSort
}

func TestEditComposerSortMatrix(t *testing.T) {
	st, pid := editFixture(t) // composer "Writer" -> derived sort
	ctx := context.Background()

	if _, sort := trackComposerRow(t, st, pid); sort != model.SortKey("Writer") {
		t.Fatalf("scan-derived composer_sort = %q, want %q", sort, model.SortKey("Writer"))
	}

	// An explicit edit stores the literal value (not SortKey-folded); the lock is
	// what makes it durable.
	if err := st.EditItemField(ctx, pid, "composer_sort", "Writer, The", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit composer_sort: %v", err)
	}
	if _, sort := trackComposerRow(t, st, pid); sort != "Writer, The" {
		t.Fatalf("composer_sort = %q, want the literal %q", sort, "Writer, The")
	}

	// Editing the locked sort itself without force is refused.
	if err := st.EditItemField(ctx, pid, "composer_sort", "X", model.SourceUser, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("edit locked composer_sort = %v, want CodeLocked", err)
	}

	// A composer edit regenerates the sort, but the locked sort survives it.
	if err := st.EditItemField(ctx, pid, "composer", "New Name", model.SourceUser, false, false); err != nil {
		t.Fatalf("edit composer over locked sort: %v", err)
	}
	comp, sort := trackComposerRow(t, st, pid)
	if comp != "New Name" || sort != "Writer, The" {
		t.Fatalf("after composer edit = (%q, %q), want composer changed and locked sort preserved", comp, sort)
	}

	// Unlocked, the composer edit regenerates the derived sort.
	if err := st.UnlockField(ctx, pid, "composer_sort"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "composer", "Third Name", model.SourceUser, false, false); err != nil {
		t.Fatalf("edit composer unlocked: %v", err)
	}
	if _, sort := trackComposerRow(t, st, pid); sort != model.SortKey("Third Name") {
		t.Fatalf("composer_sort after unlocked composer edit = %q, want %q", sort, model.SortKey("Third Name"))
	}

	// Clearing the sort reverts to the key derived from the composer.
	if err := st.EditItemField(ctx, pid, "composer_sort", "Custom Order", model.SourceUser, false, false); err != nil {
		t.Fatalf("set literal: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "composer_sort", "", model.SourceUser, false, false); err != nil {
		t.Fatalf("clear composer_sort: %v", err)
	}
	if _, sort := trackComposerRow(t, st, pid); sort != model.SortKey("Third Name") {
		t.Fatalf("cleared composer_sort = %q, want re-derived %q", sort, model.SortKey("Third Name"))
	}

	// A combined edit applies composer first (sorted field order), so the explicit
	// sort wins over the regeneration.
	if err := st.EditItemFields(ctx, pid, map[string]string{
		"composer": "Fourth Name", "composer_sort": "Fourth, The",
	}, model.SourceUser, false, false); err != nil {
		t.Fatalf("combined edit: %v", err)
	}
	comp, sort = trackComposerRow(t, st, pid)
	if comp != "Fourth Name" || sort != "Fourth, The" {
		t.Fatalf("combined edit = (%q, %q), want the explicit sort to win", comp, sort)
	}

	// Clearing the composer clears the derived sort with it.
	if err := st.EditItemFields(ctx, pid, map[string]string{
		"composer": "", "composer_sort": "",
	}, model.SourceUser, false, false); err != nil {
		t.Fatalf("clear both: %v", err)
	}
	if comp, sort = trackComposerRow(t, st, pid); comp != "" || sort != "" {
		t.Fatalf("cleared composer = (%q, %q), want both empty", comp, sort)
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("verify after sort edits: %+v, err %v", rep, err)
	}
}

func TestEditAuthorSortMatrix(t *testing.T) {
	st, pid := bookEditFixture(t) // author "Jane Author" -> derived sort
	ctx := context.Background()

	if _, sort := bookAuthorRow(t, st, pid); sort != model.SortKey("Jane Author") {
		t.Fatalf("scan-derived author_sort = %q, want %q", sort, model.SortKey("Jane Author"))
	}

	// The literal value is stored, not its SortKey folding.
	if err := st.EditItemField(ctx, pid, "author_sort", "Author, Jane", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit author_sort: %v", err)
	}
	if _, sort := bookAuthorRow(t, st, pid); sort != "Author, Jane" {
		t.Fatalf("author_sort = %q, want the literal %q", sort, "Author, Jane")
	}

	// An author edit re-derives the sort, but the locked sort survives.
	if err := st.EditItemField(ctx, pid, "author", "John Writer", model.SourceUser, false, false); err != nil {
		t.Fatalf("edit author over locked sort: %v", err)
	}
	author, sort := bookAuthorRow(t, st, pid)
	if author != "John Writer" || sort != "Author, Jane" {
		t.Fatalf("after author edit = (%q, %q), want author changed and locked sort preserved", author, sort)
	}

	// Unlocked, the author edit lets upsertBook recompute the sort from the new author.
	if err := st.UnlockField(ctx, pid, "author_sort"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "author", "Third Writer", model.SourceUser, false, false); err != nil {
		t.Fatalf("edit author unlocked: %v", err)
	}
	if _, sort := bookAuthorRow(t, st, pid); sort != model.SortKey("Third Writer") {
		t.Fatalf("author_sort after unlocked author edit = %q, want %q", sort, model.SortKey("Third Writer"))
	}

	// Clearing the sort reverts to the derived key.
	if err := st.EditItemField(ctx, pid, "author_sort", "Custom", model.SourceUser, false, false); err != nil {
		t.Fatalf("set literal: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "author_sort", "", model.SourceUser, false, false); err != nil {
		t.Fatalf("clear author_sort: %v", err)
	}
	if _, sort := bookAuthorRow(t, st, pid); sort != model.SortKey("Third Writer") {
		t.Fatalf("cleared author_sort = %q, want re-derived %q", sort, model.SortKey("Third Writer"))
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("verify after sort edits: %+v, err %v", rep, err)
	}
}

// TestCreditEditRespectsSortLocks verifies the credit surface follows the same
// sort-lock rule as the scalar path: a composer/author credit edit regenerates the
// derived sort, unless that sort is locked.
func TestCreditEditRespectsSortLocks(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	// Unlocked: the credit edit regenerates composer_sort from the new display.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Anna Arranger"}, model.SourceUser, false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	comp, sort := trackComposerRow(t, st, pid)
	if comp != "Anna Arranger" || sort != model.SortKey("Anna Arranger") {
		t.Fatalf("credit edit = (%q, %q), want regenerated sort", comp, sort)
	}

	// Locked: the credit edit updates the display but the curated sort survives.
	if err := st.EditItemField(ctx, pid, "composer_sort", "Arranger, Anna", model.SourceUser, true, false); err != nil {
		t.Fatalf("lock composer_sort: %v", err)
	}
	if _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Bob Builder"}, model.SourceUser, false, false); err != nil {
		t.Fatalf("credit edit over locked sort: %v", err)
	}
	comp, sort = trackComposerRow(t, st, pid)
	if comp != "Bob Builder" || sort != "Arranger, Anna" {
		t.Fatalf("locked credit edit = (%q, %q), want display changed and sort preserved", comp, sort)
	}

	// The book author credit follows the same rule.
	stB, bookPID := bookEditFixture(t)
	if err := stB.EditItemField(ctx, bookPID, "author_sort", "Author, Jane", model.SourceUser, true, false); err != nil {
		t.Fatalf("lock author_sort: %v", err)
	}
	if _, err := stB.SetItemCredits(ctx, bookPID, model.RoleAuthor, []string{"New Author"}, model.SourceUser, false, false); err != nil {
		t.Fatalf("author credit over locked sort: %v", err)
	}
	author, aSort := bookAuthorRow(t, stB, bookPID)
	if author != "New Author" || aSort != "Author, Jane" {
		t.Fatalf("locked author credit = (%q, %q), want display changed and sort preserved", author, aSort)
	}
}

// rescanTrackWithComposer simulates a preserve-locks rescan whose file carries a
// different composer and derived sort, the values a scan would push over an edit.
func rescanTrackWithComposer(t *testing.T, st *Store, libID int64, path, essence, content, composer string, preserve bool) {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte("01.flac"),
			Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 2,
			ContentHash: content, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "Original",
			SortKey: model.SortKey("Original"), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{
			Artist: "Alpha", ArtistSort: model.SortKey("Alpha"),
			Composer: composer, ComposerSort: model.SortKey(composer),
		},
		PreserveLocks: preserve,
	}
	if _, err := st.PutScannedTrack(context.Background(), in); err != nil {
		t.Fatalf("rescan: %v", err)
	}
}

func TestScanPreservesLockedSortNames(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	// Lock a curated composer_sort, then force-rescan with a different on-disk value.
	if err := st.EditItemField(ctx, pid, "composer_sort", "Writer, The", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit composer_sort: %v", err)
	}
	rescanTrackWithComposer(t, st, lib1ID(t, st), "/lib/Alpha/One/01.flac", "e1", "c2", "Disk Composer", true)
	comp, sort := trackComposerRow(t, st, pid)
	if comp != "Disk Composer" || sort != "Writer, The" {
		t.Fatalf("after preserve-locks rescan = (%q, %q), want the file's composer and the locked sort", comp, sort)
	}

	// An ignore-locks rescan re-derives both.
	rescanTrackWithComposer(t, st, lib1ID(t, st), "/lib/Alpha/One/01.flac", "e1", "c3", "Disk Composer", false)
	if _, sort := trackComposerRow(t, st, pid); sort != model.SortKey("Disk Composer") {
		t.Fatalf("ignore-locks rescan kept sort %q, want re-derived %q", sort, model.SortKey("Disk Composer"))
	}

	// A locked composer preserves its derived sort alongside the display, so the
	// collation cannot silently re-derive from the file's differing value. Fresh
	// fixture: only the composer is locked here, never the sort.
	st2, pid2 := editFixture(t)
	if err := st2.EditItemField(ctx, pid2, "composer", "Curated Composer", model.SourceUser, true, false); err != nil {
		t.Fatalf("edit composer: %v", err)
	}
	rescanTrackWithComposer(t, st2, lib1ID(t, st2), "/lib/Alpha/One/01.flac", "e1", "c4", "Disk Composer", true)
	comp, sort = trackComposerRow(t, st2, pid2)
	if comp != "Curated Composer" || sort != model.SortKey("Curated Composer") {
		t.Fatalf("locked composer rescan = (%q, %q), want both preserved", comp, sort)
	}
}

// lib1ID returns the fixture's single library id.
func lib1ID(t *testing.T, st *Store) int64 {
	t.Helper()
	var id int64
	if err := st.read.QueryRowContext(context.Background(), "SELECT id FROM library LIMIT 1").Scan(&id); err != nil {
		t.Fatalf("library id: %v", err)
	}
	return id
}

func TestQuerySortAndViewExposure(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Composers whose display and collation order differ: "The Zeta" collates as
	// "zeta" (article stripped), after "miller".
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "T1", artist: "A", composer: "The Zeta",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/2.flac", essence: "e2", content: "c2", title: "T2", artist: "A", composer: "Miller",
	})

	q := query.New(query.EntityTracks).OrderBy("composer_sort", false).Build()
	items, err := st.QueryItems(ctx, q, "")
	if err != nil {
		t.Fatalf("query sorted by composer_sort: %v", err)
	}
	if len(items) != 2 || items[0].Composer != "Miller" || items[1].Composer != "The Zeta" {
		t.Fatalf("composer_sort order wrong: %+v", itemComposers(items))
	}
	// The view carries the pair.
	if items[1].ComposerSort != model.SortKey("The Zeta") {
		t.Fatalf("view ComposerSort = %q, want %q", items[1].ComposerSort, model.SortKey("The Zeta"))
	}

	// Books order by author_sort the same way.
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/z.m4b", essence: "bz", content: "bz", title: "Z Book", author: "The Zebra",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/m.m4b", essence: "bm", content: "bm", title: "M Book", author: "Mole",
	})
	qb := query.New(query.EntityItems).
		Where("kind", query.OpIs, "book").
		OrderBy("author_sort", false).Build()
	books, err := st.QueryItems(ctx, qb, "")
	if err != nil {
		t.Fatalf("query sorted by author_sort: %v", err)
	}
	if len(books) != 2 || books[0].Artist != "Mole" || books[1].Artist != "The Zebra" {
		t.Fatalf("author_sort order wrong: %v, %v", books[0].Artist, books[1].Artist)
	}
}

func itemComposers(items []*model.ItemView) []string {
	out := make([]string, 0, len(items))
	for _, v := range items {
		out = append(out, v.Composer)
	}
	return out
}
