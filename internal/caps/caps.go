// Package caps resolves the optional external helper the analyze pass and enrichment can
// use. The pure-Go baseline always works: WaxFlow decodes every format WaxBin can
// tag-read, with no CGO and no external binaries. The one remaining helper is fpcalc,
// which produces Chromaprint fingerprints (preferred for grouping, and the way AcoustID
// is asked about a file not yet analyzed with it). It is taken from the configured path
// or found on PATH, reported by doctor, and never required for core use.
package caps

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/colespringer/waxbin/waxerr"
)

// onPath holds the fpcalc found on each PATH this process has searched, "" for none.
var onPath sync.Map

// Resolve finds fpcalc: at path when one is configured, else on PATH, held absolute so a
// later change of working folder cannot lose it; "" means none. A configured path that
// names no executable is CodeInvalid, since a helper someone set should not go missing
// without a word; none on PATH is no error, only no helper. The PATH search, slow where
// PATH is long, runs once a process for a given PATH, so an fpcalc installed on PATH while
// a process runs is seen once it restarts, or at once through a configured path.
func Resolve(path string) (string, error) {
	if path == "" {
		key := os.Getenv("PATH")
		if p, ok := onPath.Load(key); ok {
			return p.(string), nil
		}
		p, err := lookup("fpcalc")
		if err != nil {
			p = ""
		}
		onPath.Store(key, p)
		return p, nil
	}
	p, err := lookup(path)
	if err != nil {
		return "", waxerr.Wrapf(waxerr.CodeInvalid, "caps.Resolve", err, "fpcalc path %s", path)
	}
	return p, nil
}

func lookup(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}
