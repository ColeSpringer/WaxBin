package organize

import (
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
)

// The holds only an undo makes.
const (
	HoldMovedSince  HoldCode = "moved-since" // the file moved again after the organize being undone
	HoldUncataloged HoldCode = "uncataloged" // the catalog no longer holds the file
)

// UndoPlan builds the plan that moves an organize job's files back, from where the job
// left each one to where it found it, last move first, and takes back the companion steps
// it journaled, last first (Plan.Companions). moves is the job's committed
// journal in the order it made them, and a file the job moved twice (parked on the way to
// its name) goes back from where it stands to its first source. One standing there already
// (an undo run before) is in place, one that has moved again since is held there, and one
// the catalog no longer holds is held as one move however many rows its chain took, linked
// by path; Hold then decides the rest as for any plan (an old place another file took, a
// file gone from disk).
func UndoPlan(moves []model.OrganizeMove) *Plan {
	first := map[model.PID]int{}
	last := map[model.PID]int{}
	// For a row naming no file, start holds the row its chain starts at, open maps the path
	// an open chain ends at to its start, and end maps a chain's start to its last row.
	start := make([]int, len(moves))
	open := map[string]int{}
	end := map[int]int{}
	for i, m := range moves {
		if companion(m.Kind) {
			continue
		}
		if m.FilePID == "" {
			s, ok := open[string(m.Src)]
			if !ok {
				s = i
			}
			delete(open, string(m.Src))
			start[i], open[string(m.Dst)], end[s] = s, s, i
			continue
		}
		if _, ok := first[m.FilePID]; !ok {
			first[m.FilePID] = i
		}
		last[m.FilePID] = i
	}
	plan := &Plan{Undo: true}
	for i := len(moves) - 1; i >= 0; i-- {
		m := moves[i]
		if companion(m.Kind) {
			plan.Companions = append(plan.Companions, inverseStep(m))
			continue
		}
		if m.FilePID == "" {
			if end[start[i]] != i {
				continue
			}
			a := Action{Src: string(m.Dst), SrcBytes: m.Dst, Dst: string(moves[start[i]].Src), unseated: true}
			a.hold(HoldUncataloged, "the catalog no longer holds the file")
			plan.Actions = append(plan.Actions, a)
			continue
		}
		if last[m.FilePID] != i {
			continue
		}
		// The file goes from where it stands, and places compare as the library lays them
		// out (pathx.CollisionKey), so a folder respelled since moved nothing.
		src := moves[first[m.FilePID]].Src
		a := Action{ItemPID: m.ItemPID, FilePID: m.FilePID, Src: string(m.Path), SrcBytes: m.Path,
			Dst: string(src), RelDst: pathx.RelUnder(string(m.Root), string(src)), Root: string(m.Root)}
		at := pathx.CollisionKey(string(m.Path))
		switch {
		case at == pathx.CollisionKey(string(src)):
			a.hold(HoldInPlace, "already where the organize found it")
		case at != pathx.CollisionKey(string(m.Dst)):
			a.hold(HoldMovedSince, "moved again since the organize left it at "+string(m.Dst))
		}
		plan.Actions = append(plan.Actions, a)
	}
	return plan
}

// companion reports whether a journal row is a companion step rather than a file's move.
func companion(k model.JournalKind) bool {
	return k == model.JournalCompanion || k == model.JournalCompanionCopy || k == model.JournalCompanionDrop
}

// inverseStep is the step that takes a journaled companion step back: a move goes back, a
// copy is removed, and a removed copy is made again.
func inverseStep(m model.OrganizeMove) CompanionStep {
	switch m.Kind {
	case model.JournalCompanionCopy:
		return CompanionStep{Kind: model.JournalCompanionDrop, Src: string(m.Src), Dst: string(m.Dst)}
	case model.JournalCompanionDrop:
		return CompanionStep{Kind: model.JournalCompanionCopy, Src: string(m.Src), Dst: string(m.Dst)}
	default:
		return CompanionStep{Kind: model.JournalCompanion, Src: string(m.Dst), Dst: string(m.Src)}
	}
}
