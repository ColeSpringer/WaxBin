//go:build darwin || freebsd

package fsx

import "golang.org/x/sys/unix"

// pool reads the mount's type and source from statfs.
func pool(path string) (string, bool) {
	var st unix.Statfs_t
	if statfs(path, &st) != nil {
		return volume(path)
	}
	if id, ok := poolOfSource(unix.ByteSliceToString(st.Fstypename[:]), unix.ByteSliceToString(st.Mntfromname[:])); ok {
		return id, true
	}
	return volume(path)
}

// peers reads the mount table (getfsstat, without waiting on a mount that hangs) for the
// peers of path's pool.
func peers(path string) []string {
	var st unix.Statfs_t
	if statfs(path, &st) != nil {
		return nil
	}
	pool, ok := poolOfSource(unix.ByteSliceToString(st.Fstypename[:]), unix.ByteSliceToString(st.Mntfromname[:]))
	if !ok {
		return nil
	}
	mounts, ok := mountTable()
	if !ok {
		return nil
	}
	var cands []peer
	for _, m := range mounts {
		fstype, source := unix.ByteSliceToString(m.Fstypename[:]), unix.ByteSliceToString(m.Mntfromname[:])
		if p, ok := poolOfSource(fstype, source); ok && p == pool {
			if ok, top := peerOf(fstype, source); ok {
				cands = append(cands, peer{point: unix.ByteSliceToString(m.Mntonname[:]), top: top})
			}
		}
	}
	return pickPeers(cands)
}

func statfs(path string, st *unix.Statfs_t) error {
	for {
		if err := unix.Statfs(path, st); err != unix.EINTR {
			return err
		}
	}
}

// mountTable reads the mounts without waiting on one that hangs.
func mountTable() ([]unix.Statfs_t, bool) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n <= 0 {
		return nil, false
	}
	mounts := make([]unix.Statfs_t, n)
	if n, err = unix.Getfsstat(mounts, unix.MNT_NOWAIT); err != nil {
		return nil, false
	}
	return mounts[:min(n, len(mounts))], true
}

func mountPoints() ([]string, bool) {
	mounts, ok := mountTable()
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, unix.ByteSliceToString(m.Mntonname[:]))
	}
	return out, true
}
