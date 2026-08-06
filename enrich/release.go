package enrich

import (
	"regexp"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/model"
)

// This file holds the album release matcher: given an album's own identifiers and
// the releases MusicBrainz returned for its release group, decide which release the
// local album is, or decide nothing.
//
// There is no score, no threshold, and no tie-break anywhere in it, and that is the
// design rather than an omission. Every writer of an entity MBID fills only when
// empty, so a best-of-a-bad-lot winner would be permanent and unappealable, and a
// float invites a later maintainer to lower the bar when coverage disappoints. What
// decides a match is a uniqueness gate, the shape pickEssenceMatch already uses
// ("only when that closest item stands alone").
//
// Track and disc counts stay out: they are the signals a release group's reissue
// variants share, so where counts are the only evidence they do not discriminate.
// releaseEdition carries formats and countries only, so they stay unreachable.
//
// # The edition tier
//
// Barcode and catalog number name an edition; media and country only describe one, and
// the third tier decides on those. Two invariants follow, and getting either wrong is
// the whole risk of this tier.
//
// Uninterpretable input fails toward inclusion on both sides: an album-side value the
// matcher cannot read drops its predicate, a release-side one makes that release a
// candidate. Neither ever excludes.
//
// A predicate that does apply may only widen. The identifier tiers are safe because the
// compared set is selected by the identifier itself, so a wrong barcode retrieves
// nothing and the tier refuses. An
// edition predicate runs over the whole group and excludes, so one that is too strict
// removes the true release and hands uniqueness to a sibling. Three real cases:
//
//   - In Rainbows: the GB discbox has mediums 12" Vinyl, CD, Enhanced CD. Ripping its CD
//     gives MEDIA=CD, and "the release's single format equals the tag" would exclude the
//     true release, leaving one other GB CD to win.
//   - Nevermind has six US 12" Vinyl releases. MEDIA=Vinyl under a rule that does not
//     treat Vinyl and 12" Vinyl as compatible matches only the generically-stored one.
//   - The same failure runs the other way, which is commoner: Picard writes
//     12" Vinyl verbatim, excluding a release stored as plain Vinyl. Hence the
//     symmetric ancestry closure.
//
// Widening turns a decision into a refusal only if the true release is in the fetched
// set, and MusicBrainz coverage is incomplete. A pressing it does not know still lets
// the predicate run over the siblings it does, so a wrong-but-unique sibling can win.
// Never claim this tier cannot produce a wrong match: it cannot produce one among the
// editions MusicBrainz knows. The mitigation is providerMBEdition, its own marker in
// entity_enrichment, so these writes stay findable and undoable.
//
// Media alone, country alone, or both may decide, whichever survived interpretation.
// Country alone is deliberate: it resolves a regional pressing, which media cannot.
// Requiring both would pick an arbitrary point on the coverage/risk curve without
// touching the coverage gap, which applies to every combination equally.

// gtinWidth is the GTIN-14 width both sides of a barcode comparison are padded to.
// model.NormalizeBarcode accepts 8, 12, and 13 digits and returns them unchanged, so
// a CD tagged with a 12-digit UPC and a MusicBrainz release storing the same number
// as a zero-padded 13-digit EAN are two spellings of one barcode. The target
// population is CD rips, so that is the common case rather than an edge.
const gtinWidth = 14

// normBarcode returns a barcode's GTIN-14 form, or "" when it is absent or fails
// normalization (length and check digit). Normalizing both sides matters because a
// scan stores identifiers verbatim, so album.barcode can carry the spaces it was
// tagged with.
func normBarcode(s string) string {
	v, ok := model.NormalizeBarcode(strings.TrimSpace(s))
	if !ok || v == "" {
		return ""
	}
	return strings.Repeat("0", gtinWidth-len(v)) + v
}

