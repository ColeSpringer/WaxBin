package pathx

import (
	"path/filepath"
	"strings"
)

// LineReadings are the cleaned paths a playlist line is read as, in the order they are
// tried: the line as written, then, where a backslash is no separator and the line holds
// one, the line with its backslashes as separators, the way a playlist written on Windows
// means them. A later reading is tried only when the one before it names nothing.
func LineReadings(line string) []string {
	out := []string{filepath.Clean(filepath.FromSlash(line))}
	if filepath.Separator == '/' && strings.Contains(line, `\`) {
		out = append(out, filepath.Clean(strings.ReplaceAll(line, `\`, "/")))
	}
	return out
}

// Rooted reports whether a cleaned path is rooted below a volume it does not name, as
// Windows reads \Music\a.flac. Only Windows has such paths.
func Rooted(clean string) bool {
	return !filepath.IsAbs(clean) && strings.HasPrefix(clean, string(filepath.Separator))
}

// LineAnchored reports whether some reading of a playlist line starts at a root, absolute
// or rooted, so the line names the files at exactly that path and not every path ending
// with it. A line written on Windows from a drive's root is anchored on every platform:
// rooted on Windows, absolute elsewhere once its backslashes are read as separators.
func LineAnchored(line string) bool {
	for _, clean := range LineReadings(line) {
		if filepath.IsAbs(clean) || Rooted(clean) {
			return true
		}
	}
	return false
}
