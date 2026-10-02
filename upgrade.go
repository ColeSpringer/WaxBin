package waxbin

import (
	"context"
	"slices"
	"sort"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

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
// the best and prune or upgrade the rest. Two kinds of group come back. An item that
// holds another encoding of its recording as an alternate (the MBID key collapses the
// two onto one item at scan) comes first, as a group of its own, each encoding named by
// its file for PlanDeleteFiles. Then the cross-item groups, found through the
// fingerprint index via FindAltEncodings, which need the items analyzed
// (fingerprinted); unanalyzed items never group there. A copy
// of an item's audio is not an encoding and never appears; the duplicate_copy audit
// lists those.
//
// It is a maintenance scan: it walks every track item and probes the fingerprint
// index for each, so it is meant for occasional use, not the hot path.
func (l *Library) FindUpgrades(ctx context.Context) ([]UpgradeGroup, error) {
	groups, err := l.itemEncodingGroups(ctx)
	if err != nil {
		return nil, err
	}
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

	components, err := encodingComponents(ctx, items, l.FindAltEncodings)
	if err != nil {
		return nil, err
	}
	for _, component := range components {
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

// encodingComponents groups items whose fingerprints match, through alts. Similarity is
// symmetric but not transitive, so a group is the whole connected component reachable
// from a seed, not just the seed's direct neighbours: a chain A~B~C (with A~C below the
// floor) still groups {A,B,C}, and the result is independent of iteration order. An item
// that went between the listing and its probe, the seed included, is skipped. Only
// components of two or more come back.
func encodingComponents(ctx context.Context, items []*model.ItemView, alts func(context.Context, model.PID) ([]AltEncoding, error)) ([][]model.PID, error) {
	var out [][]model.PID
	visited := make(map[model.PID]bool, len(items))
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
			found, err := alts(ctx, cur)
			if waxerr.Is(err, waxerr.CodeNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			component = append(component, cur)
			for _, alt := range found {
				if alt.ItemPID != "" && !visited[alt.ItemPID] {
					visited[alt.ItemPID] = true
					queue = append(queue, alt.ItemPID)
				}
			}
		}
		if len(component) >= 2 {
			out = append(out, component)
		}
	}
	return out, nil
}

// itemEncodingGroups returns a group for each track item holding another encoding of its
// recording: its primary and those encodings by quality, the primary first on a tie.
func (l *Library) itemEncodingGroups(ctx context.Context) ([]UpgradeGroup, error) {
	copies, err := l.store.ItemsWithCopies(ctx)
	if err != nil {
		return nil, err
	}
	var groups []UpgradeGroup
	for _, it := range copies {
		if it.Kind != model.KindTrack {
			continue
		}
		var members []UpgradeCandidate
		for _, f := range it.Files {
			if f.Reason == model.CopySameAudio {
				continue
			}
			members = append(members, UpgradeCandidate{
				ItemPID: it.ItemPID, FilePID: f.FilePID, Title: it.Title, Artist: it.Artist,
				Codec: f.Codec, Bitrate: f.Bitrate, SampleRate: f.SampleRate, BitDepth: f.BitDepth,
				Lossless: model.LosslessCodec(f.Codec),
			})
		}
		if len(members) < 2 {
			continue
		}
		slices.SortStableFunc(members, func(a, b UpgradeCandidate) int { return model.CompareQuality(a.file(), b.file()) })
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
		Codec: it.Codec, Lossless: model.LosslessCodec(it.Codec),
	}
	if q, ok := quality[it.PID]; ok {
		c.Codec = q.Codec
		c.Bitrate = q.Bitrate
		c.SampleRate = q.SampleRate
		c.BitDepth = q.BitDepth
		c.Lossless = model.LosslessCodec(q.Codec)
	}
	return c
}

// sortByQuality orders candidates best-first by model.CompareQuality. The PIDs break
// ties for a stable, deterministic order.
func sortByQuality(cs []UpgradeCandidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if c := model.CompareQuality(a.file(), b.file()); c != 0 {
			return c < 0
		}
		if a.ItemPID != b.ItemPID {
			return a.ItemPID < b.ItemPID
		}
		return a.FilePID < b.FilePID
	})
}

func (c UpgradeCandidate) file() model.File {
	return model.File{Codec: c.Codec, Bitrate: c.Bitrate, SampleRate: c.SampleRate, BitDepth: c.BitDepth}
}
