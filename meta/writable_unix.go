//go:build unix

package meta

import (
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Replaceable reports why the file at path could not be replaced now, or nil. A write
// copies the file into a new one beside it, so the folder has to take a new file: one
// without write permission, or on a read-only mount, does not. It writes nothing.
func Replaceable(path string) error {
	dir := filepath.Dir(path)
	if err := unix.Access(dir, unix.W_OK); err != nil {
		return &fs.PathError{Op: "write", Path: dir, Err: err}
	}
	return nil
}
