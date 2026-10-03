package waxbin

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
)

// TestLibraryBeatRisesAcrossRoots: each root's progress maps onto its share of the
// whole run, so the job's progress keeps rising from one root to the next, and the
// message names the root.
func TestLibraryBeatRisesAcrossRoots(t *testing.T) {
	t.Parallel()
	var got []float64
	var msgs []string
	rec := func(p float64, msg string) error {
		got, msgs = append(got, p), append(msgs, msg)
		return nil
	}
	for i := range 2 {
		hb := libraryBeat(i, 2, rec)
		for _, p := range []float64{0.5, 1} {
			if err := hb(p, "scanned 4 files"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if want := []float64{0.25, 0.5, 0.75, 1}; !slices.Equal(got, want) {
		t.Errorf("progress = %v, want %v", got, want)
	}
	if msgs[0] != "scanned 4 files (library 1 of 2)" || msgs[3] != "scanned 4 files (library 2 of 2)" {
		t.Errorf("messages = %q, want each naming its library", msgs)
	}
	var one []string
	_ = libraryBeat(0, 1, func(_ float64, msg string) error { one = append(one, msg); return nil })(1, "scanned 4 files")
	if one[0] != "scanned 4 files" {
		t.Errorf("single-root message = %q, want it unchanged", one[0])
	}
}

// twoRootLibrary opens a library over two in-place roots holding a track each.
func twoRootLibrary(t *testing.T) *Library {
	t.Helper()
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	lib, err := Open(ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: rootA, Mode: model.ModeInPlace}, {Path: rootB, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	writeRaw(t, filepath.Join(rootA, "a.mp3"), testaudio.BuildMP3WithAudio("A", "Band", "One", 1, testaudio.AudioWithSeed(1)))
	writeRaw(t, filepath.Join(rootB, "b.mp3"), testaudio.BuildMP3WithAudio("B", "Band", "One", 2, testaudio.AudioWithSeed(2)))
	return lib
}

// TestScanSurvivesItsClosingBeat: once every library is scanned, a closing heartbeat that
// cannot be written does not turn the finished scan into a failure.
func TestScanSurvivesItsClosingBeat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := twoRootLibrary(t)
	libs, err := lib.resolveLibraries(ctx, "")
	if err != nil || len(libs) != 2 {
		t.Fatalf("libraries = %v (err %v)", libs, err)
	}
	out := &ScanResult{}
	beat := func(_ float64, msg string) error {
		if strings.Contains(msg, "libraries") {
			return errors.New("database is locked")
		}
		return nil
	}
	if err := lib.scanLibraries(ctx, libs, ScanRequest{}, out, beat); err != nil || out.Total.AudioFiles != 2 {
		t.Errorf("scan = %+v (err %v), want both files scanned and no error", out.Total, err)
	}
}

// TestScanTotalNamesASingleLibrary: a scan of one library names it at the top of its
// result as well as in its run.
func TestScanTotalNamesASingleLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := twoRootLibrary(t)
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := lib.Scan(ctx, ScanRequest{LibraryPID: libs[0].PID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total.LibraryPID != libs[0].PID || res.Total.LibraryName == "" {
		t.Errorf("total = %+v, want it to name library %s", res.Total, libs[0].PID)
	}
	all, err := lib.Scan(ctx, ScanRequest{})
	if err != nil || all.Total.LibraryPID != "" {
		t.Errorf("multi-library total = %+v (err %v), want no library named", all.Total, err)
	}
}
