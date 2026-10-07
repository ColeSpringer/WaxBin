//go:build windows

package watch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/windows"
)

// A watch on Windows is a ReadDirectoryChangesW handle held open on its folder, and
// NTFS refuses to rename or move a folder while a handle is open on a folder below it,
// so a watch on every folder, which fsnotify arms, would pin every folder that holds
// one for as long as the watcher runs. One recursive watch per root holds only the
// root open and reports changes anywhere below it.
func newSource() (source, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 0)
	if err != nil {
		return nil, os.NewSyscallError("CreateIoCompletionPort", err)
	}
	s := &recursiveSource{
		port:     port,
		events:   make(chan fsnotify.Event),
		errs:     make(chan error),
		asks:     make(chan request, 1),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
		roots:    map[uintptr]*rootWatch{},
		closing:  map[uintptr]*rootWatch{},
	}
	go s.run()
	return s, nil
}

// watchCap is 0 here: the folders below a root ride its one watch, so there is nothing
// to cap.
func watchCap(int) int { return 0 }

// watchHint is empty here: a root's one watch is not a resource that runs out.
const watchHint = ""

// notifyBuffer is the most ReadDirectoryChangesW accepts over a network share.
const notifyBuffer = 64 << 10

const notifyMask = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME | windows.FILE_NOTIFY_CHANGE_LAST_WRITE

// recursiveSource reads every root's changes off one completion port. The reads are
// issued from the goroutine that runs the port, pinned to its thread as fsnotify does,
// since Windows may cancel a thread's pending reads when the thread exits.
type recursiveSource struct {
	port     windows.Handle
	events   chan fsnotify.Event
	errs     chan error
	asks     chan request
	done     chan struct{} // closed by Close, which every send and ask watches
	finished chan struct{} // closed once the streams are closed and the roots released
	closed   sync.Once
	portMu   sync.RWMutex // holds the port open while a wake is posted to it

	// held is the roots with a watch, read off the port's goroutine by Add and Remove.
	mu   sync.Mutex
	held map[string]bool

	// roots and closing belong to the port's goroutine, keyed by completion key. A
	// dropped root whose read was pending waits in closing for the cancellation that
	// read completes with, since that packet points at its Overlapped.
	roots   map[uintptr]*rootWatch
	closing map[uintptr]*rootWatch
	last    uintptr
}

type rootWatch struct {
	key     uintptr
	path    string
	canon   string // the path the handle resolved to when opened
	handle  windows.Handle
	ov      windows.Overlapped
	buf     []byte
	pending bool      // a read is outstanding
	checked time.Time // when moved last asked
}

type request struct {
	add   bool
	path  string
	reply chan error
}

// Add watches dir whole unless a watched root already covers it; a folder under no held
// root gets a watch of its own, which is how a root is armed.
func (s *recursiveSource) Add(dir string) error {
	dir = filepath.Clean(dir)
	if s.covered(dir) {
		return nil
	}
	return s.ask(request{add: true, path: dir})
}

// Remove drops the watch on a root; a folder under one rides the root's watch.
func (s *recursiveSource) Remove(dir string) error {
	dir = filepath.Clean(dir)
	s.mu.Lock()
	held := s.held[dir]
	s.mu.Unlock()
	if !held {
		return nil
	}
	return s.ask(request{path: dir})
}

func (s *recursiveSource) Events() <-chan fsnotify.Event { return s.events }
func (s *recursiveSource) Errors() <-chan error          { return s.errs }

// Close ends the streams and releases every root with its handle, then the port.
func (s *recursiveSource) Close() error {
	s.closed.Do(func() {
		close(s.done)
		if s.wake() != nil {
			s.closePort() // a wait no wake could end ends with the port
		}
	})
	<-s.finished
	s.closePort()
	return nil
}

func (s *recursiveSource) closePort() {
	s.portMu.Lock()
	defer s.portMu.Unlock()
	if s.port != windows.InvalidHandle {
		_ = windows.CloseHandle(s.port)
		s.port = windows.InvalidHandle
	}
}

func (s *recursiveSource) covered(dir string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for root := range s.held {
		if underRoot(root, dir) {
			return true
		}
	}
	return false
}

