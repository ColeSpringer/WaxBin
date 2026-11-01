package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/spf13/cobra"
)

// TestSurfaceWriteBackSaysWhatHappened: a lost value reads as the file not holding it, not
// as a write skipped, a refused key as skipped for that key, and a whole file as skipped.
func TestSurfaceWriteBackSaysWhatHappened(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	err := surfaceWriteBack(cmd, &waxbin.WriteBackError{Failures: []waxbin.WriteBackFailure{
		{Path: "/m/a.mp3", Key: "MOOD", Lost: true, Reason: "dropped"},
		{Path: "/m/a.mp3", Lost: true, Reason: "dropped"},
		{Path: "/m/a.mp3", Key: "TPE2", Reason: "not a custom tag"},
		{Path: "/m/b.mp3", Reason: "read-only"},
	}})
	if err != nil {
		t.Fatalf("surface = %v", err)
	}
	for _, want := range []string{
		"warning: /m/a.mp3 was written but cannot hold MOOD as given",
		"warning: /m/a.mp3 was written but cannot hold every value as given",
		"warning: on-disk write-back of TPE2 skipped for /m/a.mp3: not a custom tag",
		"warning: on-disk write-back skipped for /m/b.mp3: read-only",
	} {
		if !strings.Contains(stderr.String(), want+"\n") {
			t.Errorf("stderr = %q, want a line %q", stderr.String(), want)
		}
	}
}
