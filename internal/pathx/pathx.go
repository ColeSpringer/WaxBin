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
	"unicode"

	"golang.org/x/text/unicode/norm"
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

// RelUnder returns p relative to root when p lies under it (UnderRoot), and otherwise its
// base name, never a path that climbs out of the root.
func RelUnder(root, p string) string {
	if UnderRoot(root, p) {
		if rel, err := filepath.Rel(root, p); err == nil {
			return rel
		}
	}
	return filepath.Base(p)
}

// SamePath reports whether two absolute, cleaned paths name the same location, by
// the same rules UnderRoot uses. Byte equality is wrong on Windows, where path
// comparison folds case; filepath.Rel carries the platform's rule.
func SamePath(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}

// CollisionKey is the key under which two paths name one file on a filesystem that
// matches names without regard to case or Unicode form, as NTFS, APFS and exFAT do. It
// folds on every platform, unlike SamePath: a library is laid out so it stays whole on
// any of them.
func CollisionKey(p string) string { return FoldName(filepath.Clean(p)) }

// FoldName is a name as a filesystem that ignores case and Unicode form reads it: NFC,
// each rune the least of its case folds, so two names strings.EqualFold matches share it
// (a final sigma and a capital sigma too, which lower-casing tells apart).
func FoldName(name string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, norm.NFC.String(name))
}
