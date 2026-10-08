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
// literal spelling and folds the key from it; clearing it folds the key from the
// display name; a name edit drops the spelling UNLESS it is locked; and a locked
// spelling survives a preserve-locks rescan.

// trackComposerRow reads a track's composer, its sort spelling and the key beside it.
func trackComposerRow(t *testing.T, st *Store, pid model.PID) (composer, spelling, key string) {
	t.Helper()
	if err := st.rdb().QueryRowContext(context.Background(),
		"SELECT composer, composer_sort, composer_sort_key FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
		string(pid)).Scan(&composer, &spelling, &key); err != nil {
		t.Fatalf("read composer row: %v", err)
	}
	return composer, spelling, key
}

// bookAuthorRow reads a book's author, its sort spelling and the key beside it.
func bookAuthorRow(t *testing.T, st *Store, pid model.PID) (author, spelling, key string) {
	t.Helper()
	if err := st.rdb().QueryRowContext(context.Background(),
		"SELECT author, author_sort, author_sort_key FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?",
		string(pid)).Scan(&author, &spelling, &key); err != nil {
		t.Fatalf("read author row: %v", err)
	}
	return author, spelling, key
}

func TestEditComposerSortMatrix(t *testing.T) {
	t.Parallel()
	st, pid := editFixture(t) // composer "Writer", no spelling
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	want := func(step, composer, spelling, key string) {
		t.Helper()
		if c, sp, k := trackComposerRow(t, st, pid); c != composer || sp != spelling || k != key {
			t.Fatalf("%s: composer row = (%q, %q, %q), want (%q, %q, %q)", step, c, sp, k, composer, spelling, key)
		}
	}
	want("scanned", "Writer", "", model.SortKey("Writer"))

	// An explicit edit stores the literal spelling and folds the key from it; the lock
	// is what keeps it against a file that says otherwise.
	if err := st.EditItemField(ctx, pid, "composer_sort", "Writer, The", user, model.LockOf(true), false); err != nil {
		t.Fatalf("edit composer_sort: %v", err)
	}
	want("explicit spelling", "Writer", "Writer, The", "writer, the")

	// Editing the locked sort itself without force is refused.
	if err := st.EditItemField(ctx, pid, "composer_sort", "X", user, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("edit locked composer_sort = %v, want CodeLocked", err)
	}

	// A composer edit drops the spelling, but the locked one survives it.
	if err := st.EditItemField(ctx, pid, "composer", "New Name", user, model.LockOf(false), false); err != nil {
		t.Fatalf("edit composer over locked sort: %v", err)
	}
	want("composer edit over a locked spelling", "New Name", "Writer, The", "writer, the")

	// Unlocked, the composer edit drops the spelling and the key follows the composer.
	if err := st.UnlockField(ctx, pid, "composer_sort"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "composer", "Third Name", user, model.LockOf(false), false); err != nil {
		t.Fatalf("edit composer unlocked: %v", err)
	}
	want("unlocked composer edit", "Third Name", "", model.SortKey("Third Name"))

	// Clearing the spelling folds the key from the composer again.
	if err := st.EditItemField(ctx, pid, "composer_sort", "Custom Order", user, model.LockOf(false), false); err != nil {
		t.Fatalf("set literal: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "composer_sort", "", user, model.LockOf(false), false); err != nil {
		t.Fatalf("clear composer_sort: %v", err)
	}
	want("cleared spelling", "Third Name", "", model.SortKey("Third Name"))

	// A combined edit applies composer first (sorted field order), so the explicit
	// spelling wins over the drop.
	if err := st.EditItemFields(ctx, pid, map[string]string{
		"composer": "Fourth Name", "composer_sort": "Fourth, The",
	}, user, model.LockOf(false), false); err != nil {
		t.Fatalf("combined edit: %v", err)
	}
	want("combined edit", "Fourth Name", "Fourth, The", "fourth, the")

	// Clearing the composer and its spelling leaves no key.
	if err := st.EditItemFields(ctx, pid, map[string]string{
		"composer": "", "composer_sort": "",
	}, user, model.LockOf(false), false); err != nil {
		t.Fatalf("clear both: %v", err)
	}
	want("cleared composer", "", "", "")

	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("verify after sort edits: %+v, err %v", rep, err)
	}
}

