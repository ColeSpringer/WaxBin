//go:build dragonfly || freebsd || netbsd || openbsd

package watch

import "golang.org/x/sys/unix"

// openMode opens a folder to read, which kqueue needs of a descriptor it watches here.
const openMode = unix.O_RDONLY | unix.O_NONBLOCK | unix.O_DIRECTORY | unix.O_CLOEXEC
