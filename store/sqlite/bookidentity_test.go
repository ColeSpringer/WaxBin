package sqlite

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// setBookASIN fills book.asin the way BookEnrichment does, without moving the item's
// identity_key. That gap is what adoptBookItemByASINTx exists to close.
func setBookASIN(t *testing.T, st *Store, pid model.PID, asin string) {
	t.Helper()
	if _, err := st.write.ExecContext(context.Background(),
		"UPDATE book SET asin = ? WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)",
		asin, string(pid)); err != nil {
		t.Fatalf("set asin: %v", err)
	}
}

// setBookISBN fills book.isbn the way BookEnrichment does, key column included.
func setBookISBN(t *testing.T, st *Store, pid model.PID, isbn string) {
	t.Helper()
	if _, err := st.write.ExecContext(context.Background(),
		`UPDATE book SET isbn = ?, isbn_key = ?
		 WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)`,
		isbn, identity.ISBNKey(isbn), string(pid)); err != nil {
		t.Fatalf("set isbn: %v", err)
	}
}

// TestBookPartAdoptsEnrichedASIN: a part arriving with the ASIN enrichment wrote joins
// the standing book. Without adoption its asin: key matches nothing and it forks.
func TestBookPartAdoptsEnrichedASIN(t *testing.T) {
	st, lib := entityFixture(t)

	first := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/part1.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 1000, position: 1,
	})
	setBookASIN(t, st, first.ItemPID, "B00XASIN")

	second := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/part2.m4b", essence: "be2", content: "bc2",
		title: "The Hobbit", author: "J.R.R. Tolkien", asin: "B00XASIN",
		durationMS: 2000, position: 2,
	})
	if second.ItemCreated {
		t.Fatalf("the ASIN-tagged part forked a second book (%s)", second.ItemPID)
	}
	if second.ItemPID != first.ItemPID {
		t.Fatalf("item pid = %s, want the standing book %s", second.ItemPID, first.ItemPID)
	}
	assertVerifyClean(t, st)
}

// TestBookAdoptsByEditionColumn: an edition edited in the catalog alone, or read from a
// file tagged before the reader learned the EDITION key, sits in the column while the
// stored key stays edition-less. A rescan of the file computes the key with the edition
// segment, which matches nothing, so it joins the standing book whose column already
// names that edition rather than forking. A different edition is a different book.
func TestBookAdoptsByEditionColumn(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	first := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/hobbit.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 1000, position: 1,
	})
	if err := st.EditItemField(ctx, first.ItemPID, "edition", "75th Anniversary Edition",
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("edit edition: %v", err)
	}

	again := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/hobbit.m4b", essence: "be1", content: "bc2",
		title: "The Hobbit", author: "J.R.R. Tolkien", edition: "75th Anniversary Edition",
		durationMS: 1000, position: 1,
	})
	if again.ItemCreated || again.ItemPID != first.ItemPID {
		t.Fatalf("the retagged file resolved %s (created=%v), want the standing book %s",
			again.ItemPID, again.ItemCreated, first.ItemPID)
	}

	other := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/hobbit-abridged.m4b", essence: "be2", content: "bc3",
		title: "The Hobbit", author: "J.R.R. Tolkien", edition: "Abridged",
		durationMS: 900, position: 1,
	})
	if !other.ItemCreated || other.ItemPID == first.ItemPID {
		t.Fatalf("a differently-edited file joined the standing book %s", first.ItemPID)
	}
	assertVerifyClean(t, st)
}

// TestAdoptedBookPartSeesTheLockOverlay pins the second wiring. The overlay runs before
// upsertItem, so without adoption there too the adopted part's scanned title overwrites
// the curated one on the book it is joining.
func TestAdoptedBookPartSeesTheLockOverlay(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	first := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/part1.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 1000, position: 1,
	})
	if err := st.EditItemField(ctx, first.ItemPID, "title", "The Hobbit (Unabridged)",
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit title: %v", err)
	}
	setBookASIN(t, st, first.ItemPID, "B00XASIN")

	second := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/part2.m4b", essence: "be2", content: "bc2",
		title: "The Hobbit", author: "J.R.R. Tolkien", asin: "B00XASIN",
		durationMS: 2000, position: 2, preserveLocks: true,
	})
	if second.ItemPID != first.ItemPID {
		t.Fatalf("item pid = %s, want the standing book %s", second.ItemPID, first.ItemPID)
	}
	view, err := st.ItemByPID(ctx, first.ItemPID)
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	if view.Title != "The Hobbit (Unabridged)" {
		t.Errorf("title = %q, want the locked one to survive the adopted part", view.Title)
	}
	assertVerifyClean(t, st)
}

