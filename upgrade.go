package waxbin

import (
	"context"
	"sort"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// losslessCodecs are the codec keys the quality policy treats as lossless. A
// lossless encoding always outranks a lossy one regardless of bitrate.
var losslessCodecs = map[string]bool{
	"flac": true, "alac": true, "pcm": true, "wav": true, "aiff": true,
	"ape": true, "wavpack": true, "tak": true, "tta": true, "dsd": true,
}

// UpgradeCandidate is one encoding of a recording, with the quality fields the
// policy ranks on.
type UpgradeCandidate struct {
	ItemPID    model.PID
	FilePID    model.PID
	Title      string
	Artist     string
	Codec      string
	Bitrate    int
	SampleRate int
	BitDepth   int
	Lossless   bool
	// Best marks the highest-quality member of the group (the recommended keeper);
	// the rest are lower-quality alt encodings that could be pruned or upgraded.
	Best bool
}

// UpgradeGroup is a set of catalog items that are the same recording in different
// encodings (grouped by fingerprint), ordered best-quality first.
type UpgradeGroup struct {
	Members []UpgradeCandidate
}

// FindUpgrades groups the catalog's alt encodings (the same recording in
// different files) and ranks each group by audio quality, so a consumer can keep
// the best and prune or upgrade the rest. Grouping uses the fingerprint index via
// FindAltEncodings. MBID- and essence-identical encodings already collapse to one
// item during scan, so this surfaces the cross-encoding case. Items must be
// analyzed (fingerprinted); unanalyzed items never group.
//
// It is a maintenance scan: it walks every track item and probes the fingerprint
// index for each, so it is meant for occasional use, not the hot path.
func (l *Library) FindUpgrades(ctx context.Context) ([]UpgradeGroup, error) {
	items, err := l.store.QueryItems(ctx, query.New(query.EntityTracks).Build(), "")
	if err != nil {
		return nil, err
	}
	views := make(map[model.PID]*model.ItemView, len(items))
	for _, it := range items {
		views[it.PID] = it
	}
	// Load every item's primary-file quality up front in one query, so building a
	// candidate never issues a per-file lookup.
	quality, err := l.store.FileQualitiesByItem(ctx)
	if err != nil {
		return nil, err
	}

	// Fingerprint similarity is symmetric but not transitive, so a group is the
	// whole connected component reachable from a seed, not just the seed's direct
	// neighbours. BFS the component so a chain A~B~C (with A~C below the floor) still
	// groups {A,B,C}, and the result is independent of iteration order.
	visited := make(map[model.PID]bool, len(items))
	var groups []UpgradeGroup
	for _, seed := range items {
		if visited[seed.PID] || seed.FilePID == "" {
			continue
		}
		visited[seed.PID] = true

		var component []model.PID
		queue := []model.PID{seed.PID}
		for len(queue) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			cur := queue[0]
			queue = queue[1:]
			component = append(component, cur)
			alts, err := l.FindAltEncodings(ctx, cur)
			if err != nil {
				return nil, err
			}
			for _, alt := range alts {
				if !visited[alt.ItemPID] {
					visited[alt.ItemPID] = true
					queue = append(queue, alt.ItemPID)
				}
			}
		}
		if len(component) < 2 {
			continue // no alt encodings for this seed
		}

		members := make([]UpgradeCandidate, 0, len(component))
		for _, pid := range component {
			v := views[pid]
			if v == nil {
				var e error
				if v, e = l.store.ItemByPID(ctx, pid); e != nil {
					continue // a concurrent delete raced the scan; skip it
				}
			}
			members = append(members, candidate(v, quality))
		}
		if len(members) < 2 {
			continue // members raced away
		}
		sortByQuality(members)
		members[0].Best = true
		groups = append(groups, UpgradeGroup{Members: members})
	}
	return groups, nil
}

// candidate builds an UpgradeCandidate for an item from the preloaded quality map.
// It degrades to the item view's codec when the item is absent from the map.
func candidate(it *model.ItemView, quality map[model.PID]model.File) UpgradeCandidate {
	c := UpgradeCandidate{
		ItemPID: it.PID, FilePID: it.FilePID, Title: it.Title, Artist: it.Artist,
		Codec: it.Codec, Lossless: losslessCodecs[strings.ToLower(it.Codec)],
	}
	if q, ok := quality[it.PID]; ok {
		c.Codec = q.Codec
		c.Bitrate = q.Bitrate
		c.SampleRate = q.SampleRate
		c.BitDepth = q.BitDepth
		c.Lossless = losslessCodecs[strings.ToLower(q.Codec)]
	}
	return c
}

// sortByQuality orders candidates best-first: lossless outranks lossy, then
// higher sample rate, bit depth, and bitrate. The PID breaks ties for a stable,
// deterministic order.
func sortByQuality(cs []UpgradeCandidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.Lossless != b.Lossless {
			return a.Lossless // lossless first
		}
		if a.SampleRate != b.SampleRate {
			return a.SampleRate > b.SampleRate
		}
		if a.BitDepth != b.BitDepth {
			return a.BitDepth > b.BitDepth
		}
		if a.Bitrate != b.Bitrate {
			return a.Bitrate > b.Bitrate
		}
		return a.ItemPID < b.ItemPID
	})
}
