package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// identityKeyOf reads a book/track item's stored identity key via a direct query.
func identityKeyOf(t *testing.T, st *Store, pid model.PID) string {
	t.Helper()
	var k string
	if err := st.read.QueryRow("SELECT COALESCE(identity_key,'') FROM playable_item WHERE pid = ?", string(pid)).Scan(&k); err != nil {
		t.Fatalf("read identity key: %v", err)
	}
	return k
}

// TestRekeyBook covers the identity re-anchor used by the audiobook write-back: a fresh
// key is adopted, a same/empty key is a no-op, a key already held by another book is
// skipped (not forced into a unique-index violation), and a track is never re-keyed.
func TestRekeyBook(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	book := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/T/hobbit.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "Tolkien",
	})
	other := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/T/silmarillion.m4b", essence: "be2", content: "bc2",
		title: "The Silmarillion", author: "Tolkien",
	})
	track := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/A/1.flac", essence: "te1", content: "tc1", title: "Song", artist: "Band",
	})

	// A fresh key is adopted.
	newKey := identity.BookKey("", "", "Tolkien", "The Hobbit: Illustrated", "")
	changed, err := st.RekeyBook(ctx, book.ItemPID, newKey)
	if err != nil || !changed {
		t.Fatalf("rekey to fresh key: changed=%v err=%v", changed, err)
	}
	if got := identityKeyOf(t, st, book.ItemPID); got != newKey {
		t.Fatalf("identity key = %q, want %q", got, newKey)
	}

	// Re-applying the same key is a no-op.
	if changed, err := st.RekeyBook(ctx, book.ItemPID, newKey); err != nil || changed {
		t.Fatalf("rekey to same key: changed=%v err=%v, want no-op", changed, err)
	}

	// An empty key is a no-op (the scanner would fall back to essence).
	if changed, err := st.RekeyBook(ctx, book.ItemPID, ""); err != nil || changed {
		t.Fatalf("rekey to empty: changed=%v err=%v, want no-op", changed, err)
	}

	// A key already held by another book is skipped, not forced (which would violate the
	// (kind, identity_key) unique index). The other book keeps its key.
	otherKey := identityKeyOf(t, st, other.ItemPID)
	if changed, err := st.RekeyBook(ctx, book.ItemPID, otherKey); err != nil || changed {
		t.Fatalf("rekey to a taken key: changed=%v err=%v, want skipped", changed, err)
	}
	if got := identityKeyOf(t, st, book.ItemPID); got != newKey {
		t.Errorf("book key after collision skip = %q, want unchanged %q", got, newKey)
	}

	// A track is never re-keyed (its identity is essence-anchored).
	if changed, err := st.RekeyBook(ctx, track.ItemPID, "mbid:should-be-ignored"); err != nil || changed {
		t.Fatalf("rekey track: changed=%v err=%v, want no-op", changed, err)
	}
}