// warnRecorder collects the messages a store logs, so a test can assert the operator
// was told about an ambiguity rather than only that one side of it was picked.
type warnRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (h *warnRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *warnRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *warnRecorder) WithGroup(string) slog.Handler            { return h }

func (h *warnRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *warnRecorder) has(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// TestBookASINHeldByTwoBooksAdoptsTheLowest: nothing enforces uniqueness on book.asin,
// so the tie is broken deterministically and logged for audit to report the pair.
func TestBookASINHeldByTwoBooksAdoptsTheLowest(t *testing.T) {
	rec := &warnRecorder{}
	ctx := context.Background()
	st, err := Open(ctx, OpenOptions{
		Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test", Logger: slog.New(rec),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib"), DisplayRoot: "/lib", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure library: %v", err)
	}

	// Two printings of the same book: the edition keeps them apart as items while the
	// author and title both corroborate, so the ASIN really is ambiguous between them.
	older := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/a.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", edition: "Unabridged", durationMS: 1000,
	})
	newer := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b.m4b", essence: "be2", content: "bc2",
		title: "The Hobbit", author: "J.R.R. Tolkien", edition: "Dramatised", durationMS: 1000,
	})
	setBookASIN(t, st, older.ItemPID, "B00XASIN")
	setBookASIN(t, st, newer.ItemPID, "B00XASIN")

	part := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/c.m4b", essence: "be3", content: "bc3",
		title: "The Hobbit", author: "J.R.R. Tolkien", asin: "B00XASIN",
		durationMS: 1000, position: 2,
	})
	if part.ItemPID != older.ItemPID {
		t.Errorf("item pid = %s, want the lowest-id holder %s", part.ItemPID, older.ItemPID)
	}
	if !rec.has("book identifier held by more than one matching book") {
		t.Errorf("the ambiguous ASIN was adopted silently; logged %v", rec.msgs)
	}
	assertVerifyClean(t, st)
}

// TestBookASINOnAnUnrelatedBookForks: an ASIN reaches this catalog through a tag anyone
// can write, and adopting on it alone let a wrong one swallow an unrelated book and
// overwrite its title. The author has to agree before the join happens.
func TestBookASINOnAnUnrelatedBookForks(t *testing.T) {
	st, lib := entityFixture(t)

	hobbit := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/a.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 1000,
	})
	setBookASIN(t, st, hobbit.ItemPID, "B00XASIN")

	other := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b.m4b", essence: "be2", content: "bc2",
		title: "Dune", author: "Frank Herbert", asin: "B00XASIN", durationMS: 5000,
	})
	if !other.ItemCreated || other.ItemPID == hobbit.ItemPID {
		t.Fatalf("a mis-tagged ASIN was adopted into %s", hobbit.ItemPID)
	}
	if got := scalarStr(t, st, "SELECT title FROM playable_item WHERE pid=?", string(hobbit.ItemPID)); got != "The Hobbit" {
		t.Errorf("standing book title = %q, want it untouched by the mis-tagged file", got)
	}
	assertVerifyClean(t, st)
}

// TestBookVolumeSharingASeriesASINForks: a tagger that writes the series ASIN into every
// volume gives them one key, and the author alone cannot tell them apart. The title does,
// so long as the standing book's own title has not been curated away from what its files
// say.
func TestBookVolumeSharingASeriesASINForks(t *testing.T) {
	st, lib := entityFixture(t)

	one := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/1.m4b", essence: "be1", content: "bc1",
		title: "The Fellowship of the Ring", author: "J.R.R. Tolkien", durationMS: 1000,
	})
	setBookASIN(t, st, one.ItemPID, "B00SERIES")

	two := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/2.m4b", essence: "be2", content: "bc2",
		title: "The Two Towers", author: "J.R.R. Tolkien", asin: "B00SERIES", durationMS: 1000,
	})
	if !two.ItemCreated || two.ItemPID == one.ItemPID {
		t.Fatalf("a shared series ASIN collapsed two volumes onto %s", one.ItemPID)
	}
	assertVerifyClean(t, st)
}

