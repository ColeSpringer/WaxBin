//go:build !windows && !(darwin || dragonfly || freebsd || netbsd || openbsd)

package watch

import "github.com/fsnotify/fsnotify"

// folderWrites is false: fsnotify reports each entry that changes, never the folder.
const folderWrites = false

// newSource watches one folder per Add through fsnotify.
func newSource() (source, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return fsnotifySource{w}, nil
}

type fsnotifySource struct{ w *fsnotify.Watcher }

func (s fsnotifySource) Add(dir string) error          { return s.w.Add(dir) }
func (s fsnotifySource) Remove(dir string) error       { return s.w.Remove(dir) }
func (s fsnotifySource) Events() <-chan fsnotify.Event { return s.w.Events }
func (s fsnotifySource) Errors() <-chan error          { return s.w.Errors }
func (s fsnotifySource) Close() error                  { return s.w.Close() }

// Dropped reads the watches fsnotify still holds, since it lets one go without an event
// when inotify says the filesystem under it was unmounted.
func (s fsnotifySource) Dropped(dirs []string) []string {
	held := map[string]bool{}
	for _, dir := range s.w.WatchList() {
		held[dir] = true
	}
	var lost []string
	for _, dir := range dirs {
		if !held[dir] {
			lost = append(lost, dir)
		}
	}
	return lost
}

// watchCap is the cap on watched folders, each of which holds a watch here.
func watchCap(n int) int { return n }
