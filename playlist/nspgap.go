package playlist

import (
	"fmt"
	"strings"

	"github.com/colespringer/waxbin/query"
)

// NSPGapKind classifies why a part of a query or an .nsp document has no
// counterpart on the other side.
type NSPGapKind string

const (
	NSPGapField     NSPGapKind = "field"    // no counterpart field
	NSPGapOperator  NSPGapKind = "operator" // no counterpart operator
	NSPGapValue     NSPGapKind = "value"    // a value outside the other side's domain
	NSPGapShape     NSPGapKind = "shape"    // a rule shape, such as a negation that is not notContains
	NSPGapSort      NSPGapKind = "sort"
	NSPGapLimit     NSPGapKind = "limit" // a limit mode, a seed, or the limit that rides on one
	NSPGapEntity    NSPGapKind = "entity"
	NSPGapMalformed NSPGapKind = "malformed" // import only: the document is broken, not unmappable
)

// NSPReason is the stable code for why a gap exists, one per sentence a report can
// carry, so a caller can switch on it rather than parse Reason. Kind and code are
// independent: Kind is the class a direction files the gap under and the code is the
// reason, so the same conversion failure is a malformed gap on import (a value
// Navidrome itself would not write) and a value gap on export (a stored value with no
// .nsp form), under one code. Each code's comment names the gap fields it fills.
type NSPReason string

const (
	// Import only, the document is broken rather than unmappable.
	NSPReasonMultipleRoots NSPReason = "multiple_roots"  // both all and any at the top
	NSPReasonMissingRoot   NSPReason = "missing_root"    // neither all nor any
	NSPReasonGroupNotArray NSPReason = "group_not_array" // Key
	NSPReasonRuleShape     NSPReason = "rule_shape"      // a rule that is not one operator or group
	NSPReasonOperatorShape NSPReason = "operator_shape"  // Op
	NSPReasonDaysNotNumber NSPReason = "days_not_number" // Field, Op
	NSPReasonRangeShape    NSPReason = "range_shape"     // Field, Op
	NSPReasonBadValue      NSPReason = "bad_value"       // Field, Op
	NSPReasonBadLimit      NSPReason = "bad_limit"
	NSPReasonBadOffset     NSPReason = "bad_offset"
	NSPReasonBadSort       NSPReason = "bad_sort"
	NSPReasonBadOrder      NSPReason = "bad_order"
	// A value that does not convert: malformed on import, a value gap on export. Field,
	// Op, Value.
	NSPReasonValueNotNumeric NSPReason = "value_not_numeric"
	NSPReasonValueNotBoolean NSPReason = "value_not_boolean"
	// Shape.
	NSPReasonUnsupportedKey  NSPReason = "unsupported_key" // Key, a top-level key with no WaxBin form
	NSPReasonGroupEmptied    NSPReason = "group_emptied"   // a group whose every rule was dropped
	NSPReasonUnsupportedNode NSPReason = "unsupported_node"
	NSPReasonNegation        NSPReason = "negation" // Field when the negated node is a condition
	// Field.
	NSPReasonUnsupportedField NSPReason = "unsupported_field" // Field
	// Operator, all Field and Op.
	NSPReasonDateOperator        NSPReason = "date_operator" // anything but inTheLast/notInTheLast on a date
	NSPReasonRatingTextOperator  NSPReason = "rating_text_operator"
	NSPReasonUnsupportedOperator NSPReason = "unsupported_operator"
	NSPReasonBooleanOperator     NSPReason = "boolean_operator" // anything but is/isNot on a boolean
	// Value, all Field, Op and Value. A whole star is a multiple of nspRatingScale on
	// WaxBin's 0-to-100 scale, which the format fixes.
	NSPReasonWindowTooLarge     NSPReason = "window_too_large"
	NSPReasonWindowNotWholeDays NSPReason = "window_not_whole_days"
	NSPReasonRatingNotWholeStar NSPReason = "rating_not_whole_star"
	// Sort.
	NSPReasonUnsupportedSortField NSPReason = "unsupported_sort_field" // Field
	NSPReasonRandomWithSorts      NSPReason = "random_with_sorts"      // Field, the first sort term
	NSPReasonExtraSortTerm        NSPReason = "extra_sort_term"        // Field, Value (the query.Sort)
	// Limit.
	NSPReasonRandomNeedsLimit NSPReason = "random_needs_limit" // Value
	NSPReasonLimitMode        NSPReason = "limit_mode"         // Value
	NSPReasonLimitSeed        NSPReason = "limit_seed"         // Value
	NSPReasonLimitBudget      NSPReason = "limit_budget"       // Value, Mode
	// Entity.
	NSPReasonEntityWidens NSPReason = "entity_widens" // a note; Value
	NSPReasonEntityFiles  NSPReason = "entity_files"  // Value
)

