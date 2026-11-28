//go:build !(darwin || freebsd || linux || netbsd || openbsd || windows)

package diskfree

func available(string) (int64, error) { return 0, ErrUnsupported }
