package watch

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
)

// noteOp is what the kernel says happened to a watched folder.
type noteOp uint8

const (
	noteWritten noteOp = 1 << iota // an entry in it arrived, left or was renamed
	noteDeleted
	noteRenamed
	noteRevoked // the filesystem holding it went, as an unmount takes it
)

// folderNote is a note on one folder, by the descriptor it was opened under.
type folderNote struct {
	fd int
	op noteOp
}

// folderQueue is the kernel queue a folderSource reads, one descriptor a folder. open,
// close and wake are called from any goroutine, the rest only from the source's own.
type folderQueue interface {
	// open opens dir for notes and says which device it is on.
	open(dir string) (fd int, dev uint64, err error)
	// watch asks for the notes on a folder open returned.
	watch(fd int) error
	close(fd int)
	// wait blocks until there are notes or wake is called.
	wait() ([]folderNote, error)
	wake()
	// shut lets the queue go. Only open, close and wake can come after it (an Add racing
	// the source's end), and wake then does nothing.
	shut()
}

// folderSource is the source where a watch is a descriptor held open on its folder
// (kqueue). fsnotify's kqueue backend holds one for each file in a watched folder as
// well; this holds one a folder. A folder's own note says an entry arrived, left or was
// renamed, so the folder is listed again and what changed is reported in fsnotify's
// terms: Create for a subfolder that arrived, Remove for one that left with no watch of
// its own, and Write on the folder when its files changed, a name or the file standing
// under one (an atomic save puts another there). A file rewritten in place raises
// nothing; the scheduled rescan is what finds it.
//
// One goroutine owns the tables and the queue and never touches the filesystem, so
// Close never waits on a mount that hangs. Add opens and lists its folder on the
// caller's goroutine, the note asked for first so that no change falls between the two;
// later listings run one at a time per device on goroutines of their own, so a mount
// that hangs stalls only its own.
type folderSource struct {
	q        folderQueue
	spawn    func(func()) // starts a listing; tests run them by hand
	events   chan fsnotify.Event
	errs     chan error
	asks     chan folderAsk
	done     chan struct{} // closed by Close
	finished chan struct{} // closed once the source's goroutine has let everything go
	closing  sync.Once

	// held is the folders with a watch, so Add, Remove and Dropped answer without
	// asking the source's goroutine.
	mu   sync.Mutex
	held map[string]bool

	postMu sync.Mutex
	posted []listed // listings done since the source last looked

	// The rest belongs to the source's goroutine. A dropped folder leaves the tables at
	// once and its descriptor is closed when the batch ends, so no folder opened before
	// then can take its number and a later note in the batch.
	byFD    map[int]*folder
	byPath  map[string]*folder
	below   map[string]map[string]bool // watched folders by the folder above them
	devices map[uint64]*deviceListings
	dropped []int
	batch   uint64 // counts the queue's batches
}

type folder struct {
	path string
	fd   int
	dev  uint64
	// subs and files are the last listing: its subfolders' names, and a hash over every
	// other entry (listFolder). subs is nil until the folder's first listing is in.
	subs  map[string]bool
	files uint64
	// left holds the watched subfolders gone since that listing, which explain a note.
	left map[string]bool
	// gen moves on when the listing is edited, voiding one taken before the edit.
	gen     uint64
	queued  bool   // a listing waits or runs
	started uint64 // the batch the running listing started in, 0 while none runs
	again   bool   // a note came in that the listing running may not show
}

type deviceListings struct {
	busy bool
	wait []*folder
}

type folderListing struct {
	subs  map[string]bool
	files uint64
}

type listed struct {
	f       *folder
	dl      *deviceListings
	gen     uint64
	listing folderListing
	err     error
}

type askKind uint8

const (
	askRegister askKind = iota
	askBaseline
	askRemove
)

type folderAsk struct {
	kind    askKind
	path    string
	fd      int
	dev     uint64
	listing folderListing
	reply   chan error
}

// errHeld answers a register for a folder another Add registered first.
var errHeld = errors.New("watch: folder already held")

// newFolderSource starts a source over q. spawn starts a listing; nil runs each on a
// goroutine of its own.
func newFolderSource(q folderQueue, spawn func(func())) *folderSource {
	if spawn == nil {
		spawn = func(list func()) { go list() }
	}
	s := &folderSource{
		q: q, spawn: spawn,
		events:   make(chan fsnotify.Event),
		errs:     make(chan error),
		asks:     make(chan folderAsk, 1),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
		held:     map[string]bool{},
		byFD:     map[int]*folder{},
		byPath:   map[string]*folder{},
		below:    map[string]map[string]bool{},
		devices:  map[uint64]*deviceListings{},
	}
	go s.run()
	return s
}

