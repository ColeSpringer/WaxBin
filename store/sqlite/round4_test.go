package sqlite

import (
	"context"
	"fmt"
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

	c := newThumbCache(2, 1<<20)
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

// TestThumbCacheNegativeEntries covers the negative cache. A source whose bytes do not
// decode is handed to the generator at every rung rather than short-circuiting above its
// own size, so without this the same decode is attempted and the same warning emitted
// once per request during a grid scroll. It is a separate instance from the thumbnail
// cache so a run of undecodable covers cannot flush the real thumbnails beside them.
func TestThumbCacheNegativeEntries(t *testing.T) {
	// A nil negative cache is a permanent miss and never panics, the same guard put has.
	var nilc *thumbCache
	nilc.put("h", 1, model.ArtBlob{})
	if _, ok := nilc.get("h", 1); ok {
		t.Error("nil cache should miss")
	}

	fail := newThumbCache(thumbFailMax, thumbFailBytes)
	fail.put("bad", 100, model.ArtBlob{})
	if _, ok := fail.get("bad", 100); !ok {
		t.Error("a recorded failure should read back")
	}
	if _, ok := fail.get("bad", 200); ok {
		t.Error("size is part of the key; another box should miss")
	}

	// The two caches have independent budgets, which is the point of the split: filling
	// the negative one past its bound must leave a thumbnail cached beside it untouched.
	thumbs := newThumbCache(4, 1<<20)
	thumbs.put("real", 100, model.ArtBlob{Bytes: []byte("R"), SourceHash: "real", Thumbnail: true})
	for i := range thumbFailMax * 2 {
		fail.put(fmt.Sprintf("f%d", i), 100, model.ArtBlob{})
	}
	if got, ok := thumbs.get("real", 100); !ok || string(got.Bytes) != "R" {
		t.Error("a run of generation failures evicted a real thumbnail; the budgets must be separate")
	}
	if n := fail.ll.Len(); n > thumbFailMax {
		t.Errorf("negative cache holds %d entries, want at most %d", n, thumbFailMax)
	}
}

// TestThumbCacheByteBound pins the second bound. A resolve at or above a source's own
// size re-encodes at that size, so entry count alone would let a handful of large covers
// pin far more memory than the cache is meant to hold.
func TestThumbCacheByteBound(t *testing.T) {
	c := newThumbCache(100, 1000)
	for i := range 10 {
		c.put(fmt.Sprintf("h%d", i), 100, model.ArtBlob{Bytes: make([]byte, 300)})
	}
	if c.bytes > 1000 {
		t.Errorf("cache holds %d bytes, want at most 1000", c.bytes)
	}
	if c.ll.Len() != len(c.items) {
		t.Errorf("list and map disagree: %d vs %d", c.ll.Len(), len(c.items))
	}

	// An entry larger than the whole bound is still served rather than evicting itself
	// on the way in, so a single big cover cannot become a permanent miss.
	c.put("huge", 100, model.ArtBlob{Bytes: make([]byte, 5000)})
	if _, ok := c.get("huge", 100); !ok {
		t.Error("an oversized entry evicted itself; the most recent entry must survive")
	}

	// Replacing an entry re-charges it rather than double-counting the old bytes.
	c2 := newThumbCache(10, 10000)
	c2.put("x", 1, model.ArtBlob{Bytes: make([]byte, 400)})
	c2.put("x", 1, model.ArtBlob{Bytes: make([]byte, 100)})
	if c2.bytes != 100 {
		t.Errorf("after replacing a 400-byte entry with a 100-byte one, bytes = %d, want 100", c2.bytes)
	}
}

func TestThumbCacheBytesIsolated(t *testing.T) {
	c := newThumbCache(4, 1<<20)

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
