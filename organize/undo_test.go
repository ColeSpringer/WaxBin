package organize

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestUndoPlanWalksTheJournalBack: each file goes from where the job left it to where it
// found it, a file the job moved twice (parked on the way to its name) from its last
// destination to its first source, last move first; a file elsewhere now is held where it
// stands, a journal row naming no file is held, and a file already back is in place.
func TestUndoPlanWalksTheJournalBack(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(wd)
	p := func(s string) string { return vol + filepath.FromSlash(s) }
	b := func(s string) []byte { return []byte(p(s)) }
	moves := []model.OrganizeMove{
		{FilePID: "A", ItemPID: "IA", Src: b("/r/in/a.mp3"), Dst: b("/r/out/tmp.mp3"), Path: b("/r/out/a.mp3"), Root: b("/r")},
		{FilePID: "B", ItemPID: "IB", Src: b("/r/in/b.mp3"), Dst: b("/r/out/b.mp3"), Path: b("/r/elsewhere/b.mp3"), Root: b("/r")},
		{Src: b("/r/in/c.mp3"), Dst: b("/r/out/c.mp3")},
		{FilePID: "A", ItemPID: "IA", Src: b("/r/out/tmp.mp3"), Dst: b("/r/out/a.mp3"), Path: b("/r/out/a.mp3"), Root: b("/r")},
		{FilePID: "D", ItemPID: "ID", Src: b("/r/in/d.mp3"), Dst: b("/r/in/d.mp3"), Path: b("/r/in/d.mp3"), Root: b("/r")},
	}
	plan := UndoPlan(moves)
	type want struct {
		file     model.PID
		src, dst string
		code     HoldCode
	}
	wants := []want{
		{"D", p("/r/in/d.mp3"), p("/r/in/d.mp3"), HoldInPlace},
		{"A", p("/r/out/a.mp3"), p("/r/in/a.mp3"), ""},
		{"", p("/r/out/c.mp3"), p("/r/in/c.mp3"), HoldUncataloged},
		{"B", p("/r/elsewhere/b.mp3"), p("/r/in/b.mp3"), HoldMovedSince},
	}
	if len(plan.Actions) != len(wants) {
		t.Fatalf("actions = %+v, want %d", plan.Actions, len(wants))
	}
	for i, w := range wants {
		a := plan.Actions[i]
		if a.FilePID != w.file || a.Src != w.src || a.Dst != w.dst || a.Code != w.code {
			t.Errorf("action %d = %s %s -> %s [%s], want %s %s -> %s [%s]", i, a.FilePID, a.Src, a.Dst, a.Code, w.file, w.src, w.dst, w.code)
		}
	}
	if a := plan.Actions[1]; a.RelDst != filepath.Join("in", "a.mp3") || a.Root != p("/r") || a.ItemPID != "IA" || a.Skip {
		t.Errorf("the moving action = %+v, want it relative to its root and free to move", a)
	}
}

// TestUndoPlanHoldsAnUncatalogedChainOnce: a file the catalog no longer holds, which the
// job parked on the way to its name, is one held move from where the job left it to where
// it found it, not one per journal row.
func TestUndoPlanHoldsAnUncatalogedChainOnce(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(wd)
	p := func(s string) string { return vol + filepath.FromSlash(s) }
	b := func(s string) []byte { return []byte(p(s)) }
	plan := UndoPlan([]model.OrganizeMove{
		{Src: b("/r/in/e.mp3"), Dst: b("/r/out/tmp.mp3")},
		{Src: b("/r/in/f.mp3"), Dst: b("/r/out/f.mp3")},
		{Src: b("/r/out/tmp.mp3"), Dst: b("/r/out/e.mp3")},
	})
	if len(plan.Actions) != 2 {
		t.Fatalf("actions = %+v, want one per file", plan.Actions)
	}
	for i, w := range [][2]string{{p("/r/out/e.mp3"), p("/r/in/e.mp3")}, {p("/r/out/f.mp3"), p("/r/in/f.mp3")}} {
		if a := plan.Actions[i]; a.Src != w[0] || a.Dst != w[1] || a.Code != HoldUncataloged {
			t.Errorf("action %d = %s -> %s [%s], want %s -> %s held uncataloged", i, a.Src, a.Dst, a.Code, w[0], w[1])
		}
	}
}

