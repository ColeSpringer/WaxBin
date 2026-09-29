package playlist

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// Navidrome (.nsp) smart-playlist interop. A .nsp file is a JSON rule tree: an
// "all"/"any" group of leaves, each `{"<op>": {"<field>": <value>}}`, plus an
// optional sort/order/limit. ImportNSP/ExportNSP map it to and from WaxBin's query
// rule. Mapping is all-or-nothing: an operator or field the other side cannot
// represent faithfully rejects the whole document (CodeUnsupported) rather than
// emitting a lossy partial that would silently drift on re-import. Half a rule is
// a different playlist.
//
// The escape hatch is two-part. CheckNSPExport and CheckNSPImport report
// everything with no counterpart, so a caller can show the whole loss rather than
// the first sentence of it, and ExportNSPPartial/ImportNSPPartial accept that loss
// once it has been seen, dropping those parts and handing back the rule left over.
// Each of the six runs the one walk its direction has, off these same tables,
// which is what keeps the strict sentence and the full list from disagreeing.
//
// The rules that walk follows, which are the ones a later editor would get wrong:
//
// Gap order is contract, since ExportNSP and ImportNSP return Gaps[0]. Export runs
// the Where tree depth-first, then the entity, the limit mode, the seed, the
// random/sorts combination, the sorts, and the limit that rides on a dropped mode.
// Import runs the root group depth-first, then the top-level keys with no WaxBin
// form (sorted, so a map iteration cannot leave the order to chance), then the
// limit, the offset and the sort.
//
// One gap per leaf: a condition stops at the first thing that disqualifies it,
// field before operator before value. Recording every fault on one leaf is noise a
// caller can get for itself by fixing the first and looking again.
//
// A pruned group that empties is pruned in turn, and recorded, since an empty And
// matches everything and an empty Or matches nothing. Pruning runs bottom-up, and
// a partial conversion refuses outright once a rule that started with constraints
// has none left. A rule that was empty to begin with still renders as {"all":[]}.
//
// Dropping a budget limit mode drops Limit with it, since on LimitMinutes the 60
// means minutes and "limit": 60 would say sixty tracks. A pinned LimitSeed drops
// alone, since it only fixes the shuffle order sort "random" already conveys.
//
// A negation is a shape, unless it is a notContains: .nsp has notContains and
// WaxBin has no operator for it, so Not{Cond{_, OpContains}} is reported as the
// condition it wraps, and every other Not is one shape gap naming the field it
// constrains rather than the operator underneath, which is fine on its own.
//
// The directions are not symmetric, and NSPGapMalformed is where that lives. On
// export the input is a typed query.Query, so every refusal is a real hole in
// .nsp. On import it is a file, so a refusal can also mean the file is broken:
// {"is":{"artist":"x","album":"y"}} packs two rules into one rule's shape, and
// {"limit":10} with no root group is not the whole library capped at 10. Those are
// marked malformed, and ImportNSPPartial refuses on one rather than pruning a
// construct nobody wrote on purpose.

// nspFieldToWB maps a Navidrome field name to a WaxBin query field. The vocabulary is
// Navidrome's own field table (model/criteria/fields.go), and a field maps when both
// sides hold the same value in the same vocabulary; any other field is refused. The
// date fields live in nspDateFieldToWB with their own operator restriction.
//
// Three pairs convert their value on the way across (see nspValueConvs): loved is a
// boolean in .nsp and WaxBin's starred is 0/1, rating is 0 to 5 stars against 0 to 100,
// and duration is seconds against milliseconds. The per-user fields evaluate against
// the reading user, bound at read time and never persisted.
//
// filepath is the path under a library's root in Navidrome, so it maps to rel_path
// rather than WaxBin's absolute path, and mbz_recording_id maps to recording_mbid, since
// WaxBin's mbid carries a book's release id beside a track's recording.
//
// artist, albumartist, composer and genre are not rows of Navidrome's static table,
// since it registers its role and tag names at start (AddRoles, AddTagNames).
//
// The near-misses are refused on purpose. hascoverart is an embedded-art flag where
// WaxBin's has_art counts a folder cover too; datemodified moves on catalog writes
// where Navidrome's does not; filetype and codec are a suffix against a container and
// two codec vocabularies; missing is a file flag against WaxBin's state text; the
// artist and release-track mbz_* ids have no bound alias (see the artist MBID note in
// store/sqlite/fields.go); explicitstatus is a text status against WaxBin's
// episode-only 0/1 probe.
var nspFieldToWB = map[string]string{
	"title":                "title",
	"album":                "album",
	"artist":               "artist",
	"albumartist":          "album_artist",
	"composer":             "composer",
	"genre":                "genre",
	"year":                 "year",
	"tracknumber":          "track_no",
	"discnumber":           "disc_no",
	"bpm":                  "bpm",
	"duration":             "duration_ms",
	"filepath":             "rel_path",
	"catalognumber":        "album_catalog_number",
	"mbz_recording_id":     "recording_mbid",
	"mbz_album_id":         "album_mbid",
	"mbz_release_group_id": "release_group_mbid",
	"rating":               "rating",
	"loved":                "starred",
	"playcount":            "play_count",
}

// nspDateFieldToWB maps Navidrome's date fields to WaxBin's nanosecond time fields.
// In a condition, only the relative operators map (inTheLast/notInTheLast, a whole
// number of days): WaxBin stores these fields as Unix nanoseconds while a Navidrome
// absolute rule (before/after/is) holds a date string ("2023-01-01"), so the
// absolute forms cannot round-trip faithfully and stay rejected (all-or-nothing)
// rather than quietly producing an always-true or always-empty predicate (an
// integer compared to a date string). See the dateCond methods for the
// restriction. As sort fields they map without restriction, since ordering a
// nanosecond column is exact; sort dateAdded desc is Navidrome's "recently added".
var nspDateFieldToWB = map[string]string{
	"lastplayed": "last_played",
	"dateadded":  "added",
	"dateloved":  "starred_at",
}

// wbDateFieldToNSP is the reverse date-field map for export, widened the same way
// wbFieldToNSP is; see there.
var wbDateFieldToNSP = widenAliases(invert(nspDateFieldToWB))

// nspDayNS is one day's span in nanoseconds: the unit conversion between a
// Navidrome relative-date value (days) and a WaxBin relative-time window (ns).
const nspDayNS = int64(24) * 60 * 60 * 1_000_000_000

// nspRatingScale bridges Navidrome's 0 to 5 star scale and WaxBin's 0 to 100 rating.
// A Navidrome value is multiplied by it on import and a WaxBin value divided by it on
// export. A 1:1 mapping would quietly mis-match: a Navidrome "more than 3 stars" rule
// would read as WaxBin "more than 3 out of 100" (nearly everything), and a WaxBin
// "rating at least 80" would export as "80 stars" (nothing).
const nspRatingScale = 20

