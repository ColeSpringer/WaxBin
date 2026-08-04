// Package testsock hands tests a unix socket path short enough to bind.
package testsock

import (
	"os"
	"path/filepath"
	"testing"
)

// Path returns a socket path in a temp directory removed at cleanup.
//
// It exists because t.TempDir() is unusable for a socket: it embeds the test's own
// name and a numbered subdirectory, and a unix address is capped at 104 bytes on
// darwin (108 elsewhere), so under macOS's long /var/folders TMPDIR a test fails
// with an opaque "bind: invalid argument" purely because its name is long. Naming a
// test is not supposed to be a correctness decision.
func Path(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wb")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}
