// Package pathx holds small filesystem-path helpers shared across subsystems.
// Containment is a load-bearing invariant (organize's ModeInPlace filter, scan's
// sub-path guard, config's overlap check all depend on it), so it lives in one
// place rather than being re-derived per package.
package pathx

import (
	"path/filepath"
	"strings"
)

// UnderRoot reports whether p is root itself or nested beneath it. Both should
// be absolute and cleaned for a meaningful result.
func UnderRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
