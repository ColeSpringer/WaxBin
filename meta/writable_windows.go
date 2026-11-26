//go:build windows

package meta

import (
	"os"

	"github.com/colespringer/waxbin/internal/pathx"
)

// Replaceable reports why the file at path could not be replaced now, or nil: it is
// read-only, or another process holds it open without sharing writes, as a player can.
// It opens the file for writing and closes it, which writes nothing.
func Replaceable(path string) error {
	f, err := os.OpenFile(pathx.Long(path), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}
