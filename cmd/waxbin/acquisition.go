package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// itemSourceList is the vocabulary `acquisition set --type` accepts. The store's
// ValidItemSource is what actually refuses a bad one; this only names them in the help,
// so it is spelled out rather than derived. SourceLocal is absent for the reason
// ValidItemSource gives: on an item it is the absence of a row, not a value.
const itemSourceList = "rss, youtube, manual"

// acquisitionView is the JSON shape of an item's origin provenance. AcquiredAt is a
// string for the same reason playStateView's timestamps are: a unix-nanosecond count
// does not survive a JSON consumer that parses numbers as float64.
//
// Inherited marks the answer for an item with no acquisition row, whose source type is
// the one it reads through its show or the local default. Without it a feed episode with
// no row of its own would be indistinguishable from one carrying a curated rss row, and
// only the second is something `acquisition clear` can remove.
type acquisitionView struct {
	ItemPID         string `json:"itemPid"`
	SourceType      string `json:"sourceType"`
	Inherited       bool   `json:"inherited,omitempty"`
	SourceURL       string `json:"sourceUrl,omitempty"`
	SourceID        string `json:"sourceId,omitempty"`
	Provider        string `json:"provider,omitempty"`
	ProviderVersion string `json:"providerVersion,omitempty"`
	AcquiredAt      int64  `json:"acquiredAt,string"`
	OptionsJSON     string `json:"options,omitempty"`
}

// newAcquisitionCmd builds `waxbin acquisition`. It is a command of its own rather than
// a child of `provenance`, which is field-level provenance taking exactly one pid: a
// `provenance clear <pid>` would both misfile the verb and collide with a pid literally
// named "clear".
func newAcquisitionCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "acquisition <pid>",
		Short: "Show an item's origin provenance (how and where it entered the library)",
		Long: "Prints the acquisition row for an item: the source type, the origin URL and " +
			"provider-native id, the provider that fetched it, and when. An item with no " +
			"acquisition provenance of its own is reported rather than failed on: it falls " +
			"back to the source it reads without a row, which is its show's type for a " +
			"podcast episode and local for everything else.\n\n" +
			"Recording is merge-wise. A later acquisition event replaces the fields it " +
			"actually names and leaves the rest standing, so a bare event cannot erase a url " +
			"a tag established or downgrade an rss row to manual. `acquisition set` is the " +
			"correction that can lower a field, and `acquisition clear` takes a wrong row " +
			"off outright.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pid := model.PID(args[0])
			a, err := lib.Acquisition(ctx(cmd), pid)
			if waxerr.Is(err, waxerr.CodeNotFound) {
				// No row is the answer, not a failure: it is the state `acquisition clear`
				// reports as a success. Failing here would make the command disagree with its
				// own help text. An unknown pid reaches the same CodeNotFound, though, so the
				// item read is what separates the two: without it a typo would be reported as
				// a locally scanned track and exit 0.
				v, verr := lib.Get(ctx(cmd), pid)
				if verr != nil {
					return verr
				}
				// The source is read from the item rather than assumed to be local, since an
				// episode of a feed show reads its show's type with no row of its own.
				if g.jsonOut {
					return printJSON(cmd, acquisitionView{
						ItemPID: string(pid), SourceType: string(v.Source), Inherited: true,
					})
				}
				fmt.Fprintf(out(cmd), "source type: %s (no acquisition provenance of its own)\n", v.Source)
				return nil
			}
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, acquisitionView{
					ItemPID: string(pid), SourceType: string(a.SourceType), SourceURL: a.SourceURL,
					SourceID: a.SourceID, Provider: a.Provider, ProviderVersion: a.ProviderVersion,
					AcquiredAt: a.AcquiredAt, OptionsJSON: a.OptionsJSON,
				})
			}
			w := out(cmd)
			fmt.Fprintf(w, "source type: %s\n", a.SourceType)
			for _, row := range [][2]string{
				{"source url", a.SourceURL},
				{"source id", a.SourceID},
				{"provider", a.Provider},
				{"provider version", a.ProviderVersion},
				{"options", a.OptionsJSON},
			} {
				if row[1] != "" {
					fmt.Fprintf(w, "%s: %s\n", row[0], row[1])
				}
			}
			fmt.Fprintf(w, "acquired at: %s\n", time.Unix(0, a.AcquiredAt).Format(time.RFC3339))
			return nil
		},
	}
	cmd.AddCommand(newAcquisitionSetCmd(g), newAcquisitionClearCmd(g))
	return cmd
}

