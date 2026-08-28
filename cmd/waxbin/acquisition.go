package main

import (
	"fmt"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// acquisitionView is the JSON shape of an item's origin provenance. AcquiredAt is a
// string for the same reason playStateView's timestamps are: a unix-nanosecond count
// does not survive a JSON consumer that parses numbers as float64.
type acquisitionView struct {
	ItemPID         string `json:"itemPid"`
	SourceType      string `json:"sourceType"`
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
			"provider-native id, the provider that fetched it, and when. An item that was " +
			"scanned locally and carries no SOURCE_URL/SOURCE_ID tags has no row and reads " +
			"as source:local, which this reports rather than failing on.\n\n" +
			"Recording is merge-wise. A later acquisition event replaces the fields it " +
			"actually names and leaves the rest standing, so a bare event cannot erase a url " +
			"a tag established or downgrade an rss row to manual. `acquisition clear` is the " +
			"way to take a wrong row off.",
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
				// No row is the answer, not a failure: it is what source:local means, and
				// it is the state `acquisition clear` reports as a success. Failing here
				// would make the command disagree with its own help text.
				if g.jsonOut {
					return printJSON(cmd, acquisitionView{
						ItemPID: string(pid), SourceType: string(model.SourceLocal),
					})
				}
				fmt.Fprintf(out(cmd), "source type: %s (no acquisition provenance recorded)\n", model.SourceLocal)
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
	cmd.AddCommand(newAcquisitionClearCmd(g))
	return cmd
}

func newAcquisitionClearCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "clear <pid>",
		Short: "Remove an item's origin provenance, returning it to source:local",
		Long: "Deletes the acquisition row. Since recording only ever replaces a field with " +
			"a non-empty value, this is the one way to correct a row downward: clear it, then " +
			"record the right one. An item with no row is left alone rather than reported as " +
			"an error.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pid := model.PID(args[0])
			if err := lib.ClearAcquisition(ctx(cmd), pid); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "cleared acquisition provenance for %s; it now reads source:local\n", pid)
			return nil
		},
	}
}
