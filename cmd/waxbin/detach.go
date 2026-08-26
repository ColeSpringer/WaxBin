package main

import (
	"errors"
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newDetachCmd(g *globals) *cobra.Command {
	var writeBack bool
	cmd := &cobra.Command{
		Use:   "detach PID",
		Short: "Move one track off a MusicBrainz-identified album",
		Long: "Pulls a single track off the album a MusicBrainz id ties it to, its own release id " +
			"or the release group above it, and onto the album its own tags and folder imply, " +
			"leaving that album and its other members alone. Use it when one file was tagged with " +
			"the wrong release; to disown the whole thing instead, clear that id with " +
			"`waxbin entity edit album <pid> --set mbid=` or the release_group equivalent, which " +
			"is also what an album's last member is pointed at.\n\n" +
			"A member's linkage comes from the tags on its file, so a catalog-only detach lasts " +
			"until the next scan re-resolves the item: that is its next retag, move, or content " +
			"change, and not every scan (a byte-identical `scan --force` re-resolves nothing). " +
			"--write-back strips MUSICBRAINZ_ALBUMID and MUSICBRAINZ_RELEASEGROUPID from the " +
			"file, which makes the detach durable.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()

			pid := model.PID(args[0])
			rep, err := m.Detach(ctx(cmd), pid, waxbin.DetachOptions{WriteBack: writeBack})
			// Classified before surfacing, which reports the failures and returns nil.
			warning := detachDurabilityWarning(writeBack, err)
			// The catalog change stands even when the tag strip partially failed, so
			// report those as warnings and still print the report.
			if err := surfaceWriteBack(cmd, err); err != nil {
				return err
			}
			if warning != "" {
				fmt.Fprintln(errOut(cmd), warning)
			}
			if g.jsonOut {
				return printJSON(cmd, detachView{
					ItemPID: string(rep.ItemPID), OldAlbumPID: string(rep.OldAlbumPID),
					NewAlbumPID: string(rep.NewAlbumPID), NewReleaseGroupPID: string(rep.NewReleaseGroupPID),
				})
			}
			if rep.NewAlbumPID == "" {
				// The member grouped on the release id alone, so there is no heuristic
				// chain for it to land on, exactly as a scan of the stripped file finds.
				fmt.Fprintf(out(cmd), "detached %s from album %s; it now groups on nothing\n",
					rep.ItemPID, rep.OldAlbumPID)
				return nil
			}
			fmt.Fprintf(out(cmd), "detached %s from album %s onto album %s (release group %s)\n",
				rep.ItemPID, rep.OldAlbumPID, rep.NewAlbumPID, rep.NewReleaseGroupPID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&writeBack, "write-back", false,
		"also strip the MusicBrainz release tags from the file, so a rescan cannot re-adopt it")
	return cmd
}

// detachDurabilityWarning returns the caveat to print after a detach, or "" when there is
// none. The linkage lives in the file's tags, so the catalog change lasts only until a
// scan re-resolves the item unless those tags came off.
func detachDurabilityWarning(writeBack bool, err error) string {
	if !writeBack {
		return "warning: the file's tags still name the release, so the linkage returns on " +
			"its next retag, move, or content change; pass --write-back to strip them"
	}
	// A refused or failed strip is reported as a warning and leaves a nil error behind,
	// so without this the caveat would be silent in the one case it matters most.
	var wbErr *waxbin.WriteBackError
	if errors.As(err, &wbErr) {
		return "warning: the strip did not land, so the file still names the release and " +
			"the detach reverts on its next retag, move, or content change"
	}
	return ""
}

// detachView is the JSON shape for a detach: the album left and the chain landed on.
type detachView struct {
	ItemPID            string `json:"itemPid"`
	OldAlbumPID        string `json:"oldAlbumPid"`
	NewAlbumPID        string `json:"newAlbumPid,omitempty"`
	NewReleaseGroupPID string `json:"newReleaseGroupPid,omitempty"`
}