// nspMaxRating bounds a rating on WaxBin's scale in both directions: past 2^53 a float
// no longer holds every whole number, so the scaling and the int64 conversion stop being
// exact, and nothing near it is a rating anyway.
const nspMaxRating = 1 << 53

// nspMaxDurationMS bounds a duration on both sides of the seconds bridge: below 2^51
// milliseconds (about 71,000 years) a whole millisecond count converts to seconds and
// back exactly in float64, and nothing past it reaches an int64 conversion Go leaves
// implementation-defined out of range.
const nspMaxDurationMS = 1 << 51

// asFloat coerces a JSON-decoded (float64) or programmatically-built (int/int64)
// numeric value to a float64.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// scaleRatingIn converts a Navidrome rating value (0 to 5 stars) to WaxBin's 0 to 100
// scale. The second result is the failure's code, empty when the value converts; both
// walks record it in a report rather than return an error, and only the strict entry
// points turn one back into an error.
func scaleRatingIn(v any) (any, NSPReason) {
	f, ok := asFloat(v)
	if !ok {
		return nil, NSPReasonValueNotNumeric
	}
	r := f * nspRatingScale
	if !(math.Abs(r) < nspMaxRating) {
		return nil, NSPReasonValueTooLarge
	}
	return r, ""
}

// scaleRatingOut converts a WaxBin rating value (0 to 100) back to Navidrome's 0 to 5
// scale. It rejects a value that is not a whole number of stars rather than writing a
// fractional or mismatched star count.
func scaleRatingOut(v any) (any, NSPReason) {
	f, ok := asFloat(v)
	if !ok {
		return nil, NSPReasonValueNotNumeric
	}
	if !(math.Abs(f) < nspMaxRating) {
		return nil, NSPReasonValueTooLarge
	}
	n := int64(f)
	if float64(n) != f || n%nspRatingScale != 0 {
		return nil, NSPReasonRatingNotWholeStar
	}
	return n / nspRatingScale, ""
}

// boolIn converts a .nsp boolean value to the 0/1 WaxBin's column holds. .nsp defines
// loved as a boolean while WaxBin lowers starred to CASE ... THEN 1 ELSE 0, so a bool
// crossing unconverted leaves a stored rule holding a value the engine never produces
// itself. A 0 or 1 is accepted too, since it means the same thing and refusing it would
// only reject a document that round-tripped through here.
func boolIn(v any) (any, NSPReason) {
	if b, ok := v.(bool); ok {
		if b {
			return int64(1), ""
		}
		return int64(0), ""
	}
	if f, ok := asFloat(v); ok && (f == 0 || f == 1) {
		return int64(f), ""
	}
	return nil, NSPReasonValueNotBoolean
}

// boolOut converts a WaxBin 0/1 value back to the .nsp boolean. A bool passes through:
// a rule imported before this conversion existed holds one, and it is already the value
// .nsp wants. Anything that is neither 0 nor 1 is not a state the column can be in and
// has no .nsp form.
func boolOut(v any) (any, NSPReason) {
	if b, ok := v.(bool); ok {
		return b, ""
	}
	f, ok := asFloat(v)
	if !ok || (f != 0 && f != 1) {
		return nil, NSPReasonValueNotBoolean
	}
	return f == 1, ""
}

// durationIn converts a .nsp duration in seconds to WaxBin's milliseconds. A value that
// is not a whole number of milliseconds is refused rather than rounded, since rounding
// moves the boundary a gt or lt draws. The check needs no tolerance: a whole count r
// parses to the same float r/1000 computes, anywhere below nspMaxDurationMS.
func durationIn(v any) (any, NSPReason) {
	f, ok := asFloat(v)
	if !ok {
		return nil, NSPReasonValueNotNumeric
	}
	ms := f * 1000
	if !(math.Abs(ms) < nspMaxDurationMS) {
		return nil, NSPReasonValueTooLarge
	}
	r := math.Round(ms)
	if r/1000 != f {
		return nil, NSPReasonDurationNotWholeMS
	}
	return int64(r), ""
}

// durationOut converts WaxBin milliseconds back to .nsp seconds, refusing a value
// durationIn would refuse on the way back in. encoding/json prints the shortest form,
// so 300000 exports as 300 and 300500 as 300.5.
func durationOut(v any) (any, NSPReason) {
	f, ok := asFloat(v)
	if !ok {
		return nil, NSPReasonValueNotNumeric
	}
	if !(math.Abs(f) < nspMaxDurationMS) {
		return nil, NSPReasonValueTooLarge
	}
	if f != math.Trunc(f) {
		return nil, NSPReasonDurationNotWholeMS
	}
	return f / 1000, ""
}

// nspValueConvs holds the per-field value conversions, keyed by canonical WaxBin field.
// A field absent from it crosses verbatim. Both directions report a failure as a code
// rather than an error, since the walks record it in a report and only the strict
// entry points turn one back into an error.
var nspValueConvs = map[string]struct{ in, out func(any) (any, NSPReason) }{
	"rating":      {scaleRatingIn, scaleRatingOut},
	"starred":     {boolIn, boolOut},
	"duration_ms": {durationIn, durationOut},
}

// nspValueGapCodes are the import conversion failures filed as value gaps, which a
// partial import may drop. A sub-millisecond or enormous value is one Navidrome would
// write and WaxBin has no room for, while every other failure (a rating that is not a
// number) is a value Navidrome would not write, so the document is broken.
var nspValueGapCodes = map[NSPReason]bool{
	NSPReasonDurationNotWholeMS: true,
	NSPReasonValueTooLarge:      true,
}

// nspNarrowFields are the fields that carry fewer operators than the shared table, by
// canonical WaxBin name: the operators each takes, as .nsp spells them, and the code
// for any other. .nsp defines loved as a boolean, so ordering it, ranging it, or
// matching a substring of it means nothing on either side; without the gate a converted
// value would render {"gt":{"loved":true}}.
var nspNarrowFields = map[string]struct {
	ops  map[string]bool
	code NSPReason
}{
	"starred": {map[string]bool{"is": true, "isNot": true}, NSPReasonBooleanOperator},
}

// nspPresenceFields are the fields isMissing and isPresent cross on, by Navidrome name.
// It mirrors where Navidrome allows them (missingExpr in persistence/criteria_sql.go):
// its nullable columns plus the role and tag fields. The WaxBin expressions behind them
// agree with its lowering, NULL for a number and NULL or empty for a string, since the
// text fields COALESCE to an empty string and bpm is read raw.
var nspPresenceFields = map[string]bool{
	"album": true, "artist": true, "albumartist": true, "composer": true, "genre": true,
	"bpm": true, "catalognumber": true,
	"mbz_recording_id": true, "mbz_album_id": true, "mbz_release_group_id": true,
}