// ask hands a request to the port's goroutine, wakes it, and waits for its answer.
func (s *recursiveSource) ask(r request) error {
	r.reply = make(chan error, 1)
	select {
	case <-s.done:
		return fsnotify.ErrClosed
	case s.asks <- r:
	}
	if err := s.wake(); err != nil {
		return err
	}
	select {
	case err := <-r.reply:
		return err
	case <-s.done:
		return fsnotify.ErrClosed
	}
}

// wake posts to the port, which stays open for it; after Close there is nothing to wake.
func (s *recursiveSource) wake() error {
	s.portMu.RLock()
	defer s.portMu.RUnlock()
	if s.port == windows.InvalidHandle {
		return fsnotify.ErrClosed
	}
	if err := windows.PostQueuedCompletionStatus(s.port, 0, 0, nil); err != nil {
		return os.NewSyscallError("PostQueuedCompletionStatus", err)
	}
	return nil
}

// run services the completion port until Close: a completion with no overlapped
// record is a wake for the requests queued, any other is a root's read.
func (s *recursiveSource) run() {
	runtime.LockOSThread()
	defer s.finish()
	for {
		var (
			n   uint32
			key uintptr
			ov  *windows.Overlapped
		)
		err := windows.GetQueuedCompletionStatus(s.port, &n, &key, &ov, windows.INFINITE)
		select {
		case <-s.done:
			return
		default:
		}
		if ov == nil {
			if err != nil {
				s.sendErr(os.NewSyscallError("GetQueuedCompletionStatus", err))
				return
			}
			s.serve()
			continue
		}
		if _, ok := s.closing[key]; ok {
			delete(s.closing, key)
			continue
		}
		r := s.roots[key]
		if r == nil {
			continue
		}
		r.pending = false
		switch {
		case err == nil:
			if time.Since(r.checked) >= time.Second && s.moved(r) {
				// The handle followed the root to another path, so the names it reports
				// no longer join onto this one.
				s.drop(r)
				s.send(fsnotify.Event{Name: r.path, Op: fsnotify.Rename})
				continue
			}
			s.deliver(r, n)
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			s.refused(r, err)
			continue
		case errors.Is(err, windows.ERROR_MORE_DATA):
			s.sendErr(fsnotify.ErrEventOverflow)
		case errors.Is(err, windows.ERROR_OPERATION_ABORTED):
			// Nothing here cancels a read; the read below recovers from whatever did.
		default:
			s.drop(r)
			s.sendErr(fmt.Errorf("%w: %s: %w", errCoverageLost, r.path, os.NewSyscallError("ReadDirectoryChangesW", err)))
			continue
		}
		if s.roots[key] != r {
			continue // released while its changes were being sent
		}
		if err := s.read(r); err != nil {
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				s.refused(r, err)
				continue
			}
			s.drop(r)
			s.sendErr(fmt.Errorf("%w: %s: %w", errCoverageLost, r.path, err))
		}
	}
}

// serve answers the requests queued; each posted one wake, so a wake may find none left.
func (s *recursiveSource) serve() {
	for {
		select {
		case r := <-s.asks:
			s.answer(r)
		default:
			return
		}
	}
}

func (s *recursiveSource) answer(r request) {
	if r.add {
		r.reply <- s.open(r.path)
	} else {
		r.reply <- s.release(r.path)
	}
}

// open arms a recursive watch on a root, unless one armed since Add looked covers it.
func (s *recursiveSource) open(path string) error {
	if s.covered(path) {
		return nil
	}
	name, err := windows.UTF16PtrFromString(pathx.Long(path))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return os.NewSyscallError("CreateFile", err)
	}
	s.last++
	r := &rootWatch{key: s.last, path: path, handle: h, buf: make([]byte, notifyBuffer)}
	r.canon, _ = finalPath(h) // without it a move of the root goes unnoticed
	if _, err := windows.CreateIoCompletionPort(h, s.port, r.key, 0); err != nil {
		_ = windows.CloseHandle(h)
		return os.NewSyscallError("CreateIoCompletionPort", err)
	}
	s.roots[r.key] = r
	s.mu.Lock()
	if s.held == nil {
		s.held = map[string]bool{}
	}
	s.held[path] = true
	s.mu.Unlock()
	if err := s.read(r); err != nil {
		s.drop(r)
		return err
	}
	return nil
}

// release drops the watch on a root.
func (s *recursiveSource) release(path string) error {
	for _, r := range s.roots {
		if r.path == path {
			s.drop(r)
			return nil
		}
	}
	return nil
}

