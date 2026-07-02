//go:build !unix && !windows

package diskfree

// Available is unsupported off Unix and Windows; callers skip the preflight when
// they see ErrUnsupported. The build tag excludes windows so this does not collide
// with diskfree_windows.go (which would otherwise duplicate the symbol).
func Available(path string) (uint64, error) { return 0, ErrUnsupported }
