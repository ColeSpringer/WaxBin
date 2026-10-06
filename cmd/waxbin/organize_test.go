package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/organize"
	"github.com/spf13/cobra"
)

// TestOrganizePlanNamesReadOnlyLibraries: a plan that passed over read-only libraries
// says so in both outputs, and one that did not stays quiet.
func TestOrganizePlanNamesReadOnlyLibraries(t *testing.T) {
	t.Parallel()
	render := func(json bool, plan *organize.Plan) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := emitPlan(cmd, &globals{jsonOut: json}, plan); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := render(false, &organize.Plan{Profile: "p", ReadOnlyLibraries: 2}); !strings.Contains(out, "2 read-only libraries left alone") {
		t.Errorf("plan text lacks the read-only count:\n%s", out)
	}
	if out := render(false, &organize.Plan{Profile: "p"}); strings.Contains(out, "read-only") {
		t.Errorf("plan text names read-only libraries it did not pass over:\n%s", out)
	}
	var env struct {
		Data struct {
			ReadOnly int `json:"readOnlyLibraries"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true, &organize.Plan{Profile: "p", ReadOnlyLibraries: 2})), &env); err != nil || env.Data.ReadOnly != 2 {
		t.Errorf("plan json = %+v (err %v), want readOnlyLibraries 2", env, err)
	}
}

// TestOrganizeReportSaysWhatItPruned: an applied organize says how many folders its moves
// emptied and removed, in both outputs.
func TestOrganizeReportSaysWhatItPruned(t *testing.T) {
	t.Parallel()
	render := func(json bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := emitReport(cmd, &globals{jsonOut: json}, "p", &organize.Report{Moved: 3, DirsPruned: 2}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := render(false); !strings.Contains(out, "pruned 2 folders") {
		t.Errorf("report text lacks the pruned count:\n%s", out)
	}
	var env struct {
		Data struct {
			DirsPruned int `json:"dirsPruned"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || env.Data.DirsPruned != 2 {
		t.Errorf("report json = %+v (err %v), want dirsPruned 2", env, err)
	}
}
