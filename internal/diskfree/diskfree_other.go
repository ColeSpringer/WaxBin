//go:build !unix

package diskfree

// Available is unsupported off Unix; callers skip the preflight when they see
// ErrUnsupported.
func Available(path string) (uint64, error) { return 0, ErrUnsupported }