// wbPresenceFields is nspPresenceFields by WaxBin field, every alias spelling included.
var wbPresenceFields = func() map[string]bool {
	m := map[string]bool{}
	for name := range nspPresenceFields {
		for _, spelling := range model.QueryFieldSpellings(nspFieldToWB[name]) {
			m[spelling] = true
		}
	}
	return m
}()

// wbFieldToNSP is the reverse map for export, built from nspFieldToWB and widened over
// the engine's own alias spellings. The engine treats album_artist and albumartist as
// one column, so which of them a stored rule holds is arbitrary; a reverse map covering
// only the spelling the forward map named refuses a rule .nsp carries perfectly well.
// The widening happens once at construction rather than at each lookup site, so a fifth
// site cannot forget it.
var wbFieldToNSP = widenAliases(invert(nspFieldToWB))

// invert flips a forward field map. Every value is distinct in both maps it is used on,
// so no entry is lost.
func invert(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

// widenAliases adds every other spelling the query engine accepts for a field already
// in m, mapping it to the same .nsp name. A spelling m already names explicitly wins,
// and two entries of m wanting the same spelling is a conflict rather than a race
// between them: map iteration order would otherwise pick a winner per run, and the
// export would name the same field differently between two runs of one binary.
func widenAliases(m map[string]string) map[string]string {
	add := map[string]string{}
	for field, nspName := range m {
		for _, spelling := range model.QueryFieldSpellings(field) {
			if _, ok := m[spelling]; ok {
				continue
			}
			if prior, ok := add[spelling]; ok && prior != nspName {
				panic("playlist: nsp field " + spelling + " widens to both " + prior + " and " + nspName)
			}
			add[spelling] = nspName
		}
	}
	for field, nspName := range add {
		m[field] = nspName
	}
	return m
}

// NSPExportableFields lists the WaxBin query fields that have an .nsp name at all,
// alias spellings included, in sorted order. A field absent from it can never survive a
// conversion; a field present may still be dropped for the operator or the value it
// carries, since the date fields take only the relative operators and a converted field
// only a value that converts. CheckNSPExport answers that for a particular rule, and
// this list is for the coarser question of which fields are in the vocabulary at all.
func NSPExportableFields() []string {
	out := make([]string, 0, len(wbFieldToNSP)+len(wbDateFieldToNSP))
	for f := range wbFieldToNSP {
		out = append(out, f)
	}
	for f := range wbDateFieldToNSP {
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}

// NSPImportableFields lists the Navidrome field names an import reads, in sorted order,
// which is the list a consumer describing an import shows. It is the coarse answer too:
// CheckNSPImport says what a particular document loses.
func NSPImportableFields() []string {
	out := make([]string, 0, len(nspFieldToWB)+len(nspDateFieldToWB))
	for f := range nspFieldToWB {
		out = append(out, f)
	}
	for f := range nspDateFieldToWB {
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}

// nspOpToWB maps a Navidrome leaf operator to a WaxBin operator. notContains has no
// direct WaxBin operator and is handled specially (wrapped in a Not).
var nspOpToWB = map[string]query.Op{
	"is":         query.OpIs,
	"isNot":      query.OpIsNot,
	"contains":   query.OpContains,
	"startsWith": query.OpStartsWith,
	"endsWith":   query.OpEndsWith,
	"gt":         query.OpGt,
	"lt":         query.OpLt,
	"before":     query.OpBefore,
	"after":      query.OpAfter,
	"inTheRange": query.OpInRange,
}

// nspTextOps are the substring operators, named as .nsp spells them, and
// nspScaledFields are the fields whose value converts by a numeric factor, by canonical
// WaxBin name. A substring operator has no faithful mapping on a scaled field in either
// direction, since "contains 3" and "contains 60" match different sets. Every other
// operator on those fields converts, so these are rejected rather than crossing in one
// unit or the other. (Both sides accept them: WaxBin's columns are integers LIKE
// compiles against, and Navidrome writes them because its field list does not say
// otherwise.)
var (
	nspTextOps = map[string]bool{
		"contains":    true,
		"startsWith":  true,
		"endsWith":    true,
		"notContains": true,
	}
	nspScaledFields = map[string]bool{"rating": true, "duration_ms": true}
)

// wbOpToNSP is the reverse map for export.
var wbOpToNSP = func() map[query.Op]string {
	m := make(map[query.Op]string, len(nspOpToWB))
	for k, v := range nspOpToWB {
		m[v] = k
	}
	return m
}()

func nspErr(msg string) error { return waxerr.New(waxerr.CodeUnsupported, "playlist.nsp", msg) }

// nspImporter walks an .nsp document once, building what maps and recording
// what does not, in the document's own vocabulary. It is the mirror of
// nspExporter with one asymmetry: on export the input is a typed query.Query, so
// every refusal is a real hole in .nsp, while here the input is a file and a
// refusal can also mean the file is broken. NSPGapMalformed carries that
// difference, and ImportNSPPartial refuses outright on one rather than pruning
// a construct nobody wrote on purpose.
type nspImporter struct {
	rep  NSPReport
	path []string
}

func (im *nspImporter) push(seg string) { im.path = append(im.path, seg) }
func (im *nspImporter) pop()            { im.path = im.path[:len(im.path)-1] }
func (im *nspImporter) at() string      { return nspPointer(im.path) }

// broken records g as a malformed gap at the walk's position.
func (im *nspImporter) broken(g NSPGap) {
	g.Kind, g.Path = NSPGapMalformed, im.at()
	im.rep.gap(g)
}

// nspTop decodes the document's top level. Its error is the one failure that is
// not a gap: an unparseable document has no parts to report on, only a syntax
// error.
func nspTop(data []byte) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalid, "playlist.nsp", err)
	}
	return top, nil
}

