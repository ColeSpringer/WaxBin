package inbox_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testpath"
)

// TestPlanWalksALinkedSource: a staging folder given as a link to a folder is walked as
// that folder, each staged file spelled under the source as given.
func TestPlanWalksALinkedSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "inbox")
	stage(t, filepath.Join(real, "one.mp3"), testaudio.MP3Spec{Title: "One", Artist: "A", Album: "L", Track: 1, Audio: testaudio.AudioWithSeed(1)})
	stage(t, filepath.Join(real, "deeper", "two.mp3"), testaudio.MP3Spec{Title: "Two", Artist: "A", Album: "L", Track: 2, Audio: testaudio.AudioWithSeed(2)})
	testpath.Symlink(t, real, link)
	svc := inbox.New(emptyStore{}, nil, nil, nil)
	plan, err := svc.Plan(context.Background(), importRequest(t, link))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Importable() != 2 || len(plan.Actions) != 2 {
		t.Fatalf("plan = %+v, want both staged files importable", plan)
	}
	for _, a := range plan.Actions {
		if !strings.HasPrefix(a.Src, link+string(filepath.Separator)) {
			t.Errorf("action source %s, want it spelled under the source %s", a.Src, link)
		}
	}
}

// TestPlanSkipsALinkToAFile: a source that is a link to a file, or a linked file in a
// staged folder, is not planned: a move would take the link itself into the library and
// leave its audio where it is.
func TestPlanSkipsALinkToAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	stage(t, filepath.Join(real, "one.mp3"), testaudio.MP3Spec{Title: "One", Artist: "A", Album: "L", Track: 1, Audio: testaudio.AudioWithSeed(1)})
	link := filepath.Join(dir, "one-link.mp3")
	testpath.Symlink(t, filepath.Join(real, "one.mp3"), link)
	staging := filepath.Join(dir, "staging")
	stage(t, filepath.Join(staging, "two.mp3"), testaudio.MP3Spec{Title: "Two", Artist: "A", Album: "L", Track: 2, Audio: testaudio.AudioWithSeed(2)})
	testpath.Symlink(t, filepath.Join(real, "one.mp3"), filepath.Join(staging, "linked.mp3"))
	svc := inbox.New(emptyStore{}, nil, nil, nil)
	ctx := context.Background()
	if plan, err := svc.Plan(ctx, importRequest(t, link)); err != nil || len(plan.Actions) != 0 {
		t.Errorf("plan of a link to a file = %+v (err %v), want nothing planned", plan, err)
	}
	plan, err := svc.Plan(ctx, importRequest(t, staging))
	if err != nil || len(plan.Actions) != 1 || filepath.Base(plan.Actions[0].Src) != "two.mp3" {
		t.Errorf("plan of a folder holding a linked file = %+v (err %v), want the real file alone", plan, err)
	}
}

// TestPlanHoldsADestinationBehindALink: a destination whose folder is reached through a
// link below the library root is held, since a scan would never catalog the file there.
func TestPlanHoldsADestinationBehindALink(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	stage(t, filepath.Join(staging, "one.mp3"), testaudio.MP3Spec{Title: "One", Artist: "Linked Artist", Album: "L", Track: 1, Audio: testaudio.AudioWithSeed(1)})
	svc := inbox.New(emptyStore{}, nil, nil, nil)
	ctx := context.Background()
	req := importRequest(t, staging)
	plan, err := svc.Plan(ctx, req)
	if err != nil || len(plan.Actions) != 1 || plan.Actions[0].Outcome != inbox.OutcomeImport {
		t.Fatalf("plan = %+v (err %v), want the file importable", plan, err)
	}
	first := strings.SplitN(filepath.ToSlash(plan.Actions[0].RelDst), "/", 2)[0]
	link := filepath.Join(req.Library.RootPath(), first)
	testpath.Symlink(t, t.TempDir(), link)
	plan, err = svc.Plan(ctx, req)
	if err != nil || len(plan.Actions) != 1 {
		t.Fatalf("plan with the destination folder a link = %+v (err %v)", plan, err)
	}
	if a := plan.Actions[0]; a.Outcome != inbox.OutcomeQuarantine || a.Code != inbox.HoldLinkedFolder || !strings.Contains(a.Reason, link) {
		t.Errorf("action = %+v, want it quarantined as a linked folder naming %s", a, link)
	}
}
