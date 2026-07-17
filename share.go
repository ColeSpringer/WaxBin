package waxbin

import (
	"context"
	"math"

	"github.com/colespringer/waxbin/fingerprint"
	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Cross-catalog sharing primitives. An embedder runs one WaxBin catalog per user for
// isolation, so a "share" between two catalogs is a copy, not a permission grant, and a
// playlist of local PIDs is meaningless across catalogs. These facade methods give the
// host what it needs to avoid reimplementing WaxBin's essence/MBID/fingerprint identity
// matching against the internal schema: resolve a portable identity descriptor to a
// local item (ResolveRef), export a playlist as portable refs (ExportPlaylistRefs), and
// resolve a batch of refs back to local items (ResolvePlaylistRefs). All three are pure
// catalog reads, safe on a read-only Library.

const (
	// essenceDurationTolMS bounds the duration difference when disambiguating several
	// items that share one essence (a single-file CUE album: N virtual tracks over one
	// file). The nearest within this window wins, and only when it is unambiguous.
	essenceDurationTolMS = 1000
	// descriptiveDurationTolMS bounds the duration difference on the track descriptive
	// rung, where only fuzzy metadata is left to match on. It is looser than the essence
	// tolerance because two independent rips of one recording differ more than two views
	// of the same bytes. Books have no duration gate (they are long and multi-file).
	descriptiveDurationTolMS = 3000
)

// ResolveRef walks the match ladder from most to least confident and returns the local
// item a portable ref names, along with the rung that matched so the host can report how
// sure the match is. The rungs are essence (exact bytes), strong id (an exact external
// identifier), fingerprint (the same recording in another encoding), then descriptive
// (fuzzy metadata). A clean miss returns (nil, MatchNone, nil); only a real IO failure
// returns an error. Every rung treats an empty or not-found result as a fall-through to
// the next. The strong-id and descriptive rungs dispatch on ref.Kind; essence and
// fingerprint do not care about kind.
func (l *Library) ResolveRef(ctx context.Context, ref model.PortableRef) (*model.ItemView, model.MatchRung, error) {
	// 1. Essence: identical audio bytes, the exact-copy case. A single hit is
	// authoritative whatever its kind.
	if ref.Essence != "" {
		items, err := l.store.ItemsByEssence(ctx, ref.Essence)
		if err != nil {
			return nil, model.MatchNone, err
		}
		if v := pickEssenceMatch(items, ref); v != nil {
			return v, model.MatchEssence, nil
		}
	}
	// 2. Strong id: an exact external identifier (a recording MBID for a track, a release
	// MBID/ASIN/ISBN for a book). It dispatches on kind, and the store declines an
	// ambiguous id rather than guessing.
	if v, err := l.resolveStrongID(ctx, ref); err != nil {
		return nil, model.MatchNone, err
	} else if v != nil {
		return v, model.MatchStrongID, nil
	}
	// 3. Fingerprint: the same recording in a different encoding.
	if len(ref.Fingerprint) > 0 {
		v, err := l.resolveFingerprint(ctx, ref)
		if err != nil {
			return nil, model.MatchNone, err
		}
		if v != nil {
			return v, model.MatchFingerprint, nil
		}
	}
	// 4. Descriptive: fuzzy metadata, the last resort for a different rip that carries no
	// id and no comparable fingerprint.
	if v, err := l.resolveDescriptive(ctx, ref); err != nil {
		return nil, model.MatchNone, err
	} else if v != nil {
		return v, model.MatchDescriptive, nil
	}
	return nil, model.MatchNone, nil
}

// pickEssenceMatch chooses the essence match from the 0, 1, or N items that share one
// essence. A single hit wins whatever its kind, since identical bytes are the strongest
// signal there is. N hits come from a single-file CUE album, where each virtual track has
// its own primary edge to the one shared file, or from the same bytes cataloged under two
// kinds. To pick one, it first keeps the items whose kind the ref asks for (if the ref
// names one), then takes the item whose duration is closest to the ref's, within
// essenceDurationTolMS, and only when that closest item stands alone. If nothing survives
// it returns nil, and the ladder moves on to the descriptive rung.
func pickEssenceMatch(items []*model.ItemView, ref model.PortableRef) *model.ItemView {
	switch len(items) {
	case 0:
		return nil
	case 1:
		return items[0]
	}
	cand := items
	if ref.Kind != "" {
		var byKind []*model.ItemView
		for _, v := range cand {
			if v.Kind == ref.Kind {
				byKind = append(byKind, v)
			}
		}
		cand = byKind
	}
	if len(cand) == 1 {
		return cand[0]
	}
	if len(cand) == 0 || ref.DurationMS <= 0 {
		return nil
	}
	var best *model.ItemView
	bestDelta := int64(math.MaxInt64)
	tie := false
	for _, v := range cand {
		d := absInt64(v.DurationMS - ref.DurationMS)
		if d > essenceDurationTolMS {
			continue
		}
		switch {
		case d < bestDelta:
			best, bestDelta, tie = v, d, false
		case d == bestDelta:
			tie = true
		}
	}
	if best == nil || tie {
		return nil
	}
	return best
}

// resolveStrongID runs the strong-id rung for the ref's kind. A track, or an unkinded
// ref, resolves by recording MBID; a book resolves by release MBID, ASIN, or ISBN; an
// episode has no strong-id form, so it is skipped. A lookup runs only when its field is
// set, and a store CodeNotFound (a miss, or an id that pointed at more than one item)
// becomes a fall-through to the next rung.
func (l *Library) resolveStrongID(ctx context.Context, ref model.PortableRef) (*model.ItemView, error) {
	switch ref.Kind {
	case model.KindBook:
		if ref.MBID == "" && ref.ASIN == "" && ref.ISBN == "" {
			return nil, nil
		}
		return okItem(l.store.ItemByBookIdent(ctx, ref.MBID, ref.ASIN, ref.ISBN))
	case model.KindEpisode:
		return nil, nil
	default: // track, or an unkinded ref
		if ref.MBID == "" {
			return nil, nil
		}
		return okItem(l.store.ItemByRecordingMBID(ctx, ref.MBID))
	}
}

// resolveFingerprint runs the fingerprint rung. It derives the ref's min-hash terms
// through fingerprint.TermsForAlgo, the same dispatch the analyze write side uses, probes
// the inverted index within a one-bucket duration window under the ref's algorithm and
// kind, then verifies each candidate against the full vector. It returns the single item
// scoring at or above the similarity floor. A short or corrupt fingerprint with no terms,
// an algorithm or kind with no candidates, and a tie all return nil. The floor is
// inclusive (>=) to match FindAltEncodings, so a candidate that lands exactly on the
// threshold resolves here the same way it would group there.
func (l *Library) resolveFingerprint(ctx context.Context, ref model.PortableRef) (*model.ItemView, error) {
	qSub := fingerprint.Unpack(ref.Fingerprint)
	terms := fingerprint.TermsForAlgo(ref.FingerprintAlgo, qSub)
	if len(terms) == 0 {
		return nil, nil
	}
	bucket := int(fingerprint.DurationBucket(ref.DurationMS))
	cands, err := l.store.FingerprintCandidatesByProbe(ctx, ref.Kind, ref.FingerprintAlgo, bucket-1, bucket+1, terms, altMinSharedTerms)
	if err != nil {
		return nil, err
	}
	var bestPID model.PID
	var bestSim float64
	tie := false
	for _, c := range cands {
		if c.ItemPID == "" {
			continue
		}
		// The probe guarantees c shares the ref's fingerprint algorithm, so dispatching
		// on the candidate's algo picks the matching (pure-Go vs Chromaprint) function.
		sim := fingerprint.SimilarByAlgo(c.AlgoVersion, qSub, fingerprint.Unpack(c.FP))
		if sim < altSimilarityFloor {
			continue
		}
		switch {
		case sim > bestSim:
			bestPID, bestSim, tie = c.ItemPID, sim, false
		case sim == bestSim && c.ItemPID != bestPID:
			tie = true
		}
	}
	if bestPID == "" || tie {
		return nil, nil
	}
	return l.store.ItemByPID(ctx, bestPID)
}

// resolveDescriptive runs the descriptive rung, dispatching on kind (an empty kind is
// treated as track). It seeds on the artist or author entity, which is joinable, then
// filters the seed in Go by title, by duration (tracks only), and by an album or series
// tie-break. The album is not joinable because its match_key embeds a local folder path,
// so it is compared as text instead. When the ref's artist match key is empty the rung
// returns early, since a match_key=” query could never hit anything useful.
func (l *Library) resolveDescriptive(ctx context.Context, ref model.PortableRef) (*model.ItemView, error) {
	artistKey := identity.MatchKey(ref.Artist)
	if artistKey == "" {
		return nil, nil
	}
	titleKey := identity.MatchKey(ref.Title)
	switch ref.Kind {
	case model.KindBook:
		seeds, err := l.store.ItemsByAuthorKey(ctx, artistKey)
		if err != nil {
			return nil, err
		}
		return pickDescriptiveBook(seeds, ref, titleKey), nil
	case model.KindEpisode:
		return nil, nil
	default:
		seeds, err := l.store.ItemsByArtistKey(ctx, artistKey)
		if err != nil {
			return nil, err
		}
		return pickDescriptiveTrack(seeds, ref, titleKey), nil
	}
}

// pickDescriptiveTrack keeps the seed rows whose title matches and, when both durations
// are known, whose duration is within descriptiveDurationTolMS. The album only breaks
// ties; it never filters. So one title survivor matches even when its album differs,
// because a recording turns up under many album titles. When several survive, the album
// picks the winner only if exactly one of them carries the ref's album.
func pickDescriptiveTrack(seeds []*model.ItemView, ref model.PortableRef, titleKey string) *model.ItemView {
	var survivors []*model.ItemView
	for _, v := range seeds {
		if identity.MatchKey(v.Title) != titleKey {
			continue
		}
		if v.DurationMS > 0 && ref.DurationMS > 0 &&
			absInt64(v.DurationMS-ref.DurationMS) > descriptiveDurationTolMS {
			continue
		}
		survivors = append(survivors, v)
	}
	return uniqueOrAlbumTiebreak(survivors, identity.MatchKey(ref.Album), func(v *model.ItemView) string {
		return identity.MatchKey(v.Album)
	})
}

// pickDescriptiveBook keeps the seed rows whose title matches (no duration gate: books
// are long and multi-file) and breaks a tie on the series, which a book ref carries in
// its Album field.
func pickDescriptiveBook(seeds []*model.ItemView, ref model.PortableRef, titleKey string) *model.ItemView {
	var survivors []*model.ItemView
	for _, v := range seeds {
		if identity.MatchKey(v.Title) == titleKey {
			survivors = append(survivors, v)
		}
	}
	return uniqueOrAlbumTiebreak(survivors, identity.MatchKey(ref.Album), func(v *model.ItemView) string {
		return identity.MatchKey(v.Series)
	})
}

// uniqueOrAlbumTiebreak returns the lone survivor when there is one. When several
// survive, it keeps those whose tie-break key equals want and returns that one only if
// the subset holds exactly one item. Nothing surviving, or a tie it cannot break, yields
// nil. An empty want tells it nothing, so with several survivors it gives up rather than
// letting an item that happens to also lack an album or series win by coincidence.
func uniqueOrAlbumTiebreak(survivors []*model.ItemView, want string, key func(*model.ItemView) string) *model.ItemView {
	switch len(survivors) {
	case 0:
		return nil
	case 1:
		return survivors[0]
	}
	if want == "" {
		return nil
	}
	var subset []*model.ItemView
	for _, v := range survivors {
		if key(v) == want {
			subset = append(subset, v)
		}
	}
	if len(subset) == 1 {
		return subset[0]
	}
	return nil
}

// ExportPlaylistRefs returns portable identity descriptors for a playlist's members in
// order (a static list's stored order, a smart list evaluated for userPID). A playlist
// of local PIDs cannot cross catalogs, so the host ships these refs and rebuilds the
// list on the far side via ResolvePlaylistRefs. An empty userPID selects the default
// user.
func (l *Library) ExportPlaylistRefs(ctx context.Context, playlistPID, userPID model.PID) ([]model.PortableRef, error) {
	items, err := l.Playlists().Items(ctx, playlistPID, userPID)
	if err != nil {
		return nil, err
	}
	pids := make([]model.PID, 0, len(items))
	for _, v := range items {
		pids = append(pids, v.PID)
	}
	return l.store.ItemIdentitiesByPIDs(ctx, pids)
}

// ResolvePlaylistRefs resolves a batch of portable refs to local items, preserving input
// order and reporting each entry's matched rung (MatchNone for a miss). The host derives
// the resolved and missing sets and rebuilds a local playlist from the resolved PIDs with
// the existing Playlists() primitives.
func (l *Library) ResolvePlaylistRefs(ctx context.Context, refs []model.PortableRef) ([]model.RefResolution, error) {
	out := make([]model.RefResolution, 0, len(refs))
	for _, ref := range refs {
		item, rung, err := l.ResolveRef(ctx, ref)
		if err != nil {
			return nil, err
		}
		res := model.RefResolution{Ref: ref, Rung: rung}
		if item != nil {
			res.PID = item.PID
		}
		out = append(out, res)
	}
	return out, nil
}

// okItem swallows a store CodeNotFound (a clean miss or a declined ambiguous id) into a
// nil item with no error, so a strong-id sub-lookup falls through to the next rung; any
// other error propagates.
func okItem(v *model.ItemView, err error) (*model.ItemView, error) {
	if err != nil {
		if waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return v, nil
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
