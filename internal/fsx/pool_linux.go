package fsx

import (
	"os"

	"golang.org/x/sys/unix"
)

// pool finds the mount path is on and reads its line of mountinfo.
func pool(path string) (string, bool) {
	if id, info, ok := mountOf(path); ok {
		if p, ok := mountPool(info, id); ok {
			return p, true
		}
	}
	return volume(path)
}

func mountPoints() ([]string, bool) {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, false
	}
	return mountPointsOf(info), true
}

func peers(path string) []string {
	if id, info, ok := mountOf(path); ok {
		return mountPeers(info, id)
	}
	return nil
}

// mountOf reads the mount id of path (statx, kernel 5.8 and later) and the mount table.
func mountOf(path string) (uint64, []byte, bool) {
	var st unix.Statx_t
	for {
		err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_MNT_ID, &st)
		if err == unix.EINTR {
			continue
		}
		if err != nil || st.Mask&unix.STATX_MNT_ID == 0 {
			return 0, nil, false
		}
		break
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return 0, nil, false
	}
	return st.Mnt_id, info, true
}
