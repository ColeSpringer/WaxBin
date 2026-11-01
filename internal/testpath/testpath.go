// Package testpath helps tests spell fixture paths that are absolute on every platform.
package testpath

import (
	"os"
	"path/filepath"
)

// Vol is the volume of the working directory: a drive such as "C:" on Windows, empty
// elsewhere. A fixture path spelled "/music/a.flac" has no volume on Windows, so it is
// relative there, while Vol() + "/music/a.flac" is absolute everywhere.
func Vol() string {
	wd, err := os.Getwd()
	if err != nil {
		// A silent "" would put back the volume-less paths this exists to prevent.
		panic("testpath: getwd: " + err.Error())
	}
	return filepath.VolumeName(wd)
}
