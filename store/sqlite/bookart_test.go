package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestABooksArtIsItsOwnCover: a book answers from its own cover alone. Its author's
// picture is a portrait and a genre's is a category tile, so a coverless book is
// NotFound in both art reads, while a track under the same artist keeps the whole chain.
func TestABooksArtIsItsOwnCover(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	book := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/1.m4b", essence: "eb1", content: "cb1", title: "The Dispossessed",
		author: "Ursula K. Le Guin", genres: []string{"Science Fiction"},
	}).ItemPID
	track := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Reading", artist: "Ursula K. Le Guin", album: "Readings",
	}).ItemPID
	user := model.Attribution{Source: model.SourceUser}
	author := model.PID(scalarStr(t, st, "SELECT pid FROM artist WHERE name = ?", "Ursula K. Le Guin"))
	if _, err := st.SetEntityArt(ctx, model.ArtArtist, author, model.ArtRoleFront, tinyPNG(t), "", user, model.LockUnchanged, false); err != nil {
		t.Fatalf("set the author's picture: %v", err)
	}
	genre := model.PID(scalarStr(t, st, "SELECT pid FROM genre WHERE name = ?", "Science Fiction"))
	if _, err := st.SetEntityArt(ctx, model.ArtGenre, genre, model.ArtRoleFront, testPNG(t, 5, 5).Data, "", user, model.LockUnchanged, false); err != nil {
		t.Fatalf("set the genre's picture: %v", err)
	}

	bookRef := model.EntityRef{Type: model.ArtTrack, PID: book}
	if blob, err := st.ResolveArt(ctx, bookRef, model.ArtRoleFront, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("ResolveArt on a coverless book = %+v (err %v), want CodeNotFound", blob, err)
	}
	if prov, err := st.ArtProvenance(ctx, bookRef, model.ArtRoleFront); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("ArtProvenance on a coverless book = %+v (err %v), want CodeNotFound", prov, err)
	}
	prov, err := st.ArtProvenance(ctx, model.EntityRef{Type: model.ArtTrack, PID: track}, model.ArtRoleFront)
	if err != nil || prov.Level != model.ArtArtist {
		t.Errorf("a coverless track's provenance = %+v (err %v), want the artist level", prov, err)
	}
	if !missingArtListed(t, st, book) {
		t.Error("missing_art does not list the coverless book")
	}

	if _, err := st.SetItemArt(ctx, book, model.ArtRoleFront, testPNG(t, 7, 7).Data, "", user, model.LockUnchanged, false); err != nil {
		t.Fatalf("set the book's cover: %v", err)
	}
	prov, err = st.ArtProvenance(ctx, bookRef, model.ArtRoleFront)
	if err != nil || prov.Level != model.ArtTrack {
		t.Errorf("a covered book's provenance = %+v (err %v), want the item level", prov, err)
	}
	if missingArtListed(t, st, book) {
		t.Error("missing_art still lists the book after it got a cover")
	}
}

func missingArtListed(t *testing.T, st *Store, pid model.PID) bool {
	t.Helper()
	items, _, err := st.ItemsMissingArt(context.Background(), 100)
	if err != nil {
		t.Fatalf("ItemsMissingArt: %v", err)
	}
	for _, it := range items {
		if it.PID == pid {
			return true
		}
	}
	return false
}
