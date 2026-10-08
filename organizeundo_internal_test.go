package waxbin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// TestUndoFollowsAFolderRespell: a folder an organize filled and a later one respelled
// (fsx.Speller, on a filesystem that folds case) carries the journal's paths below it with
// it, so undoing the first organize moves its files back rather than holding them all as
// moved since, as nothing moved. The respell is driven here by hand, as a Linux disk never
// asks for one.
func TestUndoFollowsAFolderRespell(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Inbox", "cover.jpg"), []byte("cover"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, title := range []string{"Midnight Drive", "Neon Rain"} {
		p := filepath.Join(root, "Inbox", title+".mp3")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: title, Artist: "The Foobars",
			Album: "Night Moves", Track: i + 1, Audio: testaudio.AudioWithSeed(byte(i + 1))}), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := Open(ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if rr, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), OrganizeOptions{}); err != nil || rr.Report.Moved != 2 {
		t.Fatalf("organize = %+v (err %v), want two moves", rr, err)
	}
	batches, err := lib.OrganizeHistory(ctx, 1)
	if err != nil || len(batches) != 1 {
		t.Fatalf("history = %+v (err %v)", batches, err)
	}
	from, to := filepath.Join(root, "The Foobars"), filepath.Join(root, "the foobars")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.store.RespellFolder(ctx, from, to); err != nil {
		t.Fatal(err)
	}

	rep, err := lib.UndoOrganize(ctx, batches[0].JobPID)
	if err != nil || rep.Moved != 2 || rep.Held != 0 {
		t.Fatalf("undo after the respell = %+v (err %v), want both files moved back", rep, err)
	}
	// The cover the organize carried comes back from the respelled folder too, which goes.
	if _, err := os.Stat(filepath.Join(root, "Inbox", "cover.jpg")); err != nil {
		t.Errorf("the cover is not back: %v", err)
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Errorf("the respelled folder is still there (err %v)", err)
	}
}
