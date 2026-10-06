package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/scan"
	"github.com/spf13/cobra"
)

// TestScanResultSaysTheSubPathWasGone: a scan of a sub-path that was not there says so,
// in both outputs, so a mistyped path does not read as an empty folder.
func TestScanResultSaysTheSubPathWasGone(t *testing.T) {
	t.Parallel()
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := renderScanResult(cmd, &globals{jsonOut: asJSON}, "J1", scan.Result{SubPathGone: true}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := render(false); !strings.Contains(out, "sub-path:     not found") {
		t.Errorf("scan text does not say the sub-path was gone:\n%s", out)
	}
	var env struct {
		Data struct {
			SubPathGone bool `json:"subPathGone"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || !env.Data.SubPathGone {
		t.Errorf("scan json = %+v (err %v), want subPathGone", env, err)
	}
}
