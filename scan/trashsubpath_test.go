package scan

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestScanRefusesASubPathInsideTheTrash: a walk starting below the trash folder would
// never meet the skip that keeps trashed files out of the catalog, so the request is
// refused, however the sub-path is spelled: absolute, relative, or in another case,
// which a filesystem that ignores case reads as the trash.
func TestScanRefusesASubPathInsideTheTrash(t *testing.T) {
	t.Parallel()
	_, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, model.TrashDirName, "x", "1.mp3"), "Trashed", 1)
	for _, sub := range []string{
		filepath.Join(root, model.TrashDirName, "x"),
		filepath.Join(model.TrashDirName, "x"),
		filepath.Join(root, model.TrashDirName),
		filepath.Join(root, ".WAXBIN-TRASH", "x"),
	} {
		_, err := sc.Scan(context.Background(), Request{Library: lib, SubPath: sub}, nil)
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Fatalf("scan of %s: %v, want CodeInvalid", sub, err)
		}
	}
}