// walk reads the document into a query, returning it with whether the root group
// survived.
//
// Like the export walk the order of the checks is the gap order, and ImportNSP
// reports Gaps[0]: the root group depth-first, then the top-level keys WaxBin
// cannot represent (sorted, since a map iteration would leave the order of the
// report to chance), then the limit, the offset and the sort.
func (im *nspImporter) walk(top map[string]json.RawMessage) (query.Query, bool) {
	q := query.Query{Entity: query.EntityItems}
	_, hasAll := top["all"]
	_, hasAny := top["any"]
	rootKey := ""
	switch {
	case hasAll && hasAny:
		// Broken rather than unmappable: which of the two the person meant is not
		// recoverable, so the walk reads "all" only to fill out the rest of the
		// report and the document is refused either way.
		im.broken(NSPGap{Code: NSPReasonMultipleRoots})
		rootKey = "all"
	case hasAll:
		rootKey = "all"
	case hasAny:
		rootKey = "any"
	default:
		im.broken(NSPGap{Code: NSPReasonMissingRoot})
	}
	kept := false
	if rootKey != "" {
		if node, ok := im.group(rootKey, top[rootKey]); ok {
			q.Where, kept = node, true
		}
	}

	keys := make([]string, 0, len(top))
	for key := range top {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		switch key {
		case "all", "any", "sort", "order", "limit", "offset":
			continue
		case "name", "comment":
			// Navidrome playlist metadata that does not affect membership. Ignore it;
			// the WaxBin playlist name and visibility are supplied separately. It is
			// not a loss, so it does not belong in the report either.
			continue
		}
		// A semantics-affecting key WaxBin cannot represent (e.g. limitPercent). It
		// is a real expressiveness gap rather than a broken document, so a partial
		// import may drop it, and says so.
		im.rep.gap(NSPGap{Kind: NSPGapShape, Code: NSPReasonUnsupportedKey, Key: key, Path: nspPointer([]string{key})})
	}

	// limit/offset parse before the sort block: the random-sort mapping below
	// checks the parsed limit value, not just whether the key is present.
	if raw, ok := top["limit"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			im.rep.gap(NSPGap{Kind: NSPGapMalformed, Code: NSPReasonBadLimit, Path: "/limit"})
		} else {
			q.Limit = n
		}
	}
	if raw, ok := top["offset"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			im.rep.gap(NSPGap{Kind: NSPGapMalformed, Code: NSPReasonBadOffset, Path: "/offset"})
		} else {
			q.Offset = n
		}
	}
	if raw, ok := top["sort"]; ok {
		im.sort(top, raw, &q)
	}
	return q, kept
}

// sort maps the sort/order pair onto the query.
func (im *nspImporter) sort(top map[string]json.RawMessage, raw json.RawMessage, q *query.Query) {
	var field string
	if err := json.Unmarshal(raw, &field); err != nil {
		im.rep.gap(NSPGap{Kind: NSPGapMalformed, Code: NSPReasonBadSort, Path: "/sort"})
		return
	}
	lower := strings.ToLower(field)
	if lower == "random" {
		// Navidrome's random sort maps to WaxBin's random limit mode: a seeded
		// shuffle, drawn fresh per evaluation (no seed is persisted). WaxBin
		// requires a positive limit with that mode, since "everything, shuffled" is
		// a playback concern rather than a selection, so a random sort with no
		// limit (or a zero/negative one) has no WaxBin form and drops instead of
		// building a query every downstream compile would reject. "order" is
		// ignored: asc/desc of a shuffle is meaningless.
		if q.Limit <= 0 {
			im.rep.gap(NSPGap{Kind: NSPGapLimit, Code: NSPReasonRandomNeedsLimit, Path: "/sort", Value: field})
			return
		}
		q.LimitMode = query.LimitRandom
		return
	}
	wb, ok := nspFieldToWB[lower]
	if !ok {
		// The date fields sort too (WaxBin's added/last_played are plain time
		// columns): "recently added" is sort dateAdded desc.
		if wb, ok = nspDateFieldToWB[lower]; !ok {
			im.rep.gap(NSPGap{Kind: NSPGapSort, Code: NSPReasonUnsupportedSortField, Path: "/sort", Field: field})
			return
		}
	}
	desc := false
	if o, ok := top["order"]; ok {
		var ord string
		if err := json.Unmarshal(o, &ord); err != nil {
			// The direction is half of what a sort says, so an unreadable one takes
			// the sort with it rather than defaulting to ascending.
			im.rep.gap(NSPGap{Kind: NSPGapMalformed, Code: NSPReasonBadOrder, Path: "/order"})
			return
		}
		desc = strings.EqualFold(ord, "desc")
	}
	q.Sorts = []query.Sort{{Field: wb, Desc: desc}}
}

// group parses an all/any group (a JSON array of rules) into an And/Or node,
// keeping the members that map.
func (im *nspImporter) group(key string, raw json.RawMessage) (query.Node, bool) {
	im.push(key)
	defer im.pop()
	var rules []json.RawMessage
	if err := json.Unmarshal(raw, &rules); err != nil {
		im.broken(NSPGap{Code: NSPReasonGroupNotArray, Key: key})
		return nil, false
	}
	nodes := make([]query.Node, 0, len(rules))
	for i, r := range rules {
		im.push(strconv.Itoa(i))
		n, ok := im.rule(r)
		im.pop()
		if ok {
			nodes = append(nodes, n)
		}
	}
	// The export walk's rule, in the other direction: a group that started with
	// members and kept none matches everything (all) or nothing (any), so it is
	// dropped in turn rather than left to flip the document's meaning.
	if len(rules) > 0 && len(nodes) == 0 {
		im.rep.gap(NSPGap{Kind: NSPGapShape, Code: NSPReasonGroupEmptied, Path: im.at()})
		return nil, false
	}
	if key == "any" {
		return query.Or{Nodes: nodes}, true
	}
	return query.And{Nodes: nodes}, true
}

// rule parses one rule element: a nested group or an operator leaf.
func (im *nspImporter) rule(raw json.RawMessage) (query.Node, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) != 1 {
		// Broken, not unmappable: {"is":{"artist":"x","album":"y"}} packs two rules
		// into one rule's shape, and pruning it would silently drop a constraint the
		// person wrote.
		im.broken(NSPGap{Code: NSPReasonRuleShape})
		return nil, false
	}
	key, val := nspOnlyEntry(m)
	if key == "all" || key == "any" {
		return im.group(key, val)
	}
	return im.leaf(key, val)
}

// nspOnlyEntry returns the entry of a map the caller has checked holds exactly one.
func nspOnlyEntry(m map[string]json.RawMessage) (string, json.RawMessage) {
	for k, v := range m {
		return k, v
	}
	return "", nil
}

// leaf parses an operator leaf `{"<op>": {"<field>": <value>}}`.
func (im *nspImporter) leaf(op string, val json.RawMessage) (query.Node, bool) {
	var fv map[string]json.RawMessage
	if err := json.Unmarshal(val, &fv); err != nil || len(fv) != 1 {
		im.broken(NSPGap{Code: NSPReasonOperatorShape, Op: op})
		return nil, false
	}
	field, rawVal := nspOnlyEntry(fv)
	if wb, ok := nspDateFieldToWB[strings.ToLower(field)]; ok {
		return im.dateCond(op, wb, field, rawVal)
	}
	wb, ok := nspFieldToWB[strings.ToLower(field)]
	if !ok {
		im.rep.gap(NSPGap{Kind: NSPGapField, Code: NSPReasonUnsupportedField, Field: field, Path: im.at()})
		return nil, false
	}
	return im.cond(op, field, wb, rawVal)
}

