package fsx

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/windows"
)

// device reports nothing on Windows, where a volume mounted on a folder is a reparse
// point that Lstat does not call a folder.
func device(fs.FileInfo) (uint64, bool) { return 0, false }

func inUse(err error) bool { return errors.Is(err, windows.ERROR_SHARING_VIOLATION) }
