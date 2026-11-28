package diskfree

import (
	"math"
	"os"

	"github.com/colespringer/waxbin/internal/pathx"
	"golang.org/x/sys/windows"
)

// available asks GetDiskFreeSpaceEx, which counts what the caller's quota leaves. The
// call takes a folder only, and wants the trailing separator on the root of a share.
func available(dir string) (int64, error) {
	name := dir
	if name != "" && !os.IsPathSeparator(name[len(name)-1]) {
		name += `\`
	}
	p, err := windows.UTF16PtrFromString(pathx.Long(name))
	if err != nil {
		return 0, &os.PathError{Op: "GetDiskFreeSpaceEx", Path: dir, Err: err}
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return 0, &os.PathError{Op: "GetDiskFreeSpaceEx", Path: dir, Err: err}
	}
	return int64(min(free, math.MaxInt64)), nil
}