// foldAlnum folds a value for comparison: uppercased with every non-alphanumeric
// dropped, so "SHVL 804" and "shvl-804" are one catalog number, and `12" Vinyl` and
// "12VINYL" are one format. Both tiers share it so a change to the folding rule cannot
// land in one and silently diverge the other. It is idempotent.
func foldAlnum(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(s) {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// barcodeSpellings returns the forms of a barcode to search for, or nothing when it
// is absent or malformed. MusicBrainz stores it as entered, and a UPC-A is routinely
// held as the zero-padded EAN-13 and vice versa, so both are asked for. Not the
// GTIN-14 normBarcode compares on: no release carries a shipping-container code, so
// querying it would only recognize hits it never retrieved.
func barcodeSpellings(barcode string) []string {
	v, ok := model.NormalizeBarcode(strings.TrimSpace(barcode))
	if !ok || v == "" {
		return nil
	}
	switch {
	case len(v) == 12:
		return []string{v, "0" + v}
	case len(v) == 13 && strings.HasPrefix(v, "0"):
		return []string{v, v[1:]}
	}
	return []string{v}
}

// matchRelease picks the release that t names, or returns ("", "") to write nothing.
// byBarcode and byCatNo are what the two identifier searches returned; each tier is
// judged only against its own results, so a release surfaced by a barcode query can
// never be adopted on a catalog number that query never constrained.
//
// A release is accepted when the album's barcode equals its barcode and exactly one
// returned release satisfies that, or, failing that, when the album's catalog number
// equals one of its label-info catalog numbers and exactly one returned release
// satisfies that. A tie at either tier writes nothing.
//
// Folding a catalog number widens the comparison, not the search: the query goes out as
// the tagged string, unlike a barcode's two spellings, so a release stored under other
// punctuation is never retrieved. What folding buys is the uniqueness gate, where two
// spellings of one number count as one release instead of a tie that writes nothing.
//
// The identifier is re-checked against the returned documents rather than the search
// hit count being taken as the answer. The query already constrains by identifier, so
// this is redundant on a well-behaved server, and that is the point: it is the same
// belt-and-braces searchReleaseGroup applies when it re-checks the title and artist
// it just searched on. Uniqueness is then a property of the compared set rather than
// of the search engine's ranking.
func matchRelease(t model.EnrichTarget, byBarcode, byCatNo []mbRelease) (mbid, reason string) {
	if want := normBarcode(t.Barcode); want != "" {
		if id, ok := soleRelease(t.ReleaseGroupMBID, byBarcode, func(r *mbRelease) bool {
			return normBarcode(r.Barcode) == want
		}); ok {
			return id, "barcode"
		}
	}
	if want := foldAlnum(t.CatalogNumber); want != "" {
		if id, ok := soleRelease(t.ReleaseGroupMBID, byCatNo, func(r *mbRelease) bool {
			for _, li := range r.LabelInfo {
				if foldAlnum(li.CatalogNumber) == want {
					return true
				}
			}
			return false
		}); ok {
			return id, "catalog number"
		}
	}
	return "", ""
}

// soleRelease returns the one release satisfying pred, and false when none or more
// than one distinct release does. A release must also clear minMatchScore and belong
// to rgMBID; a document carrying no release group cannot be verified and so is
// discarded rather than trusted, which fails closed on a write that would be
// permanent.
//
// Both extra gates read fields only a search result has, so the edition tier, working
// from browse documents, calls soleMatching directly instead.
func soleRelease(rgMBID string, releases []mbRelease, pred func(*mbRelease) bool) (string, bool) {
	ids := make([]string, 0, len(releases))
	for i := range releases {
		r := &releases[i]
		if r.ID == "" || r.Score < minMatchScore {
			continue
		}
		if r.ReleaseGroup == nil || !strings.EqualFold(r.ReleaseGroup.ID, rgMBID) {
			continue
		}
		if pred(r) {
			ids = append(ids, r.ID)
		}
	}
	return soleID(ids)
}

// soleID returns the one distinct id in ids, false when there are none or several. It
// folds case, so the same release returned twice is still one release.
func soleID(ids []string) (string, bool) {
	var found string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if found != "" && !strings.EqualFold(found, id) {
			return "", false
		}
		found = id
	}
	return found, found != ""
}

// soleMatching returns the one edition satisfying pred, false otherwise. It is the
// uniqueness core, and for the edition tier the only live gate: a browse document has
// no score and no release group to re-verify.
func soleMatching(editions []releaseEdition, pred func(releaseEdition) bool) (string, bool) {
	ids := make([]string, 0, len(editions))
	for _, e := range editions {
		if pred(e) {
			ids = append(ids, e.ID)
		}
	}
	return soleID(ids)
}

// releaseEdition is one release projected down to what the edition tier compares.
// Nothing else survives, which is what keeps counts out of the matcher.
type releaseEdition struct {
	ID        string   `json:"id"`
	Barcode   string   `json:"barcode,omitempty"`
	Formats   []string `json:"formats,omitempty"`
	Countries []string `json:"countries,omitempty"`
}

// releaseGroupEditions is a group's projected editions plus the browse's release-count.
// A nil Editions with Count above maxGroupReleases is the cached "too big to decide
// over" answer, not a missing read.
type releaseGroupEditions struct {
	Count    int              `json:"count"`
	Editions []releaseEdition `json:"editions,omitempty"`
}

// complete reports whether uniqueness may be judged over this set: every release
// accounted for, and the group small enough to enumerate.
func (g releaseGroupEditions) complete() bool {
	return g.Count <= maxGroupReleases && len(g.Editions) >= g.Count
}

// editionOf projects a browse document. The country set unions the top-level country
// with every release event's ISO codes, since a multi-country release names one.
func editionOf(r *mbRelease) releaseEdition {
	e := releaseEdition{ID: r.ID, Barcode: strings.TrimSpace(r.Barcode)}
	for _, m := range r.Media {
		if f := strings.TrimSpace(m.Format); f != "" {
			e.Formats = append(e.Formats, f)
		}
	}
	seen := map[string]bool{}
	addCountry := func(c string) {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		e.Countries = append(e.Countries, c)
	}
	addCountry(r.Country)
	for _, ev := range r.ReleaseEvents {
		if ev.Area == nil {
			continue
		}
		for _, c := range ev.Area.ISO31661Codes {
			addCountry(c)
		}
	}
	return e
}

// mediaCountPrefix splits a leading disc count off a media token. Group 1 follows an
// explicit multiplier (2xCD, 3 x LP); group 2 follows a bare digit run and a letter,
// with or without space between them, so "2CD" and "2 CD" both strip. What marks a size
// rather than a count is the inch marker, so `12" Vinyl` matches neither branch and keeps
// its 12, staying distinct from `7" Vinyl`.
//
// "12 Vinyl", unquoted, is genuinely ambiguous and reads as a count, so it offers VINYL
// as well as 12VINYL. That only widens (a 7" release joins the survivors and the tier
// refuses on the tie), which is the safe direction; requiring the letter to follow the
// digits directly would instead drop the whole predicate for "2 CD" and "3 LP".
var mediaCountPrefix = regexp.MustCompile(`^\s*\d+(?:\s*[xX×]\s*(.+)|(\s*[a-zA-Z].*))$`)

// mediaAlternatives turns the album's raw media column into the formats it may be
// offering, never a single narrowed value. It splits on , ; and / and adds a
// count-stripped form as an extra alternative rather than a replacement. Junk tokens are free: the predicate is a
// disjunction, so a Discogs-style "CD, Album, Reissue" contributes CD and nothing else.
func mediaAlternatives(raw string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s = foldAlnum(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, tok := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '/'
	}) {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		add(tok)
		// Count-stripping runs on the raw token, before folding drops the quote that
		// distinguishes a size from a count. Exactly one group is ever non-empty.
		if m := mediaCountPrefix.FindStringSubmatch(tok); m != nil {
			add(m[1] + m[2])
		}
	}
	return out
}

