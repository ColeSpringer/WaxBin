//go:build windows

package pathx

// FoldsCase reports whether the platform compares paths case-insensitively, the rule
// filepath.Rel carries and so the rule SamePath and UnderRoot follow. It is true here
// because NTFS folds case, and the catalog consults it when matching a library root so
// that C:\Music and c:\Music name one library rather than two.
//
// It is a build-tag constant rather than a probe of the actual volume because the two
// path helpers it has to agree with are themselves compile-time; a store that folded
// where SamePath did not would recreate the layer disagreement it exists to remove.
const FoldsCase = true