func TestEditAuthorSortMatrix(t *testing.T) {
	t.Parallel()
	st, pid := bookEditFixture(t) // author "Jane Author", no spelling
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	want := func(step, author, spelling, key string) {
		t.Helper()
		if a, sp, k := bookAuthorRow(t, st, pid); a != author || sp != spelling || k != key {
			t.Fatalf("%s: author row = (%q, %q, %q), want (%q, %q, %q)", step, a, sp, k, author, spelling, key)
		}
	}
	want("scanned", "Jane Author", "", model.SortKey("Jane Author"))

	if err := st.EditItemField(ctx, pid, "author_sort", "Author, Jane", user, model.LockOf(true), false); err != nil {
		t.Fatalf("edit author_sort: %v", err)
	}
	want("explicit spelling", "Jane Author", "Author, Jane", "author, jane")

	// An author edit drops the spelling, but the locked one survives.
	if err := st.EditItemField(ctx, pid, "author", "John Writer", user, model.LockOf(false), false); err != nil {
		t.Fatalf("edit author over locked sort: %v", err)
	}
	want("author edit over a locked spelling", "John Writer", "Author, Jane", "author, jane")

	// Unlocked, the author edit drops the spelling and the key follows the author.
	if err := st.UnlockField(ctx, pid, "author_sort"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "author", "Third Writer", user, model.LockOf(false), false); err != nil {
		t.Fatalf("edit author unlocked: %v", err)
	}
	want("unlocked author edit", "Third Writer", "", model.SortKey("Third Writer"))

	// Clearing the spelling folds the key from the author again.
	if err := st.EditItemField(ctx, pid, "author_sort", "Custom", user, model.LockOf(false), false); err != nil {
		t.Fatalf("set literal: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "author_sort", "", user, model.LockOf(false), false); err != nil {
		t.Fatalf("clear author_sort: %v", err)
	}
	want("cleared spelling", "Third Writer", "", model.SortKey("Third Writer"))

	rep, err := st.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() {
		t.Fatalf("verify after sort edits: %+v, err %v", rep, err)
	}
}

// TestCreditEditRespectsSortLocks verifies the credit surface follows the same
// sort-lock rule as the scalar path: a composer/author credit edit drops the sort
// spelling, unless that spelling is locked.
func TestCreditEditRespectsSortLocks(t *testing.T) {
	t.Parallel()
	st, pid := editFixture(t)
	ctx := context.Background()

	// Unlocked: the credit edit drops the spelling and the key follows the new display.
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Anna Arranger"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	comp, sort, key := trackComposerRow(t, st, pid)
	if comp != "Anna Arranger" || sort != "" || key != model.SortKey("Anna Arranger") {
		t.Fatalf("credit edit = (%q, %q, %q), want no spelling and the new display's key", comp, sort, key)
	}

	// Locked: the credit edit updates the display but the curated sort survives.
	if err := st.EditItemField(ctx, pid, "composer_sort", "Arranger, Anna", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("lock composer_sort: %v", err)
	}
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Bob Builder"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
		t.Fatalf("credit edit over locked sort: %v", err)
	}
	comp, sort, key = trackComposerRow(t, st, pid)
	if comp != "Bob Builder" || sort != "Arranger, Anna" || key != "arranger, anna" {
		t.Fatalf("locked credit edit = (%q, %q, %q), want display changed and the spelling kept", comp, sort, key)
	}

	// The book author credit follows the same rule.
	stB, bookPID := bookEditFixture(t)
	if err := stB.EditItemField(ctx, bookPID, "author_sort", "Author, Jane", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("lock author_sort: %v", err)
	}
	if _, _, err := stB.SetItemCredits(ctx, bookPID, model.RoleAuthor, []string{"New Author"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
		t.Fatalf("author credit over locked sort: %v", err)
	}
	author, aSort, aKey := bookAuthorRow(t, stB, bookPID)
	if author != "New Author" || aSort != "Author, Jane" || aKey != "author, jane" {
		t.Fatalf("locked author credit = (%q, %q, %q), want display changed and the spelling kept", author, aSort, aKey)
	}
}

// rescanTrackWithComposer simulates a preserve-locks rescan whose file carries a
// different composer and its sort spelling, the values a scan would push over an edit.
func rescanTrackWithComposer(t *testing.T, st *Store, libID int64, path, essence, content, composer, spelling string, preserve bool) {
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
			Artist: "Alpha", Composer: composer, ComposerSort: spelling,
		},
		PreserveLocks: preserve,
	}
	if _, err := st.PutScannedTrack(context.Background(), in); err != nil {
		t.Fatalf("rescan: %v", err)
	}
}

