package scan

import "github.com/colespringer/waxbin/model"

// Verdict is what a put did with one file, the one rule a scan's counters and an import's
// report both read (countOutcome, inbox.FileOutcome).
type Verdict string

const (
	VerdictCreated   Verdict = "created"   // the file made a new item
	VerdictJoined    Verdict = "joined"    // the file joined a standing item: a book's next part, an encoding taking it over
	VerdictCopy      Verdict = "copy"      // the file joined a standing item as an alternate
	VerdictRevived   Verdict = "revived"   // the file brought a missing item back
	VerdictUpdated   Verdict = "updated"   // new bytes, a re-derived field, a new row for a standing item
	VerdictSidecars  Verdict = "sidecars"  // only a sidecar (lyrics, a cue sheet, a cover) changed
	VerdictUnchanged Verdict = "unchanged" // nothing changed
)

// VerdictOf is the verdict a put's outcome reads as. A copy that arrived is a copy, and
// past that a missing item found again says so before a join or an update does.
func VerdictOf(out *model.ScanItemResult) Verdict {
	switch {
	case out.ItemCreated:
		return VerdictCreated
	case out.AttachedAsCopy && out.Joined:
		return VerdictCopy
	case out.Revived:
		return VerdictRevived
	case out.AttachedAsCopy && out.ContentChanged:
		return VerdictUpdated
	case out.AttachedAsCopy:
		return VerdictUnchanged
	case out.Joined:
		return VerdictJoined
	case out.ContentChanged, out.MetadataChanged, out.FileCreated:
		return VerdictUpdated
	case out.SidecarsChanged:
		return VerdictSidecars
	}
	return VerdictUnchanged
}
