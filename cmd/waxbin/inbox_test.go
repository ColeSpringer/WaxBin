package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testcatalog"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// TestInboxImportForcesAKind: `inbox import --as book` imports a staged folder's plain
// files as books, each pinned with a kind lock as `import --as` pins one, and --as refuses
// a kind an import cannot force.
func TestInboxImportForcesAKind(t *testing.T) {
	t.Parallel()
	root, staging := t.TempDir(), t.TempDir()
	db := testcatalog.Seed(t, filepath.Join(t.TempDir(), "catalog.db"))
	src := filepath.Join(staging, "Persuasion", "01.mp3")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Chapter 01", Artist: "Jane Austen",
		Album: "Persuasion", Track: 1, Audio: testaudio.AudioWithSeed(1)}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLIJSON(t, db, root, "inbox", "import", staging, "--as", "episode"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("--as episode = %v, want CodeInvalid", err)
	}
	out, err := runCLIJSON(t, db, root, "inbox", "import", staging, "--as", "book", "--apply")
	if err != nil {
		t.Fatalf("inbox import --as book: %v", err)
	}
	var env struct {
		Data struct {
			Imported int `json:"imported"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Data.Imported != 1 {
		t.Fatalf("report %q (err %v), want one import", out, err)
	}
	ctx := context.Background()
	lib := openCLILib(t, ctx, db, root, true)
	defer lib.Close()
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("books = %d (err %v), want the import", len(books), err)
	}
	rows, err := lib.Provenance(ctx, books[0].PID)
	if err != nil {
		t.Fatal(err)
	}
	locked := false
	for _, r := range rows {
		locked = locked || (r.Field == model.KindLockField && r.Locked)
	}
	if !locked {
		t.Errorf("provenance = %+v, want the forced kind locked", rows)
	}
}

// TestImportReportSaysWhatItPruned: an applied import says how many staging folders it
// emptied and removed, in both outputs.
func TestImportReportSaysWhatItPruned(t *testing.T) {
	t.Parallel()
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := emitImportReport(cmd, &globals{jsonOut: asJSON}, "/staging", &inbox.Report{Imported: 2, DirsPruned: 1}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := render(false); !strings.Contains(out, "pruned 1 folder") {
		t.Errorf("report text lacks the pruned count:\n%s", out)
	}
	var env struct {
		Data struct {
			DirsPruned int `json:"dirsPruned"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || env.Data.DirsPruned != 1 {
		t.Errorf("report json = %+v (err %v), want dirsPruned 1", env, err)
	}
}

// TestImportReportNamesWhatEachFileDid: an applied import names each file that joined, came
// back to or copied an item standing before it, and the JSON carries every file's verdict.
func TestImportReportNamesWhatEachFileDid(t *testing.T) {
	t.Parallel()
	rep := &inbox.Report{Imported: 8, Files: []inbox.FileOutcome{
		{Path: "/lib/a.mp3", ItemPID: "ITEMA", Verdict: scan.VerdictCreated},
		{Path: "/lib/b.mp3", ItemPID: "ITEMB", Verdict: scan.VerdictJoined},
		{Path: "/lib/c.mp3", ItemPID: "ITEMC", Verdict: scan.VerdictRevived, Relinked: true, Restores: "ITEMC"},
		{Path: "/lib/d.mp3", ItemPID: "ITEMD", Verdict: scan.VerdictCopy, AttachedAsCopy: true},
		{Path: "/lib/e.mp3", ItemPID: "ITEME", Verdict: scan.VerdictUnchanged, Restores: "ITEME"},
		{Path: "/lib/f.mp3", ItemPID: "ITEMF", Verdict: scan.VerdictRevived, AttachedAsCopy: true},
		{Path: "/lib/rip.flac", Verdict: scan.VerdictRevived},
		{Path: "/lib/rip2.flac", Verdict: scan.VerdictUpdated, Restores: "TRACK1"},
	}}
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := emitImportReport(cmd, &globals{jsonOut: asJSON}, "/staging", rep); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	text := render(false)
	for _, want := range []string{"joined /lib/b.mp3: item ITEMB", "revived /lib/c.mp3: item ITEMC", "copy /lib/d.mp3: item ITEMD",
		"restored /lib/e.mp3: item ITEME", "revived /lib/f.mp3: item ITEMF", "revived /lib/rip.flac\n", "restored /lib/rip2.flac: item TRACK1"} {
		if !strings.Contains(text, want) {
			t.Errorf("report text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "item \n") {
		t.Errorf("report text names an empty item:\n%s", text)
	}
	if strings.Contains(text, "/lib/a.mp3") {
		t.Errorf("report text names a file that made a new item:\n%s", text)
	}
	var env struct {
		Data struct {
			Files []struct {
				Path     string `json:"path"`
				Verdict  string `json:"verdict"`
				Relinked bool   `json:"relinked"`
				Restores string `json:"restores"`
			} `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || len(env.Data.Files) != 8 {
		t.Fatalf("report json = %+v (err %v)", env, err)
	}
	for i, want := range []string{"created", "joined", "revived", "copy", "unchanged", "revived"} {
		if got := env.Data.Files[i]; got.Verdict != want || got.Relinked != (i == 2) || (got.Restores != "") != (i == 2 || i == 4) {
			t.Errorf("file %d = %+v, want verdict %s", i, got, want)
		}
	}
}

// TestImportPlanNamesEachHold: a plan names each refusal's code beside its reason and the
// destination a quarantine would have taken, and the JSON carries the code, destination,
// occupant and the item an import restores.
func TestImportPlanNamesEachHold(t *testing.T) {
	t.Parallel()
	plan := &inbox.Plan{Source: "/staging", Actions: []inbox.Action{
		{Src: "/staging/a.mp3", Dst: "/lib/a.mp3", Outcome: inbox.OutcomeImport, Restores: "ITEMA"},
		{Src: "/staging/b.mp3", Dst: "/lib/a.mp3", Outcome: inbox.OutcomeQuarantine, Code: inbox.HoldClaimed,
			Reason: "destination already claimed by another staged file", Occupant: &inbox.Occupant{Path: "/lib/a.mp3", StagedSrc: "/staging/a.mp3"}},
		{Src: "/staging/c.mp3", Outcome: inbox.OutcomeDuplicate, Code: inbox.HoldDuplicateCatalog, Reason: "audio already in the catalog",
			Occupant: &inbox.Occupant{Path: "/lib/c.mp3", FilePID: "FILEC", ItemPID: "ITEMC"}},
	}}
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := emitImportPlan(cmd, &globals{jsonOut: asJSON}, plan); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	text := render(false)
	for _, want := range []string{
		"quarantine /staging/b.mp3 [claimed] (destination already claimed by another staged file)\n             -> /lib/a.mp3",
		"duplicate  /staging/c.mp3 [duplicate] (audio already in the catalog)",
		"restores item ITEMA",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plan text lacks %q:\n%s", want, text)
		}
	}
	var env struct {
		Data struct {
			Actions []struct {
				Dst      string          `json:"dst"`
				Code     string          `json:"code"`
				Restores string          `json:"restores"`
				Occupant *inbox.Occupant `json:"occupant"`
			} `json:"actions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || len(env.Data.Actions) != 3 {
		t.Fatalf("plan json = %+v (err %v)", env, err)
	}
	if a := env.Data.Actions; a[0].Restores != "ITEMA" || a[1].Code != "claimed" || a[1].Dst != "/lib/a.mp3" ||
		a[1].Occupant == nil || a[1].Occupant.StagedSrc != "/staging/a.mp3" || a[2].Code != "duplicate" || a[2].Occupant == nil || a[2].Occupant.ItemPID != "ITEMC" {
		t.Errorf("plan json actions = %+v", a)
	}
}

// TestImportReportNamesItsHolds: an applied import names each file it did not bring in,
// with its code and reason, in both outputs.
func TestImportReportNamesItsHolds(t *testing.T) {
	t.Parallel()
	rep := &inbox.Report{Duplicates: 1, Quarantined: 1, Holds: []inbox.Held{
		{Src: "/staging/a.mp3", Outcome: inbox.OutcomeDuplicate, Code: inbox.HoldLandedSince, Reason: "audio reached the catalog since the plan",
			Occupant: &inbox.Occupant{Path: "/lib/a.mp3", ItemPID: "ITEMA"}},
		{Src: "/staging/b.mp3", Dst: "/lib/b.mp3", Outcome: inbox.OutcomeQuarantine, Code: inbox.HoldExists, Reason: "destination already exists in the library"},
	}}
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := emitImportReport(cmd, &globals{jsonOut: asJSON}, "/staging", rep); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	text := render(false)
	for _, want := range []string{
		"duplicate /staging/a.mp3 [landed-since] (audio reached the catalog since the plan)",
		"quarantine /staging/b.mp3 [exists] (destination already exists in the library)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report text lacks %q:\n%s", want, text)
		}
	}
	var env struct {
		Data struct {
			Holds []inbox.Held `json:"holds"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || len(env.Data.Holds) != 2 ||
		env.Data.Holds[0].Code != inbox.HoldLandedSince || env.Data.Holds[0].Occupant == nil || env.Data.Holds[1].Dst != "/lib/b.mp3" {
		t.Errorf("report json holds = %+v (err %v)", env.Data.Holds, err)
	}
}
