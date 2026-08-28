package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/spf13/cobra"
)

func TestLoadBatchCredits(t *testing.T) {
	doc := `[
		{"itemPid": "i1", "role": "author", "names": ["Ursula K. Le Guin"]},
		{"itemPid": "i1", "role": "narrator", "names": []},
		{"itemPid": "i2", "role": "composer", "names": ["Comp A", "Comp B"]}
	]`

	t.Run("reads a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "credits.json")
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		edits, err := loadBatchCredits(&cobra.Command{}, path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		// One item under two roles is a legitimate pair of entries, and an empty name
		// list is how a batch clears a role.
		if len(edits) != 3 || edits[0].Role != model.RoleAuthor || edits[1].ItemPID != "i1" ||
			len(edits[1].Names) != 0 || len(edits[2].Names) != 2 {
			t.Fatalf("edits = %+v", edits)
		}
	})

	t.Run("reads stdin via dash", func(t *testing.T) {
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader(doc))
		edits, err := loadBatchCredits(cmd, "-")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(edits) != 3 {
			t.Fatalf("edits = %+v", edits)
		}
	})

	t.Run("rejects malformed documents", func(t *testing.T) {
		for name, bad := range map[string]string{
			"empty array": `[]`,
			"no pid":      `[{"role": "author", "names": ["x"]}]`,
			"no role":     `[{"itemPid": "i1", "names": ["x"]}]`,
			"not json":    `{`,
			// The engine refuses a repeated (item, role) pair unconditionally, so the
			// loader refuses it too instead of letting a dry-run preview both entries.
			"duplicate pair": `[{"itemPid": "i1", "role": "author", "names": ["a"]},
				{"itemPid": "i1", "role": "author", "names": ["b"]}]`,
		} {
			cmd := &cobra.Command{}
			cmd.SetIn(strings.NewReader(bad))
			if _, err := loadBatchCredits(cmd, "-"); err == nil {
				t.Errorf("%s: want error, got nil", name)
			}
		}
	})
}

// creditCLIFixture seeds a one-track catalog and returns its db path, root, and the
// track's pid, with the seeding library closed so the CLI can take the write lock.
func creditCLIFixture(t *testing.T) (string, string, model.PID) {
	t.Helper()
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(filepath.Join(root, "song.mp3"),
		testaudio.BuildMP3("Song", "Artist", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Where("title", query.OpIs, "Song").Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("query: %d items (err %v)", len(items), err)
	}
	pid := items[0].PID
	if err := lib.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return db, root, pid
}

func openCLILib(t *testing.T, ctx context.Context, db, root string, readOnly bool) *waxbin.Library {
	t.Helper()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:   db,
		Roots:    []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		ReadOnly: readOnly,
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	return lib
}

// runCreditCLI executes the credit command through the root command, the way a real
// invocation reaches it, and returns what it printed.
func runCreditCLI(t *testing.T, db, root string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native", "credit"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// TestCreditSingleDryRunMutatesNothing: --dry-run on the single-item path previews the
// edit and returns before a mutator opens, so the store keeps what it held.
func TestCreditSingleDryRunMutatesNothing(t *testing.T) {
	db, root, pid := creditCLIFixture(t)

	stdout, _, err := runCreditCLI(t, db, root, string(pid), "--role", "composer", "--name", "Comp One", "--dry-run")
	if err != nil {
		t.Fatalf("credit --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "would set") || !strings.Contains(stdout, "Comp One") {
		t.Fatalf("preview = %q, want the names it would set", stdout)
	}

	// The store read is the proof: no composer credit landed.
	ctx := context.Background()
	lib := openCLILib(t, ctx, db, root, true)
	defer lib.Close()
	credits, err := lib.Credits(ctx, pid)
	if err != nil {
		t.Fatalf("credits: %v", err)
	}
	for _, c := range credits {
		if c.Role == model.RoleComposer {
			t.Fatalf("composer credit %q stored by a dry run", c.Name)
		}
	}
}

// TestCreditSingleSkipLocked: a locked credit role under --skip-locked is reported as
// a skip on stdout with a zero exit, and the curated credit stays.
func TestCreditSingleSkipLocked(t *testing.T) {
	db, root, pid := creditCLIFixture(t)
	ctx := context.Background()
	seed := openCLILib(t, ctx, db, root, false)
	if _, _, err := seed.SetCredits(ctx, pid, model.RoleComposer, []string{"Curated Composer"},
		waxbin.CreditEditOptions{Lock: model.LockOn}); err != nil {
		t.Fatalf("seed locked credit: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stdout, _, err := runCreditCLI(t, db, root, string(pid), "--role", "composer", "--name", "Other", "--skip-locked")
	if err != nil {
		t.Fatalf("credit --skip-locked: %v", err)
	}
	if !strings.Contains(stdout, "skipped") {
		t.Fatalf("output = %q, want the skip reported", stdout)
	}

	lib := openCLILib(t, ctx, db, root, true)
	defer lib.Close()
	credits, err := lib.Credits(ctx, pid)
	if err != nil {
		t.Fatalf("credits: %v", err)
	}
	var composers []string
	for _, c := range credits {
		if c.Role == model.RoleComposer {
			composers = append(composers, c.Name)
		}
	}
	if len(composers) != 1 || composers[0] != "Curated Composer" {
		t.Fatalf("composers = %v, want the locked credit untouched", composers)
	}
}

// TestPrintCreditBatchResult pins the credit printer's shape: the summary counts both
// edits and distinct items, a skipped line carries its role, and an item edited under
// two roles warns once about its write-back failure, since its roles were mirrored in
// one pass.
func TestPrintCreditBatchResult(t *testing.T) {
	res := &waxbin.CreditBatchResult{
		Edited: []model.ItemCreditEdit{
			{ItemPID: "i1", Role: model.RoleAuthor, Names: []string{"A"}},
			{ItemPID: "i1", Role: model.RoleTranslator, Names: []string{"T"}},
			{ItemPID: "i2", Role: model.RoleNarrator, Names: []string{"N"}},
		},
		Skipped: []model.ItemCreditEdit{{ItemPID: "i3", Role: model.RoleNarrator}},
		WriteBackErrors: map[model.PID]*waxbin.WriteBackError{
			"i1": {ItemPID: "i1", Failures: []waxbin.WriteBackFailure{{Path: "/b.m4b", Reason: "no tag"}}},
		},
	}
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	printCreditBatchResult(cmd, res)

	if !strings.Contains(stdout.String(), "3 credit edit(s) across 2 item(s)") {
		t.Errorf("summary = %q, want the edit and distinct item counts", stdout.String())
	}
	if !strings.Contains(stdout.String(), "i3 narrator") {
		t.Errorf("skipped lines = %q, want the pid with its role", stdout.String())
	}
	if n := strings.Count(stderr.String(), "/b.m4b"); n != 1 {
		t.Errorf("write-back warning printed %d times, want once for the item's two entries", n)
	}
}