// TestBookPartAdoptsAnEnrichedISBN is the ASIN case's twin, and the reason book.isbn_key
// exists: identity.BookKey strips an ISBN's separators while the raw column keeps what
// the tag said, so the two spellings below would never have compared equal against the
// column itself.
func TestBookPartAdoptsAnEnrichedISBN(t *testing.T) {
	st, lib := entityFixture(t)

	first := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/part1.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 1000, position: 1,
	})
	setBookISBN(t, st, first.ItemPID, "978-0-13-468599-1")

	// The arriving part spells the same ISBN without separators.
	second := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/part2.m4b", essence: "be2", content: "bc2",
		title: "The Hobbit", author: "J.R.R. Tolkien", isbn: "9780134685991",
		durationMS: 2000, position: 2,
	})
	if second.ItemCreated || second.ItemPID != first.ItemPID {
		t.Fatalf("the ISBN-tagged part forked (%s), want the standing book %s",
			second.ItemPID, first.ItemPID)
	}
	assertVerifyClean(t, st)
}

// TestBookISBNOnAnUnrelatedBookForks: the ISBN arm corroborates exactly as the ASIN one
// does, so a mis-tagged identifier cannot swallow another book.
func TestBookISBNOnAnUnrelatedBookForks(t *testing.T) {
	st, lib := entityFixture(t)

	hobbit := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/a.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 1000,
	})
	setBookISBN(t, st, hobbit.ItemPID, "978-0-13-468599-1")

	other := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b.m4b", essence: "be2", content: "bc2",
		title: "Dune", author: "Frank Herbert", isbn: "9780134685991", durationMS: 5000,
	})
	if !other.ItemCreated || other.ItemPID == hobbit.ItemPID {
		t.Fatalf("a mis-tagged ISBN was adopted into %s", hobbit.ItemPID)
	}
	assertVerifyClean(t, st)
}

// TestDescriptiveBookKeyIsUntouched: a book: key carries no identifier to look up, so
// the adoption probe has nothing to do and two different books stay apart.
func TestDescriptiveBookKeyIsUntouched(t *testing.T) {
	st, lib := entityFixture(t)

	first := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/a.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 1000,
	})
	second := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b.m4b", essence: "be2", content: "bc2",
		title: "The Silmarillion", author: "J.R.R. Tolkien", durationMS: 1000,
	})
	if !second.ItemCreated || second.ItemPID == first.ItemPID {
		t.Fatalf("descriptive keys collapsed onto one book: %s vs %s", first.ItemPID, second.ItemPID)
	}
	assertVerifyClean(t, st)
}

// TestBookIdentLookupIgnoresISBNSeparators: the cross-catalog resolver compares the same
// canonical key the adoption does, so a caller holding either spelling finds the book.
func TestBookIdentLookupIgnoresISBNSeparators(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	book := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/a.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", isbn: "978-0-13-468599-1", durationMS: 1000,
	})
	for _, spelling := range []string{"978-0-13-468599-1", "9780134685991", "ISBN 978 0 13 468599 1"} {
		got, err := st.ItemByBookIdent(ctx, "", "", spelling)
		if err != nil {
			t.Fatalf("lookup %q: %v", spelling, err)
		}
		if got.PID != book.ItemPID {
			t.Errorf("lookup %q = %s, want %s", spelling, got.PID, book.ItemPID)
		}
	}
}

// TestVerifyCatchesISBNKeyDrift: isbn_key is generated in Go beside the raw column, so a
// writer that sets one without the other is drift `db verify` has to name rather than a
// book that silently stops resolving.
func TestVerifyCatchesISBNKeyDrift(t *testing.T) {
	st, lib := entityFixture(t)
	book := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/a.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", isbn: "978-0-13-468599-1", durationMS: 1000,
	})
	assertVerifyClean(t, st)

	if _, err := st.write.ExecContext(context.Background(),
		"UPDATE book SET isbn = ? WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)",
		"978-0-306-40615-7", string(book.ItemPID)); err != nil {
		t.Fatalf("skew the raw column: %v", err)
	}
	rep, err := st.VerifyDerived(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.BookISBNKeyDrift != 1 || rep.Consistent() {
		t.Errorf("report = %+v, want one isbn-key drift and an inconsistent verdict", rep)
	}
}