// Add watches dir unless it is held: it opens the folder, asks for its notes, and lists
// it, on the caller's goroutine.
func (s *folderSource) Add(dir string) error {
	dir = filepath.Clean(dir)
	if s.holds(dir) {
		return nil
	}
	select {
	case <-s.done:
		return fsnotify.ErrClosed
	case <-s.finished:
		return fsnotify.ErrClosed // the queue failed and the source ended without a Close
	default:
	}
	fd, dev, err := s.q.open(dir)
	if err != nil {
		return err
	}
	if err := s.ask(folderAsk{kind: askRegister, path: dir, fd: fd, dev: dev}); err != nil {
		s.q.close(fd) // not registered, so still the caller's
		if errors.Is(err, errHeld) {
			return nil
		}
		return err
	}
	l, err := listFolder(dir)
	if err != nil {
		_ = s.ask(folderAsk{kind: askRemove, path: dir})
		return err
	}
	return s.ask(folderAsk{kind: askBaseline, path: dir, fd: fd, listing: l})
}

// Remove lets one folder go; the folders below it keep their watches.
func (s *folderSource) Remove(dir string) error {
	dir = filepath.Clean(dir)
	if !s.holds(dir) {
		return nil
	}
	if err := s.ask(folderAsk{kind: askRemove, path: dir}); err != nil && !errors.Is(err, fsnotify.ErrClosed) {
		return err
	}
	return nil
}

// Dropped returns the folders of dirs the source does not hold.
func (s *folderSource) Dropped(dirs []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lost []string
	for _, dir := range dirs {
		if !s.held[dir] {
			lost = append(lost, dir)
		}
	}
	return lost
}

func (s *folderSource) Events() <-chan fsnotify.Event { return s.events }
func (s *folderSource) Errors() <-chan error          { return s.errs }

// Close ends the streams and gives every descriptor back.
func (s *folderSource) Close() error {
	s.closing.Do(func() {
		close(s.done)
		s.q.wake()
	})
	<-s.finished
	return nil
}

func (s *folderSource) holds(dir string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[dir]
}

// ask hands a request to the source's goroutine and waits for its answer. One the
// goroutine never took is answered ErrClosed once it is gone, which a queue that failed
// leaves it before any Close.
func (s *folderSource) ask(a folderAsk) error {
	a.reply = make(chan error, 1)
	select {
	case s.asks <- a:
	case <-s.done:
		return fsnotify.ErrClosed
	case <-s.finished:
		return fsnotify.ErrClosed
	}
	s.q.wake()
	select {
	case err := <-a.reply:
		return err
	case <-s.finished:
		select {
		case err := <-a.reply:
			return err
		default:
			return fsnotify.ErrClosed
		}
	}
}

// run services the queue until Close.
func (s *folderSource) run() {
	defer s.finish()
	for {
		notes, err := s.q.wait()
		select {
		case <-s.done:
			return
		default:
		}
		s.batch++
		if err != nil {
			s.sendErr(err)
			return
		}
		for _, n := range notes {
			s.note(n)
		}
		s.serve()
		s.collect()
		for _, fd := range s.dropped {
			s.q.close(fd)
		}
		s.dropped = s.dropped[:0]
	}
}

func (s *folderSource) note(n folderNote) {
	f := s.byFD[n.fd]
	if f == nil {
		return // a folder let go earlier in the batch
	}
	switch {
	case n.op&noteRevoked != 0:
		s.revoked(f)
	case n.op&noteDeleted != 0:
		s.gone(f, fsnotify.Remove)
	case n.op&noteRenamed != 0:
		s.gone(f, fsnotify.Rename)
	case n.op&noteWritten != 0:
		s.written(f)
	}
}

// written lists a folder again, unless a listing to come will show what the note says:
// one waiting, or one that started in this batch, after the note was made.
func (s *folderSource) written(f *folder) {
	switch {
	case f.subs == nil:
		f.again = true // its first listing is still being taken
	case !f.queued:
		s.queue(f)
	case f.started != 0 && f.started < s.batch:
		f.again = true
	}
}

