// Package caps detects the optional external helper the analyze pass can use.
// The pure-Go baseline always works: WaxFlow decodes every format WaxBin can
// tag-read, with no CGO and no external binaries. The one remaining helper is
// fpcalc, which produces Chromaprint fingerprints (preferred for grouping, and
// required for AcoustID). It is capability-detected, reported by doctor, and never
// required for core use.
package caps

import (
	"os/exec"
	"sync"
)

// Caps reports which optional helpers are available on PATH. The pure-Go
// decoders and fingerprint do not appear here; they are always present.
type Caps struct {
	Fpcalc     bool // fpcalc: Chromaprint fingerprint (preferred grouping; AcoustID)
	FpcalcPath string
}

var (
	once   sync.Once
	cached Caps
)

// Detect probes PATH for the optional helpers once and caches the result. The
// look-ups are cheap, and a helper appearing/disappearing mid-process is not a
// case worth re-probing on every analyze file.
func Detect() Caps {
	once.Do(func() {
		if p, err := exec.LookPath("fpcalc"); err == nil {
			cached.Fpcalc, cached.FpcalcPath = true, p
		}
	})
	return cached
}
