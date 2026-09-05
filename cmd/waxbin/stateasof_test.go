package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

// runStateCLI executes the state command through the root command with JSON output
// and returns what it printed.
func runStateCLI(t *testing.T, db, root string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native", "--json", "state"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

// TestStateSetAsOfCoversPlaybackWrites drives the flag combinations --as-of reaches
// through a real catalog: a recorded play, a count reset composed with a play at one
// recorded time, and a recorded checkpoint older than the play.
func TestStateSetAsOfCoversPlaybackWrites(t *testing.T) {
	db, root, pid := creditCLIFixture(t)
	type view struct {
		PositionMS     int64 `json:"positionMs"`
		Played         bool  `json:"played"`
		Finished       bool  `json:"finished"`
		PlayCount      int   `json:"playCount"`
		LastPlayedAt   int64 `json:"lastPlayedAt,string"`
		LastProgressAt int64 `json:"lastProgressAt,string"`
	}
	set := func(args ...string) view {
		t.Helper()
		out, err := runStateCLI(t, db, root, append([]string{"set", string(pid)}, args...)...)
		if err != nil {
			t.Fatalf("state set %v: %v", args, err)
		}
		var env struct {
			Data view `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("state set %v printed %q: %v", args, out, err)
		}
		return env.Data
	}

	play := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	got := set("--finished", "--as-of", play.Format(time.RFC3339))
	if !got.Played || !got.Finished || got.PlayCount != 1 || got.LastPlayedAt != play.UnixNano() {
		t.Fatalf("recorded play = %+v, want one finished play at %d", got, play.UnixNano())
	}

	// A reset composed with a play at one recorded time lands the play: the count
	// is exactly one and the flags are the play's.
	again := play.Add(24 * time.Hour)
	got = set("--reset-count", "--played", "--as-of", again.Format(time.RFC3339))
	if !got.Played || got.Finished || got.PlayCount != 1 || got.LastPlayedAt != again.UnixNano() {
		t.Fatalf("reset composed with a play = %+v, want played, unfinished, count 1 at %d", got, again.UnixNano())
	}

	// A checkpoint recorded before that play keeps its position but not its stamp.
	earlier := play.Add(-24 * time.Hour)
	got = set("--position", "5000", "--as-of", earlier.Format(time.RFC3339))
	if got.PositionMS != 5000 || got.LastProgressAt != again.UnixNano() {
		t.Fatalf("older checkpoint = %+v, want position 5000 with the stamp kept at %d", got, again.UnixNano())
	}
}