// gone drops a folder deleted or moved, and the watched folders below it, and reports it
// at the path it was added under. Its parent is listed again, so that a folder standing
// under the old name since is reported as arrived, and the parent is told the folder left
// with its own watch, which the parent's note then needs no Write for.
func (s *folderSource) gone(f *folder, op fsnotify.Op) {
	parent := s.byPath[filepath.Dir(f.path)]
	s.dropTree(f)
	if parent != nil && parent != f {
		name := filepath.Base(f.path)
		if parent.left == nil {
			parent.left = map[string]bool{}
		}
		parent.left[name] = true
		switch {
		case parent.subs == nil:
			parent.again = true
		case !parent.queued:
			delete(parent.subs, name)
			s.queue(parent)
		case parent.started != 0 && parent.started < s.batch:
			delete(parent.subs, name)
			parent.gen++ // the listing running may still show it
		default:
			delete(parent.subs, name)
		}
	}
	s.send(fsnotify.Event{Name: f.path, Op: op})
}

// revoked handles a folder whose filesystem went. Every watched folder on it hears the
// same, so the top watched folder on that device is reported once, as removed, and every
// folder below it let go: an ejected root and a volume mounted below one alike.
func (s *folderSource) revoked(f *folder) {
	top := f
	for {
		p := s.byPath[filepath.Dir(top.path)]
		if p == nil || p == top || p.dev != f.dev {
			break
		}
		top = p
	}
	s.dropTree(top)
	s.send(fsnotify.Event{Name: top.path, Op: fsnotify.Remove})
}

func (s *folderSource) dropTree(f *folder) {
	for kid := range s.below[f.path] {
		if k := s.byPath[kid]; k != nil {
			s.dropTree(k)
		}
	}
	s.drop(f)
}

func (s *folderSource) drop(f *folder) {
	delete(s.byFD, f.fd)
	delete(s.byPath, f.path)
	if p := filepath.Dir(f.path); p != f.path {
		delete(s.below[p], f.path)
		if len(s.below[p]) == 0 {
			delete(s.below, p)
		}
	}
	s.mu.Lock()
	delete(s.held, f.path)
	s.mu.Unlock()
	s.dropped = append(s.dropped, f.fd)
}

func (s *folderSource) serve() {
	for {
		select {
		case a := <-s.asks:
			s.answer(a)
		default:
			return
		}
	}
}

func (s *folderSource) answer(a folderAsk) {
	switch a.kind {
	case askRegister:
		a.reply <- s.register(a)
	case askBaseline:
		if f := s.byPath[a.path]; f != nil && f.fd == a.fd {
			f.subs, f.files = a.listing.subs, a.listing.files
			if f.again {
				f.again = false
				s.queue(f)
			}
		}
		a.reply <- nil
	case askRemove:
		if f := s.byPath[a.path]; f != nil {
			s.drop(f)
		}
		a.reply <- nil
	}
}

func (s *folderSource) register(a folderAsk) error {
	if s.byPath[a.path] != nil {
		return errHeld
	}
	if err := s.q.watch(a.fd); err != nil {
		return err
	}
	f := &folder{path: a.path, fd: a.fd, dev: a.dev}
	s.byFD[a.fd], s.byPath[a.path] = f, f
	if p := filepath.Dir(a.path); p != a.path {
		if s.below[p] == nil {
			s.below[p] = map[string]bool{}
		}
		s.below[p][a.path] = true
	}
	s.mu.Lock()
	s.held[a.path] = true
	s.mu.Unlock()
	return nil
}

func (s *folderSource) queue(f *folder) {
	f.queued = true
	dl := s.devices[f.dev]
	if dl == nil {
		dl = &deviceListings{}
		s.devices[f.dev] = dl
	}
	if dl.busy {
		dl.wait = append(dl.wait, f)
		return
	}
	s.start(f, dl)
}

func (s *folderSource) start(f *folder, dl *deviceListings) {
	dl.busy = true
	f.started = s.batch
	path, gen := f.path, f.gen
	s.spawn(func() {
		l, err := listFolder(path)
		s.postMu.Lock()
		s.posted = append(s.posted, listed{f: f, dl: dl, gen: gen, listing: l, err: err})
		s.postMu.Unlock()
		s.q.wake()
	})
}

// next starts the listing waiting longest on a device, if it is free.
func (s *folderSource) next(dl *deviceListings) {
	for !dl.busy && len(dl.wait) > 0 {
		f := dl.wait[0]
		dl.wait = dl.wait[1:]
		if s.byFD[f.fd] == f && f.queued && f.started == 0 {
			s.start(f, dl)
		}
	}
}