// drop forgets a root and closes its handle, which cancels a read still pending on it.
func (s *recursiveSource) drop(r *rootWatch) {
	delete(s.roots, r.key)
	if r.pending {
		s.closing[r.key] = r
	}
	s.mu.Lock()
	delete(s.held, r.path)
	s.mu.Unlock()
	_ = windows.CloseHandle(r.handle)
}

// refused handles a read the root refused, which is how a removed root reports itself;
// a root still at its path has lost its live coverage instead, and the scheduled
// rescans carry it from here.
func (s *recursiveSource) refused(r *rootWatch, err error) {
	s.drop(r)
	if _, serr := os.Lstat(r.path); serr != nil {
		s.send(fsnotify.Event{Name: r.path, Op: fsnotify.Remove})
		return
	}
	s.sendErr(fmt.Errorf("%w: %s: %w", errCoverageLost, r.path, err))
}

// moved reports whether the handle resolves to another path than it was opened at,
// which a rename or move of the root itself leaves it doing. The answer is a query of
// the handle's name, so run asks at most once a second.
func (s *recursiveSource) moved(r *rootWatch) bool {
	r.checked = time.Now()
	if r.canon == "" {
		return false
	}
	cur, err := finalPath(r.handle)
	return err == nil && !strings.EqualFold(cur, r.canon)
}

// finalPath is the path a handle resolves to now.
func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

// read issues the next overlapped read on a root; the port reports its completion.
func (s *recursiveSource) read(r *rootWatch) error {
	r.ov = windows.Overlapped{}
	err := windows.ReadDirectoryChanges(r.handle, &r.buf[0], uint32(len(r.buf)), true, notifyMask, nil, &r.ov, 0)
	if err != nil {
		return os.NewSyscallError("ReadDirectoryChangesW", err)
	}
	r.pending = true
	return nil
}

// deliver sends the changes a completed read holds. A read that completes with nothing
// means the kernel's buffer overflowed and it dropped them all, which the overflow
// error says; the scheduled rescan is the backstop.
func (s *recursiveSource) deliver(r *rootWatch, n uint32) {
	if n == 0 {
		s.sendErr(fsnotify.ErrEventOverflow)
		return
	}
	for off := uint32(0); ; {
		raw := (*windows.FileNotifyInformation)(unsafe.Pointer(&r.buf[off]))
		name := windows.UTF16ToString(unsafe.Slice(&raw.FileName, raw.FileNameLength/2))
		full := filepath.Join(r.path, name)
		switch raw.Action {
		case windows.FILE_ACTION_ADDED, windows.FILE_ACTION_RENAMED_NEW_NAME:
			s.send(fsnotify.Event{Name: full, Op: fsnotify.Create})
		case windows.FILE_ACTION_REMOVED:
			s.send(fsnotify.Event{Name: full, Op: fsnotify.Remove})
		case windows.FILE_ACTION_MODIFIED:
			s.send(fsnotify.Event{Name: full, Op: fsnotify.Write})
		case windows.FILE_ACTION_RENAMED_OLD_NAME:
			s.send(fsnotify.Event{Name: full, Op: fsnotify.Rename})
		}
		if raw.NextEntryOffset == 0 {
			return
		}
		off += raw.NextEntryOffset
		if off >= n {
			s.sendErr(errors.New("watch: a change record points past the read, changes may be missed"))
			return
		}
	}
}

// send hands an event to the consumer. A request that arrives meanwhile is answered
// here, since the consumer may be the one asking before it reads.
func (s *recursiveSource) send(ev fsnotify.Event) {
	for {
		select {
		case s.events <- ev:
			return
		case r := <-s.asks:
			s.answer(r)
		case <-s.done:
			return
		}
	}
}

func (s *recursiveSource) sendErr(err error) {
	for {
		select {
		case s.errs <- err:
			return
		case r := <-s.asks:
			s.answer(r)
		case <-s.done:
			return
		}
	}
}

// finish releases every root and closes the streams. It runs on the port's goroutine
// as it exits; Close closes the port itself once that is done.
func (s *recursiveSource) finish() {
	for _, r := range s.roots {
		s.drop(r)
	}
	close(s.events)
	close(s.errs)
	close(s.finished)
}