// mediaSynonyms folds non-MusicBrainz spellings onto the canonical folded format name.
//
// Codec names are deliberately absent, and the omission looks harmless but is not. A
// ripper writing FLAC or WAV into MEDIA describes the file, not the edition, and those
// are the classic CD-rip codecs, so mapping them to Digital Media would hand a group's
// lone digital release a uniqueness win over a CD rip. A broken tag reads as no
// evidence, which interpretability already gives.
var mediaSynonyms = map[string]string{
	"COMPACTDISC": "CD",
	"CDDA":        "CD",
	"CDAUDIO":     "CD",
	"LP":          "VINYL",

	"DIGITAL":                 "DIGITALMEDIA",
	"WEB":                     "DIGITALMEDIA",
	"FILE":                    "DIGITALMEDIA",
	"DOWNLOAD":                "DIGITALMEDIA",
	"DIGITALDOWNLOAD":         "DIGITALMEDIA",
	"INTERNETDOWNLOAD":        "DIGITALMEDIA",
	"LOSSLESSDIGITALDOWNLOAD": "DIGITALMEDIA",
	"STREAMING":               "DIGITALMEDIA",
}

// formatAncestors maps a folded MusicBrainz format to its parents, multi-parent because
// a hybrid disc genuinely is two things. It may only ever be widened: removing an entry
// turns a compatible pair into an excluding one. Siblings stay distinct.
var formatAncestors = map[string][]string{
	"8CMCD":                           {"CD"},
	"BLUSPECCD":                       {"CD"},
	"CDR":                             {"CD"},
	"CDG":                             {"CD"},
	"COPYCONTROLCD":                   {"CD"},
	"DATACD":                          {"CD"},
	"DTSCD":                           {"CD"},
	"ENHANCEDCD":                      {"CD"},
	"HDCD":                            {"CD"},
	"MINIMAXCD":                       {"CD"},
	"SHMCD":                           {"CD"},
	"HYBRIDSACD":                      {"SACD"},
	"HYBRIDSACDSACDLAYER2CHANNELS":    {"SACD"},
	"HYBRIDSACDSACDLAYERMULTICHANNEL": {"SACD"},
	// Plays in a CD transport and an SACD player, hence the multi-parent table.
	"HYBRIDSACDCDLAYER":  {"CD", "SACD"},
	"7VINYL":             {"VINYL"},
	"10VINYL":            {"VINYL"},
	"12VINYL":            {"VINYL"},
	"FLEXIDISC":          {"VINYL"},
	"VINYLDISCCDSIDE":    {"CD"},
	"VINYLDISCVINYLSIDE": {"VINYL"},
	"VINYLDISCDVDSIDE":   {"DVD"},
	"DVDAUDIO":           {"DVD"},
	"DVDVIDEO":           {"DVD"},
}