// dateCond builds a relative-time condition for a Navidrome date field. The .nsp
// value is a number of days; WaxBin's operators take a nanosecond window, so the
// value converts on the way in (and back out in nspExporter.dateCond). Only
// inTheLast/notInTheLast map; any other operator on a date field has no WaxBin
// form (see nspDateFieldToWB), as does a fractional or non-positive day count,
// which would have no faithful export.
func (im *nspImporter) dateCond(op, wbField, nspField string, rawVal json.RawMessage) (query.Node, bool) {
	var wbOp query.Op
	switch op {
	case "inTheLast":
		wbOp = query.OpInTheLast
	case "notInTheLast":
		wbOp = query.OpNotInTheLast
	default:
		im.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonDateOperator, Field: nspField, Op: op, Path: im.at()})
		return nil, false
	}
	var days float64
	if err := json.Unmarshal(rawVal, &days); err != nil {
		im.broken(NSPGap{Code: NSPReasonDaysNotNumber, Field: nspField, Op: op})
		return nil, false
	}
	// Bound the magnitude before converting: past MaxInt64/nspDayNS (about 106,751
	// days, or 292 years) the nanosecond multiply below would overflow int64 and
	// silently wrap, in part of that range to a positive and plausible-looking
	// window, and a float that large makes the int64 conversion itself
	// implementation-defined. The bound is two-sided for that second reason: a
	// negative day count is rejected either way, but only after a conversion whose
	// result the spec does not define.
	if math.Abs(days) > float64(math.MaxInt64/nspDayNS) {
		im.valueGap(NSPReasonWindowTooLarge, nspField, op, days)
		return nil, false
	}
	n := int64(days)
	if float64(n) != days || n <= 0 {
		im.valueGap(NSPReasonWindowNotWholeDays, nspField, op, days)
		return nil, false
	}
	return query.Cond{Field: wbField, Op: wbOp, Value: n * nspDayNS}, true
}

// cond builds a condition (or a negated one) for a leaf operator, converting the value
// of a field in nspValueConvs. nspField is the name as written, for the report; wbField
// is the resolved WaxBin one.
func (im *nspImporter) cond(op, nspField, wbField string, rawVal json.RawMessage) (query.Node, bool) {
	if op == "inTheRange" {
		var vals []any
		if err := json.Unmarshal(rawVal, &vals); err != nil || len(vals) != 2 {
			im.broken(NSPGap{Code: NSPReasonRangeShape, Field: nspField, Op: op})
			return nil, false
		}
		if !im.opAllowed(op, nspField, wbField) {
			return nil, false
		}
		if conv, ok := nspValueConvs[wbField]; ok {
			for i := range vals {
				sv, code := conv.in(vals[i])
				if code != "" {
					im.convGap(code, nspField, op, vals[i])
					return nil, false
				}
				vals[i] = sv
			}
		}
		return query.Cond{Field: wbField, Op: query.OpInRange, Values: vals}, true
	}
	var v any
	if err := json.Unmarshal(rawVal, &v); err != nil {
		im.broken(NSPGap{Code: NSPReasonBadValue, Field: nspField, Op: op})
		return nil, false
	}
	if op == "isMissing" || op == "isPresent" {
		return im.presence(op, nspField, wbField, v)
	}
	if nspScaledFields[wbField] && nspTextOps[op] {
		im.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonScaledTextOperator, Field: nspField, Op: op, Path: im.at()})
		return nil, false
	}
	if !im.opAllowed(op, nspField, wbField) {
		return nil, false
	}
	if op == "notContains" {
		return query.Not{Node: query.Cond{Field: wbField, Op: query.OpContains, Value: v}}, true
	}
	wbOp, ok := nspOpToWB[op]
	if !ok {
		im.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonUnsupportedOperator, Field: nspField, Op: op, Path: im.at()})
		return nil, false
	}
	if conv, ok := nspValueConvs[wbField]; ok {
		sv, code := conv.in(v)
		if code != "" {
			im.convGap(code, nspField, op, v)
			return nil, false
		}
		v = sv
	}
	return query.Cond{Field: wbField, Op: wbOp, Value: v}, true
}

// presence builds an isMissing or isPresent condition. Navidrome allows them only on
// nspPresenceFields and takes a boolean that flips them, so isMissing false is
// isPresent.
func (im *nspImporter) presence(op, nspField, wbField string, v any) (query.Node, bool) {
	if !nspPresenceFields[strings.ToLower(nspField)] {
		im.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonPresenceOperator, Field: nspField, Op: op, Path: im.at()})
		return nil, false
	}
	b, ok := v.(bool)
	if !ok {
		im.broken(NSPGap{Code: NSPReasonValueNotBoolean, Field: nspField, Op: op, Value: v})
		return nil, false
	}
	if (op == "isMissing") == b {
		return query.Cond{Field: wbField, Op: query.OpIsMissing}, true
	}
	return query.Cond{Field: wbField, Op: query.OpIsPresent}, true
}

// convGap records a value that did not convert: a value gap for the codes in
// nspValueGapCodes, and otherwise a broken document.
func (im *nspImporter) convGap(code NSPReason, field, op string, val any) {
	if nspValueGapCodes[code] {
		im.valueGap(code, field, op, val)
		return
	}
	im.broken(NSPGap{Code: code, Field: field, Op: op, Value: val})
}

// opAllowed reports whether op crosses on wbField, recording the gap when it does not.
// It is where a field narrower than the shared operator table says so (nspNarrowFields).
func (im *nspImporter) opAllowed(op, nspField, wbField string) bool {
	n, narrow := nspNarrowFields[wbField]
	if !narrow || n.ops[op] {
		return true
	}
	im.rep.gap(NSPGap{Kind: NSPGapOperator, Code: n.code, Field: nspField, Op: op, Path: im.at()})
	return false
}

func (im *nspImporter) valueGap(code NSPReason, field, op string, val any) {
	im.rep.gap(NSPGap{Kind: NSPGapValue, Code: code, Field: field, Op: op, Value: val, Path: im.at()})
}

// CheckNSPImport reports what an import of data could not carry. Its only error
// is the malformed-JSON case: a document that will not parse has no parts to
// report on, only a syntax error. A NSPGapMalformed gap in the report is the
// other thing: the document parses but is broken, which is a different sentence
// for the person reading it than "this document is unmappable".
func CheckNSPImport(data []byte) (NSPReport, error) {
	im := nspImporter{rep: NSPReport{Direction: NSPDirImport}}
	top, err := nspTop(data)
	if err != nil {
		return im.rep, err
	}
	im.walk(top)
	return im.rep, nil
}

