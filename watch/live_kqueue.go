//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package watch

import (
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// folderWrites is true: the folder source reports a folder written, not its files.
const folderWrites = true

// newSource watches through kqueue, one descriptor a folder (folderSource).
func newSource() (source, error) {
	q, err := newKqueue()
	if err != nil {
		return nil, err
	}
	return newFolderSource(q, nil), nil
}

// watchCap stops the walk within three quarters of the open-file limit, since each
// watched folder holds a descriptor open here.
func watchCap(n int) int {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return n
	}
	return descriptorBudget(uint64(lim.Cur), n)
}

const vnodeNotes = unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_REVOKE

// kqueue is the folderQueue over a kernel queue, woken through a pipe it also reads.
type kqueue struct {
	kq           int
	wakeR, wakeW int
	buf          [256]unix.Kevent_t
	mu           sync.RWMutex // holds the pipe open while a wake writes to it
	shutDown     bool
}

func newKqueue() (*kqueue, error) {
	// darwin has no pipe2, so close-on-exec is set after the calls, under the fork lock
	// as os.Pipe does, so that no child started meanwhile inherits them.
	var p [2]int
	syscall.ForkLock.RLock()
	call := "kqueue"
	kq, err := unix.Kqueue()
	if err == nil {
		unix.CloseOnExec(kq)
		if call, err = "pipe", unix.Pipe(p[:]); err != nil {
			unix.Close(kq)
		} else {
			unix.CloseOnExec(p[0])
			unix.CloseOnExec(p[1])
		}
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, os.NewSyscallError(call, err)
	}
	q := &kqueue{kq: kq, wakeR: p[0], wakeW: p[1]}
	for _, fd := range p {
		if err := unix.SetNonblock(fd, true); err != nil {
			q.shut()
			return nil, os.NewSyscallError("fcntl", err)
		}
	}
	var ev unix.Kevent_t
	unix.SetKevent(&ev, q.wakeR, unix.EVFILT_READ, unix.EV_ADD)
	if err := q.change(ev); err != nil {
		q.shut()
		return nil, err
	}
	return q, nil
}

func (q *kqueue) change(ev unix.Kevent_t) error {
	for {
		_, err := unix.Kevent(q.kq, []unix.Kevent_t{ev}, nil, nil)
		if err != unix.EINTR {
			return os.NewSyscallError("kevent", err)
		}
	}
}

func (q *kqueue) open(dir string) (int, uint64, error) {
	for {
		fd, err := unix.Open(dir, openMode, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return -1, 0, &os.PathError{Op: "open", Path: dir, Err: err}
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			return -1, 0, &os.PathError{Op: "fstat", Path: dir, Err: err}
		}
		return fd, uint64(st.Dev), nil
	}
}

func (q *kqueue) watch(fd int) error {
	var ev unix.Kevent_t
	unix.SetKevent(&ev, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR)
	ev.Fflags = vnodeNotes
	return q.change(ev)
}

func (q *kqueue) close(fd int) { unix.Close(fd) }

func (q *kqueue) wait() ([]folderNote, error) {
	for {
		n, err := unix.Kevent(q.kq, nil, q.buf[:], nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, os.NewSyscallError("kevent", err)
		}
		var notes []folderNote
		for _, ev := range q.buf[:n] {
			fd := int(ev.Ident)
			if fd == q.wakeR {
				q.drain()
				continue
			}
			var op noteOp
			if ev.Fflags&unix.NOTE_WRITE != 0 {
				op |= noteWritten
			}
			if ev.Fflags&unix.NOTE_DELETE != 0 {
				op |= noteDeleted
			}
			if ev.Fflags&unix.NOTE_RENAME != 0 {
				op |= noteRenamed
			}
			// A filesystem gone from under the folder can come as EV_EOF alone (FreeBSD's
			// dead vnode), and its watch is as gone as a revoked one.
			if ev.Fflags&unix.NOTE_REVOKE != 0 || ev.Flags&unix.EV_EOF != 0 {
				op |= noteRevoked
			}
			notes = append(notes, folderNote{fd: fd, op: op})
		}
		return notes, nil
	}
}

func (q *kqueue) drain() {
	var b [64]byte
	for {
		if n, err := unix.Read(q.wakeR, b[:]); n <= 0 || err != nil {
			return
		}
	}
}

// wake writes to the pipe, unless shut closed it and its number could be another file's.
// A pipe too full to take the byte holds a wake already.
func (q *kqueue) wake() {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if !q.shutDown {
		_, _ = unix.Write(q.wakeW, []byte{0})
	}
}

func (q *kqueue) shut() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutDown {
		return
	}
	q.shutDown = true
	unix.Close(q.kq)
	unix.Close(q.wakeR)
	unix.Close(q.wakeW)
}
