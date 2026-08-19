//go:build !windows

package pathx

// FoldsCase reports whether the platform compares paths case-insensitively, the rule
// filepath.Rel carries and so the rule SamePath and UnderRoot follow. It is false here,
// which keeps raw path bytes as a library root's identity: a POSIX path is a byte
// string with no declared encoding, so folding it is not defined.
//
// That makes it false on darwin too, where a stock APFS volume does fold. Deliberate:
// pathx cannot fold there without folding for every POSIX caller, and the constant's
// job is to keep the catalog's rule and pathx's identical. The audit's library_conflict
// check reports the collision on those volumes instead.
const FoldsCase = false
