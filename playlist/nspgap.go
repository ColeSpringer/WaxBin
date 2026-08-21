package playlist

import "strings"

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
// query.Op to name. Value carries the offending value on a NSPGapValue gap, so a
// caller renders "85" without picking the Reason sentence apart.
//
// A gap names what the walk examined before it stopped, since a leaf is checked
// field first, then operator, then value, and stops at the first fault. So a
// field gap leaves Op empty (the operator was never reached) while an operator
// gap carries the field it was used on. Reading Fields and Ops as "these have no
// counterpart at all" is therefore wrong in one case: an operator that is fine
// elsewhere but not on a date field puts both names in the report.
type NSPGap struct {
	Kind   NSPGapKind `json:"kind"`
	Field  string     `json:"field,omitempty"`
	Op     string     `json:"op,omitempty"`
	Value  any        `json:"value,omitempty"`
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

func (r *NSPReport) gap(g NSPGap)  { r.Gaps = append(r.Gaps, g) }
func (r *NSPReport) note(g NSPGap) { r.Notes = append(r.Notes, g) }

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
