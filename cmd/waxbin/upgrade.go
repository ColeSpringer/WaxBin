package main

import (
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/spf13/cobra"
)

func newUpgradeCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "List alt-encoding groups ranked by quality",
		Long: "Finds catalog items that are the same recording in different encodings " +
			"(grouped by audio fingerprint) and ranks each group by quality: lossless over " +
			"lossy, then sample rate, bit depth, and bitrate. The best encoding is marked " +
			"as the keeper; the rest are lower-quality copies to prune or upgrade. Items must " +
			"be analyzed first (`waxbin analyze`). This is a maintenance scan over the whole " +
			"catalog; it reports only, and never deletes.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			groups, err := lib.FindUpgrades(ctx(cmd))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toUpgradeViews(groups))
			}
			w := out(cmd)
			if len(groups) == 0 {
				fmt.Fprintln(w, "no alt-encoding groups found")
				return nil
			}
			for i, grp := range groups {
				fmt.Fprintf(w, "group %d:\n", i+1)
				for _, m := range grp.Members {
					marker := "  "
					if m.Best {
						marker = "* " // keeper
					}
					fmt.Fprintf(w, "%s%s  %s  [%s]\n", marker, m.Title, qualityLabel(m), m.ItemPID)
				}
			}
			fmt.Fprintf(w, "\n%d alt-encoding group(s); * marks the highest-quality keeper\n", len(groups))
			return nil
		},
	}
	return cmd
}

func qualityLabel(m waxbin.UpgradeCandidate) string {
	q := m.Codec
	if m.Lossless {
		q += " lossless"
	} else if m.Bitrate > 0 {
		q += fmt.Sprintf(" %dkbps", m.Bitrate)
	}
	if m.SampleRate > 0 {
		q += fmt.Sprintf(" %dHz", m.SampleRate)
	}
	if m.BitDepth > 0 {
		q += fmt.Sprintf("/%dbit", m.BitDepth)
	}
	return q
}

type upgradeMemberView struct {
	ItemPID    string `json:"itemPid"`
	FilePID    string `json:"filePid"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Codec      string `json:"codec"`
	Bitrate    int    `json:"bitrate"`
	SampleRate int    `json:"sampleRate"`
	BitDepth   int    `json:"bitDepth"`
	Lossless   bool   `json:"lossless"`
	Best       bool   `json:"best"`
}

type upgradeGroupView struct {
	Members []upgradeMemberView `json:"members"`
}

func toUpgradeViews(groups []waxbin.UpgradeGroup) []upgradeGroupView {
	out := make([]upgradeGroupView, 0, len(groups))
	for _, grp := range groups {
		ms := make([]upgradeMemberView, 0, len(grp.Members))
		for _, m := range grp.Members {
			ms = append(ms, upgradeMemberView{
				ItemPID: string(m.ItemPID), FilePID: string(m.FilePID),
				Title: m.Title, Artist: m.Artist, Codec: m.Codec,
				Bitrate: m.Bitrate, SampleRate: m.SampleRate, BitDepth: m.BitDepth,
				Lossless: m.Lossless, Best: m.Best,
			})
		}
		out = append(out, upgradeGroupView{Members: ms})
	}
	return out
}