// TestUndoPlanHoldsAMovedFileWhereItIs: a file moved again since the organize is held where
// it stands now, so the place the organize left it, empty since, is free for another file
// of the job to go back to; nor does a file the catalog dropped claim where it was left.
func TestUndoPlanHoldsAMovedFileWhereItIs(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(wd)
	p := func(s string) string { return vol + filepath.FromSlash(s) }
	b := func(s string) []byte { return []byte(p(s)) }
	// The job moved Y from B to C, then X from A to B; X has since moved on to D. It also
	// moved W from E to F and a file the catalog has since dropped from G to E.
	plan := UndoPlan([]model.OrganizeMove{
		{FilePID: "Y", ItemPID: "IY", Src: b("/r/b.mp3"), Dst: b("/r/c.mp3"), Path: b("/r/c.mp3"), Root: b("/r")},
		{FilePID: "X", ItemPID: "IX", Src: b("/r/a.mp3"), Dst: b("/r/b.mp3"), Path: b("/r/d.mp3"), Root: b("/r")},
		{FilePID: "W", ItemPID: "IW", Src: b("/r/e.mp3"), Dst: b("/r/f.mp3"), Path: b("/r/f.mp3"), Root: b("/r")},
		{Src: b("/r/g.mp3"), Dst: b("/r/e.mp3")},
	})
	markCollisions(plan, nil)
	for _, a := range plan.Actions {
		switch a.FilePID {
		case "X":
			if a.Code != HoldMovedSince || a.Src != p("/r/d.mp3") {
				t.Errorf("X = %s -> %s [%s], want held where it stands, %s", a.Src, a.Dst, a.Code, p("/r/d.mp3"))
			}
		case "Y":
			if a.Skip || a.Src != p("/r/c.mp3") || a.Dst != p("/r/b.mp3") {
				t.Errorf("Y = %s -> %s [%s %s], want free to go back to the place X left", a.Src, a.Dst, a.Code, a.Reason)
			}
		case "W":
			// Whether the dropped file is still at E is Hold's look at the disk to decide.
			if a.Skip {
				t.Errorf("W = %s -> %s [%s %s], want no collision with where the dropped file was left", a.Src, a.Dst, a.Code, a.Reason)
			}
		}
	}
}

// TestUndoPlanFindsAFileAlreadyBack: a file the job moved that stands where the job found it
// again (an undo already run, say) is in place, not moved since; and one whose folder was
// respelled since is where the job left it, going back from where it stands.
func TestUndoPlanFindsAFileAlreadyBack(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(wd)
	p := func(s string) string { return vol + filepath.FromSlash(s) }
	b := func(s string) []byte { return []byte(p(s)) }
	plan := UndoPlan([]model.OrganizeMove{
		{FilePID: "E", ItemPID: "IE", Src: b("/r/in/e.mp3"), Dst: b("/r/The Foobars/e.mp3"), Path: b("/r/in/e.mp3"), Root: b("/r")},
		{FilePID: "F", ItemPID: "IF", Src: b("/r/in/f.mp3"), Dst: b("/r/The Foobars/f.mp3"), Path: b("/r/the foobars/f.mp3"), Root: b("/r")},
	})
	for _, a := range plan.Actions {
		switch a.FilePID {
		case "E":
			if a.Code != HoldInPlace {
				t.Errorf("E = %s -> %s [%s %s], want in place", a.Src, a.Dst, a.Code, a.Reason)
			}
		case "F":
			if a.Skip || a.Src != p("/r/the foobars/f.mp3") || a.Dst != p("/r/in/f.mp3") {
				t.Errorf("F = %s -> %s [%s %s], want it going back from the respelled folder", a.Src, a.Dst, a.Code, a.Reason)
			}
		}
	}
}
