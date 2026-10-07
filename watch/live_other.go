//go:build !windows

package watch

import "github.com/fsnotify/fsnotify"

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

// watchCap is the cap on watched folders, each of which holds a watch here.
func watchCap(n int) int { return n }

// watchHint points at the usual cause of a failed watch arm on Linux, the per-user
// inotify watch limit, so a degraded warning is actionable.
const watchHint = "on Linux, raise fs.inotify.max_user_watches (sysctl) if this is watch exhaustion"
