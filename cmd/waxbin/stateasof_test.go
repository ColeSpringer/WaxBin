package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/port"
)

// runStateCLI executes the state command through the root command with JSON output
// and returns what it printed.
func runStateCLI(t *testing.T, db, root string, args ...string) (string, error) {
	t.Helper()
	return runCLIJSON(t, db, root, append([]string{"state"}, args...)...)
}

// runCLIJSON executes one command through the root command against a catalog, with
// JSON output, and returns what it printed.
func runCLIJSON(t *testing.T, db, root string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native", "--json"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

// TestStateSetAsOfCoversPlaybackWrites drives the flag combinations --as-of reaches
// through a real catalog: a recorded play, a count reset composed with a play at one
// recorded time, and a recorded checkpoint older than the play.
func TestStateSetAsOfCoversPlaybackWrites(t *testing.T) {
	db, root, pid := creditCLIFixture(t)
	type view struct {
		PositionMS     int64  `json:"positionMs"`
		Played         bool   `json:"played"`
		Finished       bool   `json:"finished"`
		PlayCount      int    `json:"playCount"`
		LastPlayedAt   int64  `json:"lastPlayedAt,string"`
		LastProgressAt int64  `json:"lastProgressAt,string"`
		SessionPID     string `json:"sessionPid"`
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
	if got.PositionMS != 5000 || got.LastProgressAt != again.UnixNano() || got.SessionPID != "" {
		t.Fatalf("older checkpoint = %+v, want position 5000 with the stamp kept at %d and no session", got, again.UnixNano())
	}

	// A play with --session also logs a listening session at the recorded time, which
	// is what fills that year's review; the plays above logged none. The session's
	// pid is the one thing the play state cannot show, so the command reports it.
	got = set("--played", "--session", "240000", "--client", "lastfm", "--as-of", again.Format(time.RFC3339))
	if got.PlayCount != 2 || got.SessionPID == "" {
		t.Fatalf("play with a session = %+v, want the count at 2 and the session pid", got)
	}
	// The session carries the client the import named, which the export shows.
	doc, err := runCLIJSON(t, db, root, "export")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	snap, err := port.ReadSnapshot(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("export printed %q: %v", doc, err)
	}
	if len(snap.PlaySessions) != 1 || snap.PlaySessions[0].PID != got.SessionPID || snap.PlaySessions[0].Client != "lastfm" {
		t.Fatalf("exported sessions = %+v, want the one logged under client lastfm", snap.PlaySessions)
	}
	out, err := runCLIJSON(t, db, root, "stats", "--year", "2020")
	if err != nil {
		t.Fatalf("stats --year: %v", err)
	}
	var review struct {
		Data struct {
			Sessions      int   `json:"sessions"`
			MinutesPlayed int64 `json:"minutesPlayed"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &review); err != nil {
		t.Fatalf("stats --year printed %q: %v", out, err)
	}
	if review.Data.Sessions != 1 || review.Data.MinutesPlayed != 4 {
		t.Fatalf("2020 in review = %+v, want 1 session of 4 minutes", review.Data)
	}
}
