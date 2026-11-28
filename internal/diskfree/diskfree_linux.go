package diskfree

import "golang.org/x/sys/unix"

const probe = "statfs"

// statfs counts in the fragment size, as df does. Bsize is the preferred I/O size, and
// a FUSE or CephFS mount reports the two apart.
func statfs(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	if st.Frsize > 0 {
		return span(st.Bavail, st.Frsize), nil
	}
	return span(st.Bavail, st.Bsize), nil
}
