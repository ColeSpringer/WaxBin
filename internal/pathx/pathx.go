// Package pathx holds small filesystem-path helpers shared across subsystems.
// Containment is a load-bearing invariant (organize's ModeInPlace filter, scan's
// sub-path guard, config's overlap check all depend on it), so it lives in one
// place rather than being re-derived per package.
//
// Case is part of that invariant. UnderRoot and SamePath both carry the platform's
// rule through filepath.Rel, which folds case on Windows only, and FoldsCase exposes
// that same rule as a constant so the catalog can match a library root by it instead
// of by raw bytes. Everything that compares two paths goes through one of the three,
// so no layer can end up folding while another does not.
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

// SamePath reports whether two absolute, cleaned paths name the same location, by
// the same rules UnderRoot uses. Byte equality is wrong on Windows, where path
// comparison folds case; filepath.Rel carries the platform's rule.
func SamePath(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}
