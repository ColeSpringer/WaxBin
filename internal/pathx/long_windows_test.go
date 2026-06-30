//go:build windows

package pathx

import (
	"strings"
	"testing"
)

// TestLongPrefixesLongPaths checks the Windows extended-length transforms. It is
// build-tagged for Windows, so it runs on Windows CI (and is excluded elsewhere).
func TestLongPrefixesLongPaths(t *testing.T) {
	driveLong := `C:\music\` + strings.Repeat("a", 300) + `.flac`
	if got := Long(driveLong); !strings.HasPrefix(got, `\\?\C:\music\`) {
		t.Errorf("drive-letter long path not prefixed: %q", got)
	}

	uncLong := `\\server\share\` + strings.Repeat("b", 300) + `.flac`
	if got := Long(uncLong); !strings.HasPrefix(got, `\\?\UNC\server\share\`) {
		t.Errorf("UNC long path not converted to extended UNC: %q", got)
	}

	already := `\\?\C:\already\prefixed`
	if got := Long(already); got != already {
		t.Errorf("already-prefixed path changed: %q", got)
	}
}