// NSPReasons returns every reason code.
func NSPReasons() []NSPReason {
	return []NSPReason{
		NSPReasonMultipleRoots, NSPReasonMissingRoot, NSPReasonGroupNotArray, NSPReasonRuleShape,
		NSPReasonOperatorShape, NSPReasonDaysNotNumber, NSPReasonRangeShape, NSPReasonBadValue,
		NSPReasonBadLimit, NSPReasonBadOffset, NSPReasonBadSort, NSPReasonBadOrder,
		NSPReasonValueNotNumeric, NSPReasonValueNotBoolean,
		NSPReasonUnsupportedKey, NSPReasonGroupEmptied, NSPReasonUnsupportedNode, NSPReasonNegation,
		NSPReasonUnsupportedField,
		NSPReasonDateOperator, NSPReasonRatingTextOperator, NSPReasonUnsupportedOperator, NSPReasonBooleanOperator,
		NSPReasonWindowTooLarge, NSPReasonWindowNotWholeDays, NSPReasonRatingNotWholeStar,
		NSPReasonUnsupportedSortField, NSPReasonRandomWithSorts, NSPReasonExtraSortTerm,
		NSPReasonRandomNeedsLimit, NSPReasonLimitMode, NSPReasonLimitSeed, NSPReasonLimitBudget,
		NSPReasonEntityWidens, NSPReasonEntityFiles,
	}
}

// NSPDirection is which way a report's mapping ran, and so whose vocabulary its
// Field and Op strings are written in.
type NSPDirection string

const (
	NSPDirExport NSPDirection = "export"
	NSPDirImport NSPDirection = "import"
)

// NSPGap is one part of a query or an .nsp document with no faithful counterpart
// on the other side. Field and Op name it in the vocabulary of the side being
// read, which is why the report carries a Direction: WaxBin's names on export,
// Navidrome's on import. Both are plain strings because only one direction has a
// query.Op to name; the one exception is a negated contains on rating, which has no
// WaxBin operator of its own and is named notContains. Value carries the offending
// value, Key a document key, and Mode the limit mode a budget limit rides on, as each
// Code says. Reason is rendered from the code and those fields, never written by hand.
//
// A gap names what the walk examined before it stopped, since a leaf is checked
// field first, then operator, then value, and stops at the first fault. So a
// field gap leaves Op empty (the operator was never reached) while an operator
// gap carries the field it was used on. Reading Fields and Ops as "these have no
// counterpart at all" is therefore wrong in one case: an operator that is fine
// elsewhere but not on a date field puts both names in the report.
type NSPGap struct {
	Kind   NSPGapKind `json:"kind"`
	Code   NSPReason  `json:"code"`
	Field  string     `json:"field,omitempty"`
	Op     string     `json:"op,omitempty"`
	Value  any        `json:"value,omitempty"`
	Key    string     `json:"key,omitempty"`
	Mode   string     `json:"mode,omitempty"`
	Path   string     `json:"path"`   // an RFC 6901 JSON Pointer, see nspPointer
	Reason string     `json:"reason"` // the sentence ExportNSP/ImportNSP returns for this gap
}