// mediaFlatFormats are MusicBrainz formats with no ancestry to record: they are neither
// a parent nor a child, so nothing above puts them in the vocabulary. Without them a
// media-only album tagged "Cassette" reads as uninterpretable before the fetch and never
// browses its group at all, which is the one case the group-derived clause below cannot
// rescue. Widen freely.
var mediaFlatFormats = []string{
	"CASSETTE", "MINIDISC", "DAT", "DCC", "MICROCASSETTE", "REELTOREEL",
	"8TRACKCARTRIDGE", "SHELLAC", "PIANOROLL", "WAXCYLINDER",
	"BLURAY", "HDDVD", "LASERDISC", "VHS", "BETAMAX", "VCD", "SVCD",
	"USBFLASHDRIVE", "SDCARD", "SLOTMUSIC", "PLAYBUTTON",
}

// mediaVocabulary is every folded name the tables above know, derived so it cannot
// drift from them.
var mediaVocabulary = func() map[string]bool {
	v := map[string]bool{}
	for child, parents := range formatAncestors {
		v[child] = true
		for _, p := range parents {
			v[p] = true
		}
	}
	for spelling, canon := range mediaSynonyms {
		v[spelling] = true
		v[canon] = true
	}
	for _, f := range mediaFlatFormats {
		v[f] = true
	}
	return v
}()