// ImportNSP parses a Navidrome .nsp document into a WaxBin item query. It rejects
// (all-or-nothing) any operator or field WaxBin cannot represent, reporting the
// first gap; CheckNSPImport lists them all and ImportNSPPartial builds the rule
// that is left once they are dropped.
func ImportNSP(data []byte) (query.Query, error) {
	top, err := nspTop(data)
	if err != nil {
		return query.Query{}, err
	}
	im := nspImporter{rep: NSPReport{Direction: NSPDirImport}}
	q, _ := im.walk(top)
	if !im.rep.OK() {
		return query.Query{}, nspErr(im.rep.Gaps[0].Reason)
	}
	return q, nspRuleCheck(q)
}

// NSPImport is a lossy read: the rule the document maps to once the parts with
// no WaxBin form are dropped, and what those were.
type NSPImport struct {
	Rule   query.Query
	Report NSPReport
}

// ImportNSPPartial builds the rule from what maps, reporting each part it
// dropped. It refuses a malformed document outright, since pruning a broken
// construct would turn a rule the person wrote into one nobody can see is
// missing, and it refuses when a document that started with rules has none left.
func ImportNSPPartial(data []byte) (*NSPImport, error) {
	top, err := nspTop(data)
	if err != nil {
		return nil, err
	}
	im := nspImporter{rep: NSPReport{Direction: NSPDirImport}}
	q, kept := im.walk(top)
	for _, g := range im.rep.Gaps {
		if g.Kind == NSPGapMalformed {
			return nil, nspErr("nsp: a malformed document cannot be partially imported: " + strings.TrimPrefix(g.Reason, "nsp: "))
		}
	}
	if !kept {
		return nil, nspErr("nsp: nothing in this document has a WaxBin form, so a partial import would match the whole library")
	}
	if err := nspRuleCheck(q); err != nil {
		return nil, err
	}
	return &NSPImport{Rule: q, Report: im.rep}, nil
}

// nspRuleCheck marshal-validates an imported rule so a node shape the rule codec
// cannot represent fails the import. No compilation happens here: field, entity,
// and limit-mode validation runs where the rule is stored (CreatePlaylist and
// SetPlaylistRule compile against the store's field whitelist), and since the
// import maps only fields and combinations it knows, those checks serve as a
// backstop rather than the primary barrier.
func nspRuleCheck(q query.Query) error {
	_, err := query.MarshalRule(q)
	// envelope.Wrap stamps its own op on the way out, so returning this raw would
	// surface a playlist.nsp failure under someone else's name. It stays internal
	// on purpose: a value JSON cannot encode is a bug in the value, not a mapping
	// the format cannot express.
	return waxerr.Wrap(waxerr.CodeInternal, "playlist.nsp", err)
}

// nspExporter walks a query once, rendering what maps to .nsp and recording
// what does not. Every export entry point runs this one walk: ExportNSP refuses
// when the report holds a gap, ExportNSPPartial keeps the render along with the
// rule it actually describes, and CheckNSPExport keeps only the report.
type nspExporter struct {
	rep  NSPReport
	path []string
}

func (e *nspExporter) push(seg string) { e.path = append(e.path, seg) }
func (e *nspExporter) pop()            { e.path = e.path[:len(e.path)-1] }
func (e *nspExporter) at() string      { return nspPointer(e.path) }

// walk renders q as an .nsp group, returning the group, the rule that group
// actually describes, and whether the condition tree survived.
//
// The order of the checks below is the gap order, and it is contract: ExportNSP
// reports Gaps[0], so moving a check moves the sentence a caller sees. It runs
// the Where tree depth-first, then the entity, the limit mode and seed, the
// random/sorts combination, the sorts, and last the limit that rides on a
// dropped mode.
func (e *nspExporter) walk(q query.Query) (map[string]any, query.Query, bool) {
	group, where, kept := e.root(q.Where)
	if !kept {
		// Nothing survived the tree, but the walk goes on so the report covers the
		// sort and limit too; the caller refuses on kept rather than on the
		// placeholder group.
		group = map[string]any{"all": []any{}}
	}
	rule := query.Query{Entity: q.Entity, Where: where, Offset: q.Offset}

	// This is not the boundary that validates the entity (validatePlaylistRule
	// does, where the rule is stored). It reports only the two values that would
	// round-trip to something else, so any other value, the zero one included,
	// stays silent.
	switch q.Entity {
	case query.EntityTracks:
		// The render is faithful; the loss is on the way back, since Navidrome has
		// no kind distinction and ImportNSP always builds items. In a mixed library
		// the re-imported rule picks up audiobooks and episodes too.
		e.rep.note(NSPGap{Kind: NSPGapEntity, Code: NSPReasonEntityWidens, Path: "/entity", Value: string(q.Entity)})
	case query.EntityFiles:
		e.rep.gap(NSPGap{Kind: NSPGapEntity, Code: NSPReasonEntityFiles, Path: "/entity", Value: string(q.Entity)})
		rule.Entity = query.EntityItems
	}

	// Only the unseeded random mode maps back (Navidrome's sort "random"); the
	// budget modes (minutes/megabytes) and a pinned seed have no .nsp
	// representation. They are two gaps rather than one because they drop
	// independently: a seed drops alone, since it only pins the shuffle order that
	// sort "random" already conveys, and one sentence covering both would claim
	// the mode was unrepresentable on a document that keeps it.
	dropBudget := q.LimitMode != query.LimitCount && q.LimitMode != query.LimitRandom
	if dropBudget {
		e.rep.gap(NSPGap{Kind: NSPGapLimit, Code: NSPReasonLimitMode, Path: "/limitMode", Value: string(q.LimitMode)})
	}
	if q.LimitSeed != 0 {
		e.rep.gap(NSPGap{Kind: NSPGapLimit, Code: NSPReasonLimitSeed, Path: "/limitSeed", Value: q.LimitSeed})
	}
	sorts := q.Sorts
	if q.LimitMode == query.LimitRandom {
		if q.Limit <= 0 {
			// sort "random" with no limit is a document ImportNSP turns away
			// ("everything, shuffled" is a playback concern), so writing one would
			// leave the strict exporter emitting what the strict importer refuses.
			// Compile forbids the query too, but ExportNSP takes any query.Query a
			// caller hands it.
			e.rep.gap(NSPGap{Kind: NSPGapLimit, Code: NSPReasonRandomNeedsLimit, Path: "/limitMode", Value: string(q.LimitMode)})
		} else {
			// Random plus Sorts is not even a valid WaxBin query (compile rejects
			// the combination), and rendering both would let the sort block below
			// silently overwrite the shuffle. The shuffle is the more faithful half,
			// so the sorts are what go.
			if len(sorts) > 0 {
				e.rep.gap(NSPGap{Kind: NSPGapSort, Code: NSPReasonRandomWithSorts, Path: "/sorts", Field: sorts[0].Field})
				sorts = nil
			}
			group["sort"] = "random"
			rule.LimitMode = query.LimitRandom
		}
	}
	if len(sorts) > 0 {
		s := sorts[0]
		nspField, ok := wbFieldToNSP[s.Field]
		if !ok {
			// The date fields export as sorts too (mirroring the import).
			nspField, ok = wbDateFieldToNSP[s.Field]
		}
		if !ok {
			e.rep.gap(NSPGap{Kind: NSPGapSort, Code: NSPReasonUnsupportedSortField, Path: "/sorts/0", Field: s.Field})
		}
		// .nsp carries one sort key, so the later terms have nowhere to go, each a gap of
		// its own at its own pointer. A gap and not a note because CompileAt joins every
		// term, which makes a two-term sort a real stored rule that an export quietly
		// keeping the first hands back reordered. The counter-argument is that with no
		// limit the order is presentation and not membership; the carve-out it implies,
		// gap when Limit > 0 and note otherwise, waits until someone asks. The later terms
		// are not field-checked in turn, since they are going either way.
		for i, extra := range sorts[1:] {
			e.rep.gap(NSPGap{Kind: NSPGapSort, Code: NSPReasonExtraSortTerm,
				Path: "/sorts/" + strconv.Itoa(i+1), Field: extra.Field, Value: extra})
		}
		if ok {
			group["sort"] = nspField
			group["order"] = "asc"
			if s.Desc {
				group["order"] = "desc"
			}
			sorts = sorts[:1:1]
		} else {
			sorts = nil
		}
	}
	rule.Sorts = sorts
	if dropBudget {
		// The value means minutes or megabytes, so writing it as "limit" would say
		// that many tracks instead. It drops with the mode that gave it its unit.
		if q.Limit != 0 {
			e.rep.gap(NSPGap{Kind: NSPGapLimit, Code: NSPReasonLimitBudget, Path: "/limit", Value: q.Limit,
				Mode: string(q.LimitMode)})
		}
	} else {
		rule.Limit = q.Limit
		if q.Limit > 0 {
			group["limit"] = q.Limit
		}
	}
	if q.Offset > 0 {
		group["offset"] = q.Offset
	}
	return group, rule, kept
}

