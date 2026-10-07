package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// partialError reads the error member of a --json document's data, failing the test on
// a document that is not one.
func partialError(t *testing.T, out []byte) (string, bool) {
	t.Helper()
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("printed %q: %v", out, err)
	}
	raw, ok := env.Data["error"]
	if !ok {
		return "", false
	}
	var msg string
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("error member %s: %v", raw, err)
	}
	return msg, true
}

// TestPartialRunsSayWhatFailed: a --json report printed by a run that then fails names
// the failure, so a host reading stdout cannot take it for a success, and a run that
// succeeds carries no error member.
func TestPartialRunsSayWhatFailed(t *testing.T) {
	t.Parallel()
	failure := waxerr.New(waxerr.CodeCanceled, "edit", "write-back canceled")
	jsonCmd := func() (*cobra.Command, *bytes.Buffer) {
		cmd := &cobra.Command{}
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		return cmd, &stdout
	}
	g := &globals{jsonOut: true}
	for name, print := range map[string]func(cmd *cobra.Command, err error) error{
		"edit": func(cmd *cobra.Command, err error) error {
			return printBatchEditResult(cmd, g, &waxbin.BatchEditResult{Edited: []model.PID{"i1"}}, err)
		},
		"credit": func(cmd *cobra.Command, err error) error {
			return printCreditBatchResult(cmd, g, &waxbin.CreditBatchResult{
				Edited: []model.ItemCreditEdit{{ItemPID: "i1", Role: model.RoleComposer}}}, err)
		},
		"kind": func(cmd *cobra.Command, err error) error {
			return emitKindReport(cmd, g, model.KindBook, &waxbin.KindReport{Converted: []model.PID{"i1"}}, err)
		},
	} {
		cmd, stdout := jsonCmd()
		if err := print(cmd, failure); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if msg, ok := partialError(t, stdout.Bytes()); !ok || !strings.Contains(msg, "write-back canceled") {
			t.Errorf("%s: error member = %q (present %v), want the failure", name, msg, ok)
		}
		cmd, stdout = jsonCmd()
		if err := print(cmd, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := partialError(t, stdout.Bytes()); ok {
			t.Errorf("%s: a clean run printed an error member", name)
		}
	}
}

// TestTrashPurgeReportsWhatItPurgedBeforeAFailure: purging two entries where the second
// fails prints what went before, with the failure, and exits non-zero.
func TestTrashPurgeReportsWhatItPurgedBeforeAFailure(t *testing.T) {
	db, root, p := playlistCLIFixture(t)
	if _, err := runCLIJSON(t, db, root, "rm", string(p["One"]), "--apply"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	out, err := runCLIJSON(t, db, root, "trash", "list")
	if err != nil {
		t.Fatalf("trash list: %v", err)
	}
	var listed struct {
		Data []struct {
			PID string `json:"pid"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed.Data) != 1 {
		t.Fatalf("trash list printed %q (err %v)", out, err)
	}
	out, err = runCLIJSON(t, db, root, "trash", "purge", listed.Data[0].PID, "no-such-entry")
	if err == nil {
		t.Fatal("purging a missing entry succeeded")
	}
	msg, ok := partialError(t, []byte(out))
	if !ok || msg == "" || !strings.Contains(out, `"purged": 1`) {
		t.Errorf("purge printed %s, want one purged and the failure", out)
	}
}

// TestFailingVerdictsNameTheFailure: a report whose verdict fails the command (drift
// under db verify, an error finding under audit, an .nsp export with no counterpart)
// carries that failure as its error member.
func TestFailingVerdictsNameTheFailure(t *testing.T) {
	db, root, p := playlistCLIFixture(t)
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec("UPDATE album SET year = 1900"); err != nil {
		t.Fatalf("drift the album year: %v", err)
	}
	_ = raw.Close()
	rule := filepath.Join(t.TempDir(), "rule.json")
	doc, err := query.MarshalRule(query.New(query.EntityItems).Where("tag.MOOD", query.OpIs, "calm").Build())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rule, doc, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIJSON(t, db, root, "smartplaylist", "create", "Calm", "--rule", rule)
	if err != nil {
		t.Fatalf("smartplaylist create: %v", err)
	}
	var created struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("create printed %q: %v", out, err)
	}
	_ = p
	for _, args := range [][]string{
		{"db", "verify"},
		{"audit", "--check", string(model.CheckDerivedData)},
		{"smartplaylist", "export-nsp", created.Data["smartplaylist"]},
	} {
		out, err := runCLIJSON(t, db, root, args...)
		if err == nil {
			t.Errorf("%v succeeded, want it to fail", args)
			continue
		}
		if msg, ok := partialError(t, []byte(out)); !ok || msg == "" {
			t.Errorf("%v printed %.200s, want the failure as the error member", args, out)
		}
	}
}
