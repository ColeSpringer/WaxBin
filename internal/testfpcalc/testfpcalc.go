// Package testfpcalc writes stand-ins for the fpcalc binary.
package testfpcalc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	// Write the executable in a child, so a concurrent fork in the test process
	// cannot inherit its write descriptor and make exec fail with ETXTBSY. Run
	// waits for the writer and its children to exit before returning the path.
	cmd := exec.Command("/bin/sh", "-c", `cat > "$1" && chmod 755 "$1"`, "testfpcalc", bin)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write fpcalc stand-in: %v: %s", err, out)
	}
	return bin
}