// root renders the top-level rule as an all/any group (wrapping a bare leaf).
func (e *nspExporter) root(n query.Node) (map[string]any, query.Node, bool) {
	if n == nil {
		return map[string]any{"all": []any{}}, nil, true
	}
	e.push("where")
	defer e.pop()
	switch n.(type) {
	case query.And, query.Or:
		return e.node(n)
	}
	leaf, kept, ok := e.node(n)
	if !ok {
		return nil, nil, false
	}
	return map[string]any{"all": []any{leaf}}, kept, true
}

// node renders one node (group or leaf) as an .nsp object, returning the node
// that survived alongside it.
func (e *nspExporter) node(n query.Node) (map[string]any, query.Node, bool) {
	switch node := n.(type) {
	case query.And:
		arr, kept, ok := e.children(node.Nodes)
		if !ok {
			return nil, nil, false
		}
		return map[string]any{"all": arr}, query.And{Nodes: kept}, true
	case query.Or:
		arr, kept, ok := e.children(node.Nodes)
		if !ok {
			return nil, nil, false
		}
		return map[string]any{"any": arr}, query.Or{Nodes: kept}, true
	case query.Not:
		return e.not(node)
	case query.Cond:
		return e.cond(node)
	default:
		e.rep.gap(NSPGap{Kind: NSPGapShape, Code: NSPReasonUnsupportedNode, Path: e.at()})
		return nil, nil, false
	}
}

// children renders a group's members, keeping the ones that map.
func (e *nspExporter) children(nodes []query.Node) ([]any, []query.Node, bool) {
	arr := make([]any, 0, len(nodes))
	kept := make([]query.Node, 0, len(nodes))
	e.push("nodes")
	for i, n := range nodes {
		e.push(strconv.Itoa(i))
		obj, surv, ok := e.node(n)
		e.pop()
		if !ok {
			continue
		}
		arr = append(arr, obj)
		kept = append(kept, surv)
	}
	e.pop()
	// An empty And matches everything and an empty Or matches nothing, so a group
	// that started with members and kept none is dropped in turn rather than left
	// to flip the rule's meaning. A group that was empty to begin with says what
	// it always said and stays.
	if len(nodes) > 0 && len(kept) == 0 {
		e.rep.gap(NSPGap{Kind: NSPGapShape, Code: NSPReasonGroupEmptied, Path: e.at()})
		return nil, nil, false
	}
	return arr, kept, true
}

// not renders a negation. The only shape .nsp can express is notContains.
func (e *nspExporter) not(n query.Not) (map[string]any, query.Node, bool) {
	c, isCond := n.Node.(query.Cond)
	if !isCond || c.Op != query.OpContains {
		// The negation is the thing with no form here, not the operator under it,
		// so Op stays empty and only the field it constrains is named.
		g := NSPGap{Kind: NSPGapShape, Code: NSPReasonNegation, Path: e.at()}
		if isCond {
			g.Field = c.Field
		}
		e.rep.gap(g)
		return nil, nil, false
	}
	// A date field has an .nsp name but takes only the relative operators, so a
	// notContains on one is an operator gap rather than a missing field.
	if _, isDate := wbDateFieldToNSP[c.Field]; isDate {
		e.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonDateOperator, Field: c.Field, Op: string(c.Op), Path: e.at()})
		return nil, nil, false
	}
	field, ok := wbFieldToNSP[c.Field]
	if !ok {
		e.rep.gap(NSPGap{Kind: NSPGapField, Code: NSPReasonUnsupportedField, Field: c.Field, Path: e.at()})
		return nil, nil, false
	}
	if nspScaledFields[model.CanonicalQueryField(c.Field)] {
		// WaxBin spells this operator as a negation and has no name for it, so the gap
		// names it the way .nsp does.
		e.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonScaledTextOperator, Field: c.Field, Op: "notContains", Path: e.at()})
		return nil, nil, false
	}
	if !e.opAllowed(c, "notContains") {
		return nil, nil, false
	}
	return map[string]any{"notContains": map[string]any{field: c.Value}}, n, true
}

