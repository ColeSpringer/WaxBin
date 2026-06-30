package main

import (
	"fmt"
	"io"
	"os"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newBackupCmd(g *globals) *cobra.Command {
	var redact bool
	cmd := &cobra.Command{
		Use:   "backup <dest.db>",
		Short: "Write a full byte-copy backup of the catalog",
		Long: "Writes a self-contained copy of the catalog (the disaster-recovery " +
			"artifact). The copy contains the secret table; pass --redact-secrets to strip " +
			"credentials from a copy that will leave the host. Runs read-only, so it is safe " +
			"alongside a writer.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if err := lib.Backup(ctx(cmd), args[0], redact); err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					Dest     string `json:"dest"`
					Redacted bool   `json:"redacted"`
				}{args[0], redact})
			}
			fmt.Fprintf(out(cmd), "Backed up catalog to %s%s\n", args[0], redactNote(redact))
			return nil
		},
	}
	cmd.Flags().BoolVar(&redact, "redact-secrets", false, "strip the secret table from the backup copy")
	return cmd
}

func redactNote(redact bool) string {
	if redact {
		return " (secrets redacted)"
	}
	return " (contains secrets; protect like the catalog)"
}

func newRestoreCmd(g *globals) *cobra.Command {
	var (
		force bool
		root  string
	)
	cmd := &cobra.Command{
		Use:   "restore <backup.db>",
		Short: "Restore the catalog from a backup (optionally onto a new root)",
		Long: "Replaces the configured catalog with a validated backup. Refuses to " +
			"overwrite an existing catalog unless --force. With --root, re-points the single " +
			"library at a new path afterward (a portable restore onto a new machine/mount). " +
			"Ensure no other process has the catalog open.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if g.readOnly {
				return waxerr.New(waxerr.CodeInvalid, "restore", "cannot restore in --read-only mode")
			}
			cfg, err := g.loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := port.Restore(ctx(cmd), args[0], cfg.DBPath, force); err != nil {
				return err
			}

			relocated := ""
			if root != "" {
				lib, _, err := g.open(cmd)
				if err != nil {
					return err
				}
				defer lib.Close()
				libs, err := lib.Libraries(ctx(cmd))
				if err != nil {
					return err
				}
				if len(libs) != 1 {
					return waxerr.New(waxerr.CodeInvalid, "restore",
						"--root relocates a single library; the restored catalog has none or several")
				}
				if err := lib.RelocateRoot(ctx(cmd), libs[0].PID, root); err != nil {
					return err
				}
				relocated = root
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
	return cmd
}

func newExportCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "export [file.json]",
		Short: "Write a logical JSON export of metadata and user state (no secrets)",
		Long: "Exports catalog metadata plus critical per-user playback state as versioned " +
			"JSON. It never contains secrets and is for inspection/portability; the byte " +
			"backup is the disaster-recovery path. Writes to the file or, if omitted, stdout.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

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
				fmt.Fprintf(out(cmd), "Exported %d items, %d play states (v%d) to %s\n",
					man.Items, man.PlayStates, man.Version, args[0])
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
			man, err := lib.Export(ctx(cmd), io.Discard)
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
			res, err := lib.Scan(ctx(cmd), waxbin.ScanRequest{})
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, scanResultJSON(res))
			}
			fmt.Fprintf(out(cmd), "Rebuilt catalog: %d audio files, %d items created, %d updated\n",
				res.Total.AudioFiles, res.Total.ItemsCreated, res.Total.ItemsUpdated)
			return nil
		},
	}
}

// scanResultJSON renders a scan/rebuild tally; mirrors the scan command's shape.
func scanResultJSON(res *waxbin.ScanResult) any {
	return struct {
		JobPID       string `json:"jobPid"`
		AudioFiles   int    `json:"audioFiles"`
		ItemsCreated int    `json:"itemsCreated"`
		ItemsUpdated int    `json:"itemsUpdated"`
		Relinked     int    `json:"relinked"`
		Errored      int    `json:"errored"`
	}{string(res.JobPID), res.Total.AudioFiles, res.Total.ItemsCreated,
		res.Total.ItemsUpdated, res.Total.Relinked, res.Total.Errored}
}
