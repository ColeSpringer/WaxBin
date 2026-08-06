package enrich

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// ed builds a projected edition, in MusicBrainz's own format spelling.
func ed(id string, formats []string, countries ...string) releaseEdition {
	return releaseEdition{ID: id, Formats: formats, Countries: countries}
}

// group wraps editions as a complete fetched set, which the matcher requires.
func group(eds ...releaseEdition) releaseGroupEditions {
	return releaseGroupEditions{Count: len(eds), Editions: eds}
}

// editionTarget is an album whose only evidence is media and country.
func editionTarget(media, country string) model.EnrichTarget {
	return model.EnrichTarget{
		Type: model.EnrichAlbumType, ReleaseGroupMBID: testRG, Media: media, Country: country,
	}
}

func TestFoldAlnumIsIdempotent(t *testing.T) {
	for _, s := range []string{`12" Vinyl`, "CD-R", "Hybrid SACD (CD layer)", "  compact disc ", "8cm CD"} {
		once := foldAlnum(s)
		if twice := foldAlnum(once); twice != once {
			t.Errorf("foldAlnum(%q) = %q, folding again = %q", s, once, twice)
		}
	}
}

// TestFormatAncestryIsSymmetric: both directions occur in real libraries, and siblings
// must stay distinct or a 7" and a 12" pressing collapse into one answer.
func TestFormatAncestryIsSymmetric(t *testing.T) {
	compatible := [][2]string{
		{"Vinyl", `12" Vinyl`},
		{"Vinyl", "Flexi-disc"},
		{"CD", "Enhanced CD"},
		{"CD", "SHM-CD"},
		{"CD", "Hybrid SACD (CD layer)"},
		{"SACD", "Hybrid SACD (CD layer)"},
		{"DVD", "DVD-Audio"},
		{"CD", "CD"},
	}
	for _, p := range compatible {
		a, b := foldAlnum(p[0]), foldAlnum(p[1])
		if !formatsCompatible(a, b) || !formatsCompatible(b, a) {
			t.Errorf("%q and %q should be compatible in both directions", p[0], p[1])
		}
	}
	incompatible := [][2]string{
		{`12" Vinyl`, `7" Vinyl`},
		{"CD", "Vinyl"},
		{"Enhanced CD", "SHM-CD"},
		{"DVD-Audio", "DVD-Video"},
		{"CD", "DVD"},
	}
	for _, p := range incompatible {
		a, b := foldAlnum(p[0]), foldAlnum(p[1])
		if formatsCompatible(a, b) || formatsCompatible(b, a) {
			t.Errorf("%q and %q should be distinct in both directions", p[0], p[1])
		}
	}
}

// TestFormatCompatibilityIsReflexiveAndSymmetric runs over the real vocabulary rather
// than a hand-picked list, which catches a later table entry with a typo in it.
func TestFormatCompatibilityIsReflexiveAndSymmetric(t *testing.T) {
	var vocab []string
	for f := range mediaVocabulary {
		vocab = append(vocab, f)
	}
	slices.Sort(vocab)
	for _, a := range vocab {
		if !formatsCompatible(a, a) {
			t.Errorf("formatsCompatible(%q, %q) = false, want reflexive", a, a)
		}
		for _, b := range vocab {
			if formatsCompatible(a, b) != formatsCompatible(b, a) {
				t.Errorf("formatsCompatible is asymmetric for (%q, %q)", a, b)
			}
		}
	}
}

