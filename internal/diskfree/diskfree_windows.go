//go:build windows

package diskfree

import "golang.org/x/sys/windows"

// Available returns the bytes available to the calling user on the volume holding
// path, via GetDiskFreeSpaceExW. path may be a file or directory; the API resolves
// it to its containing volume.
func Available(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &totalFree); err != nil {
		return 0, err
	}
	return freeToCaller, nil
}
