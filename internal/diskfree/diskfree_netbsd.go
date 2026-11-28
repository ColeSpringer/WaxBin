package diskfree

import "golang.org/x/sys/unix"

const probe = "statvfs"

// statfs reads statvfs, the only form NetBSD has, which counts in the fragment size.
func statfs(dir string) (int64, error) {
	var st unix.Statvfs_t
	if err := unix.Statvfs(dir, &st); err != nil {
		return 0, err
	}
	if st.Frsize > 0 {
		return span(st.Bavail, st.Frsize), nil
	}
	return span(st.Bavail, st.Bsize), nil
}
