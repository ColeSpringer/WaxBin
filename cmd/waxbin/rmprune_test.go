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
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/trash"
	"github.com/spf13/cobra"
)

// TestRmReportsThePrunedFolders: rm says how many folders the delete emptied and removed,
// in JSON and in text.
func TestRmReportsThePrunedFolders(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, title := range []string{"One", "Two"} {
		path := filepath.Join(root, title, "1.mp3")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, testaudio.BuildMP3WithAudio(title, "Artist", title, 1, testaudio.AudioWithSeed(byte(i+1))), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), "cover.jpg"), []byte("jpeg"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pids := map[string]string{}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %d (err %v), want two", len(items), err)
	}
	for _, it := range items {
		pids[it.Title] = string(it.PID)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := runCLIJSON(t, db, root, "rm", pids["One"], "--apply")
	if err != nil {
		t.Fatalf("rm: %v (%s)", err, out)
	}
	var rep struct {
		Data struct {
			DirsPruned int `json:"dirsPruned"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Data.DirsPruned != 1 {
		t.Fatalf("rm printed %q (err %v), want one folder pruned", out, err)
	}

	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", db, "--root", root + ":managed:waxbin-native", "rm", pids["Two"], "--permanent", "--apply"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("rm --permanent: %v", err)
	}
	if !strings.Contains(stdout.String(), "pruned 1 folder") {
		t.Fatalf("rm printed %q, want it to say one folder was pruned", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, "Two")); !os.IsNotExist(err) {
		t.Errorf("the emptied folder is still there (err %v)", err)
	}
}

// TestRmDryRunListsWhatGoesWithAFile: the dry run names each file's own sidecars under it,
// and says that a permanent delete also takes a managed folder's companions.
func TestRmDryRunListsWhatGoesWithAFile(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	album := filepath.Join(root, "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(album, "1.mp3"), testaudio.BuildMP3("One", "Artist", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(album, "1.lrc"), []byte("[00:00.00]la"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v), want one", len(items), err)
	}
	pid := string(items[0].PID)
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := runCLIJSON(t, db, root, "rm", pid)
	if err != nil {
		t.Fatalf("rm: %v (%s)", err, out)
	}
	var plan struct {
		Data struct {
			Actions []struct {
				Sidecars []string `json:"sidecars"`
			} `json:"actions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil || len(plan.Data.Actions) != 1 ||
		len(plan.Data.Actions[0].Sidecars) != 1 || plan.Data.Actions[0].Sidecars[0] != filepath.Join(album, "1.lrc") {
		t.Fatalf("rm plan printed %q (err %v), want the lyrics under the file", out, err)
	}

	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", db, "--root", root + ":managed:waxbin-native", "rm", pid, "--permanent"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("rm --permanent: %v", err)
	}
	if text := stdout.String(); !strings.Contains(text, "with  "+filepath.Join(album, "1.lrc")) || !strings.Contains(text, "companions") {
		t.Fatalf("rm --permanent dry run printed %q, want the lyrics listed and the folder companions noted", text)
	}
}

// TestRmDryRunNamesARipsTracks: deleting a cue rip's file archives every track it plays,
// so the dry run names them in both outputs, not only the track the action was planned
// for.
func TestRmDryRunNamesARipsTracks(t *testing.T) {
	t.Parallel()
	plan := &trash.Plan{Mode: model.DeleteTrash, Actions: []trash.Action{{
		ItemPID: "one", FilePID: "f", Src: filepath.Join(t.TempDir(), "album.wav"), Tracks: []model.PID{"one", "two"},
	}}}
	render := func(json bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := emitDeletePlan(cmd, &globals{jsonOut: json}, plan); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if text := render(false); !strings.Contains(text, "one, two") {
		t.Errorf("dry run does not name the rip's tracks:\n%s", text)
	}
	var env struct {
		Data struct {
			Actions []struct {
				Tracks []string `json:"tracks"`
			} `json:"actions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || len(env.Data.Actions) != 1 || len(env.Data.Actions[0].Tracks) != 2 {
		t.Errorf("dry run json = %+v (err %v), want the rip's two tracks", env, err)
	}
}
