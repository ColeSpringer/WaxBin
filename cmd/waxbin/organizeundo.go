package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/spf13/cobra"
)

func newOrganizeUndoCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "undo <job-pid>",
		Short: "Move back the files an organize moved",
		Long: "Moves each file an organize job moved back to where it found it, its own " +
			"sidecars with it, and puts back the covers and other folder companions the job " +
			"carried as its journal records them (a copy the job made goes, while it still " +
			"matches its original), under a job of its own that `organize history` lists and " +
			"that can be undone in turn. Tags the organize wrote stay. A file already where the " +
			"job found it is in place; one moved again since, one the catalog no longer holds, " +
			"one whose old place another file now holds and one gone from disk are held, and the " +
			"report says why. `organize history` names the job pids.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			job := model.PID(args[0])
			px, err := g.jobServer(cmd)
			if err != nil {
				return err
			}
			if px != nil {
				defer px.Close()
				undo, err := px.RunOrganizeUndo(ctx(cmd), job)
				if err != nil {
					return err
				}
				done, err := g.tailJob(cmd, undo)
				if err != nil {
					return err
				}
				var rr organize.RunResult
				if err := unmarshalJobResult(done, &rr); err != nil {
					return err
				}
				return emitMoves(cmd, g, "Undid organize "+string(job), "", &rr.Report)
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			rep, err := lib.UndoOrganize(ctx(cmd), job)
			if err != nil {
				return err
			}
			return emitMoves(cmd, g, "Undid organize "+string(job), "", rep)
		},
	}
}

func newOrganizeHistoryCmd(g *globals) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List the organize jobs whose moves the journal holds",
		Long: "Lists the organize and undo jobs the organize journal holds moves for, newest " +
			"first, with how many of their moves committed, rolled back, or wait on a crash " +
			"recovery. A job listed here can be undone with `organize undo`; `db vacuum " +
			"--journal-days` prunes old ones.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			batches, err := lib.OrganizeHistory(ctx(cmd), limit)
			if err != nil {
				return err
			}
			if g.jsonOut {
				type batchJSON struct {
					JobPID     string `json:"jobPid"`
					Kind       string `json:"kind"`
					State      string `json:"state"`
					StartedAt  int64  `json:"startedAt,string"`
					Committed  int    `json:"committed"`
					RolledBack int    `json:"rolledBack"`
					Planned    int    `json:"planned"`
				}
				rows := make([]batchJSON, 0, len(batches))
				for _, b := range batches {
					rows = append(rows, batchJSON{string(b.JobPID), b.Kind, string(b.State), b.StartedAt, b.Committed, b.RolledBack, b.Planned})
				}
				return printJSON(cmd, rows)
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "JOB\tKIND\tSTATE\tSTARTED\tMOVED\tROLLED BACK\tPENDING")
			for _, b := range batches {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\n", b.JobPID, b.Kind, b.State,
					time.Unix(0, b.StartedAt).Local().Format("2006-01-02 15:04:05"), b.Committed, b.RolledBack, b.Planned)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "(%s)\n", plural(len(batches), "organize job"))
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "list at most this many jobs (0 = all)")
	return cmd
}