// collect takes in the listings done since the last look.
func (s *folderSource) collect() {
	s.postMu.Lock()
	done := s.posted
	s.posted = nil
	s.postMu.Unlock()
	for _, r := range done {
		// The folders waiting on the device go first, so a folder that changes while it is
		// listed, and is queued again below, waits its turn behind them.
		r.dl.busy = false
		s.next(r.dl)
		f := r.f
		if s.byFD[f.fd] == f {
			f.queued, f.started = false, 0
			if r.gen != f.gen {
				f.again = false
				s.queue(f)
			} else {
				s.apply(f, r)
				if f.again && s.byFD[f.fd] == f {
					f.again = false
					s.queue(f)
				}
			}
		}
	}
}

// apply reports what a listing shows changed since the one before.
func (s *folderSource) apply(f *folder, r listed) {
	if r.err != nil {
		switch {
		case errors.Is(r.err, fs.ErrNotExist) || errors.Is(r.err, syscall.ENOTDIR):
			// The folder went or was replaced; its own note says so.
		case descriptorsSpent(r.err):
			s.sendErr(r.err)
		default:
			s.send(fsnotify.Event{Name: f.path, Op: fsnotify.Write}) // what changed is the rescan's to find
		}
		return
	}
	var arrived, gone []string
	for name := range r.listing.subs {
		if !f.subs[name] {
			arrived = append(arrived, name)
		}
	}
	for name := range f.subs {
		if !r.listing.subs[name] {
			gone = append(gone, name)
		}
	}
	left := f.left
	changed := r.listing.files != f.files
	f.subs, f.files, f.left = r.listing.subs, r.listing.files, nil
	slices.Sort(arrived)
	slices.Sort(gone)
	for _, name := range arrived {
		s.send(fsnotify.Event{Name: filepath.Join(f.path, name), Op: fsnotify.Create})
	}
	for _, name := range gone {
		// A watched folder that left reports itself.
		if path := filepath.Join(f.path, name); !left[name] && s.byPath[path] == nil {
			s.send(fsnotify.Event{Name: path, Op: fsnotify.Remove})
		}
	}
	// A listing that shows nothing new, as a second one for a note the first showed,
	// sends nothing: a Write on a root would rescan the whole library.
	if changed {
		s.send(fsnotify.Event{Name: f.path, Op: fsnotify.Write})
	}
}

// send hands an event to the consumer. A request that arrives meanwhile is answered
// here, since the consumer may be the one asking before it reads.
func (s *folderSource) send(ev fsnotify.Event) {
	for {
		select {
		case s.events <- ev:
			return
		case a := <-s.asks:
			s.answer(a)
		case <-s.done:
			return
		}
	}
}

func (s *folderSource) sendErr(err error) {
	for {
		select {
		case s.errs <- err:
			return
		case a := <-s.asks:
			s.answer(a)
		case <-s.done:
			return
		}
	}
}

// finish gives back every descriptor and ends the streams, as the source's goroutine
// exits.
func (s *folderSource) finish() {
	for fd := range s.byFD {
		s.q.close(fd)
	}
	for _, fd := range s.dropped {
		s.q.close(fd)
	}
	s.q.shut()
	s.mu.Lock()
	s.held = map[string]bool{}
	s.mu.Unlock()
	close(s.events)
	close(s.errs)
	close(s.finished)
}

// listFolder reads a folder's subfolder names and hashes every other entry by its name,
// the file standing under it (fileID), its size and its modification time, so a file
// replaced under its own name changes the hash as a name does.
func listFolder(dir string) (folderListing, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return folderListing{}, err
	}
	l := folderListing{subs: map[string]bool{}}
	h := fnv.New64a()
	var b [24]byte
	for _, e := range entries {
		if e.IsDir() {
			l.subs[e.Name()] = true
			continue
		}
		h.Write([]byte(e.Name()))
		h.Write([]byte{0})
		if info, err := e.Info(); err == nil {
			binary.LittleEndian.PutUint64(b[:8], fileID(info))
			binary.LittleEndian.PutUint64(b[8:16], uint64(info.Size()))
			binary.LittleEndian.PutUint64(b[16:], uint64(info.ModTime().UnixNano()))
			h.Write(b[:])
		}
	}
	l.files = h.Sum64()
	return l, nil
}
