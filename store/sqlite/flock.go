package sqlite

import (
	"encoding/json"
	"os"

	"github.com/colespringer/waxbin/waxerr"
	"github.com/gofrs/flock"
)

// OwnerInfo is the metadata written into the lockfile by the current write
// owner. It is advisory only; liveness is the OS flock itself, never a PID
// check, so this works across container and host processes where PIDs are not
// comparable.
type OwnerInfo struct {
	Owner        string `json:"owner"`
	IPCSocket    string `json:"ipc_socket,omitempty"`
	PID          int    `json:"pid"`
	AcquiredAtNS int64  `json:"acquired_at_ns"`
}

// writeLock is the held OS advisory lock guarding write ownership. The lock is
// associated with the lockfile inode and released by the kernel on process
// exit, so a crash never leaves a stale owner.
type writeLock struct {
	fl   *flock.Flock
	path string
}

// acquireWriteLock takes the exclusive advisory lock on lockPath without
// blocking. It returns CodeConflict if another live owner holds it, naming that
// owner when the lockfile metadata is readable.
func acquireWriteLock(lockPath, owner, ipcSocket string, nowNS int64) (*writeLock, error) {
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "sqlite.acquireWriteLock", err)
	}
	if !locked {
		msg := "library is owned by another process"
		if info, e := readOwnerInfo(lockPath); e == nil && info.Owner != "" {
			msg += " (owner=" + info.Owner + ")"
		}
		return nil, waxerr.New(waxerr.CodeConflict, "sqlite.acquireWriteLock", msg)
	}

	// Record owner metadata. The advisory flock does not block this write, and
	// truncating the lockfile contents does not drop the lock.
	info := OwnerInfo{Owner: owner, IPCSocket: ipcSocket, PID: os.Getpid(), AcquiredAtNS: nowNS}
	if data, err := json.Marshal(info); err == nil {
		_ = os.WriteFile(lockPath, data, 0o600)
	}
	return &writeLock{fl: fl, path: lockPath}, nil
}

// release clears the lockfile metadata and drops the lock. The metadata is
// truncated while the lock is still held, so a process that acquires next never
// reads this owner's stale info; an interleaving reader sees an empty file
// (reported as "no owner") rather than a misleading name.
func (w *writeLock) release() error {
	if w == nil || w.fl == nil {
		return nil
	}
	_ = os.Truncate(w.path, 0)
	if err := w.fl.Unlock(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "sqlite.writeLock.release", err)
	}
	return nil
}

// readOwnerInfo reads the lockfile metadata without taking the lock. Used to
// report who owns a contended library.
func readOwnerInfo(lockPath string) (OwnerInfo, error) {
	var info OwnerInfo
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, err
	}
	return info, nil
}
