package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// bookInputWithPID builds a minimal single-file book scan input carrying a preferred
// item PID hint (the WAXBIN_ITEM_PID a rebuild reads back from the file).
func bookInputWithPID(libID int64, path, essence, title, author string, preferred model.PID) model.PutScannedBookInput {
	return model.PutScannedBookInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 10, MTimeNS: 1,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence,
		},
		Book:             model.Book{Author: author, AuthorSort: model.SortKey(author), Authors: []string{author}},
		Chapters:         []model.Chapter{{Position: 0, Title: title}},
		PreferredItemPID: preferred,
	}
}

func TestPutScannedBookAdoptsPreferredPID(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// A rebuild passes the stamped PID; a fresh, unclaimed hint is adopted so the book
	// keeps its original identity (play state, provenance ride along).
	want := model.NewPID()
	res, err := st.PutScannedBook(ctx, bookInputWithPID(lib.ID, "/lib/a.m4b", "e1", "Book One", "Author", want))
	if err != nil {
		t.Fatalf("put book: %v", err)
	}
	if !res.ItemCreated {
		t.Fatal("expected a new book item")
	}
	if res.ItemPID != want {
		t.Fatalf("book did not adopt the stamped PID: got %s, want %s", res.ItemPID, want)
	}

	// Identity stays essence-first: a DIFFERENT book carrying an already-taken hint must
	// mint a fresh PID rather than collide.
	res2, err := st.PutScannedBook(ctx, bookInputWithPID(lib.ID, "/lib/b.m4b", "e2", "Book Two", "Author", want))
	if err != nil {
		t.Fatalf("put book 2: %v", err)
	}
	if res2.ItemPID == want {
		t.Fatal("a taken preferred PID must not be reused; expected a fresh PID")
	}
}

func TestThumbCacheLRU(t *testing.T) {
	// A nil cache is a permanent miss and never panics (defensive).
	var nilc *thumbCache
	if _, ok := nilc.get("h", 1); ok {
		t.Error("nil cache should miss")
	}
	nilc.put("h", 1, model.ArtBlob{})

	c := newThumbCache(2)
	a := model.ArtBlob{Bytes: []byte("A"), SourceHash: "ha", Thumbnail: true}
	b := model.ArtBlob{Bytes: []byte("B"), SourceHash: "hb", Thumbnail: true}
	d := model.ArtBlob{Bytes: []byte("D"), SourceHash: "hd", Thumbnail: true}

	c.put("ha", 100, a)
	c.put("hb", 100, b)
	if got, ok := c.get("ha", 100); !ok || string(got.Bytes) != "A" {
		t.Fatalf("get (ha,100) = %q ok=%v, want A", got.Bytes, ok)
	}
	// (ha,100) was just used, so it is MRU; inserting a third entry evicts hb (LRU).
	c.put("hd", 100, d)
	if _, ok := c.get("hb", 100); ok {
		t.Error("hb should have been evicted as least-recently-used")
	}
	if _, ok := c.get("ha", 100); !ok {
		t.Error("ha should survive (recently used before eviction)")
	}
	if _, ok := c.get("hd", 100); !ok {
		t.Error("hd should be present")
	}
	// size is part of the key: same hash, different box size is a distinct entry.
	if _, ok := c.get("ha", 200); ok {
		t.Error("(ha,200) is a different key from (ha,100); should miss")
	}
	// An update in place replaces the value without growing the cache.
	c.put("ha", 100, d)
	if got, _ := c.get("ha", 100); string(got.Bytes) != "D" {
		t.Errorf("in-place update = %q, want D", got.Bytes)
	}
}

func TestThumbCacheBytesIsolated(t *testing.T) {
	c := newThumbCache(4)

	// The cache must not share a backing array with the caller who PUT the bytes: a
	// later mutation of that slice must not reach the cache.
	src := []byte("ORIG")
	c.put("h", 50, model.ArtBlob{Bytes: src, SourceHash: "h"})
	src[0] = 'X'
	got1, ok := c.get("h", 50)
	if !ok || string(got1.Bytes) != "ORIG" {
		t.Fatalf("put did not copy: cache shows %q after caller mutated its slice", got1.Bytes)
	}

	// And two GETs must not alias each other: mutating one returned slice must not
	// affect the cache or a second reader.
	got1.Bytes[0] = 'Z'
	got2, _ := c.get("h", 50)
	if string(got2.Bytes) != "ORIG" {
		t.Errorf("get did not copy: a mutated returned slice corrupted the cache (%q)", got2.Bytes)
	}
}