func TestScanPreservesLockedSortNames(t *testing.T) {
	t.Parallel()
	st, pid := editFixture(t)
	ctx := context.Background()

	// Lock a curated spelling, then force-rescan a file stating another.
	if err := st.EditItemField(ctx, pid, "composer_sort", "Writer, The", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit composer_sort: %v", err)
	}
	rescanTrackWithComposer(t, st, lib1ID(t, st), "/lib/Alpha/One/01.flac", "e1", "c2", "Disk Composer", "Composer, Disk", true)
	if comp, sort, key := trackComposerRow(t, st, pid); comp != "Disk Composer" || sort != "Writer, The" || key != "writer, the" {
		t.Fatalf("after preserve-locks rescan = (%q, %q, %q), want the file's composer and the locked spelling", comp, sort, key)
	}

	// An ignore-locks rescan takes the file's spelling.
	rescanTrackWithComposer(t, st, lib1ID(t, st), "/lib/Alpha/One/01.flac", "e1", "c3", "Disk Composer", "Composer, Disk", false)
	if _, sort, key := trackComposerRow(t, st, pid); sort != "Composer, Disk" || key != "composer, disk" {
		t.Fatalf("ignore-locks rescan = (%q, %q), want the file's spelling and its key", sort, key)
	}

	// A locked composer keeps its spelling beside it, so the file's spelling of another
	// name cannot land next to the curated one. Fresh fixture: only the composer is
	// locked here, never the sort.
	st2, pid2 := editFixture(t)
	if err := st2.EditItemField(ctx, pid2, "composer", "Curated Composer", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit composer: %v", err)
	}
	rescanTrackWithComposer(t, st2, lib1ID(t, st2), "/lib/Alpha/One/01.flac", "e1", "c4", "Disk Composer", "Composer, Disk", true)
	if comp, sort, key := trackComposerRow(t, st2, pid2); comp != "Curated Composer" || sort != "" || key != model.SortKey("Curated Composer") {
		t.Fatalf("locked composer rescan = (%q, %q, %q), want the curated composer, no spelling and its key", comp, sort, key)
	}
}

// lib1ID returns the fixture's single library id.
func lib1ID(t *testing.T, st *Store) int64 {
	t.Helper()
	var id int64
	if err := st.rdb().QueryRowContext(context.Background(), "SELECT id FROM library LIMIT 1").Scan(&id); err != nil {
		t.Fatalf("library id: %v", err)
	}
	return id
}

func TestQuerySortAndViewExposure(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Composers whose display and collation order differ: "The Zeta", spelled
	// "Zeta, The", collates as "zeta, the", after "miller".
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "T1", artist: "A", composer: "The Zeta",
		composerSort: "Zeta, The",
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
	// The view carries the spelling as stated, and none where the file states none.
	if items[1].ComposerSort != "Zeta, The" || items[0].ComposerSort != "" {
		t.Fatalf("view ComposerSort = %q and %q, want \"\" and the spelling", items[0].ComposerSort, items[1].ComposerSort)
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
