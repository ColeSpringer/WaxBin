package waxbin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
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
// local item (ResolveRef), export a playlist or any list of items as portable refs
// (ExportPlaylistRefs, PortableRefs), and resolve a batch of refs back to local items
// (ResolvePlaylistRefs). ResolveOptions.Accept scopes a resolution to the items a caller
// may hold, for a catalog that serves more than one. All of them are pure catalog reads,
// safe on a read-only Library.

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
	// Accept, when set, admits the items the caller may hold. Each rung offers it every
	// candidate before narrowing them (the nearest length, the best fingerprint, the album
	// tie-break), so a refused item never answers, even alone, and never counts in a tie.
	Accept model.ItemFilter
}

// RefMatch is what ResolveRefWith found: the item, the rung that matched it, and the
// items that tied for it, sorted by pid, with Candidates their number (the item alone, and
// 1, for a unique match). On a miss Item is nil, Rung is MatchNone, and Tied is the first
// tie the ladder declined, empty when it found nothing.
type RefMatch struct {
	Item       *model.ItemView
	Rung       model.MatchRung
	Candidates int
	Tied       []*model.ItemView
}

// ResolveRefWith is ResolveRef with options, answering with the tie it saw as well.
func (l *Library) ResolveRefWith(ctx context.Context, ref model.PortableRef, opts ResolveOptions) (RefMatch, error) {
	var declined []*model.ItemView
	// settle answers a rung's candidates: one is a match, a tie is taken when the caller
	// accepts one the rung proves, and is otherwise declined.
	settle := func(rung model.MatchRung, tied []*model.ItemView, proven bool) (RefMatch, bool) {
		switch {
		case len(tied) == 1:
			return RefMatch{Item: tied[0], Rung: rung, Candidates: 1, Tied: tied}, true
		case len(tied) > 1 && opts.AcceptSameRecording && proven:
			return RefMatch{Item: presentFirst(tied), Rung: rung, Candidates: len(tied), Tied: byPID(tied)}, true
		case len(tied) > 1 && declined == nil:
			declined = byPID(tied)
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
		tied := essenceCandidates(items, ref, opts.Accept)
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
	if m, ok := settle(model.MatchStrongID, opts.Accept.Admit(tied), true); ok {
		return m, nil
	}
	// 3. Fingerprint: the same recording in a different encoding.
	if len(ref.Fingerprint) > 0 {
		tied, err := l.fingerprintCandidates(ctx, ref, opts)
		if err != nil {
			return RefMatch{Rung: model.MatchNone}, err
		}
		if m, ok := settle(model.MatchFingerprint, tied, true); ok {
			return m, nil
		}
	}
	// 4. Descriptive: fuzzy metadata, the last resort for a different rip that carries no
	// id and no comparable fingerprint.
	tied, err = l.descriptiveCandidates(ctx, ref, opts)
	if err != nil {
		return RefMatch{Rung: model.MatchNone}, err
	}
	if m, ok := settle(model.MatchDescriptive, tied, sameRecording(tied)); ok {
		return m, nil
	}
	return RefMatch{Rung: model.MatchNone, Candidates: len(declined), Tied: declined}, nil
}

// byPID returns a tie's items sorted by pid.
func byPID(tied []*model.ItemView) []*model.ItemView {
	return slices.SortedFunc(slices.Values(tied), func(a, b *model.ItemView) int {
		return strings.Compare(string(a.PID), string(b.PID))
	})
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

// essenceCandidates narrows the 0, 1, or N items that share one essence to the admitted
// ones the ref can mean. N hits come from a single-file CUE album, where each virtual
// track has its own primary edge to the one shared file, or from the same bytes cataloged
// under two kinds. A lone hit is authoritative whatever its kind, and so is the lone hit
// of the kind the ref names; the scope (accept) only decides whether the caller may hold
// it. Several of the ref's kind narrow to the admitted ones whose duration is closest to
// the ref's, within essenceDurationTolMS, a check that still runs when the scope leaves
// one, so a ref never resolves to another window of a rip. A ref with no duration leaves
// every admitted item of its kind tied.
func essenceCandidates(items []*model.ItemView, ref model.PortableRef, accept model.ItemFilter) []*model.ItemView {
	if len(items) < 2 {
		return accept.Admit(items)
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
		return accept.Admit(cand)
	}
	var best []*model.ItemView
	bestDelta := int64(math.MaxInt64)
	for _, v := range accept.Admit(cand) {
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
// verifies each candidate against the full vector. It returns the admitted items scoring
// the best similarity at or above the floor, several when they tie. A short or corrupt
// fingerprint with no terms, or an algorithm or kind with no candidates, returns nil. The
// floor is inclusive (>=) to match FindAltEncodings, so a candidate that lands exactly on
// the threshold resolves here the same way it would group there.
func (l *Library) fingerprintCandidates(ctx context.Context, ref model.PortableRef, opts ResolveOptions) ([]*model.ItemView, error) {
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
	type scored struct {
		pid model.PID
		sim float64
	}
	var above []scored
	var top float64
	for _, c := range cands {
		if c.ItemPID == "" {
			continue
		}
		// The probe guarantees c shares the ref's fingerprint algorithm, so dispatching
		// on the candidate's algo picks the matching (pure-Go vs Chromaprint) function.
		if sim := fingerprint.SimilarByAlgo(c.AlgoVersion, qSub, fingerprint.Unpack(c.FP)); sim >= altSimilarityFloor {
			above = append(above, scored{c.ItemPID, sim})
			top = max(top, sim)
		}
	}
	if opts.Accept == nil {
		// Unscoped, only the best can answer, so only they are read.
		above = slices.DeleteFunc(above, func(c scored) bool { return c.sim < top })
	}
	if len(above) == 0 {
		return nil, nil
	}
	pids := make([]model.PID, len(above))
	for i, c := range above {
		pids[i] = c.pid
	}
	views, err := l.store.ItemsByPIDs(ctx, pids)
	if err != nil {
		return nil, err
	}
	admitted := make(map[model.PID]*model.ItemView, len(views))
	for _, v := range opts.Accept.Admit(views) {
		admitted[v.PID] = v
	}
	var best []*model.ItemView
	var bestSim float64
	for _, c := range above {
		v, ok := admitted[c.pid]
		switch {
		case !ok:
		case c.sim > bestSim:
			best, bestSim = []*model.ItemView{v}, c.sim
		case c.sim == bestSim && !slices.Contains(best, v):
			best = append(best, v)
		}
	}
	return best, nil
}

// descriptiveCandidates runs the descriptive rung, dispatching on kind (an empty kind is
// treated as track). It seeds on the artist or author entity, which is joinable, then
// filters the seed in Go by title, by duration (tracks only), and by an album or series
// tie-break. The album is not joinable because its match_key embeds a local folder path,
// so it is compared as text instead. When the ref's artist match key is empty the rung
// returns early, since a query for an empty match key could never hit anything useful.
func (l *Library) descriptiveCandidates(ctx context.Context, ref model.PortableRef, opts ResolveOptions) ([]*model.ItemView, error) {
	artistKey := identity.MatchKey(ref.Artist)
	if artistKey == "" {
		return nil, nil
	}
	titleKey, albumKey := identity.MatchKey(ref.Title), identity.MatchKey(ref.Album)
	switch ref.Kind {
	case model.KindBook:
		seeds, err := l.store.ItemsByAuthorKey(ctx, artistKey)
		if err != nil {
			return nil, err
		}
		return albumTiebreak(opts.Accept.Admit(descriptiveBooks(seeds, titleKey)), albumKey, func(v *model.ItemView) string {
			return identity.MatchKey(v.Series)
		}), nil
	case model.KindEpisode:
		return nil, nil
	default:
		seeds, err := l.store.ItemsByArtistKey(ctx, artistKey)
		if err != nil {
			return nil, err
		}
		return albumTiebreak(opts.Accept.Admit(descriptiveTracks(seeds, ref, titleKey)), albumKey, func(v *model.ItemView) string {
			return identity.MatchKey(v.Album)
		}), nil
	}
}

// descriptiveTracks keeps the seed rows whose title matches and, when both durations are
// known, whose duration is within descriptiveDurationTolMS. The album only breaks ties
// (albumTiebreak); it never filters. So one title survivor matches even when its album
// differs, because a recording turns up under many album titles.
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
	return survivors
}

// descriptiveBooks keeps the seed rows whose title matches (no duration gate: books are
// long and multi-file); a tie breaks on the series, which a book ref carries in its Album
// field.
func descriptiveBooks(seeds []*model.ItemView, titleKey string) []*model.ItemView {
	var survivors []*model.ItemView
	for _, v := range seeds {
		if identity.MatchKey(v.Title) == titleKey {
			survivors = append(survivors, v)
		}
	}
	return survivors
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
	refs, _, err := l.PortableRefs(ctx, pids)
	return refs, err
}

// PortableRefs returns a portable identity descriptor for each pid in order, a pid
// listed twice described twice, and names in missing, in order, the pids that name no
// item. It is the export half of a list the host holds itself, such as a queue or a
// selection; ExportPlaylistRefs is it for a playlist's entries.
func (l *Library) PortableRefs(ctx context.Context, pids []model.PID) (refs []model.PortableRef, missing []model.PID, err error) {
	byPID, err := l.store.ItemIdentities(ctx, pids)
	if err != nil {
		return nil, nil, err
	}
	refs = make([]model.PortableRef, 0, len(pids))
	for _, pid := range pids {
		if ref, ok := byPID[pid]; ok {
			refs = append(refs, ref)
		} else {
			missing = append(missing, pid)
		}
	}
	return refs, missing, nil
}

// ResolvePlaylistRefs resolves a batch of portable refs to local items, preserving input
// order and reporting each entry's matched rung (MatchNone for a miss). The host derives
// the resolved and missing sets and rebuilds a local playlist from the resolved PIDs with
// the existing Playlists() primitives.
func (l *Library) ResolvePlaylistRefs(ctx context.Context, refs []model.PortableRef) ([]model.RefResolution, error) {
	return l.ResolvePlaylistRefsWith(ctx, refs, ResolveOptions{})
}

// ResolvePlaylistRefsWith is ResolvePlaylistRefs with options, each entry resolved as
// ResolveRefWith resolves it. A ref the batch names more than once is resolved once.
func (l *Library) ResolvePlaylistRefsWith(ctx context.Context, refs []model.PortableRef, opts ResolveOptions) ([]model.RefResolution, error) {
	out := make([]model.RefResolution, 0, len(refs))
	seen := make(map[[sha256.Size]byte]model.RefResolution, len(refs))
	for _, ref := range refs {
		key := refDigest(ref)
		if res, ok := seen[key]; ok {
			res.Tied = slices.Clone(res.Tied)
			out = append(out, res)
			continue
		}
		m, err := l.ResolveRefWith(ctx, ref, opts)
		if err != nil {
			return nil, err
		}
		res := model.RefResolution{Ref: ref, Rung: m.Rung, Candidates: m.Candidates}
		if m.Item != nil {
			res.PID = m.Item.PID
		}
		for _, v := range m.Tied {
			res.Tied = append(res.Tied, v.PID)
		}
		seen[key] = res
		out = append(out, res)
	}
	return out, nil
}

// refDigest is a digest of every field of a ref, a fingerprint's bytes included, each
// length-prefixed so neighbors cannot trade bytes, so two refs share one only when they
// are the same ref.
func refDigest(ref model.PortableRef) [sha256.Size]byte {
	h := sha256.New()
	var n [8]byte
	num := func(v int64) {
		binary.BigEndian.PutUint64(n[:], uint64(v))
		h.Write(n[:])
	}
	text := func(s string) {
		num(int64(len(s)))
		io.WriteString(h, s)
	}
	text(string(ref.Kind))
	text(ref.Essence)
	num(int64(len(ref.Fingerprint)))
	h.Write(ref.Fingerprint)
	num(int64(ref.FingerprintAlgo))
	num(ref.FingerprintBucket)
	for _, s := range []string{ref.MBID, ref.ASIN, ref.ISBN, ref.Artist, ref.Title, ref.Album} {
		text(s)
	}
	num(ref.DurationMS)
	return [sha256.Size]byte(h.Sum(nil))
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
