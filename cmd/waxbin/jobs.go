package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newJobsCmd(g *globals) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List recent jobs and their state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			list, err := lib.Jobs(ctx(cmd), limit)
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, jobViews(list))
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PID\tKIND\tSCOPE\tTARGET\tSTATE\tPROGRESS\tMESSAGE")
			for _, j := range list {
				target := ""
				if j.TargetType != "" {
					target = j.TargetType + ":" + string(j.TargetPID)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%.0f%%\t%s\n",
					j.PID, j.Kind, j.Scope, target, j.State, j.Progress*100, j.Message)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "max jobs to list")
	cmd.AddCommand(newJobsCancelCmd(g))
	return cmd
}

func newJobsCancelCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <job-pid>",
		Short: "Cancel a job a server runs",
		Long: "Cancels a job a running server (waxbin serve) runs, waits for it to end, and says " +
			"how it ended. What the job did stands: a scan, analyze or enrichment picks up where " +
			"it stopped when run again, an organize keeps the moves it made (organize undo takes " +
			"them back), and a kind change or library removal finishes when asked again. With no " +
			"server, a job that has ended says so, and a running one is refused: it runs in another " +
			"process, which is where to interrupt it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid := model.PID(args[0])
			px, err := g.jobServer(cmd)
			if err != nil {
				return err
			}
			if px != nil {
				defer px.Close()
			}
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			job, err := lib.Job(ctx(cmd), pid)
			if err != nil {
				return err
			}
			// Only a job this command finds running is one it cancels.
			sent := false
			if job.State == model.JobRunning {
				if px == nil {
					return waxerr.New(waxerr.CodeConflict, "cli.jobs.cancel",
						"job "+string(pid)+" runs in another process with no server to reach it; interrupt it there")
				}
				if err := px.CancelJob(ctx(cmd), pid); err != nil {
					return err
				}
				sent = true
			}
			for err == nil && job.State == model.JobRunning {
				select {
				case <-ctx(cmd).Done():
					return waxerr.FromContext("cli.jobs.cancel", ctx(cmd).Err(), waxerr.CodeCanceled)
				case <-time.After(100 * time.Millisecond):
				}
				job, err = lib.Job(ctx(cmd), pid)
			}
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, jobViews([]*model.Job{job})[0])
			}
			fmt.Fprintln(out(cmd), cancelOutcome(job, sent))
			return nil
		},
	}
}

// cancelOutcome says how a job ended for `jobs cancel`: by the cancel this command sent, some
// other way after it was sent, or before the command found it.
func cancelOutcome(job *model.Job, sent bool) string {
	switch {
	case sent && job.State == model.JobCanceled && job.ErrorReason == jobs.ReasonCancelRequested:
		return fmt.Sprintf("Job %s (%s) canceled at %.0f%%", job.PID, job.Kind, job.Progress*100)
	case sent:
		return fmt.Sprintf("Job %s (%s) ended before the cancel took effect: %s", job.PID, job.Kind, job.State)
	}
	return fmt.Sprintf("Job %s (%s) had already ended: %s", job.PID, job.Kind, job.State)
}
