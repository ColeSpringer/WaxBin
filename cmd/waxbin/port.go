package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newBackupCmd(g *globals) *cobra.Command {
	var opts port.BackupOptions
	cmd := &cobra.Command{
		Use:   "backup <dest.db>",
		Short: "Write a full byte-copy backup of the catalog",
		Long: "Writes a self-contained copy of the catalog (the disaster-recovery " +
			"artifact). The copy contains the secret table; pass --redact-secrets to strip " +
			"credentials from a copy that will leave the host. --no-thumbnails leaves out the " +
			"generated thumbnails, which the catalog makes again on demand, so the copy is " +
			"smaller. Runs read-only, so it is safe alongside a writer.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if err := lib.Backup(ctx(cmd), args[0], opts); err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					Dest              string `json:"dest"`
					Redacted          bool   `json:"redacted"`
					ThumbnailsOmitted bool   `json:"thumbnailsOmitted"`
				}{args[0], opts.RedactSecrets, opts.OmitThumbnails})
			}
			fmt.Fprintf(out(cmd), "Backed up catalog to %s%s\n", args[0], backupNote(opts))
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.RedactSecrets, "redact-secrets", false, "strip the secret table from the backup copy")
	cmd.Flags().BoolVar(&opts.OmitThumbnails, "no-thumbnails", false, "leave the generated thumbnails out of the backup copy")
	return cmd
}

func backupNote(opts port.BackupOptions) string {
	note := " (contains secrets; protect like the catalog"
	if opts.RedactSecrets {
		note = " (secrets redacted"
	}
	if opts.OmitThumbnails {
		note += "; thumbnails left out"
	}
	return note + ")"
}

func newRestoreCmd(g *globals) *cobra.Command {
	var (
		force, allowAbsent bool
		root               string
	)
	cmd := &cobra.Command{
		Use:   "restore <backup.db>",
		Short: "Restore the catalog from a backup (optionally onto a new root)",
		Long: "Replaces the configured catalog with a validated backup. Refuses to " +
			"overwrite an existing catalog unless --force. With --root, re-points the single " +
			"library at a new path afterward (a portable restore onto a new machine/mount); " +
			"the path must be a folder, or one mounted later with --allow-absent. " +
			"Under a running server it takes the maintenance hand-off, so the server closes " +
			"its handles (which is what lets the file be replaced on Windows at all) and " +
			"reopens on the restored catalog, the command failing if it cannot; a server " +
			"running a job refuses until the job completes. Any other process holding the " +
			"catalog makes it refuse.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if g.readOnly {
				return waxerr.New(waxerr.CodeInvalid, "restore", "cannot restore in --read-only mode")
			}
			cfg, err := g.loadConfig(cmd)
			if err != nil {
				return err
			}
			// Before the hand-off, so a restore refused for its own arguments leaves a
			// running server and its consumers undisturbed.
			if err := port.CheckRestore(ctx(cmd), args[0], cfg.DBPath, force); err != nil {
				return err
			}
			if sock := advertisedSocket(cfg.DBPath); sock != "" {
				px, err := beginMaintenance(ctx(cmd), sock)
				if err != nil {
					if msg, ok := versionRefusal(err); ok {
						return protocolMismatch("restore", msg)
					}
					return err
				}
				if px != nil {
					g.maintConn = px
					fmt.Fprintln(errOut(cmd), "waxbin: server is running; took the lock via maintenance mode")
				}
			}
			// The server releases the lock before it answers, but a loaded filesystem can
			// show it held a moment longer, so a hand-off probes with the open's retry.
			probe := func() error { return ensureNoCatalogOwner(cfg.DBPath) }
			if g.maintConn != nil {
				err = retryConflict(ctx(cmd), probe)
			} else {
				err = probe()
			}
			if err != nil {
				return err
			}
			if err := port.Restore(ctx(cmd), args[0], cfg.DBPath, force); err != nil {
				return err
			}

			relocated := ""
			if root != "" {
				if relocated, err = relocateRestored(cmd, g, root, allowAbsent); err != nil {
					return err
				}
			}
			// Ended here rather than by cleanup, so a server that cannot reopen the
			// restored catalog is reported instead of a success over a closed server.
			if err := g.endMaintenance(); err != nil {
				return waxerr.Wrapf(waxerr.CodeIO, "restore", err,
					"the catalog was restored, but the server could not reopen it")
			}

			if g.jsonOut {
				return printJSON(cmd, struct {
					Restored  string `json:"restored"`
					Relocated string `json:"relocated,omitempty"`
				}{cfg.DBPath, relocated})
			}
			fmt.Fprintf(out(cmd), "Restored catalog to %s\n", cfg.DBPath)
			if relocated != "" {
				fmt.Fprintf(out(cmd), "Relocated library root to %s\n", relocated)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing catalog")
	cmd.Flags().StringVar(&root, "root", "", "re-point the single library at this new root path")
	cmd.Flags().BoolVar(&allowAbsent, "allow-absent", false, "let --root name a folder that does not exist yet (a drive mounted later)")
	return cmd
}

// relocateRestored re-points the restored catalog's single library at root, a relative
// one taken from the working directory, and returns the path it used. The library is
// closed before it returns, so a server taking the lock back finds it free.
func relocateRestored(cmd *cobra.Command, g *globals, root string, allowAbsent bool) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", waxerr.Wrapf(waxerr.CodeInvalid, "restore", err, "resolving %q", root)
	}
	lib, _, err := g.open(cmd)
	if err != nil {
		return "", err
	}
	defer lib.Close()
	libs, err := lib.Libraries(ctx(cmd))
	if err != nil {
		return "", err
	}
	if len(libs) != 1 {
		return "", waxerr.New(waxerr.CodeInvalid, "restore",
			"--root relocates a single library; the restored catalog has none or several")
	}
	var opts []waxbin.RootOption
	if allowAbsent {
		opts = append(opts, waxbin.AllowAbsent())
	}
	return abs, lib.RelocateRoot(ctx(cmd), libs[0].PID, abs, opts...)
}

func newExportCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "export [file.json]",
		Short: "Write a logical JSON export of metadata and user state (no secrets)",
		Long: "Exports catalog metadata, critical per-user playback state, and the listening " +
			"log as versioned JSON. It never contains secrets and is for inspection/portability; " +
			"the byte backup is the disaster-recovery path. Writes to the file or, if omitted, stdout.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			if len(args) == 0 && g.jsonOut {
				return printJSONStream(cmd, func(w io.Writer) error {
					_, err := lib.Export(ctx(cmd), w)
					return err
				})
			}
			var w io.Writer = out(cmd)
			var file *os.File
			if len(args) == 1 {
				file, err = os.Create(args[0])
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, "export", err)
				}
				defer file.Close()
				w = file
			}
			man, err := lib.Export(ctx(cmd), w)
			if err != nil {
				return err
			}
			if file != nil { // wrote to a file: summarize to stdout
				if g.jsonOut {
					return printJSON(cmd, man)
				}
				fmt.Fprintf(out(cmd), "Exported %d items, %d play states, %d play sessions (v%d) to %s\n",
					man.Items, man.PlayStates, man.PlaySessions, man.Version, args[0])
			}
			return nil
		},
	}
}

func newManifestCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "manifest",
		Short: "Print the export manifest (counts and versions) without the body",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			man, err := lib.Manifest(ctx(cmd))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, man)
			}
			w := out(cmd)
			fmt.Fprintf(w, "format:         %s\n", man.Format)
			fmt.Fprintf(w, "export version: %d\n", man.Version)
			fmt.Fprintf(w, "schema version: %d\n", man.SchemaVersion)
			fmt.Fprintf(w, "libraries:      %d\n", man.Libraries)
			fmt.Fprintf(w, "items:          %d\n", man.Items)
			fmt.Fprintf(w, "play states:    %d\n", man.PlayStates)
			fmt.Fprintf(w, "play sessions:  %d\n", man.PlaySessions)
			return nil
		},
	}
}

func newRebuildCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the catalog from the filesystem by scanning every root",
		Long: "Re-derives the catalog from the files on disk by scanning the configured " +
			"roots. This is catalog disaster-recovery: it restores structure, but public ids " +
			"are freshly minted unless WAXBIN_PID stamping was enabled. A full DB backup is " +
			"the disaster-recovery artifact.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			// A rebuild adopts WAXBIN_ITEM_PID tags to restore original item identities
			// where they were stamped (essence-first; unstamped items mint fresh).
			res, err := lib.Scan(ctx(cmd), waxbin.ScanRequest{AdoptStampedPIDs: true})
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, scanResultJSON(res))
			}
			fmt.Fprintf(out(cmd), "Rebuilt catalog: %d audio files, %d items created, %d updated, %d copies\n",
				res.Total.AudioFiles, res.Total.ItemsCreated, res.Total.ItemsUpdated, res.Total.Copies)
			for _, root := range unreachableRoots(res.Runs) {
				fmt.Fprintf(out(cmd), "  %s is not reachable, so nothing under it was rebuilt\n", root)
			}
			return nil
		},
	}
}

// scanResultJSON renders a scan/rebuild tally; mirrors the scan command's shape.
func scanResultJSON(res *waxbin.ScanResult) any {
	return struct {
		JobPID           string   `json:"jobPid"`
		AudioFiles       int      `json:"audioFiles"`
		ItemsCreated     int      `json:"itemsCreated"`
		ItemsUpdated     int      `json:"itemsUpdated"`
		Copies           int      `json:"copies"`
		Relinked         int      `json:"relinked"`
		Errored          int      `json:"errored"`
		WalkErrors       int      `json:"walkErrors"`
		RootUnreachable  bool     `json:"rootUnreachable,omitempty"`
		UnreachableRoots []string `json:"unreachableRoots,omitempty"`
	}{string(res.JobPID), res.Total.AudioFiles, res.Total.ItemsCreated,
		res.Total.ItemsUpdated, res.Total.Copies, res.Total.Relinked, res.Total.Errored, res.Total.WalkErrors,
		res.Total.RootUnreachable, unreachableRoots(res.Runs)}
}
