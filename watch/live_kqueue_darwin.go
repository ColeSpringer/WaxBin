package watch

import "golang.org/x/sys/unix"

// openMode opens a folder for its notes alone, which leaves a watched volume free to be
// ejected.
const openMode = unix.O_EVTONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
