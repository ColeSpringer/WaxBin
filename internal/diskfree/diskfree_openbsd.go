package diskfree

import "golang.org/x/sys/unix"

const probe = "statfs"

func statfs(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return span(st.F_bavail, st.F_bsize), nil
}
