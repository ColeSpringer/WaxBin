// Package fsx provides the long-path-safe filesystem move/copy primitives shared
// by organize, inbox, and trash. The helpers keep cross-device fallback,
// no-clobber behavior, mode preservation, partial-copy cleanup, and Windows
// extended-length path handling consistent across callers.
package fsx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/colespringer/waxbin/internal/pathx"
)

// ErrExist is returned when a move/copy would overwrite an existing destination.
// Callers translate it to their own typed error (a conflict, or a skip).
var ErrExist = errors.New("fsx: destination already exists")

// MoveOrCopy moves src to dst, or copies it (leaving src in place) when asCopy is
// set. It is the importer's primitive (move staged files, or copy to keep them).
func MoveOrCopy(src, dst string, asCopy bool) error {
	if asCopy {
		return Copy(src, dst)
	}
	return Move(src, dst)
}

// Move relocates src to dst, creating dst's parent directory and falling back to a
// copy+remove across filesystems. It refuses to overwrite an existing dst
// (ErrExist) and is long-path-safe on Windows.
func Move(src, dst string) error {
	if err := ensureAbsent(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(pathx.Long(filepath.Dir(dst)), 0o755); err != nil {
		return err
	}
	if err := os.Rename(pathx.Long(src), pathx.Long(dst)); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return copyThenRemove(src, dst)
		}
		return err
	}
	return nil
}

// Copy copies src to a fresh dst (ErrExist if it exists), creating dst's parent,
// fsync'ing, and removing the partial copy on any error. Long-path-safe.
func Copy(src, dst string) error {
	if err := ensureAbsent(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(pathx.Long(filepath.Dir(dst)), 0o755); err != nil {
		return err
	}
	return copyContents(src, dst)
}

// ensureAbsent returns ErrExist if dst is present, nil if it is absent, or the
// stat error otherwise.
func ensureAbsent(dst string) error {
	_, err := os.Lstat(pathx.Long(dst))
	if err == nil {
		return ErrExist
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func copyThenRemove(src, dst string) error {
	if err := copyContents(src, dst); err != nil {
		return err
	}
	return os.Remove(pathx.Long(src))
}

// copyContents copies src to dst with O_EXCL (so it never clobbers even if a racing
// writer beat the ensureAbsent check), preserves the source mode, fsyncs, and
// removes a partial dst on any failure so a failed move leaves no stray bytes.
func copyContents(src, dst string) error {
	in, err := os.Open(pathx.Long(src))
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(pathx.Long(dst), os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = out.Close()
			_ = os.Remove(pathx.Long(dst))
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
