package waxbin

import (
	"context"
	"math"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/fingerprint"
	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
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
// the next, and so a tie it cannot break: ResolveRefWith can take a tie between copies
// of one recording instead. The strong-id and descriptive rungs dispatch on ref.Kind;
// essence and fingerprint do not care about kind.
func (l *Library) ResolveRef(ctx context.Context, ref model.PortableRef) (*model.ItemView, model.MatchRung, error) {
	m, err := l.ResolveRefWith(ctx, ref, ResolveOptions{})
	return m.Item, m.Rung, err
}

// ResolveOptions tunes ResolveRefWith.
type ResolveOptions struct {
	// AcceptSameRecording takes a tie ResolveRef declines when every item in it is
	// provably the same recording, answering with the present one with the lowest pid.
	// The tying rung's own evidence is the proof: the ref's audio bytes in a whole file of
	// each (two windows a cue sheet cuts from one rip are two recordings), the ref's
	// recording or release id on each, or a fingerprint each matches. On the descriptive
	// rung, where only names tie, the items have to share an id of their own, or be one
	// album track held twice: the same album, disc and track, with lengths that agree.
	AcceptSameRecording bool
}

// RefMatch is what ResolveRefWith found: the item, the rung that matched it, and how many
// items tied for it, 1 for a unique match. On a miss Item is nil, Rung is MatchNone, and
// Candidates is the size of the first tie the ladder declined, 0 when it found nothing.
type RefMatch struct {
	Item       *model.ItemView
	Rung       model.MatchRung
	Candidates int
}

// ResolveRefWith is ResolveRef with options, answering with the tie it saw as well.
func (l *Library) ResolveRefWith(ctx context.Context, ref model.PortableRef, opts ResolveOptions) (RefMatch, error) {
	declined := 0
	// settle answers a rung's candidates: one is a match, a tie is taken when the caller
	// accepts one the rung proves, and is otherwise declined.
	settle := func(rung model.MatchRung, tied []*model.ItemView, proven bool) (RefMatch, bool) {
		switch {
		case len(tied) == 1:
			return RefMatch{Item: tied[0], Rung: rung, Candidates: 1}, true
		case len(tied) > 1 && opts.AcceptSameRecording && proven:
			return RefMatch{Item: presentFirst(tied), Rung: rung, Candidates: len(tied)}, true
		case len(tied) > 1 && declined == 0:
			declined = len(tied)
		}
		return RefMatch{}, false
	}
	// 1. Essence: identical audio bytes, the exact-copy case. A single hit is
	// authoritative whatever its kind.
	if ref.Essence != "" {
		items, err := l.store.ItemsByEssence(ctx, ref.Essence)
		if err != nil {
			return RefMatch{Rung: model.MatchNone}, err
		}
		tied := essenceCandidates(items, ref)
		whole := false
		if opts.AcceptSameRecording && len(tied) > 1 {
			if whole, err = l.wholeFiles(ctx, tied); err != nil {
				return RefMatch{Rung: model.MatchNone}, err
			}
		}
		if m, ok := settle(model.MatchEssence, tied, whole); ok {
			return m, nil
		}
	}
	// 2. Strong id: an exact external identifier (a recording MBID for a track, a release
	// MBID/ASIN/ISBN for a book), dispatched on kind. Every candidate carries one of the
	// ref's ids, which proves a tie.
	tied, err := l.strongIDCandidates(ctx, ref)
	if err != nil {
		return RefMatch{Rung: model.MatchNone}, err
	}
	if m, ok := settle(model.MatchStrongID, tied, true); ok {
		return m, nil
	}
	// 3. Fingerprint: the same recording in a different encoding.
	if len(ref.Fingerprint) > 0 {
		tied, err := l.fingerprintCandidates(ctx, ref)
		if err != nil {
			return RefMatch{Rung: model.MatchNone}, err
		}
		if m, ok := settle(model.MatchFingerprint, tied, true); ok {
			return m, nil
		}
	}
	// 4. Descriptive: fuzzy metadata, the last resort for a different rip that carries no
	// id and no comparable fingerprint.
	tied, err = l.descriptiveCandidates(ctx, ref)
	if err != nil {
		return RefMatch{Rung: model.MatchNone}, err
	}
	if m, ok := settle(model.MatchDescriptive, tied, sameRecording(tied)); ok {
		return m, nil
	}
	return RefMatch{Rung: model.MatchNone, Candidates: declined}, nil
}

// presentFirst picks a tie's answer: the present item with the lowest pid, else the
// lowest pid of all.
func presentFirst(tied []*model.ItemView) *model.ItemView {
	best := tied[0]
	for _, v := range tied[1:] {
		bestHere, here := best.State == model.StatePresent, v.State == model.StatePresent
		if here && !bestHere || here == bestHere && v.PID < best.PID {
			best = v
		}
	}
	return best
}

// sharedID reports whether every item carries one external id: the same recording or
// release MBID, or the same ASIN.
func sharedID(items []*model.ItemView) bool {
	return sharesValue(items, func(v *model.ItemView) string { return v.MBID }) ||
		sharesValue(items, func(v *model.ItemView) string { return v.ASIN })
}

func sharesValue(items []*model.ItemView, value func(*model.ItemView) string) bool {
	if len(items) == 0 {
		return false
	}
	want := strings.ToLower(value(items[0]))
	if want == "" {
		return false
	}
	for _, v := range items[1:] {
		if strings.ToLower(value(v)) != want {
			return false
		}
	}
	return true
}

// sameRecording reports whether a descriptive tie is provably one recording: the items
// share an id, or they are one album track held twice, the same album, disc and track
// number, their lengths within the descriptive tolerance of each other. A track with no
// number names no track, and books have no such second test, since one title read twice
// is two recordings.
func sameRecording(items []*model.ItemView) bool {
	if len(items) == 0 || sharedID(items) {
		return len(items) > 0
	}
	first := items[0]
	album := identity.MatchKey(first.Album)
	if first.Kind == model.KindBook || album == "" || first.TrackNo <= 0 {
		return false
	}
	for i, v := range items {
		if identity.MatchKey(v.Album) != album || v.DiscNo != first.DiscNo || v.TrackNo != first.TrackNo {
			return false
		}
		for _, w := range items[i+1:] {
			if v.DurationMS > 0 && w.DurationMS > 0 && absInt64(v.DurationMS-w.DurationMS) > descriptiveDurationTolMS {
				return false
			}
		}
	}
	return true
}

// wholeFiles reports whether every item of an essence tie is the shared file whole: none a
// window a cue sheet cuts from it, and no book of more than one part.
func (l *Library) wholeFiles(ctx context.Context, items []*model.ItemView) (bool, error) {
	for _, v := range items {
		if v.Virtual {
			return false, nil
		}
		if v.Kind != model.KindBook {
			continue
		}
		files, err := l.store.ItemFiles(ctx, v.PID)
		if err != nil {
			return false, err
		}
		parts := 0
		for _, f := range files {
			if f.Role != "alternate" {
				parts++
			}
		}
		if parts > 1 {
			return false, nil
		}
	}
	return true, nil
}

// essenceCandidates narrows the 0, 1, or N items that share one essence to the ones the
// ref can mean. N hits come from a single-file CUE album, where each virtual track has
// its own primary edge to the one shared file, or from the same bytes cataloged under
// two kinds. It keeps the items whose kind the ref asks for (if the ref names one), then
// the ones whose duration is closest to the ref's, within essenceDurationTolMS. A ref
// with no duration leaves every item of its kind tied.
func essenceCandidates(items []*model.ItemView, ref model.PortableRef) []*model.ItemView {
	if len(items) < 2 {
		return items
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
	if len(cand) < 2 || ref.DurationMS <= 0 {
		return cand
	}
	var best []*model.ItemView
	bestDelta := int64(math.MaxInt64)
	for _, v := range cand {
		d := absInt64(v.DurationMS - ref.DurationMS)
		switch {
		case d > essenceDurationTolMS:
		case d < bestDelta:
			best, bestDelta = []*model.ItemView{v}, d
		case d == bestDelta:
			best = append(best, v)
		}
	}
	return best
}

// strongIDCandidates runs the strong-id rung for the ref's kind. A track, or an unkinded
// ref, resolves by recording MBID; a book resolves by release MBID, ASIN, or ISBN; an
// episode has no strong-id form, so it is skipped. A lookup runs only when its field is
// set. Several items answering one id is a tie: a recording legitimately appears on both
// a single and a compilation.
func (l *Library) strongIDCandidates(ctx context.Context, ref model.PortableRef) ([]*model.ItemView, error) {
	switch ref.Kind {
	case model.KindBook:
		if ref.MBID == "" && ref.ASIN == "" && ref.ISBN == "" {
			return nil, nil
		}
		return l.store.ItemsByBookIdent(ctx, ref.MBID, ref.ASIN, ref.ISBN)
	case model.KindEpisode:
		return nil, nil
	default: // track, or an unkinded ref
		if ref.MBID == "" {
			return nil, nil
		}
		return l.store.ItemsByRecordingMBID(ctx, ref.MBID)
	}
}

// fingerprintCandidates runs the fingerprint rung. It derives the ref's min-hash terms
// through fingerprint.TermsForAlgo, the same dispatch the analyze write side uses, probes
// the inverted index within a one-bucket window around the fingerprint's own duration
// bucket (DurationMS's for a ref without one) under the ref's algorithm and kind, then
// verifies each candidate against the full vector. It returns the items scoring the best
// similarity at or above the floor, several when they tie. A short or corrupt fingerprint
// with no terms, or an algorithm or kind with no candidates, returns nil. The floor is
// inclusive (>=) to match FindAltEncodings, so a candidate that lands exactly on the
// threshold resolves here the same way it would group there.
func (l *Library) fingerprintCandidates(ctx context.Context, ref model.PortableRef) ([]*model.ItemView, error) {
	qSub := fingerprint.Unpack(ref.Fingerprint)
	terms := fingerprint.TermsForAlgo(ref.FingerprintAlgo, qSub)
	if len(terms) == 0 {
		return nil, nil
	}
	bucket := int(ref.FingerprintBucket)
	if bucket == 0 {
		bucket = int(fingerprint.DurationBucket(ref.DurationMS))
	}
	cands, err := l.store.FingerprintCandidatesByProbe(ctx, ref.Kind, ref.FingerprintAlgo, bucket-1, bucket+1, terms, altMinSharedTerms)
	if err != nil {
		return nil, err
	}
	var best []model.PID
	var bestSim float64
	for _, c := range cands {
		if c.ItemPID == "" {
			continue
		}
		// The probe guarantees c shares the ref's fingerprint algorithm, so dispatching
		// on the candidate's algo picks the matching (pure-Go vs Chromaprint) function.
		sim := fingerprint.SimilarByAlgo(c.AlgoVersion, qSub, fingerprint.Unpack(c.FP))
		switch {
		case sim < altSimilarityFloor:
		case sim > bestSim:
			best, bestSim = []model.PID{c.ItemPID}, sim
		case sim == bestSim && !slices.Contains(best, c.ItemPID):
			best = append(best, c.ItemPID)
		}
	}
	out := make([]*model.ItemView, 0, len(best))
	for _, pid := range best {
		v, err := l.store.ItemByPID(ctx, pid)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// descriptiveCandidates runs the descriptive rung, dispatching on kind (an empty kind is
// treated as track). It seeds on the artist or author entity, which is joinable, then
// filters the seed in Go by title, by duration (tracks only), and by an album or series
// tie-break. The album is not joinable because its match_key embeds a local folder path,
// so it is compared as text instead. When the ref's artist match key is empty the rung
// returns early, since a query for an empty match key could never hit anything useful.
func (l *Library) descriptiveCandidates(ctx context.Context, ref model.PortableRef) ([]*model.ItemView, error) {
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
		return descriptiveBooks(seeds, ref, titleKey), nil
	case model.KindEpisode:
		return nil, nil
	default:
		seeds, err := l.store.ItemsByArtistKey(ctx, artistKey)
		if err != nil {
			return nil, err
		}
		return descriptiveTracks(seeds, ref, titleKey), nil
	}
}

// descriptiveTracks keeps the seed rows whose title matches and, when both durations are
// known, whose duration is within descriptiveDurationTolMS. The album only breaks ties;
// it never filters. So one title survivor matches even when its album differs, because a
// recording turns up under many album titles.
func descriptiveTracks(seeds []*model.ItemView, ref model.PortableRef, titleKey string) []*model.ItemView {
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
	return albumTiebreak(survivors, identity.MatchKey(ref.Album), func(v *model.ItemView) string {
		return identity.MatchKey(v.Album)
	})
}

// descriptiveBooks keeps the seed rows whose title matches (no duration gate: books are
// long and multi-file) and breaks a tie on the series, which a book ref carries in its
// Album field.
func descriptiveBooks(seeds []*model.ItemView, ref model.PortableRef, titleKey string) []*model.ItemView {
	var survivors []*model.ItemView
	for _, v := range seeds {
		if identity.MatchKey(v.Title) == titleKey {
			survivors = append(survivors, v)
		}
	}
	return albumTiebreak(survivors, identity.MatchKey(ref.Album), func(v *model.ItemView) string {
		return identity.MatchKey(v.Series)
	})
}

// albumTiebreak narrows several survivors to those whose tie-break key equals want, and
// leaves the tie standing when none does. An empty want tells it nothing, so with several
// survivors the tie stands rather than letting an item that happens to also lack an
// album or series win by coincidence.
func albumTiebreak(survivors []*model.ItemView, want string, key func(*model.ItemView) string) []*model.ItemView {
	if len(survivors) < 2 || want == "" {
		return survivors
	}
	var subset []*model.ItemView
	for _, v := range survivors {
		if key(v) == want {
			subset = append(subset, v)
		}
	}
	if len(subset) == 0 {
		return survivors
	}
	return subset
}

// ExportPlaylistRefs returns a portable identity descriptor for each of a playlist's
// entries in order (a static list's stored order, a smart list evaluated for userPID),
// an item listed twice exported twice. A playlist of local PIDs cannot cross catalogs,
// so the host ships these refs and rebuilds the list on the far side via
// ResolvePlaylistRefs. An empty userPID selects the default user.
func (l *Library) ExportPlaylistRefs(ctx context.Context, playlistPID, userPID model.PID) ([]model.PortableRef, error) {
	items, err := l.Playlists().Items(ctx, playlistPID, userPID)
	if err != nil {
		return nil, err
	}
	pids := make([]model.PID, 0, len(items))
	for _, v := range items {
		pids = append(pids, v.PID)
	}
	byPID, err := l.store.ItemIdentities(ctx, pids)
	if err != nil {
		return nil, err
	}
	refs := make([]model.PortableRef, 0, len(pids))
	for _, pid := range pids {
		if ref, ok := byPID[pid]; ok {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// ResolvePlaylistRefs resolves a batch of portable refs to local items, preserving input
// order and reporting each entry's matched rung (MatchNone for a miss). The host derives
// the resolved and missing sets and rebuilds a local playlist from the resolved PIDs with
// the existing Playlists() primitives.
func (l *Library) ResolvePlaylistRefs(ctx context.Context, refs []model.PortableRef) ([]model.RefResolution, error) {
	return l.ResolvePlaylistRefsWith(ctx, refs, ResolveOptions{})
}

// ResolvePlaylistRefsWith is ResolvePlaylistRefs with options, each entry resolved as
// ResolveRefWith resolves it.
func (l *Library) ResolvePlaylistRefsWith(ctx context.Context, refs []model.PortableRef, opts ResolveOptions) ([]model.RefResolution, error) {
	out := make([]model.RefResolution, 0, len(refs))
	for _, ref := range refs {
		m, err := l.ResolveRefWith(ctx, ref, opts)
		if err != nil {
			return nil, err
		}
		res := model.RefResolution{Ref: ref, Rung: m.Rung, Candidates: m.Candidates}
		if m.Item != nil {
			res.PID = m.Item.PID
		}
		out = append(out, res)
	}
	return out, nil
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
