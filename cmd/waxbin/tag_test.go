package main

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/meta"
)

// TestTagCommandRouting confirms the parent-with-RunE plus `keys` subcommand shape
// routes unambiguously: `tag keys` reaches the read subcommand while `tag <ulid>`
// reaches the parent set/list RunE. This is only sound because an item pid is a ULID
// and can never be the literal "keys", so no cobra Args/TraverseChildren tweak is needed.
func TestTagCommandRouting(t *testing.T) {
	tagCmd := newTagCmd(&globals{})

	c, _, err := tagCmd.Find([]string{"keys"})
	if err != nil {
		t.Fatalf("find `tag keys`: %v", err)
	}
	if c.Name() != "keys" {
		t.Fatalf("`tag keys` routed to %q, want the keys subcommand", c.Name())
	}

	c, rest, err := tagCmd.Find([]string{"01HZZZZZZZZZZZZZZZZZZZZZZZZ"})
	if err != nil {
		t.Fatalf("find `tag <ulid>`: %v", err)
	}
	if c.Name() != "tag" {
		t.Fatalf("`tag <ulid>` routed to %q, want the tag command", c.Name())
	}
	if len(rest) != 1 || rest[0] != "01HZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("`tag <ulid>` did not preserve the pid arg: %v", rest)
	}
}

// runTagCLI executes the tag command through the root command and returns what it printed.
func runTagCLI(t *testing.T, db, root string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native", "tag"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// TestTagWriteBackWritesTheFile: `tag --write-back` writes the custom tag into the backing
// file as well as the catalog, and like every other set-side flag it needs --key.
func TestTagWriteBackWritesTheFile(t *testing.T) {
	db, root, pid := creditCLIFixture(t)

	stdout, _, err := runTagCLI(t, db, root, string(pid), "--key", "mood", "--value", "calm", "--write-back")
	if err != nil || !strings.Contains(stdout, "set tag MOOD") {
		t.Fatalf("tag --write-back = %q (err %v), want the set reported", stdout, err)
	}
	fm, err := meta.NewReader().Read(context.Background(), filepath.Join(root, "song.mp3"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := fm.Tags.Custom["MOOD"]; !slices.Equal(got, []string{"calm"}) {
		t.Errorf("MOOD on disk = %v, want [calm]", got)
	}

	if _, _, err := runTagCLI(t, db, root, string(pid), "--write-back"); err == nil || !strings.Contains(err.Error(), "--key is required") {
		t.Errorf("tag --write-back without --key = %v, want the --key usage error", err)
	}
}