// NSPReport is what one mapping could not carry. Gaps block the strict
// conversion; on a partial conversion they are what it dropped. Notes are losses
// that do not block anything, so a caller can mention them without refusing.
type NSPReport struct {
	Direction NSPDirection `json:"direction"`
	Gaps      []NSPGap     `json:"gaps,omitempty"`
	Notes     []NSPGap     `json:"notes,omitempty"`
}

// OK reports whether the mapping carries. It is a statement about
// expressiveness only: CheckNSPExport discards the render, so an OK report does
// not promise the query's values will marshal to JSON (see ExportNSP).
func (r NSPReport) OK() bool { return len(r.Gaps) == 0 }

// All returns the gaps followed by the notes.
func (r NSPReport) All() []NSPGap {
	if len(r.Gaps) == 0 && len(r.Notes) == 0 {
		return nil
	}
	out := make([]NSPGap, 0, len(r.Gaps)+len(r.Notes))
	return append(append(out, r.Gaps...), r.Notes...)
}

// Fields returns the named fields over All, deduped, in first-seen order.
func (r NSPReport) Fields() []string {
	return nspNames(r.All(), func(g NSPGap) string { return g.Field })
}

// Ops returns the named operators over All, deduped, in first-seen order.
func (r NSPReport) Ops() []string { return nspNames(r.All(), func(g NSPGap) string { return g.Op }) }