// formatClosure returns a folded format and every ancestor above it.
func formatClosure(f string) []string {
	out := []string{f}
	for i := 0; i < len(out); i++ {
		for _, p := range formatAncestors[out[i]] {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// mediaCanonical resolves a folded token through the synonym table, on both sides.
// MusicBrainz stores no synonym spelling today, so the release side is defensive, but
// omitting it there would let a release be excluded by an album carrying the very same
// spelling, and the release side must fail toward inclusion.
func mediaCanonical(f string) string {
	if c, ok := mediaSynonyms[f]; ok {
		return c
	}
	return f
}

// formatsCompatible reports whether two folded formats may describe one edition: equal,
// or either an ancestor of the other. Closed symmetrically, so a specific tag accepts a
// generic release and vice versa.
func formatsCompatible(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return slices.Contains(formatClosure(a), b) || slices.Contains(formatClosure(b), a)
}

// mediaInterpretable reports whether any alternative names a format in mediaVocabulary
// or one the fetched group actually uses. The second clause only ever fires after a
// fetch, so it adds discrimination to an album that already had an interpretable value;
// a format the tables have never heard of is what mediaFlatFormats exists to keep out of
// that gap, since editionEvidence gates the fetch on the tables alone.
func mediaInterpretable(alts []string, editions []releaseEdition) bool {
	for _, a := range alts {
		c := mediaCanonical(a)
		if mediaVocabulary[c] || mediaVocabulary[a] {
			return true
		}
		for _, e := range editions {
			for _, f := range e.Formats {
				if mediaCanonical(foldAlnum(f)) == c {
					return true
				}
			}
		}
	}
	return false
}

// mediaCompatible reports whether any of a release's medium formats is compatible with
// any alternative. A release with no format is uninterpretable, so it stays a candidate.
func mediaCompatible(alts []string, e releaseEdition) bool {
	if len(e.Formats) == 0 {
		return true
	}
	for _, f := range e.Formats {
		rf := mediaCanonical(foldAlnum(f))
		for _, a := range alts {
			if formatsCompatible(mediaCanonical(a), rf) {
				return true
			}
		}
	}
	return false
}

// countryAlternatives splits the raw country column on , ; / and & ("US & Europe"),
// adding a token's canonical alpha-2 form alongside the original rather than instead of
// it. That
// covers an alpha-3 code and UK, which MusicBrainz stores as GB.
func countryAlternatives(raw string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, tok := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '/' || r == '&'
	}) {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		add(tok)
		if a2, ok := model.CountryAlias(tok); ok {
			add(a2)
		}
	}
	return out
}

// supraNational is the set MusicBrainz uses for a release with no single country.
var supraNational = map[string]bool{"XW": true, "XE": true}

// countryInterpretable reports whether any alternative is a comparable country code. An
// album-side XW/XE is not: resolving it to members needs a table WaxBin does not carry,
// so it drops the predicate rather than excluding every specific-country release.
func countryInterpretable(alts []string) bool {
	for _, a := range alts {
		if len(a) == 2 && !supraNational[a] {
			return true
		}
	}
	return false
}

// countryCompatible reports whether a release matches any country alternative. An empty
// or only-XW/XE country set is unresolvable, so it stays a candidate: it may create a
// tie, never an exclusion.
func countryCompatible(alts []string, e releaseEdition) bool {
	specific := false
	for _, c := range e.Countries {
		if !supraNational[c] {
			specific = true
		}
		if slices.Contains(alts, c) {
			return true
		}
	}
	return !specific
}

// mediaAffirmed reports whether some release positively carries a compatible format, as
// opposed to surviving only because its format is unrecorded.
func mediaAffirmed(alts []string, editions []releaseEdition) bool {
	for _, e := range editions {
		if len(e.Formats) > 0 && mediaCompatible(alts, e) {
			return true
		}
	}
	return false
}

// countryAffirmed reports whether some release positively names one of the alternatives.
// A supra-national code does not count: it is exactly the unresolvable value that makes a
// release a candidate for everything.
func countryAffirmed(alts []string, editions []releaseEdition) bool {
	for _, e := range editions {
		for _, c := range e.Countries {
			if !supraNational[c] && slices.Contains(alts, c) {
				return true
			}
		}
	}
	return false
}

// matchEdition picks the release t's media and country describe, or writes nothing. An
// incomplete fetched set refuses outright: uniqueness over a partial group is not
// uniqueness.
//
// The barcode check is free coverage, not a fourth tier. A browse carries barcode
// anyway, and uniqueness over the whole group beats uniqueness over a search-filtered
// subset. Browsing for every barcode album whose search missed would reopen tier
// ordering, so it is not done.
func matchEdition(t model.EnrichTarget, g releaseGroupEditions) (mbid, reason string, edition bool) {
	if !g.complete() || len(g.Editions) == 0 {
		return "", "", false
	}
	if want := normBarcode(t.Barcode); want != "" {
		if id, ok := soleMatching(g.Editions, func(e releaseEdition) bool {
			return normBarcode(e.Barcode) == want
		}); ok {
			// A printed identifier, so not an edition-tier decision however it was reached.
			return id, "barcode in group", false
		}
	}
	mediaAlts, countryAlts := editionPredicates(t, g.Editions)
	// A predicate no release affirms carries no positive evidence: it can only exclude,
	// leaving the releases whose value MusicBrainz does not record to win by default,
	// which is a coin flip dressed as evidence. Drop it, so a country the group does not
	// use (a nonstandard code, or a pressing MusicBrainz lacks) fails toward inclusion
	// like any other value the matcher cannot act on.
	if len(mediaAlts) > 0 && !mediaAffirmed(mediaAlts, g.Editions) {
		mediaAlts = nil
	}
	if len(countryAlts) > 0 && !countryAffirmed(countryAlts, g.Editions) {
		countryAlts = nil
	}
	if len(mediaAlts) == 0 && len(countryAlts) == 0 {
		return "", "", false
	}
	id, ok := soleMatching(g.Editions, func(e releaseEdition) bool {
		return (len(mediaAlts) == 0 || mediaCompatible(mediaAlts, e)) &&
			(len(countryAlts) == 0 || countryCompatible(countryAlts, e))
	})
	if !ok {
		return "", "", false
	}
	// Everything below decided on descriptive evidence, which is what the edition
	// provider marker records. The flag is returned rather than inferred from the reason,
	// so a reworded log line cannot change which marker gets written.
	switch {
	case len(mediaAlts) > 0 && len(countryAlts) > 0:
		return id, "medium and country", true
	case len(mediaAlts) > 0:
		return id, "medium", true
	default:
		return id, "country", true
	}
}

// editionPredicates returns the album-side alternatives that survived interpretation,
// each nil when its predicate drops. A nil editions only costs the media side its
// "a format the group uses" clause, which is what lets editionEvidence run pre-fetch.
func editionPredicates(t model.EnrichTarget, editions []releaseEdition) (media, country []string) {
	if alts := mediaAlternatives(t.Media); mediaInterpretable(alts, editions) {
		media = alts
	}
	if alts := countryAlternatives(t.Country); countryInterpretable(alts) {
		country = alts
	}
	return media, country
}

// editionEvidence reports whether an album has anything to decide on, judged without
// the group. The queue gate fires on a non-empty column, not an interpretable one, so
// MEDIA=FLAC would otherwise cost a browse to learn it says nothing. It is the
// conservative half: a format newer than the tables reads as uninterpretable here, which
// costs coverage and never correctness.
func editionEvidence(t model.EnrichTarget) bool {
	media, country := editionPredicates(t, nil)
	return len(media) > 0 || len(country) > 0
}
