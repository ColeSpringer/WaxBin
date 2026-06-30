//go:build windows

package pathx

import "strings"

// Long returns the Windows extended-length form of an absolute path so a
// filesystem operation is not bounded by the legacy 260-character MAX_PATH. The
// "\\?\" prefix opts a path out of the limit (and out of further normalization),
// so it is added only to a clean absolute path that is already long enough to
// need it:
//
//   - "C:\very\long\path"            -> "\\?\C:\very\long\path"
//   - "\\server\share\long\path"     -> "\\?\UNC\server\share\long\path"
//   - already-prefixed / short paths -> returned unchanged (kept readable in logs)
//
// A short path is left alone so error messages and the catalog keep the plain form.
func Long(abs string) string {
	if strings.HasPrefix(abs, `\\?\`) {
		return abs
	}
	if strings.HasPrefix(abs, `\\`) {
		// UNC path (\\server\share\...). The extended form drops the leading "\\"
		// and prefixes "\\?\UNC\", yielding "\\?\UNC\server\share\...".
		if len(abs) < 260 {
			return abs
		}
		return `\\?\UNC\` + abs[2:]
	}
	if len(abs) < 260 {
		return abs
	}
	return `\\?\` + abs
}
