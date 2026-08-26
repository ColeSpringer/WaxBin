package main

import (
	"fmt"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newLockCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "lock <pid> <field>...",
		Short: "Lock item fields against enrichment and organize writes",
		Long: "Locks one or more of an item's fields so enrichment and organize leave them " +
			"alone. The \"art\" field is the item's front cover, and locking it here writes " +
			"the same row `waxbin art lock <pid>` does (its --type defaults to track). An " +
			"auxiliary slot is \"art.<role>\" (art.back, art.disc, art.booklet, " +
			"art.background), the row `waxbin art lock <pid> --role <role>` writes. That " +
			"overlap is deliberate: an item's art locks have one home.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			pid := model.PID(args[0])
			if err := m.Lock(ctx(cmd), pid, args[1:]...); err != nil {
				return err
			}
			return reportProvenance(cmd, g, m, pid)
		},
	}
}

func newUnlockCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "unlock <pid> <field>...",
		Short: "Clear locks on item fields",
		Long: "Clears locks on one or more of an item's fields. Unlocking \"art\" clears the " +
			"same row `waxbin art unlock <pid>` does, and \"art.<role>\" the row `--role " +
			"<role>` writes; an item's art locks have one home. For a non-item entity's " +
			"artwork, use `waxbin art unlock --type <entity>`.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			pid := model.PID(args[0])
			if err := m.Unlock(ctx(cmd), pid, args[1:]...); err != nil {
				return err
			}
			return reportProvenance(cmd, g, m, pid)
		},
	}
}

func newProvenanceCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "provenance <pid>",
		Short: "Show an item's field provenance (source + lock state)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			return reportProvenance(cmd, g, lib, model.PID(args[0]))
		},
	}
}

type provRow struct {
	Field     string `json:"field"`
	Source    string `json:"source"`
	Locked    bool   `json:"locked"`
	Value     string `json:"value,omitempty"`
	Provider  string `json:"provider,omitempty"`
	SourceURL string `json:"sourceUrl,omitempty"`
	Updated   int64  `json:"updatedAt,string"` // unix ns; a string, see playStateView
}

// reportProvenance prints an item's provenance rows, including the "art" row the
// store overlays from the item's own cover. An item with no curated or locked fields
// and no cover reports that every field is plain tag-sourced. It reads through a
// provenanceReader, so it works with a directly-opened Library or a proxied mutator
// alike.
func reportProvenance(cmd *cobra.Command, g *globals, lib provenanceReader, pid model.PID) error {
	rows, err := lib.Provenance(ctx(cmd), pid)
	if err != nil {
		return err
	}
	if g.jsonOut {
		out := make([]provRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, provRow{
				Field: r.Field, Source: string(r.Source), Locked: r.Locked,
				Value: r.Value, Provider: r.Provider, SourceURL: r.SourceURL, Updated: r.UpdatedAt,
			})
		}
		return printJSON(cmd, struct {
			PID  model.PID `json:"pid"`
			Rows []provRow `json:"provenance"`
		}{pid, out})
	}
	w := out(cmd)
	if len(rows) == 0 {
		fmt.Fprintf(w, "%s: all fields tag-sourced, unlocked, no cover of its own\n", pid)
		return nil
	}
	for _, r := range rows {
		lock := ""
		if r.Locked {
			lock = " [locked]"
		}
		src := attribution(r.Source, r.Provider, r.SourceURL)
		if r.Value != "" {
			fmt.Fprintf(w, "%-12s %s%s = %q\n", r.Field, src, lock, r.Value)
		} else {
			fmt.Fprintf(w, "%-12s %s%s\n", r.Field, src, lock)
		}
	}
	return nil
}

// The --source vocabularies the two stamped curation commands show in their help. They
// are display strings only; the gate is the model's own ValidForArt/ValidForLyrics.
const (
	artSourceList    = "tag|sidecar|user|enrichment|feed|generated"
	lyricsSourceList = "tag|sidecar|user|enrichment"
)

// keepLockUsage is the --keep-lock help text, phrased once for the seven commands that
// carry it. The --force note matters: a locked field refuses the write itself, so
// keeping a lock through a change means overriding it for that one write.
func keepLockUsage(what string) string {
	return "leave the stored lock on " + what + " as it is, neither locking nor unlocking" +
		" (a locked one still needs --force to write through)"
}

// lockChange maps the two lock flags onto the store's three-state instruction. Neither
// flag locks, which is the default every curation command has always had; --no-lock
// unlocks; --keep-lock leaves the stored lock alone. cobra refuses both at once, so the
// order here only settles a case that cannot arrive.
func lockChange(noLock, keepLock bool) model.LockChange {
	switch {
	case keepLock:
		return model.LockUnchanged
	case noLock:
		return model.LockOff
	default:
		return model.LockOn
	}
}

// parseAttribution builds a curation write's attribution from its --source, --provider
// and --source-url flags. Naming none of them is a hand edit, which is what an unstated
// origin means everywhere. ok is the model gate for the surface being written
// (Attribution.ValidForArt, Attribution.ValidForLyrics), so the CLI keeps no vocabulary
// of its own; it only splits the one refusal into messages naming the flag at fault.
func parseAttribution(op, source, provider, sourceURL string,
	ok func(model.Attribution) bool, list string) (model.Attribution, error) {
	attr := model.Attribution{
		Source: model.ProvenanceSource(source), Provider: provider, SourceURL: sourceURL,
	}
	if attr == (model.Attribution{}) {
		return attr, nil
	}
	attr = attr.OrUser()
	if ok(attr) {
		return attr, nil
	}
	switch {
	case attr.Source == model.SourceEnrichment && attr.Provider == "":
		return attr, waxerr.New(waxerr.CodeInvalid, op,
			"--source enrichment needs --provider naming the service that supplied it")
	case attr.Provider != "" && ok(model.Attribution{Source: attr.Source, SourceURL: attr.SourceURL}):
		return attr, waxerr.New(waxerr.CodeInvalid, op,
			"--provider names an enrichment service; --source "+string(attr.Source)+" has none")
	default:
		return attr, waxerr.New(waxerr.CodeInvalid, op,
			fmt.Sprintf("unknown --source %q; valid: %s", source, list))
	}
}
