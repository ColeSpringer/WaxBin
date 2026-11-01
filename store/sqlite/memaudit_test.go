//go:build libc.memgrind && linux

package sqlite

import (
	"fmt"
	"os"
	"testing"

	"modernc.org/libc"
)

// TestMain audits the SQLite driver's C heap for the whole run, which takes a build with
// -tags=libc.memgrind: a double free, or a free of memory libc never handed out, panics
// naming both call sites; freed memory is zeroed, so a later read of it tends to fail the
// test that made it; and anything still allocated after the last test fails the run.
// libc's copy of the environment is made first, since it is kept for the life of the
// process.
func TestMain(m *testing.M) {
	libc.Environ()
	libc.MemAuditStart()
	code := m.Run()
	if err := libc.MemAuditReport(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
