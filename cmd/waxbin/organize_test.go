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

// TestOrganizePlanSaysWhyAMoveIsHeld: a plan prints each held move with its code and
// reason apart from the moves already in place, and counts the holds, in both outputs.
func TestOrganizePlanSaysWhyAMoveIsHeld(t *testing.T) {
	t.Parallel()
	plan := &organize.Plan{Profile: "p", Actions: []organize.Action{
		{Src: "/lib/in/a.mp3", Dst: "/lib/A/Al/01 - A.mp3"},
		{Src: "/lib/in/b.mp3", Dst: "/lib/A/Al/02 - B.mp3", Skip: true, Code: organize.HoldOccupied,
			Reason: "destination holds a file the catalog does not know"},
		{Src: "/lib/A/Al/03 - C.mp3", Dst: "/lib/A/Al/03 - C.mp3", Skip: true, Code: organize.HoldInPlace, Reason: "already in place"},
	}}
	render := func(json bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := emitPlan(cmd, &globals{jsonOut: json}, plan); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	text := render(false)
	if !strings.Contains(text, "1 held") || !strings.Contains(text, "hold  /lib/in/b.mp3 [occupied]") ||
		!strings.Contains(text, "skip  /lib/A/Al/03 - C.mp3") {
		t.Errorf("plan text does not tell the hold apart:\n%s", text)
	}
	var env struct {
		Data struct {
			Pending int `json:"pending"`
			Held    int `json:"held"`
			Actions []struct {
				Code string `json:"code"`
			} `json:"actions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil {
		t.Fatal(err)
	}
	if d := env.Data; d.Pending != 1 || d.Held != 1 || len(d.Actions) != 3 ||
		d.Actions[0].Code != "" || d.Actions[1].Code != "occupied" || d.Actions[2].Code != "in-place" {
		t.Errorf("plan json = %+v, want one pending, one held and each action's code", d)
	}
}

// TestOrganizeReportCountsHolds: an applied organize says how many moves it held and,
// for each, why, in both outputs.
func TestOrganizeReportCountsHolds(t *testing.T) {
	t.Parallel()
	render := func(json bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		rep := &organize.Report{Moved: 1, Held: 2, Holds: []organize.HeldMove{
			{Src: "/lib/in/b.mp3", Dst: "/lib/A/b.mp3", Code: organize.HoldOccupied, Reason: "destination holds a file"},
			{Src: "/lib/in/c.mp3", Dst: "/lib/A/c.mp3", Code: organize.HoldMissing, Reason: "file is not on disk"},
		}}
		if err := emitReport(cmd, &globals{jsonOut: json}, "p", rep); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := render(false); !strings.Contains(out, "held 2") || !strings.Contains(out, "HOLD /lib/in/b.mp3 [occupied] (destination holds a file)") ||
		!strings.Contains(out, "HOLD /lib/in/c.mp3 [missing]") {
		t.Errorf("report text does not say what it held and why:\n%s", out)
	}
	var env struct {
		Data struct {
			Held  int `json:"held"`
			Holds []struct {
				Src  string `json:"Src"`
				Code string `json:"Code"`
			} `json:"holds"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || env.Data.Held != 2 || len(env.Data.Holds) != 2 || env.Data.Holds[0].Code != "occupied" {
		t.Errorf("report json = %+v (err %v), want held 2 with each hold's code", env, err)
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
