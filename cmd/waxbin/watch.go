package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newWatchCmd(g *globals) *cobra.Command {
	var (
		libraryPID   string
		interval     time.Duration
		fullInterval time.Duration
		live         bool
		writeSettle  time.Duration
		maxWatchDirs int
		doAnalyze    bool
		syncSources  bool
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Keep the catalog in sync with the filesystem (foreground)",
		Long: `Watch runs a foreground loop that rescans the library on a schedule (and,
with --live, on filesystem events), reconciling new, changed, and deleted files.

WATCH IS A FOREGROUND MODE. A read-write WaxBin holds an exclusive lock on the
catalog for its whole lifetime, so while watch runs every other mutating command
in another terminal (organize, analyze, enrich, import, scan --force) is
refused (read-only queries still work). Stop watch (Ctrl-C) to do manual work.

Scheduled rescans are the primary mechanism because filesystem events are
unreliable on WSL2, NFS, SMB, and bind mounts; --live is an optimization layered
on top, and a periodic full-content rescan (--full-interval) catches changes the
fast-path's size+mtime check misses.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			// Exit cleanly on SIGINT/SIGTERM: cancel the watch context so Run returns a
			// CodeCanceled error, then close (flush + release the lock).
			runCtx, stop := signal.NotifyContext(ctx(cmd), os.Interrupt, syscall.SIGTERM)
			defer stop()

			var onActivity func(waxbin.WatchActivity)
			if g.jsonOut {
				onActivity = func(a waxbin.WatchActivity) {
					_ = printJSON(cmd, struct {
						Event   string `json:"event"`
						Trigger string `json:"trigger"`
						Changed bool   `json:"changed"`
					}{"heartbeat", a.Trigger, a.Changed})
				}
			} else {
				fmt.Fprintf(out(cmd), "watching (interval %s, live=%v); Ctrl-C to stop\n", interval, live)
			}

			err = lib.Watch(runCtx, waxbin.WatchOptions{
				LibraryPID:         model.PID(libraryPID),
				Interval:           interval,
				FullRescanInterval: fullInterval,
				Live:               live,
				WriteSettle:        writeSettle,
				MaxWatchDirs:       maxWatchDirs,
				Analyze:            doAnalyze,
				SyncSources:        syncSources,
				OnActivity:         onActivity,
			})
			// A canceled watch is the normal Ctrl-C shutdown path: report it as canceled
			// (exit code 8) but without a usage dump.
			if waxerr.Is(err, waxerr.CodeCanceled) && !g.jsonOut {
				fmt.Fprintln(out(cmd), "watch stopped")
			}
			return err
		},
	}
	cmd.Flags().StringVar(&libraryPID, "library", "", "watch only the library with this pid")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "scheduled rescan cadence")
	cmd.Flags().DurationVar(&fullInterval, "full-interval", 6*time.Hour, "full-content rescan cadence (0 disables)")
	cmd.Flags().BoolVar(&live, "live", false, "also react to filesystem events (best-effort; falls back to scheduled)")
	cmd.Flags().DurationVar(&writeSettle, "write-settle", 2*time.Second, "quiet window before a live rescan fires")
	cmd.Flags().IntVar(&maxWatchDirs, "max-watch-dirs", 0, "cap live fsnotify watches (0 = unlimited; excess covered by scheduled rescans)")
	cmd.Flags().BoolVar(&doAnalyze, "analyze", false, "run the analyze pass after a rescan that changed something")
	cmd.Flags().BoolVar(&syncSources, "sync-sources", false, "sync podcast feeds and apply retention each cycle")
	return cmd
}
