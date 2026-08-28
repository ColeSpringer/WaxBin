package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// acquisitionCLIFixture seeds a one-track catalog whose file carries the acquisition
// tags a mis-tagged rip has, and returns its db path, root, and the track's pid with
// the seeding library closed so the CLI can take the write lock.
func acquisitionCLIFixture(t *testing.T) (string, string, model.PID) {
	t.Helper()
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	if err := os.WriteFile(src, testaudio.BuildMP3("Ripped", "Artist", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.NewWriter().Apply(ctx, src, []meta.TagEdit{
		{Key: "SOURCE_URL", Values: []string{"https://wrong.test/x"}},
		{Key: "SOURCE_ID", Values: []string{"wrong-1"}},
	}); err != nil {
		t.Fatalf("stamp acquisition tags: %v", err)
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Where("title", query.OpIs, "Ripped").Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("query: %d items (err %v)", len(items), err)
	}
	pid := items[0].PID
	if err := lib.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return db, root, pid
}

// decodeAcquisitionJSON unwraps the CLI's JSON envelope down to the acquisition view.
func decodeAcquisitionJSON(t *testing.T, stdout string) acquisitionView {
	t.Helper()
	var env struct {
		Data acquisitionView `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}
	return env.Data
}

// runAcquisitionCLI executes the acquisition command through the root command, the way
// a real invocation reaches it, and returns what it printed.
func runAcquisitionCLI(t *testing.T, db, root string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native", "acquisition"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// TestAcquisitionSetCorrectsAndLocks walks the correction WaxDeck asked for: the wrong
// tag-derived origin is replaced, the emptied fields are gone, and the field is locked
// so a later scan leaves it alone.
func TestAcquisitionSetCorrectsAndLocks(t *testing.T) {
	db, root, pid := acquisitionCLIFixture(t)

	stdout, _, err := runAcquisitionCLI(t, db, root, "set", string(pid),
		"--type", "manual", "--url", "https://right.test/y")
	if err != nil {
		t.Fatalf("acquisition set: %v", err)
	}
	if !strings.Contains(stdout, "source:manual") {
		t.Errorf("output = %q, want it to name the recorded type", stdout)
	}

	stdout, _, err = runAcquisitionCLI(t, db, root, string(pid), "--json")
	if err != nil {
		t.Fatalf("acquisition read: %v", err)
	}
	got := decodeAcquisitionJSON(t, stdout)
	if got.SourceType != "manual" || got.SourceURL != "https://right.test/y" {
		t.Errorf("read back %+v, want the correction", got)
	}
	if got.SourceID != "" {
		t.Errorf("source id = %q, want the authoritative replace to have emptied it", got.SourceID)
	}

	// A second unforced set is refused, which is the lock the first one left behind.
	_, _, err = runAcquisitionCLI(t, db, root, "set", string(pid), "--type", "rss")
	if !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("unforced set over a lock = %v, want CodeLocked", err)
	}
	if _, _, err := runAcquisitionCLI(t, db, root, "set", string(pid), "--type", "rss", "--force"); err != nil {
		t.Fatalf("forced set: %v", err)
	}
}

// TestAcquisitionSetFlagWiring covers the flags that carry no behaviour of their own:
// the required type, the RFC3339 stamp, and the two lock flags cobra keeps apart.
func TestAcquisitionSetFlagWiring(t *testing.T) {
	db, root, pid := acquisitionCLIFixture(t)

	if _, _, err := runAcquisitionCLI(t, db, root, "set", string(pid)); err == nil {
		t.Error("acquisition set with no --type was accepted, want the required flag to refuse")
	}
	_, _, err := runAcquisitionCLI(t, db, root, "set", string(pid), "--type", "manual", "--acquired-at", "yesterday")
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("bad --acquired-at = %v, want CodeInvalid", err)
	}
	if _, _, err := runAcquisitionCLI(t, db, root, "set", string(pid),
		"--type", "manual", "--no-lock", "--keep-lock"); err == nil {
		t.Error("--no-lock with --keep-lock was accepted, want cobra to refuse the pair")
	}
	// local is the absence of a row, so the store points at the clear instead.
	_, _, err = runAcquisitionCLI(t, db, root, "set", string(pid), "--type", "local")
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "clear") {
		t.Errorf("--type local = %v, want CodeInvalid naming the clear", err)
	}

	if _, _, err := runAcquisitionCLI(t, db, root, "set", string(pid),
		"--type", "manual", "--acquired-at", "2020-05-04T03:02:01Z", "--no-lock"); err != nil {
		t.Fatalf("acquisition set with a stamp: %v", err)
	}
	stdout, _, err := runAcquisitionCLI(t, db, root, string(pid), "--json")
	if err != nil {
		t.Fatalf("acquisition read: %v", err)
	}
	got := decodeAcquisitionJSON(t, stdout)
	if got.AcquiredAt != 1588561321000000000 {
		t.Errorf("acquired at = %d, want the RFC3339 stamp in ns", got.AcquiredAt)
	}
}

// TestAcquisitionClearLocksByDefault: a bare clear locks, which is what makes it hold,
// and --no-lock is the opt-out spelled the way every other curation verb spells it. The
// wording no longer claims the item reads local, which would be wrong for an episode of
// a feed show, and a repeated clear is not refused by its own predecessor's lock.
func TestAcquisitionClearLocksByDefault(t *testing.T) {
	db, root, pid := acquisitionCLIFixture(t)

	stdout, stderr, err := runAcquisitionCLI(t, db, root, "clear", string(pid))
	if err != nil {
		t.Fatalf("acquisition clear: %v", err)
	}
	if strings.Contains(stdout, "source:local") {
		t.Errorf("output = %q, want no claim that it now reads local", stdout)
	}
	if strings.Contains(stderr, "warning") {
		t.Errorf("a locking clear warned about durability: %q", stderr)
	}
	// The clear is idempotent: its own lock must not refuse the next run, which is what a
	// batch correction over a mixed selection does.
	if _, _, err := runAcquisitionCLI(t, db, root, "clear", string(pid)); err != nil {
		t.Fatalf("second clear = %v, want the idempotent no-op", err)
	}
	// Declining the lock without stripping the tags leaves the clear undone by the next
	// scan, which is the one combination that warns.
	_, stderr, err = runAcquisitionCLI(t, db, root, "clear", string(pid), "--force", "--no-lock")
	if err != nil {
		t.Fatalf("forced unlocking clear: %v", err)
	}
	if !strings.Contains(stderr, "next full scan") {
		t.Errorf("stderr = %q, want the durability warning", stderr)
	}
}

// TestAcquisitionReadReportsTheFallback: an item with no row of its own reports the
// source it actually reads, rather than assuming local. The read command stays on the
// read-only open.
func TestAcquisitionReadReportsTheFallback(t *testing.T) {
	db, root, pid := acquisitionCLIFixture(t)
	if _, _, err := runAcquisitionCLI(t, db, root, "clear", string(pid)); err != nil {
		t.Fatalf("acquisition clear: %v", err)
	}
	stdout, _, err := runAcquisitionCLI(t, db, root, string(pid))
	if err != nil {
		t.Fatalf("acquisition read: %v", err)
	}
	if !strings.Contains(stdout, "no acquisition provenance of its own") {
		t.Errorf("output = %q, want the fallback wording", stdout)
	}
	if !strings.Contains(stdout, "source type: local") {
		t.Errorf("output = %q, want a track's fallback to read local", stdout)
	}
}

// TestAcquisitionWriteBackStripsTheTags: the file loses the evidence, so the clear
// holds across a rescan with the lock released.
func TestAcquisitionWriteBackStripsTheTags(t *testing.T) {
	db, root, pid := acquisitionCLIFixture(t)

	if _, _, err := runAcquisitionCLI(t, db, root, "clear", string(pid), "--write-back", "--no-lock"); err != nil {
		t.Fatalf("acquisition clear --write-back: %v", err)
	}
	ctx := context.Background()
	fm, err := meta.NewReader().Read(ctx, filepath.Join(root, "song.mp3"))
	if err != nil {
		t.Fatalf("read the cleared file: %v", err)
	}
	if fm.Tags.Acquisition.Present() {
		t.Errorf("file still carries acquisition tags: %+v", fm.Tags.Acquisition)
	}

	lib := openCLILib(t, ctx, db, root, false)
	defer lib.Close()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if _, err := lib.Acquisition(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("the origin came back with no lock and no tags: %v", err)
	}
}

// TestAcquisitionReadSeparatesUnknownPidFromNoRow: both reach CodeNotFound in the store,
// and only the item read tells them apart. Without it a typo reads as a locally scanned
// track and exits 0.
func TestAcquisitionReadSeparatesUnknownPidFromNoRow(t *testing.T) {
	db, root, pid := acquisitionCLIFixture(t)
	if _, _, err := runAcquisitionCLI(t, db, root, "clear", string(pid)); err != nil {
		t.Fatalf("acquisition clear: %v", err)
	}
	if _, _, err := runAcquisitionCLI(t, db, root, string(pid)); err != nil {
		t.Fatalf("read of a row-less item = %v, want it reported", err)
	}
	if _, _, err := runAcquisitionCLI(t, db, root, "01HZZZZZZZZZZZZZZZZZZZZZZZ"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("read of an unknown pid = %v, want CodeNotFound", err)
	}
}

// TestAcquisitionJSONMarksAnInheritedSource: a source read through the item rather than
// off a row of its own carries the discriminator, or a feed episode with no row would be
// indistinguishable from one carrying a curated rss row.
func TestAcquisitionJSONMarksAnInheritedSource(t *testing.T) {
	db, root, pid := acquisitionCLIFixture(t)

	stdout, _, err := runAcquisitionCLI(t, db, root, string(pid), "--json")
	if err != nil {
		t.Fatalf("acquisition read: %v", err)
	}
	if got := decodeAcquisitionJSON(t, stdout); got.Inherited {
		t.Errorf("an item with its own row reported inherited: %+v", got)
	}
	if _, _, err := runAcquisitionCLI(t, db, root, "clear", string(pid)); err != nil {
		t.Fatalf("acquisition clear: %v", err)
	}
	stdout, _, err = runAcquisitionCLI(t, db, root, string(pid), "--json")
	if err != nil {
		t.Fatalf("acquisition read: %v", err)
	}
	got := decodeAcquisitionJSON(t, stdout)
	if !got.Inherited || got.SourceType != string(model.SourceLocal) {
		t.Errorf("read back %+v, want an inherited local", got)
	}
}

// TestAcquisitionSetRefusesBadInput covers the two guards the shared timestamp parser
// carries and the one this surface adds: an out-of-range date wraps silently through
// UnixNano, the epoch collides with the store's "keep the stamp" sentinel, and nothing
// downstream parses the options blob.
func TestAcquisitionSetRefusesBadInput(t *testing.T) {
	db, root, pid := acquisitionCLIFixture(t)
	for _, tc := range []struct{ what, flag, value string }{
		{"out of range", "--acquired-at", "3000-01-01T00:00:00Z"},
		{"the epoch sentinel", "--acquired-at", "1970-01-01T00:00:00Z"},
		{"malformed options", "--options", "{not json"},
	} {
		_, _, err := runAcquisitionCLI(t, db, root, "set", string(pid), "--type", "manual", tc.flag, tc.value)
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s: %s %q = %v, want CodeInvalid", tc.what, tc.flag, tc.value, err)
		}
	}
	// A unix-nanosecond stamp is accepted too, the way every other time flag takes one.
	if _, _, err := runAcquisitionCLI(t, db, root, "set", string(pid),
		"--type", "manual", "--acquired-at", "1588561321000000000"); err != nil {
		t.Fatalf("unix ns stamp: %v", err)
	}
}