func TestMediaAlternatives(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"CD", []string{"CD"}},
		// A Discogs-style descriptor; the junk tokens match no format.
		{"CD, Album, Reissue", []string{"CD", "ALBUM", "REISSUE"}},
		// A count contributes the stripped form ALONGSIDE the raw one, with or without
		// a space between the count and the format.
		{"2xCD", []string{"2XCD", "CD"}},
		{"3 x LP", []string{"3XLP", "LP"}},
		{"2CD", []string{"2CD", "CD"}},
		{"2 CD", []string{"2CD", "CD"}},
		{"3 LP", []string{"3LP", "LP"}},
		// The inch marker is what marks a size, so 12" and 7" Vinyl stay distinct.
		{`12" Vinyl`, []string{"12VINYL"}},
		{`7" Vinyl`, []string{"7VINYL"}},
		// Unquoted, it is ambiguous and reads as a count too. That only widens.
		{"12 Vinyl", []string{"12VINYL", "VINYL"}},
		{"", nil},
	} {
		if got := mediaAlternatives(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("mediaAlternatives(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestMediaInterpretabilityDropsCodecs pins the codec omission: MEDIA=FLAC describes the
// file, and mapping it to Digital Media would hand a group's lone digital release a win.
func TestMediaInterpretabilityDropsCodecs(t *testing.T) {
	eds := []releaseEdition{
		ed("rel-cd", []string{"CD"}),
		ed("rel-digital", []string{"Digital Media"}),
	}
	for _, codec := range []string{"FLAC", "WAV", "MP3", "ALAC"} {
		if mediaInterpretable(mediaAlternatives(codec), eds) {
			t.Errorf("MEDIA=%s reads as interpretable; a codec must be no evidence", codec)
		}
	}
	// So the tier refuses rather than picking the digital release.
	if id, _, _ := matchEdition(editionTarget("FLAC", ""), group(eds...)); id != "" {
		t.Errorf("MEDIA=FLAC matched %q, want no match", id)
	}
	// A real digital spelling still works.
	if !mediaInterpretable(mediaAlternatives("Digital Download"), eds) {
		t.Error("MEDIA=Digital Download should interpret through the synonym table")
	}
}

// TestMediaInterpretableFromTheGroupsOwnFormats: the tables are not a coverage ceiling,
// since a format MusicBrainz adds later interprets once the group is in hand.
func TestMediaInterpretableFromTheGroupsOwnFormats(t *testing.T) {
	alts := mediaAlternatives("Holographic Disc")
	if mediaInterpretable(alts, nil) {
		t.Error("a format no table knows should not interpret without the group's formats")
	}
	eds := []releaseEdition{ed("rel-a", []string{"Holographic Disc"})}
	if !mediaInterpretable(alts, eds) {
		t.Error("a format the group actually uses should interpret")
	}
}

// TestFlatFormatsInterpretBeforeTheFetch is the gap mediaFlatFormats closes. A cassette
// has no ancestry to record, so without the list a media-only album carrying one reads
// as no evidence and never browses its group, which is the one case the group-derived
// clause above cannot rescue because it runs after the fetch it would have to justify.
func TestFlatFormatsInterpretBeforeTheFetch(t *testing.T) {
	for _, f := range []string{"Cassette", "Blu-ray", "MiniDisc", "8-Track Cartridge", "Shellac"} {
		if !editionEvidence(editionTarget(f, "")) {
			t.Errorf("MEDIA=%s should be evidence worth a browse", f)
		}
	}
	g := group(ed("rel-tape", []string{"Cassette"}, "US"), ed("rel-cd", []string{"CD"}, "US"))
	if id, _, _ := matchEdition(editionTarget("Cassette", ""), g); id != "rel-tape" {
		t.Errorf("matched %q, want rel-tape", id)
	}
}

// TestEditionRefusesAMultiFormatRelease is the In Rainbows case: a rule of "the single
// format equals the tag" would exclude the discbox and hand the answer to the other GB CD.
func TestEditionRefusesAMultiFormatRelease(t *testing.T) {
	g := group(
		ed("rel-discbox", []string{`12" Vinyl`, "CD", "Enhanced CD"}, "GB"),
		ed("rel-gb-cd", []string{"CD"}, "GB"),
	)
	if id, _, _ := matchEdition(editionTarget("CD", "GB"), g); id != "" {
		t.Errorf("matched %q; the discbox must survive the predicate and force a tie", id)
	}
	// Both really did survive: without the sibling the discbox wins.
	only := group(ed("rel-discbox", []string{`12" Vinyl`, "CD", "Enhanced CD"}, "GB"))
	if id, _, _ := matchEdition(editionTarget("CD", "GB"), only); id != "rel-discbox" {
		t.Errorf("matched %q, want rel-discbox (a multi-format release must be a candidate)", id)
	}
}

// TestEditionRefusesSiblingPressingsInBothTagDirections is the Nevermind case: sibling
// pressings stay candidates whichever spelling the library carries.
func TestEditionRefusesSiblingPressingsInBothTagDirections(t *testing.T) {
	g := group(
		ed("rel-generic", []string{"Vinyl"}, "US"),
		ed("rel-12a", []string{`12" Vinyl`}, "US"),
		ed("rel-12b", []string{`12" Vinyl`}, "US"),
	)
	for _, tagged := range []string{"Vinyl", `12" Vinyl`, "LP"} {
		if id, _, _ := matchEdition(editionTarget(tagged, "US"), g); id != "" {
			t.Errorf("MEDIA=%s matched %q, want a refusal (siblings are ambiguous)", tagged, id)
		}
	}
	// A 7" pressing is a different record and does not join the tie.
	seven := group(ed("rel-7", []string{`7" Vinyl`}, "US"), ed("rel-12", []string{`12" Vinyl`}, "US"))
	if id, _, _ := matchEdition(editionTarget(`12" Vinyl`, "US"), seven); id != "rel-12" {
		t.Errorf("matched %q, want rel-12 (a 7\" sibling is a distinct format)", id)
	}
}

func TestCountryAlternatives(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"GB", []string{"GB"}},
		{"us", []string{"US"}},
		// A three-letter code contributes its alpha-2 form alongside.
		{"USA", []string{"USA", "US"}},
		{"US & Europe", []string{"US", "EUROPE"}},
		{"EU/US", []string{"EU", "US"}},
		{"", nil},
	} {
		if got := countryAlternatives(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("countryAlternatives(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestCountryPredicateFailsTowardInclusion covers both sides: an album-side XW/XE drops
// the predicate, a release-side one stays a candidate.
func TestCountryPredicateFailsTowardInclusion(t *testing.T) {
	for _, supra := range []string{"XW", "XE"} {
		if countryInterpretable(countryAlternatives(supra)) {
			t.Errorf("album-side %s should drop the country predicate", supra)
		}
	}
	// Dropping it leaves a media tie, rather than the one XE release winning by default.
	g := group(
		ed("rel-gb", []string{"CD"}, "GB"),
		ed("rel-eu", []string{"CD"}, "XE"),
	)
	if id, _, _ := matchEdition(editionTarget("CD", "XE"), g); id != "" {
		t.Errorf("album-side XE matched %q; the predicate must drop, leaving a media tie", id)
	}

	alts := countryAlternatives("JP")
	for _, e := range []releaseEdition{
		ed("rel-worldwide", nil, "XW"),
		ed("rel-europe", nil, "XE"),
		ed("rel-unknown", nil),
	} {
		if !countryCompatible(alts, e) {
			t.Errorf("%s should stay a candidate: an unresolvable release country never excludes", e.ID)
		}
	}
	if countryCompatible(alts, ed("rel-us", nil, "US")) {
		t.Error("a release naming a different country should be excluded")
	}
	// The release-event union is what makes a multi-country release match.
	if !countryCompatible(alts, ed("rel-multi", nil, "XE", "JP")) {
		t.Error("a release whose event set includes JP should match")
	}
}

// TestEditionMatchesOnCountryAlone pins that country alone may fire: it is what a
// regional pressing turns on, which media cannot express.
func TestEditionMatchesOnCountryAlone(t *testing.T) {
	g := group(
		ed("rel-us-a", []string{"CD"}, "US"),
		ed("rel-us-b", []string{"CD"}, "US"),
		ed("rel-jp", []string{"CD"}, "JP"),
	)
	id, reason, _ := matchEdition(editionTarget("", "JP"), g)
	if id != "rel-jp" || reason != "country" {
		t.Errorf("matchEdition = (%q, %q), want (rel-jp, country)", id, reason)
	}
	// Tagged only with its medium the same album is ambiguous, which is the asymmetry.
	if got, _, _ := matchEdition(editionTarget("CD", ""), g); got != "" {
		t.Errorf("media alone matched %q, want a refusal", got)
	}
	// USA folds to US, so an alpha-3 tag reaches the same answer.
	usOnly := group(ed("rel-us", []string{"CD"}, "US"), ed("rel-jp", []string{"CD"}, "JP"))
	if got, _, _ := matchEdition(editionTarget("", "USA"), usOnly); got != "rel-us" {
		t.Errorf("country USA matched %q, want rel-us", got)
	}
}

func TestEditionReasonNamesTheEvidence(t *testing.T) {
	g := group(
		ed("rel-gb-cd", []string{"CD"}, "GB"),
		ed("rel-gb-vinyl", []string{"Vinyl"}, "GB"),
		ed("rel-us-cd", []string{"CD"}, "US"),
	)
	if id, reason, _ := matchEdition(editionTarget("CD", "GB"), g); id != "rel-gb-cd" || reason != "medium and country" {
		t.Errorf("matchEdition = (%q, %q), want (rel-gb-cd, medium and country)", id, reason)
	}
	vinylOnly := group(ed("rel-cd", []string{"CD"}), ed("rel-vinyl", []string{"Vinyl"}))
	if id, reason, _ := matchEdition(editionTarget("Vinyl", ""), vinylOnly); id != "rel-vinyl" || reason != "medium" {
		t.Errorf("matchEdition = (%q, %q), want (rel-vinyl, medium)", id, reason)
	}
}

// TestEditionTakesAFreeBarcodeWin: a browse carries barcode anyway, so a barcode unique
// over the WHOLE group decides first, at no extra request.
func TestEditionTakesAFreeBarcodeWin(t *testing.T) {
	withBarcode := ed("rel-a", []string{"CD"}, "US")
	withBarcode.Barcode = "0075992739429"
	g := group(withBarcode, ed("rel-b", []string{"CD"}, "US"))
	tgt := editionTarget("CD", "US")
	tgt.Barcode = "075992739429" // the UPC-A spelling of the same code
	id, reason, _ := matchEdition(tgt, g)
	if id != "rel-a" || reason != "barcode in group" {
		t.Errorf("matchEdition = (%q, %q), want (rel-a, barcode in group)", id, reason)
	}
}

// TestEditionRefusesAnIncompleteSet: uniqueness over part of a group is not uniqueness.
func TestEditionRefusesAnIncompleteSet(t *testing.T) {
	overCap := releaseGroupEditions{Count: maxGroupReleases + 1}
	if id, _, _ := matchEdition(editionTarget("CD", "JP"), overCap); id != "" {
		t.Errorf("an over-cap group matched %q, want no match", id)
	}
	short := releaseGroupEditions{Count: 5, Editions: []releaseEdition{ed("rel-jp", []string{"CD"}, "JP")}}
	if id, _, _ := matchEdition(editionTarget("CD", "JP"), short); id != "" {
		t.Errorf("a short read matched %q, want no match", id)
	}
}

// TestEditionEvidenceGatesTheFetch: the queue gate fires on a non-empty column, so this
// is what keeps a MEDIA=FLAC album from spending a browse to learn it says nothing.
func TestEditionEvidenceGatesTheFetch(t *testing.T) {
	for _, tc := range []struct {
		media, country string
		want           bool
	}{
		{"CD", "", true},
		{"", "GB", true},
		{"2xCD", "US & Europe", true},
		{"FLAC", "", false},
		{"", "XW", false},
		{"", "", false},
		{"CD, Album, Reissue", "", true},
	} {
		if got := editionEvidence(editionTarget(tc.media, tc.country)); got != tc.want {
			t.Errorf("editionEvidence(media=%q, country=%q) = %v, want %v",
				tc.media, tc.country, got, tc.want)
		}
	}
}

// TestUninterpretableInputMatchesAbsentInput states the interpretation invariant as an
// equivalence. If these diverge, an unreadable tag has become a predicate that excludes.
func TestUninterpretableInputMatchesAbsentInput(t *testing.T) {
	g := group(
		ed("rel-gb-cd", []string{"CD"}, "GB"),
		ed("rel-us-cd", []string{"CD"}, "US"),
		ed("rel-gb-vinyl", []string{"Vinyl"}, "GB"),
	)
	for _, tc := range []struct{ garbage, absent model.EnrichTarget }{
		{editionTarget("FLAC", "GB"), editionTarget("", "GB")},
		{editionTarget("CD", "XW"), editionTarget("CD", "")},
		{editionTarget("FLAC", "XE"), editionTarget("", "")},
	} {
		gotID, gotReason, _ := matchEdition(tc.garbage, g)
		wantID, wantReason, _ := matchEdition(tc.absent, g)
		if gotID != wantID || gotReason != wantReason {
			t.Errorf("media=%q country=%q gave (%q,%q); the same album with those values absent gave (%q,%q)",
				tc.garbage.Media, tc.garbage.Country, gotID, gotReason, wantID, wantReason)
		}
	}
}

// TestAddingAnAlternativeNeverShrinksTheSurvivors is the widening invariant: an
// alternative that removed a survivor could delete the true release.
func TestAddingAnAlternativeNeverShrinksTheSurvivors(t *testing.T) {
	eds := []releaseEdition{
		ed("rel-cd", []string{"CD"}, "GB"),
		ed("rel-shm", []string{"SHM-CD"}, "JP"),
		ed("rel-vinyl", []string{"Vinyl"}, "US"),
		ed("rel-12", []string{`12" Vinyl`}, "US"),
		ed("rel-box", []string{"CD", "DVD-Video"}, "XE"),
		ed("rel-nowhere", []string{"Cassette"}),
	}
	survivors := func(alts []string) []string {
		var out []string
		for _, e := range eds {
			if mediaCompatible(alts, e) {
				out = append(out, e.ID)
			}
		}
		return out
	}
	base := []string{foldAlnum("CD")}
	for _, extra := range []string{"Vinyl", `12" Vinyl`, "Cassette", "REISSUE", "ALBUM", "SACD"} {
		wider := append(slices.Clone(base), foldAlnum(extra))
		before, after := survivors(base), survivors(wider)
		for _, id := range before {
			if !slices.Contains(after, id) {
				t.Errorf("adding the alternative %q dropped survivor %s (%v -> %v)", extra, id, before, after)
			}
		}
	}

	countrySurvivors := func(alts []string) []string {
		var out []string
		for _, e := range eds {
			if countryCompatible(alts, e) {
				out = append(out, e.ID)
			}
		}
		return out
	}
	cbase := []string{"GB"}
	for _, extra := range []string{"US", "JP", "XE", "EUROPE"} {
		wider := append(slices.Clone(cbase), extra)
		before, after := countrySurvivors(cbase), countrySurvivors(wider)
		for _, id := range before {
			if !slices.Contains(after, id) {
				t.Errorf("adding the country %q dropped survivor %s (%v -> %v)", extra, id, before, after)
			}
		}
	}
}

// TestNoAlbumTokenExcludesAReleaseInItsClosure is what makes the generic/specific
// asymmetry safe in both directions.
func TestNoAlbumTokenExcludesAReleaseInItsClosure(t *testing.T) {
	var vocab []string
	for f := range mediaVocabulary {
		vocab = append(vocab, f)
	}
	slices.Sort(vocab)
	for _, tok := range vocab {
		for _, rel := range formatClosure(tok) {
			e := releaseEdition{ID: "rel", Formats: []string{rel}}
			if !mediaCompatible([]string{tok}, e) {
				t.Errorf("album token %q excluded a release stored as %q, which is in its closure", tok, rel)
			}
		}
	}
}

// TestEditionOfProjectsBrowseDocuments checks the projection, including the country
// union (MusicBrainz routinely lists XE alongside the specific codes).
func TestEditionOfProjectsBrowseDocuments(t *testing.T) {
	r := &mbRelease{
		ID:      "rel-a",
		Barcode: " 0075992739429 ",
		Media:   []mbMedium{{Format: "CD"}, {Format: ""}, {Format: "DVD-Video"}},
		Country: "xe",
		ReleaseEvents: []mbReleaseEvent{
			{Area: &mbArea{ISO31661Codes: []string{"GB", "XE"}}},
			{Area: nil},
			{Area: &mbArea{ISO31661Codes: []string{"DE"}}},
		},
	}
	got := editionOf(r)
	want := releaseEdition{
		ID: "rel-a", Barcode: "0075992739429",
		Formats:   []string{"CD", "DVD-Video"},
		Countries: []string{"XE", "GB", "DE"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("editionOf = %+v, want %+v", got, want)
	}
}

// TestSoleMatchingIsTheOnlyGate: a browse document has no score and no release group, so
// soleRelease would reject every edition. The split is what keeps the tier alive.
func TestSoleMatchingIsTheOnlyGate(t *testing.T) {
	eds := []releaseEdition{ed("rel-a", []string{"CD"}), ed("rel-b", []string{"Vinyl"})}
	if id, ok := soleMatching(eds, func(e releaseEdition) bool { return e.ID == "rel-a" }); !ok || id != "rel-a" {
		t.Errorf("soleMatching = (%q, %v), want (rel-a, true)", id, ok)
	}
	if _, ok := soleMatching(eds, func(releaseEdition) bool { return true }); ok {
		t.Error("two survivors must not resolve")
	}
	if _, ok := soleMatching(eds, func(releaseEdition) bool { return false }); ok {
		t.Error("no survivor must not resolve")
	}
	// The same release twice is still one release; comparison folds case.
	dup := []releaseEdition{ed("rel-a", []string{"CD"}), ed("REL-A", []string{"CD"})}
	if id, ok := soleMatching(dup, func(releaseEdition) bool { return true }); !ok || !strings.EqualFold(id, "rel-a") {
		t.Errorf("duplicate ids = (%q, %v), want rel-a and true", id, ok)
	}
	// soleRelease rejects the equivalent document, which is why the core was extracted.
	browseish := []mbRelease{{ID: "rel-a"}}
	if _, ok := soleRelease(testRG, browseish, func(*mbRelease) bool { return true }); ok {
		t.Error("soleRelease accepted a document with no score and no release group")
	}
}

// TestSpacedCountStillInterprets: "2 CD" is a real Discogs-derived spelling, and before
// the count regex allowed the space its only alternative was the uninterpretable "2CD",
// so the whole media predicate dropped silently.
func TestSpacedCountStillInterprets(t *testing.T) {
	g := group(
		ed("rel-cd", []string{"CD"}, "GB"),
		ed("rel-vinyl", []string{"Vinyl"}, "GB"),
	)
	for _, tagged := range []string{"2 CD", "2CD", "2xCD", "2 x CD"} {
		if !mediaInterpretable(mediaAlternatives(tagged), nil) {
			t.Errorf("MEDIA=%s should interpret through its count-stripped form", tagged)
		}
		if id, _, _ := matchEdition(editionTarget(tagged, ""), g); id != "rel-cd" {
			t.Errorf("MEDIA=%s matched %q, want rel-cd", tagged, id)
		}
	}
}

// TestUKMatchesAGBRelease: MusicBrainz stores the United Kingdom as GB, so without the
// alias a UK-tagged album excludes its own release and can land on whatever is left.
func TestUKMatchesAGBRelease(t *testing.T) {
	g := group(
		ed("rel-gb", []string{"CD"}, "GB"),
		ed("rel-us", []string{"CD"}, "US"),
	)
	for _, tagged := range []string{"UK", "uk", "GB", "GBR"} {
		id, reason, _ := matchEdition(editionTarget("", tagged), g)
		if id != "rel-gb" || reason != "country" {
			t.Errorf("country %q = (%q, %q), want (rel-gb, country)", tagged, id, reason)
		}
	}
}

// TestAPredicateNoReleaseAffirmsDrops closes the wrong-match hole a value the group does
// not use would otherwise open: it excludes every release that names a value and leaves
// the ones MusicBrainz records nothing for to win by default.
func TestAPredicateNoReleaseAffirmsDrops(t *testing.T) {
	// The album says JP; the group has no JP release and one release with no country at
	// all. Excluding on country would hand that one a uniqueness win.
	g := group(
		ed("rel-gb", []string{"CD"}, "GB"),
		ed("rel-us", []string{"CD"}, "US"),
		ed("rel-unknown", []string{"CD"}),
	)
	if id, _, _ := matchEdition(editionTarget("", "JP"), g); id != "" {
		t.Errorf("a country the group never uses matched %q, want no match", id)
	}
	// The same shape on media: no release is vinyl, so the format-less one must not win.
	if id, _, _ := matchEdition(editionTarget("Vinyl", ""), g); id != "" {
		t.Errorf("a format the group never uses matched %q, want no match", id)
	}
	// A supra-national country is not an affirmation either, for the same reason.
	xe := group(ed("rel-eu", []string{"CD"}, "XE"), ed("rel-unknown", []string{"CD"}))
	if id, _, _ := matchEdition(editionTarget("", "GB"), xe); id != "" {
		t.Errorf("only XE and an unknown country matched %q, want no match", id)
	}
	// The guard is specific: a country the group does use still decides. (Not over g,
	// whose country-less release is a deliberate candidate and would tie.)
	known := group(ed("rel-gb", []string{"CD"}, "GB"), ed("rel-us", []string{"CD"}, "US"))
	if id, _, _ := matchEdition(editionTarget("", "GB"), known); id != "rel-gb" {
		t.Errorf("matched %q, want rel-gb (an affirmed predicate still fires)", id)
	}
}

// TestANonISOCountryCannotDecide: countryInterpretable accepts any two-letter token, so
// a nonstandard code becomes a live predicate. What keeps it from writing a wrong id is
// countryAffirmed, since no release ever names it. UK is the one that matters in
// practice and is handled properly, by folding to the GB MusicBrainz stores.
func TestANonISOCountryCannotDecide(t *testing.T) {
	g := group(
		ed("rel-gb", []string{"CD"}, "GB"),
		ed("rel-us", []string{"CD"}, "US"),
		ed("rel-unknown", []string{"CD"}),
	)
	for _, bogus := range []string{"ZZ", "QQ", "XX"} {
		if id, _, _ := matchEdition(editionTarget("", bogus), g); id != "" {
			t.Errorf("country %q matched %q; a code no release carries must decide nothing", bogus, id)
		}
	}
	// Over a group with no country-less release to tie against, UK resolves outright.
	known := group(ed("rel-gb", []string{"CD"}, "GB"), ed("rel-us", []string{"CD"}, "US"))
	if id, _, _ := matchEdition(editionTarget("", "UK"), known); id != "rel-gb" {
		t.Errorf("country UK matched %q, want rel-gb", id)
	}
}

// TestBarcodeInGroupIsNotAnEditionMatch: the free barcode win rides the edition tier's
// fetch but is printed-identifier strength, so it must not take the edition provider
// marker an operator would use to revert weak writes in bulk. matchEdition reports which
// it was, so rewording a reason cannot change the marker.
func TestBarcodeInGroupIsNotAnEditionMatch(t *testing.T) {
	withBarcode := ed("rel-a", []string{"CD"}, "US")
	withBarcode.Barcode = "0075992739429"
	g := group(withBarcode, ed("rel-b", []string{"Vinyl"}, "GB"))

	tgt := editionTarget("CD", "US")
	tgt.Barcode = "0075992739429"
	id, reason, edition := matchEdition(tgt, g)
	if id != "rel-a" || reason != "barcode in group" {
		t.Fatalf("matchEdition = (%q, %q), want (rel-a, barcode in group)", id, reason)
	}
	if edition {
		t.Error("a barcode win must not report as an edition-tier decision")
	}

	// The descriptive branches do.
	id, reason, edition = matchEdition(editionTarget("Vinyl", ""), g)
	if id != "rel-b" || !edition {
		t.Errorf("matchEdition = (%q, %q, %v), want rel-b decided by the edition tier", id, reason, edition)
	}
}
