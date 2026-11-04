//go:build windows

package fsx

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// junction makes a junction at link to target with mklink /J, which needs no privilege,
// skipping the test where it cannot.
func junction(t *testing.T, target, link string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("no junctions here: %v: %s", err, out)
	}
}

// TestJunctionsAreLinks: a junction is a link: a root that is one is walked as the folder
// it reaches, one below a root is reported as a link and not followed, a sub-path through
// one is found, and a path through one resolves to the folder it reaches.
func TestJunctionsAreLinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	writeFile(t, filepath.Join(real, "a", "1.mp3"), "one")
	jroot, jsub := filepath.Join(dir, "jroot"), filepath.Join(real, "jsub")
	junction(t, real, jroot)
	junction(t, filepath.Join(real, "a"), jsub)
	info, err := os.Lstat(jroot)
	if err != nil {
		t.Fatal(err)
	}
	if !IsLink(info) {
		t.Fatalf("a junction stats as %v, want a link", info.Mode())
	}
	var paths, links []string
	err = WalkRoot(jroot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		if EntryIsLink(d) {
			links = append(links, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkRoot(junction): %v", err)
	}
	want := []string{jroot, filepath.Join(jroot, "a"), filepath.Join(jroot, "a", "1.mp3"), filepath.Join(jroot, "jsub")}
	slices.Sort(paths)
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Errorf("WalkRoot(junction) reported %v, want the folder's entries under the junction %v", paths, want)
	}
	if !slices.Equal(links, []string{filepath.Join(jroot, "jsub")}) {
		t.Errorf("links reported = %v, want the junction below the root alone", links)
	}
	if link, ok := LinkBelow(real, filepath.Join(jsub, "x")); !ok || link != jsub {
		t.Errorf("LinkBelow through a junction = %q, %t; want %q", link, ok, jsub)
	}
	realNow, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveLinks(filepath.Join(jroot, "a", "new")); err != nil || !strings.EqualFold(got, filepath.Join(realNow, "a", "new")) {
		t.Errorf("ResolveLinks through a junction = %q, %v; want %q", got, err, filepath.Join(realNow, "a", "new"))
	}
}
