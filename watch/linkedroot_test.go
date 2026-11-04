package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testpath"
	"github.com/fsnotify/fsnotify"
)

// TestLiveWatchArmsALinkedRoot: a root that is a link to a folder is armed as that
// folder, each watch spelled under the root, so the events it reports name paths the
// scan files under the root as registered.
func TestLiveWatchArmsALinkedRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "lib")
	if err := os.MkdirAll(filepath.Join(real, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	testpath.Symlink(t, real, link)
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	added, exhausted := lw.armRoot(context.Background(), link)
	if added != 2 || exhausted || lw.total() != 2 {
		t.Fatalf("armRoot(link) = %d added, exhausted %t, %d armed; want the root and its folder", added, exhausted, lw.total())
	}
	if src.told(link, false) != 1 || src.told(filepath.Join(link, "a"), false) != 1 {
		t.Errorf("source was told %v, want the link and the folder below it spelled under the link", src.adds)
	}
	if src.told(real, false) != 0 || src.told(filepath.Join(real, "a"), false) != 0 {
		t.Errorf("source was told %v, want nothing spelled under the folder the link reaches", src.adds)
	}
}

// TestHandleEventLeavesALinkUnarmed: a link created under a root is not a folder to arm,
// since the scan does not follow it; its parent's rescan counts it.
func TestHandleEventLeavesALinkUnarmed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	testpath.Symlink(t, elsewhere, link)
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	if _, ex := lw.armRoot(context.Background(), root); ex {
		t.Fatal("arming the root hit a ceiling")
	}
	fired := make(chan string, 4)
	deb := newDebouncer(10*time.Millisecond, func(dir string) { fired <- dir })
	defer deb.stop()
	w := newWatcher(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{})
	w.handleEvent(lw, deb, fsnotify.Event{Name: link, Op: fsnotify.Create})
	select {
	case dir := <-fired:
		if dir != root {
			t.Errorf("scheduled %s, want the parent folder %s", dir, root)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no rescan was scheduled")
	}
	if src.told(link, false) != 0 || src.told(filepath.Join(link, "sub"), false) != 0 || lw.watched(link) {
		t.Errorf("source was told %v, want the link and what it reaches left unarmed", src.adds)
	}
}

// TestAddTreeFollowsOnlyARoot: a link handed to addTree is followed only when it is the
// root being armed, so a folder swapped for a link between an event and its arming is
// left unwatched rather than watched through the link.
func TestAddTreeFollowsOnlyARoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "link")
	if err := os.MkdirAll(filepath.Join(real, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	testpath.Symlink(t, real, link)
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	if added, _ := lw.addTree(link, false); added != 0 || src.told(link, false) != 0 {
		t.Errorf("addTree of a link below a root armed %d (told %v), want nothing", added, src.adds)
	}
	if added, _ := lw.addTree(link, true); added != 2 || src.told(link, false) != 1 || src.told(filepath.Join(link, "a"), false) != 1 {
		t.Errorf("addTree of a link root armed %d (told %v), want the root and its folder", added, src.adds)
	}
}

// TestFollowReArmsARetargetedLinkRoot: a root that is a link, pointed at another folder
// since it was armed, is released and armed again on the next tick, so the folder it
// reaches now has live coverage; one pointing where it did is left alone.
func TestFollowReArmsARetargetedLinkRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, second, link := filepath.Join(dir, "first"), filepath.Join(dir, "second"), filepath.Join(dir, "link")
	for _, p := range []string{filepath.Join(first, "a"), filepath.Join(second, "b")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	testpath.Symlink(t, first, link)
	ctx := context.Background()
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	set := []Root{{LibraryPID: "L1", Path: link}}
	if _, ex := lw.armRoot(ctx, link); ex {
		t.Fatal("arming hit a ceiling")
	}
	lw.follow(ctx, set)
	if src.told(link, false) != 1 || src.told(link, true) != 0 {
		t.Fatalf("a tick with the link where it was told the source %v / dropped %v, want it left alone", src.adds, src.drops)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	testpath.Symlink(t, second, link)
	lw.follow(ctx, set)
	if src.told(link, true) == 0 || src.told(link, false) != 2 {
		t.Fatalf("a tick after the retarget told the source %v / dropped %v, want the root released and armed again", src.adds, src.drops)
	}
	if lw.watched(filepath.Join(link, "a")) || !lw.watched(filepath.Join(link, "b")) {
		t.Errorf("watched after the retarget: a %t, b %t; want the folder it reaches now", lw.watched(filepath.Join(link, "a")), lw.watched(filepath.Join(link, "b")))
	}
}
