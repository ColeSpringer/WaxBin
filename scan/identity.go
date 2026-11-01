package scan

import (
	"slices"

	"github.com/colespringer/waxbin/model"
)

// identities is what a scan's puts did to items' pids, the Result's Converted, Absorbed
// and Created, gathered from every put the scan made: the walk's, the folder rule's
// second looks, and the re-reads of promoted files.
type identities struct {
	converted         []model.PID
	isConverted       map[model.PID]bool
	absorbed, created map[model.PID]model.PID
}

// note records one put's outcome. A nil log records nothing.
func (l *identities) note(out *model.ScanItemResult) {
	if l == nil || out == nil {
		return
	}
	if out.Rekinded && out.ItemPID != "" && !l.isConverted[out.ItemPID] {
		if l.isConverted == nil {
			l.isConverted = map[model.PID]bool{}
		}
		l.isConverted[out.ItemPID] = true
		l.converted = append(l.converted, out.ItemPID)
	}
	for p, into := range out.Absorbed {
		if l.absorbed == nil {
			l.absorbed = map[model.PID]model.PID{}
		}
		l.absorbed[p] = into
	}
	for p, from := range out.Minted {
		if l.created == nil {
			l.created = map[model.PID]model.PID{}
		}
		l.created[p] = from
	}
}

// repid renames an item the log names, for a rebuild handing a fresh item its stamp.
func (l *identities) repid(from, to model.PID) {
	if l == nil {
		return
	}
	if l.isConverted[from] {
		delete(l.isConverted, from)
		l.isConverted[to] = true
		l.converted[slices.Index(l.converted, from)] = to
	}
	for _, m := range []map[model.PID]model.PID{l.absorbed, l.created} {
		for k, v := range m {
			if v == from {
				m[k] = to
			}
		}
		if v, ok := m[from]; ok {
			delete(m, from)
			m[to] = v
		}
	}
}

// fill writes the log into res, merged with what it holds (identitiesInto).
func (l *identities) fill(res *Result) {
	if l == nil {
		return
	}
	identitiesInto(res, l.converted, l.absorbed, l.created)
}

// Changed reports whether the scan changed the catalog: an item made, updated, relinked,
// marked missing or re-read for a sidecar, a copy joined, promoted or dropped, or an item's
// pid changed.
func (r *Result) Changed() bool {
	return r.ItemsCreated > 0 || r.ItemsUpdated > 0 || r.Relinked > 0 || r.Missing > 0 || r.SidecarsUpdated > 0 ||
		r.Copies > 0 || r.Promoted > 0 || r.Dropped > 0 || len(r.Converted) > 0 || len(r.Absorbed) > 0 || len(r.Created) > 0
}

// MergeIdentities adds another result's Converted, Absorbed and Created to r's, as a scan
// over several libraries totals them.
func (r *Result) MergeIdentities(o *Result) {
	identitiesInto(r, o.Converted, o.Absorbed, o.Created)
}

// identitiesInto adds identities to res and settles them (model.SettleIdentities).
func identitiesInto(res *Result, converted []model.PID, absorbed, created map[model.PID]model.PID) {
	have := make(map[model.PID]bool, len(res.Converted)+len(converted))
	for _, p := range res.Converted {
		have[p] = true
	}
	for _, p := range converted {
		if !have[p] {
			have[p] = true
			res.Converted = append(res.Converted, p)
		}
	}
	for p, into := range absorbed {
		if res.Absorbed == nil {
			res.Absorbed = map[model.PID]model.PID{}
		}
		res.Absorbed[p] = into
	}
	for p, from := range created {
		if res.Created == nil {
			res.Created = map[model.PID]model.PID{}
		}
		res.Created[p] = from
	}
	res.Converted = model.SettleIdentities(res.Converted, res.Absorbed, res.Created)
}
