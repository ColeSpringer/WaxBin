package caps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testfpcalc"
	"github.com/colespringer/waxbin/waxerr"
)

// TestResolveTakesAConfiguredBinary: a configured path that names an executable is the
// fpcalc in use.
func TestResolveTakesAConfiguredBinary(t *testing.T) {
	t.Parallel()
	bin := testfpcalc.Write(t, "", "", 0)
	if got, err := Resolve(bin); err != nil || got != bin {
		t.Fatalf("Resolve(%s) = %q (err %v), want it in use", bin, got, err)
	}
}

// TestResolveRefusesAPathThatDoesNotResolve: a configured path naming nothing, or a folder,
// is refused rather than quietly read as no fpcalc.
func TestResolveRefusesAPathThatDoesNotResolve(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "missing", "fpcalc"), dir, plain} {
		if got, err := Resolve(p); !waxerr.Is(err, waxerr.CodeInvalid) || got != "" {
			t.Errorf("Resolve(%s) = %q (err %v), want CodeInvalid", p, got, err)
		}
	}
}

// TestResolveMakesARelativePathAbsolute: a configured path relative to the working folder
// is held absolute, so fpcalc is still found after the process changes folder. Not
// parallel: it changes the process's working folder.
func TestResolveMakesARelativePathAbsolute(t *testing.T) {
	bin := testfpcalc.Write(t, "", "", 0)
	t.Chdir(filepath.Dir(filepath.Dir(bin)))
	if got, err := Resolve(filepath.Join(filepath.Base(filepath.Dir(bin)), "fpcalc")); err != nil || got != bin {
		t.Fatalf("Resolve(relative) = %q (err %v), want %s", got, err, bin)
	}
}

// TestResolveSearchesAPathOnce: the PATH search, slow where PATH is long, runs once for a
// given PATH, so a process opening many catalogs pays it once, and again only when PATH
// changes. Not parallel: it sets PATH.
func TestResolveSearchesAPathOnce(t *testing.T) {
	bin := testfpcalc.Write(t, "", "", 0)
	t.Setenv("PATH", filepath.Dir(bin))
	if got, err := Resolve(""); err != nil || got != bin {
		t.Fatalf("Resolve on PATH = %q (err %v), want %s", got, err, bin)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if got, err := Resolve(""); err != nil || got != bin {
		t.Errorf("Resolve on the same PATH = %q (err %v), want the first answer, not a second search", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if got, err := Resolve(""); err != nil || got != "" {
		t.Errorf("Resolve on a new PATH = %q (err %v), want a fresh search finding none", got, err)
	}
}
