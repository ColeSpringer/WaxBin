// Package testfpcalc writes stand-ins for the fpcalc binary.
package testfpcalc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Write returns the path of an executable that ignores its arguments, prints stdout and
// stderr, and exits with code. It is a shell script, so the test is skipped on Windows.
func Write(t testing.TB, stdout, stderr string, code int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fpcalc stand-in is a shell script")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"out": stdout, "err": stderr} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "fpcalc")
	script := fmt.Sprintf("#!/bin/sh\ncat '%s/out'\ncat '%s/err' >&2\nexit %d\n", dir, dir, code)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}
