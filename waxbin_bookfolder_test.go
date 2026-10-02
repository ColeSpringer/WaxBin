package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// untaggedBook opens an audiobook root holding one untagged part, Someone/Tome/01.mp3,
// scans it into a book named for its folder, and returns the library and the book.
func untaggedBook(t *testing.T, ctx context.Context) (*waxbin.Library, model.PID, string, string) {
	t.Helper()
	root, db := t.TempDir(), filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "Someone", "Tome", "01.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(1)}))
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Tome")
	if err := lib.Playback().Checkpoint(ctx, "", pid, 4000, nil); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	return lib, pid, root, db
}

// keepsBook asserts the book survived with its play position, as the catalog's only item.
func keepsBook(t *testing.T, ctx context.Context, lib *waxbin.Library, pid model.PID) {
	t.Helper()
	v, err := lib.Get(ctx, pid)
	if err != nil || v.Kind != model.KindBook || v.State != model.StatePresent {
		t.Fatalf("book = %+v (err %v), want it present", v, err)
	}
	if st, err := lib.Playback().State(ctx, "", pid); err != nil || st.PositionMS != 4000 {
		t.Errorf("play state = %+v (err %v), want the position kept", st, err)
	}
	if items, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), ""); len(items) != 1 {
		t.Errorf("items = %d, want the one book", len(items))
	}
}

// TestFolderNamedBookSurvivesAFolderRename: a one-part book named for its folder keeps its
// item when the folder is renamed, its tags still naming no book.
func TestFolderNamedBookSurvivesAFolderRename(t *testing.T) {
	ctx := context.Background()
	lib, pid, root, _ := untaggedBook(t, ctx)
	if err := os.Rename(filepath.Join(root, "Someone", "Tome"), filepath.Join(root, "Someone", "Tome - Book One")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	keepsBook(t, ctx, lib, pid)
	// Its one file names it, now after the renamed folder.
	if v, _ := lib.Get(ctx, pid); v.Title != "Tome - Book One" {
		t.Errorf("title = %q, want the renamed folder's", v.Title)
	}
}

// TestFolderNamedBookSurvivesOrganize: organize moves a book named for its folder into a
// folder its own layout names, and a forced scan keeps the item.
func TestFolderNamedBookSurvivesOrganize(t *testing.T) {
	ctx := context.Background()
	lib, pid, _, _ := untaggedBook(t, ctx)
	if err := lib.EditFields(ctx, pid, map[string]string{"year": "1937"}, waxbin.EditOptions{}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan organize: %v", err)
	}
	if _, err := lib.ApplyOrganize(ctx, plan); err != nil {
		t.Fatalf("organize: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	keepsBook(t, ctx, lib, pid)
}

// TestFolderNamedBookSurvivesAnAuthorWriteBack: an author written back to a book whose
// tags name no title leaves its key alone, and a forced scan keeps the item.
func TestFolderNamedBookSurvivesAnAuthorWriteBack(t *testing.T) {
	ctx := context.Background()
	lib, pid, _, db := untaggedBook(t, ctx)
	key := storedIdentityKey(t, ctx, db, pid)
	if err := lib.EditFields(ctx, pid, map[string]string{"author": "Real Author"}, waxbin.EditOptions{WriteBack: true}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := storedIdentityKey(t, ctx, db, pid); got != key {
		t.Errorf("identity key = %q after the write-back, want %q kept", got, key)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	keepsBook(t, ctx, lib, pid)
}

// TestJoinedPartKeepsItsOwedTitle: a part the folder rule keeps in its book is held to its
// own tags when the scan settles owed rows, so a catalog-only title edit stays owed on it.
func TestJoinedPartKeepsItsOwedTitle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	for i := 1; i <= 3; i++ {
		spec := testaudio.MP3Spec{Title: "Chapter " + string(rune('0'+i)), Artist: "Author", AlbumArtist: "Author",
			Album: "Tome", Track: i, Audio: testaudio.AudioWithSeed(byte(i))}
		if i == 1 {
			spec.TXXX = []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
		}
		writeFile(t, filepath.Join(root, "Author", "Tome", "0"+string(rune('0'+i))+".mp3"), testaudio.BuildMP3FromSpec(spec))
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Tome")
	if err := lib.EditFields(ctx, pid, map[string]string{"title": "Tome Edited"}, waxbin.EditOptions{}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	writeFile(t, filepath.Join(root, "Author", "Tome", "02.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Chapter Two", Artist: "Author", AlbumArtist: "Author", Album: "Tome", Track: 2, Audio: testaudio.AudioWithSeed(2)}))
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	files, err := lib.ItemFiles(ctx, pid)
	if err != nil || len(files) != 3 {
		t.Fatalf("book files = %d (err %v), want the retagged part kept", len(files), err)
	}
	owed, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{ItemPID: pid, Code: model.DiagTagWriteOwed})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	held := 0
	for _, d := range owed {
		if d.TagKey == "title" {
			held++
		}
	}
	if held != 3 {
		t.Errorf("owed title rows = %d, want every part still owing the edited title", held)
	}
}