// cond renders a single condition as an operator object, converting the value of a
// field in nspValueConvs and recording a gap for one that does not convert rather than
// writing a mismatched one.
func (e *nspExporter) cond(c query.Cond) (map[string]any, query.Node, bool) {
	if nspField, ok := wbDateFieldToNSP[c.Field]; ok {
		return e.dateCond(c, nspField)
	}
	field, ok := wbFieldToNSP[c.Field]
	if !ok {
		e.rep.gap(NSPGap{Kind: NSPGapField, Code: NSPReasonUnsupportedField, Field: c.Field, Path: e.at()})
		return nil, nil, false
	}
	if c.Op == query.OpIsMissing || c.Op == query.OpIsPresent {
		return e.presence(c, field)
	}
	op, ok := wbOpToNSP[c.Op]
	if !ok {
		e.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonUnsupportedOperator, Field: c.Field, Op: string(c.Op), Path: e.at()})
		return nil, nil, false
	}
	canon := model.CanonicalQueryField(c.Field)
	if nspScaledFields[canon] && nspTextOps[op] {
		e.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonScaledTextOperator, Field: c.Field, Op: string(c.Op), Path: e.at()})
		return nil, nil, false
	}
	if !e.opAllowed(c, op) {
		return nil, nil, false
	}
	conv, converts := nspValueConvs[canon]
	if c.Op == query.OpInRange {
		vals := c.Values
		if converts {
			vals = make([]any, len(c.Values))
			for i, x := range c.Values {
				sv, code := conv.out(x)
				if code != "" {
					e.valueGap(code, c, x)
					return nil, nil, false
				}
				vals[i] = sv
			}
		}
		return map[string]any{op: map[string]any{field: vals}}, c, true
	}
	val := c.Value
	if converts {
		sv, code := conv.out(c.Value)
		if code != "" {
			e.valueGap(code, c, c.Value)
			return nil, nil, false
		}
		val = sv
	}
	return map[string]any{op: map[string]any{field: val}}, c, true
}

// presence renders isMissing or isPresent in the true form, on the fields Navidrome
// allows them on.
func (e *nspExporter) presence(c query.Cond, field string) (map[string]any, query.Node, bool) {
	if !wbPresenceFields[c.Field] {
		e.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonPresenceOperator, Field: c.Field, Op: string(c.Op), Path: e.at()})
		return nil, nil, false
	}
	return map[string]any{string(c.Op): map[string]any{field: true}}, c, true
}

// opAllowed reports whether nspOp crosses on c's field, recording the gap when it does
// not. It mirrors the importer's.
func (e *nspExporter) opAllowed(c query.Cond, nspOp string) bool {
	n, narrow := nspNarrowFields[model.CanonicalQueryField(c.Field)]
	if !narrow || n.ops[nspOp] {
		return true
	}
	e.rep.gap(NSPGap{Kind: NSPGapOperator, Code: n.code, Field: c.Field, Op: string(c.Op), Path: e.at()})
	return false
}

// dateCond renders a relative-time condition back to .nsp days. A window that is
// not a whole number of days has no .nsp representation (the whole-star rating
// precedent), as does any non-relative operator on a date field.
func (e *nspExporter) dateCond(c query.Cond, nspField string) (map[string]any, query.Node, bool) {
	var op string
	switch c.Op {
	case query.OpInTheLast:
		op = "inTheLast"
	case query.OpNotInTheLast:
		op = "notInTheLast"
	default:
		e.rep.gap(NSPGap{Kind: NSPGapOperator, Code: NSPReasonDateOperator, Field: c.Field, Op: string(c.Op), Path: e.at()})
		return nil, nil, false
	}
	// The window is read as int64 directly rather than through asFloat: a window
	// past about 104 days exceeds 2^53 ns and would silently round in a float64.
	var ns int64
	switch v := c.Value.(type) {
	case int64:
		ns = v
	case int:
		ns = int64(v)
	case float64:
		ns = int64(v)
		if float64(ns) != v {
			e.valueGap(NSPReasonWindowNotWholeDays, c, c.Value)
			return nil, nil, false
		}
	default:
		e.valueGap(NSPReasonValueNotNumeric, c, c.Value)
		return nil, nil, false
	}
	if ns <= 0 || ns%nspDayNS != 0 {
		e.valueGap(NSPReasonWindowNotWholeDays, c, c.Value)
		return nil, nil, false
	}
	return map[string]any{op: map[string]any{nspField: ns / nspDayNS}}, c, true
}

func (e *nspExporter) valueGap(code NSPReason, c query.Cond, val any) {
	e.rep.gap(NSPGap{Kind: NSPGapValue, Code: code, Field: c.Field, Op: string(c.Op), Value: val, Path: e.at()})
}

// CheckNSPExport reports what an export of q could not carry, without rendering
// anything. It never fails; see NSPReport.OK for the one thing an OK report does
// not promise.
func CheckNSPExport(q query.Query) NSPReport {
	e := nspExporter{rep: NSPReport{Direction: NSPDirExport}}
	e.walk(q)
	return e.rep
}

// ExportNSP renders a WaxBin item query as a Navidrome .nsp document. It rejects
// (all-or-nothing) any node, operator, or field with no faithful .nsp
// representation, reporting the first gap; CheckNSPExport lists them all and
// ExportNSPPartial renders what is left once they are dropped.
func ExportNSP(q query.Query) ([]byte, error) {
	e := nspExporter{rep: NSPReport{Direction: NSPDirExport}}
	group, _, _ := e.walk(q)
	if !e.rep.OK() {
		return nil, nspErr(e.rep.Gaps[0].Reason)
	}
	data, err := json.MarshalIndent(group, "", "  ")
	if err != nil {
		// A value JSON cannot encode is a bug in the value the caller built, not a
		// mapping .nsp cannot express, so it stays internal and out of the report.
		return nil, waxerr.Wrap(waxerr.CodeInternal, "playlist.nsp", err)
	}
	return data, nil
}

// NSPExport is a lossy render: the document, the rule that document actually
// describes, and what was dropped to get there. Rule is what makes the lossy
// path honest, since a caller can show it in WaxBin's own vocabulary before
// writing the file. It carries every adjustment the render made, which is what
// pins the invariant that ExportNSP(Rule) returns Data byte for byte.
type NSPExport struct {
	Data   []byte
	Rule   query.Query
	Report NSPReport
}

// ExportNSPPartial renders q with the parts that have no .nsp form dropped,
// reporting each one. It refuses only when a rule that started with constraints
// has none left, since a document matching the whole library is not a partial
// render of one that matched some of it.
func ExportNSPPartial(q query.Query) (*NSPExport, error) {
	e := nspExporter{rep: NSPReport{Direction: NSPDirExport}}
	group, rule, kept := e.walk(q)
	if q.Where != nil && !kept {
		return nil, nspErr("nsp: nothing in this rule has an .nsp form, so a partial export would match the whole library")
	}
	data, err := json.MarshalIndent(group, "", "  ")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "playlist.nsp", err)
	}
	return &NSPExport{Data: data, Rule: rule, Report: e.rep}, nil
}
