package main

import (
	"fmt"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newShowCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "show <pid>",
		Short: "Show one item by public id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			v, err := lib.Get(ctx(cmd), model.PID(args[0]))
			if err != nil {
				return err
			}

			// ReplayGain is optional (only after analyze); absent loudness is not an
			// error, so a NotFound is simply omitted.
			ld, ldErr := lib.Loudness(ctx(cmd), v.PID)
			if ldErr != nil && !waxerr.Is(ldErr, waxerr.CodeNotFound) {
				return ldErr
			}

			if g.jsonOut {
				return printJSON(cmd, showView{Item: toItemView(v), ReplayGain: loudnessView(ld)})
			}
			w := out(cmd)
			fmt.Fprintf(w, "pid:          %s\n", v.PID)
			fmt.Fprintf(w, "kind/state:   %s / %s\n", v.Kind, v.State)
			fmt.Fprintf(w, "title:        %s\n", v.Title)
			fmt.Fprintf(w, "artist:       %s\n", v.Artist)
			fmt.Fprintf(w, "album artist: %s\n", v.AlbumArtist)
			fmt.Fprintf(w, "album:        %s\n", v.Album)
			fmt.Fprintf(w, "track/disc:   %d / %d\n", v.TrackNo, v.DiscNo)
			fmt.Fprintf(w, "year:         %d\n", v.Year)
			fmt.Fprintf(w, "genre:        %s\n", v.Genre)
			fmt.Fprintf(w, "codec:        %s\n", v.Codec)
			fmt.Fprintf(w, "duration(ms): %d\n", v.DurationMS)
			if v.Virtual {
				// A cue-carved virtual track plays only this window of the shared file.
				fmt.Fprintf(w, "window(ms):   [%d, %d) of a shared file\n", v.StartMS, v.EndMS)
			}
			if ld != nil {
				fmt.Fprintf(w, "replaygain:   track %+.2f dB (peak %.3f)", ld.TrackGainDB, ld.TrackPeak)
				if ld.HasAlbum {
					fmt.Fprintf(w, ", album %+.2f dB (peak %.3f)", ld.AlbumGainDB, ld.AlbumPeak)
				}
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "file pid:     %s\n", v.FilePID)
			fmt.Fprintf(w, "path:         %s\n", v.DisplayPath)
			return nil
		},
	}
}
