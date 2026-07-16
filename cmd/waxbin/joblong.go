package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// jobServer returns a proxy client to a running server for this catalog, or nil
// when none is advertised (so the command runs the job directly). A read-only
// invocation never submits, and a config error is surfaced.
//
// Long jobs (scan/analyze/enrich/organize) submit to the server rather than
// proxying each mutation: the server runs the whole pass in its own process and
// stays available, and the client follows progress through the read-only job row.
func (g *globals) jobServer(cmd *cobra.Command) (*proxy.Client, error) {
	if g.readOnly {
		return nil, nil
	}
	cfg, err := g.loadConfig(cmd)
	if err != nil {
		return nil, err
	}
	return dialServer(cfg.DBPath), nil
}

// tailJob follows a server-run job to completion through a read-only catalog
// handle (WAL lets it read the job row while the server holds the write lock),
// printing progress to stderr, and returns the terminal job. A failed, crashed, or
// canceled job returns an error carrying the job's recorded message. Interrupting
// the CLI stops watching but leaves the job running on the server.
func (g *globals) tailJob(cmd *cobra.Command, jobPID model.PID) (*model.Job, error) {
	lib, _, err := g.openRead(cmd)
	if err != nil {
		return nil, err
	}
	defer lib.Close()

	var lastMsg string
	printed := false
	lastPct := -1.0
	for {
		job, err := lib.Job(ctx(cmd), jobPID)
		if err != nil {
			return nil, err
		}
		if !g.jsonOut && job.Message != "" && job.Message != lastMsg {
			fmt.Fprintf(errOut(cmd), "\r[%3.0f%%] %-64s", job.Progress*100, truncate(job.Message, 64))
			lastMsg, lastPct, printed = job.Message, job.Progress, true
		}
		switch job.State {
		case model.JobDone:
			// finalize stamps progress to 1.0 but does not change the message, so the
			// last in-loop line can be stuck at an intermediate percent. Ensure the final
			// display shows 100%.
			finishProgressLine(cmd, g, job, printed, lastPct)
			return job, nil
		case model.JobFailed, model.JobCrashed, model.JobCanceled:
			if !g.jsonOut && printed {
				fmt.Fprintln(errOut(cmd))
			}
			return job, jobError(job)
		}
		select {
		case <-ctx(cmd).Done():
			return nil, waxerr.FromContext("cli.tailJob", ctx(cmd).Err(), waxerr.CodeCanceled)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// finishProgressLine closes out the progress display for a completed job so it never
// ends stuck at an intermediate percent. If the last line already reached 100%, it
// just terminates the carriage-return line with a newline; otherwise it repaints a
// final 100% line. It is a no-op for --json, and for a job that finished before
// emitting any progress (nothing to close).
func finishProgressLine(cmd *cobra.Command, g *globals, job *model.Job, printed bool, lastPct float64) {
	if g.jsonOut {
		return
	}
	if printed && lastPct >= 1.0 {
		fmt.Fprintln(errOut(cmd))
		return
	}
	if !printed && job.Message == "" {
		return
	}
	fmt.Fprintf(errOut(cmd), "\r[100%%] %-64s\n", truncate(job.Message, 64))
}

// jobError turns a non-successful terminal job into an error. The original error
// class is not carried on the job row, so a failed job maps to CodeInternal (its
// message is preserved) and a canceled one to CodeCanceled.
func jobError(job *model.Job) error {
	msg := job.Error
	if msg == "" {
		msg = "job " + string(job.State)
	}
	code := waxerr.CodeInternal
	if job.State == model.JobCanceled {
		code = waxerr.CodeCanceled
	}
	return waxerr.New(code, "job."+job.Kind, msg)
}

// unmarshalJobResult decodes a finished job's JSON Result summary into v. An empty
// result (a job that recorded none) leaves v at its zero value.
func unmarshalJobResult(job *model.Job, v any) error {
	if job.Result == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(job.Result), v); err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "cli.jobResult", err)
	}
	return nil
}

// truncate shortens s to at most n display runes, appending an ellipsis when it
// cuts. It counts and slices by rune, not byte, so a multibyte character (a
// non-ASCII filename or artist in a progress message) is never split into an
// invalid UTF-8 fragment.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}
