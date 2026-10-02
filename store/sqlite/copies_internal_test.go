package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestCopyJoinedReportsANewEdge: a put reports Joined when it attached the file as an
// alternate it was not already, a new copy or a row no item held, and not when it read
// an attached copy again or followed it to a new path.
func TestCopyJoinedReportsANewEdge(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	track := func(path, content string) model.PutScannedTrackInput {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return model.PutScannedTrackInput{
			LibraryID: lib.ID,
			File: model.File{Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
				Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 1, ContentHash: content,
				EssenceHash: "sha256:SAME", ScanState: model.ScanIndexed},
			Item:  model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: "Song", SortKey: "song", IdentityKey: "essence:sha256:SAME"},
			Track: model.Track{Artist: "Artist", Album: "Album", TrackNo: 1},
		}
	}
	put := func(in model.PutScannedTrackInput) *model.ScanItemResult {
		t.Helper()
		r, err := st.PutScannedTrack(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	put(track(filepath.Join(root, "a", "1.mp3"), "c1"))
	cp := track(filepath.Join(root, "b", "1.mp3"), "c2")
	if r := put(cp); !r.AttachedAsCopy || !r.Joined {
		t.Errorf("new copy = %+v, want it joined", r)
	}
	if r := put(cp); r.Joined {
		t.Errorf("copy read again = %+v, want nothing joined", r)
	}
	moved := track(filepath.Join(root, "c", "1.mp3"), "c2")
	if err := os.Remove(filepath.Join(root, "b", "1.mp3")); err != nil {
		t.Fatal(err)
	}
	if r := put(moved); !r.Relinked || r.Joined {
		t.Errorf("moved copy = %+v, want it relinked and not joined", r)
	}
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM item_file WHERE file_id = (SELECT id FROM file WHERE path = ?)", moved.File.Path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := put(moved); !r.AttachedAsCopy || !r.Joined {
		t.Errorf("row no item held = %+v, want it joined as a copy", r)
	}
}