// newAcquisitionSetCmd builds `waxbin acquisition set`, the corrective write. Recording
// is merge-wise everywhere else, so this is the one surface where an empty value is a
// claim and a wrong row can be lowered rather than only written over.
func newAcquisitionSetCmd(g *globals) *cobra.Command {
	var sourceType, sourceURL, sourceID, provider, providerVersion, options, acquiredAt string
	var noLock, keepLock, force, writeBack bool
	cmd := &cobra.Command{
		Use:   "set <pid>",
		Short: "Record the right origin provenance on an item, replacing whatever stands",
		Long: "Replaces an item's acquisition row with exactly what is given. Every field is " +
			"written as passed, an omitted one included, so this is the way to empty a wrong " +
			"url, id or provider and to lower a wrong source type. Recording from an import " +
			"or a scan is merge-wise and never lowers a field, which is why a correction " +
			"needs its own verb.\n\n" +
			"--acquired-at moves the historical acquisition time; omitting it keeps the stamp " +
			"the row already carries. The type is required and is one of " + itemSourceList +
			": local is the absence of a row rather than a value, so `acquisition clear` is " +
			"the way to it.\n\n" +
			"The acquisition field is locked by default, so a later import or scan leaves the " +
			"correction alone. --write-back also copies the url and id onto the backing " +
			"file(s), so a rebuild or another tool reading the tags sees the correction " +
			"rather than the original. It is not a substitute for the lock: the source type " +
			"and provider have no tag to carry them, so a file re-derived on its own reads " +
			"manual with no provider whatever this recorded. An episode's file is refused, " +
			"since retention re-fetches it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := model.AcquisitionInput{
				SourceType: model.SourceType(sourceType), SourceURL: sourceURL, SourceID: sourceID,
				Provider: provider, ProviderVersion: providerVersion, OptionsJSON: options,
			}
			// Nothing downstream parses the options blob, so this is the only place a
			// hand-typed one is checked at all. Storing malformed text under a name that
			// says JSON would hand the next reader a parse failure instead of a refusal.
			if options != "" && !json.Valid([]byte(options)) {
				return waxerr.New(waxerr.CodeInvalid, "acquisition set", "--options is not valid JSON")
			}
			// parseAsOf is the repo's one timestamp parser, and both of its guards matter
			// here: UnixNano wraps silently outside about 1678..2262, and the zero it would
			// produce for the epoch is the store's "stamp it for me" sentinel, so an
			// out-of-range date would move the stamp somewhere absurd and 1970 would quietly
			// mean "leave it alone".
			at, err := parseAsOf(acquiredAt)
			if err != nil {
				return err
			}
			if at != nil {
				if *at == 0 {
					return waxerr.New(waxerr.CodeInvalid, "acquisition set",
						"--acquired-at cannot be the unix epoch, which is the sentinel for keeping the stored stamp")
				}
				in.AcquiredAt = *at
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			pid := model.PID(args[0])
			if err := surfaceWriteBack(cmd, m.SetAcquisition(ctx(cmd), pid, in,
				waxbin.AcquisitionEditOptions{
					Lock: lockChange(noLock, keepLock), Force: force, WriteBack: writeBack,
				})); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "recorded acquisition provenance for %s; it now reads source:%s\n",
				pid, in.SourceType)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&sourceType, "type", "", "source type: "+itemSourceList+" (required)")
	f.StringVar(&sourceURL, "url", "", "origin URL the item was acquired from")
	f.StringVar(&sourceID, "id", "", "provider-native id, meaningful only with --provider")
	f.StringVar(&provider, "provider", "", "provider that acquired the item")
	f.StringVar(&providerVersion, "provider-version", "", "version of the acquiring provider")
	f.StringVar(&options, "options", "", "provider options as a JSON document")
	f.StringVar(&acquiredAt, "acquired-at", "", "historical acquisition time (unix ns or RFC3339); omit to keep the stored stamp")
	f.BoolVar(&noLock, "no-lock", false, "clear the stored lock on the acquisition field")
	f.BoolVar(&keepLock, "keep-lock", false, keepLockUsage("the acquisition field"))
	cmd.MarkFlagsMutuallyExclusive("no-lock", "keep-lock")
	f.BoolVar(&force, "force", false, "override a locked acquisition field")
	f.BoolVar(&writeBack, "write-back", false, "also write the acquisition tags to the backing file(s) on disk")
	_ = cmd.MarkFlagRequired("type")
	return cmd
}

func newAcquisitionClearCmd(g *globals) *cobra.Command {
	var noLock, force, writeBack bool
	cmd := &cobra.Command{
		Use:   "clear <pid>",
		Short: "Remove an item's origin provenance, returning it to the source it reads without one",
		Long: "Deletes the acquisition row, so the item falls back to the source it reads " +
			"without one: its show's type for a podcast episode, and local for everything " +
			"else. An item that already reads that way is left alone rather than reported as " +
			"an error, so a batch correction need not check first. `acquisition set` is the " +
			"way to record a corrected row instead of removing it.\n\n" +
			"The acquisition field is locked by default, and that is what makes the clear " +
			"stick. The file still carries the SOURCE_URL and SOURCE_ID tags the row was " +
			"derived from, so without the lock the next full scan records the same wrong " +
			"origin again. --no-lock opts out; --write-back strips the tags from the backing " +
			"file(s) as well, which keeps the clear with no lock holding it. An episode's " +
			"file is refused, since retention re-fetches it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			pid := model.PID(args[0])
			// Neither flag is the clear's default lock, which the store spells LockUnchanged
			// and resolves to a lock of its own. lockChange's default is LockOn, the same
			// thing said directly, so the two verbs of this command read alike.
			if err := surfaceWriteBack(cmd, m.ClearAcquisition(ctx(cmd), pid,
				waxbin.AcquisitionEditOptions{
					Lock: lockChange(noLock, false), Force: force, WriteBack: writeBack,
				})); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "cleared acquisition provenance for %s; it now reads the source it has without a row of its own\n", pid)
			if w := clearDurabilityWarning(noLock, writeBack); w != "" {
				fmt.Fprintln(errOut(cmd), w)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&noLock, "no-lock", false,
		"do not lock the acquisition field, so a later scan may re-derive it from the file's tags")
	f.BoolVar(&force, "force", false, "override a locked acquisition field")
	f.BoolVar(&writeBack, "write-back", false, "also strip the acquisition tags from the backing file(s) on disk")
	return cmd
}

// clearDurabilityWarning names the one combination that leaves the clear undone by the
// next scan: the lock declined and the file's own tags left in place. It is the shape
// detach warns about, so it warns the same way rather than printing a plain success over
// a change that will not survive. Either half alone is enough, so neither warns.
func clearDurabilityWarning(noLock, writeBack bool) string {
	if !noLock || writeBack {
		return ""
	}
	return "warning: --no-lock without --write-back leaves the file's SOURCE_URL/SOURCE_ID in place, " +
		"so the next full scan will record the same origin again"
}