func nspNames(gaps []NSPGap, pick func(NSPGap) string) []string {
	var out []string
	seen := make(map[string]bool, len(gaps))
	for _, g := range gaps {
		name := pick(g)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func (r *NSPReport) gap(g NSPGap) {
	g.Reason = g.sentence(r.Direction)
	r.Gaps = append(r.Gaps, g)
}

func (r *NSPReport) note(g NSPGap) {
	g.Reason = g.sentence(r.Direction)
	r.Notes = append(r.Notes, g)
}

// sentence renders a gap's Reason from its code and fields, in the words of the side
// the report reads.
func (g NSPGap) sentence(dir NSPDirection) string {
	imp := dir == NSPDirImport
	other := ".nsp"
	if imp {
		other = "WaxBin"
	}
	switch g.Code {
	case NSPReasonMultipleRoots:
		return "nsp: multiple root groups"
	case NSPReasonMissingRoot:
		return "nsp: missing all/any root group"
	case NSPReasonGroupNotArray:
		return "nsp: " + g.Key + " must be an array"
	case NSPReasonRuleShape:
		return "nsp: each rule needs exactly one operator or group"
	case NSPReasonOperatorShape:
		return "nsp: operator " + g.Op + " needs exactly one field"
	case NSPReasonDaysNotNumber:
		return "nsp: " + g.Op + " needs a number of days"
	case NSPReasonRangeShape:
		return "nsp: " + g.Op + " needs a [low, high] array"
	case NSPReasonBadValue:
		return "nsp: bad value for " + g.Op
	case NSPReasonBadLimit:
		return "nsp: bad limit"
	case NSPReasonBadOffset:
		return "nsp: bad offset"
	case NSPReasonBadSort:
		return "nsp: bad sort"
	case NSPReasonBadOrder:
		return "nsp: bad order"
	case NSPReasonValueNotNumeric:
		return "nsp: " + g.Field + " value must be numeric"
	case NSPReasonValueNotBoolean:
		if imp {
			return fmt.Sprintf("nsp: %s value %v is not a boolean", g.Field, g.Value)
		}
		return fmt.Sprintf("nsp: WaxBin %s value %v is neither 0 nor 1 and has no Navidrome boolean equivalent", g.Field, g.Value)
	case NSPReasonUnsupportedKey:
		return "nsp: unsupported top-level key: " + g.Key
	case NSPReasonGroupEmptied:
		return "nsp: every rule in this group has no " + other + " form"
	case NSPReasonUnsupportedNode:
		return "nsp: unsupported rule node"
	case NSPReasonNegation:
		return "nsp: unsupported negation (only notContains maps)"
	case NSPReasonUnsupportedField:
		return "nsp: unsupported field: " + g.Field
	case NSPReasonDateOperator:
		field := g.Field
		if name, ok := wbDateFieldToNSP[field]; ok && !imp {
			field = name
		}
		return "nsp: only inTheLast/notInTheLast are supported on " + field
	case NSPReasonRatingTextOperator:
		scale := "0-to-5"
		if imp {
			scale = "0-to-100"
		}
		return "nsp: " + g.Op + " on rating has no " + other + " equivalent, since the " + scale +
			" scale conversion does not carry a substring match"
	case NSPReasonUnsupportedOperator:
		return "nsp: unsupported operator: " + g.Op
	case NSPReasonBooleanOperator:
		return "nsp: only is/isNot are supported on " + g.Field + ", which is a boolean"
	case NSPReasonWindowTooLarge:
		return fmt.Sprintf("nsp: %s window of %v days is too large", g.Op, g.Value)
	case NSPReasonWindowNotWholeDays:
		if imp {
			return fmt.Sprintf("nsp: %s needs a positive whole number of days, got %v", g.Op, g.Value)
		}
		return fmt.Sprintf("nsp: %s window %v ns is not a whole number of days and has no .nsp equivalent", g.Op, g.Value)
	case NSPReasonRatingNotWholeStar:
		return fmt.Sprintf("nsp: WaxBin rating %v is not a whole star (a multiple of %d) and has no Navidrome 0-5 equivalent",
			g.Value, nspRatingScale)
	case NSPReasonUnsupportedSortField:
		return "nsp: unsupported sort field: " + g.Field
	case NSPReasonRandomWithSorts:
		return "nsp: random limit mode combined with sorts is not exportable"
	case NSPReasonExtraSortTerm:
		term := g.Field
		if s, ok := g.Value.(query.Sort); ok && s.Desc {
			term += " desc"
		}
		return "nsp: .nsp holds a single sort term, so the sort term " + term + " has no .nsp representation"
	case NSPReasonRandomNeedsLimit:
		if imp {
			return "nsp: sort random requires a positive limit"
		}
		return "nsp: random limit mode requires a positive limit"
	case NSPReasonLimitMode:
		return fmt.Sprintf("nsp: limit mode %v has no .nsp representation", g.Value)
	case NSPReasonLimitSeed:
		return "nsp: a pinned limit seed has no .nsp representation"
	case NSPReasonLimitBudget:
		return fmt.Sprintf("nsp: limit %v is a %s budget with no .nsp representation", g.Value, g.Mode)
	case NSPReasonEntityWidens:
		return "nsp: .nsp has no track/book distinction, so this rule re-imports as one over every kind of item"
	case NSPReasonEntityFiles:
		return "nsp: a selection over file rows is not a playlist of items"
	}
	return ""
}

// nspPointer renders a walk position as an RFC 6901 JSON Pointer. Most segments
// are fixed keys or array indices, but not all of them are ours: the import walk
// names a top-level key the document supplied, and a key holding a "/" would
// otherwise read as two segments in a pointer a rule editor is meant to follow.
func nspPointer(seg []string) string {
	if len(seg) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range seg {
		b.WriteByte('/')
		if strings.ContainsAny(s, "~/") {
			s = nspPointerEscape.Replace(s)
		}
		b.WriteString(s)
	}
	return b.String()
}

// nspPointerEscape is RFC 6901's escaping, "~" first so an escaped "/" is not
// escaped again.
var nspPointerEscape = strings.NewReplacer("~", "~0", "/", "~1")