func TestMigration0024ReclassifiesChapters(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// Books written through the store default to source='embedded', which is exactly the
	// state v22's DEFAULT left every pre-existing chapter in. Craft the three shapes:
	//   synthetic     - one open-ended (0..0) chapter, the scanner's placeholder
	//   multi         - two real chapters (must stay embedded)
	//   singleBounded - one chapter but with real end_ms (must stay embedded)
	synthetic := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/synth.m4b", essence: "se", content: "sc", title: "Synth", author: "A",
		chapters: []model.Chapter{{Position: 0, Title: "whole", FileStartMS: 0, FileEndMS: 0}},
	})
	multi := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/multi.m4b", essence: "me", content: "mc", title: "Multi", author: "A",
		chapters: []model.Chapter{
			{Position: 0, Title: "a", FileStartMS: 0, FileEndMS: 0},
			{Position: 1, Title: "b", FileStartMS: 1000, FileEndMS: 0},
		},
	})
	singleBounded := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/single.m4b", essence: "1e", content: "1c", title: "Single", author: "A",
		chapters: []model.Chapter{{Position: 0, Title: "only", FileStartMS: 0, FileEndMS: 5000}},
	})

	// An episode whose (only-possible) podcast_url chapters were mislabeled 'embedded' by
	// v22. file_id is nil (irrelevant to the episode rule, which keys on item kind).
	if _, err := st.write.ExecContext(ctx,
		`INSERT INTO playable_item(pid, kind, state, title, sort_key, created_at, updated_at)
		 VALUES ('ep-round4', 'episode', 'present', 'Ep', 'ep', 1, 1)`); err != nil {
		t.Fatalf("insert episode item: %v", err)
	}
	var epID int64
	if err := st.write.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE pid = 'ep-round4'").Scan(&epID); err != nil {
		t.Fatalf("episode id: %v", err)
	}
	if _, err := st.write.ExecContext(ctx,
		`INSERT INTO chapter(book_item_id, file_id, position, title, start_ms, end_ms, source)
		 VALUES (?, NULL, 0, 'segment', 0, 60000, 'embedded')`, epID); err != nil {
		t.Fatalf("insert episode chapter: %v", err)
	}

	// Apply the actual shipped migration SQL (re-running it is a no-op on already-correct
	// rows, so this also exercises its safety on re-run).
	sqlBytes, err := migrationsFS.ReadFile("migrations/0024_chapter_source_backfill.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := st.write.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("apply migration 0024: %v", err)
	}

	sources := func(bookPID model.PID) []string {
		t.Helper()
		rows, err := st.write.QueryContext(ctx,
			`SELECT c.source FROM chapter c JOIN playable_item pi ON pi.id = c.book_item_id
			 WHERE pi.pid = ? ORDER BY c.position`, string(bookPID))
		if err != nil {
			t.Fatalf("query sources: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if got := sources(synthetic.ItemPID); len(got) != 1 || got[0] != "synthetic" {
		t.Errorf("synthetic book chapter source = %v, want [synthetic]", got)
	}
	if got := sources(multi.ItemPID); len(got) != 2 || got[0] != "embedded" || got[1] != "embedded" {
		t.Errorf("multi-chapter book sources = %v, want both embedded (untouched)", got)
	}
	if got := sources(singleBounded.ItemPID); len(got) != 1 || got[0] != "embedded" {
		t.Errorf("single bounded (real end_ms) chapter source = %v, want [embedded] (not synthetic)", got)
	}
	if got := sources("ep-round4"); len(got) != 1 || got[0] != "podcast_url" {
		t.Errorf("episode chapter source = %v, want [podcast_url]", got)
	}
}
