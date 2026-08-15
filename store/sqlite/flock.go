package sqlite

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/colespringer/waxbin/waxerr"
	"github.com/gofrs/flock"
)

// OwnerInfo is the metadata the current write owner records beside the lockfile.
// It is advisory only; liveness is the OS flock itself, never a PID check, so
// this works across container and host processes where PIDs are not comparable.
//
// The record lives in a sidecar file (ownerPath), not in the lockfile: Windows
// byte-range locks are mandatory, so while the lock is held every other handle's
// read or write of the locked file fails, including this process's own. The
// advisory POSIX flock never conflicted, which is what let the in-file design
// appear to work.
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
// owner when its owner record is readable.
func acquireWriteLock(lockPath, owner, ipcSocket string, nowNS int64) (*writeLock, error) {
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "sqlite.acquireWriteLock", err)
	}
	if !locked {
		return nil, ownedElsewhere(lockPath, "sqlite.acquireWriteLock")
	}

	// Record owner metadata beside the lockfile. The write error is surfaced, not
	// discarded: a discarded error is what let the old in-file write fail silently
	// on Windows for as long as it existed, and a directory where this write fails
	// cannot host the catalog either.
	info := OwnerInfo{Owner: owner, IPCSocket: ipcSocket, PID: os.Getpid(), AcquiredAtNS: nowNS}
	data, err := json.Marshal(info)
	if err == nil {
		err = os.WriteFile(ownerPath(lockPath), data, 0o600)
	}
	if err != nil {
		_ = fl.Unlock()
		return nil, waxerr.Wrap(waxerr.CodeIO, "sqlite.acquireWriteLock", err)
	}
	return &writeLock{fl: fl, path: lockPath}, nil
}

// ownerPath is the sidecar file carrying the owner record for a lockfile. Derived
// here so every caller keeps passing the lock path.
func ownerPath(lockPath string) string { return lockPath + ".owner" }

// ownedElsewhere is the CodeConflict a held lock raises, naming the owner when
// its record is readable.
func ownedElsewhere(lockPath, op string) error {
	msg := "library is owned by another process"
	if info, e := readOwnerInfo(lockPath); e == nil && info.Owner != "" {
		msg += " (owner=" + info.Owner + ")"
	}
	return waxerr.New(waxerr.CodeConflict, op, msg)
}

// ProbeWriteLock reports whether the catalog's write lock is free, taking it only
// long enough to find out. It exists for commands that manipulate the catalog files
// directly (db reset): a full Open would answer the same question but migrate,
// reclaim jobs, seed the default user and checkpoint the WAL of the very file the
// caller is about to preserve as its recovery copy.
func ProbeWriteLock(dbPath string) error {
	const op = "sqlite.ProbeWriteLock"
	lockPath := dbPath + ".waxlock"
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if !locked {
		return ownedElsewhere(lockPath, op)
	}
	return fl.Unlock()
}

// acquireWriteLockRetry is acquireWriteLock with bounded exponential backoff over
// a live-owner conflict. It exists for the maintenance-mode reopen: as a hand-off
// ends, a foreground process may still be releasing the flock, so the daemon must
// wait that transient hold out rather than fail its reopen. A persistently held
// lock still surfaces the CodeConflict (naming the owner); a non-conflict error is
// terminal immediately; ctx cancellation aborts the wait.
func acquireWriteLockRetry(ctx context.Context, lockPath, owner, ipcSocket string) (*writeLock, error) {
	const maxAttempts = 60
	const maxBackoff = 250 * time.Millisecond
	backoff := 5 * time.Millisecond
	for attempt := 0; ; attempt++ {
		lock, err := acquireWriteLock(lockPath, owner, ipcSocket, nowNS())
		if err == nil {
			return lock, nil
		}
		if !waxerr.Is(err, waxerr.CodeConflict) || attempt >= maxAttempts {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, waxerr.FromContext("sqlite.acquireWriteLockRetry", ctx.Err(), waxerr.CodeConflict)
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// ReadOwnerInfo reads the owner record for the lockfile at lockPath without
// taking the lock. It is the exported form a CLI uses to discover a running
// server's advertised IPC socket before deciding whether to proxy a mutation.
func ReadOwnerInfo(lockPath string) (OwnerInfo, error) { return readOwnerInfo(lockPath) }

// release removes the owner record and drops the lock. The record goes while the
// lock is still held, so a process that acquires next never reads this owner's
// stale info; an interleaving reader sees a missing file (reported as "no owner")
// rather than a misleading name.
func (w *writeLock) release() error {
	if w == nil || w.fl == nil {
		return nil
	}
	// A reader mid-ReadFile blocks the delete on Windows (Go opens files without
	// FILE_SHARE_DELETE), so fall back to truncating: an empty record reads as "no
	// owner", the same answer removal gives.
	if err := os.Remove(ownerPath(w.path)); err != nil && !os.IsNotExist(err) {
		_ = os.Truncate(ownerPath(w.path), 0)
	}
	if err := w.fl.Unlock(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "sqlite.writeLock.release", err)
	}
	return nil
}

// readOwnerInfo reads the owner record without taking the lock. Used to report
// who owns a contended library.
func readOwnerInfo(lockPath string) (OwnerInfo, error) {
	var info OwnerInfo
	data, err := os.ReadFile(ownerPath(lockPath))
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, err
	}
	return info, nil
}
