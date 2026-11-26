package inbox_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
)

// assignedRequest is an import of staging assigning every file to Dune by Frank Herbert, at
// the places given by file name.
func assignedRequest(t *testing.T, staging string, places map[string]int) inbox.Request {
	t.Helper()
	req := importRequest(t, staging)
	req.ForceKind = model.KindBook
	req.Assign = &inbox.Assignment{Title: "Dune", Author: "Frank Herbert", Parts: 3, Places: map[string]int{}}
	for name, p := range places {
		req.Assign.Places[filepath.Join(staging, name)] = p
	}
	return req
}

// TestAnAssignedImportNumbersItsParts: files whose tags key books of their own plan into the
// assigned book, numbered by the places assigned, as one unit, each carrying the assignment
// its scan records.
func TestAnAssignedImportNumbersItsParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	for i, name := range []string{"x.mp3", "y.mp3", "z.mp3"} {
		stage(t, filepath.Join(staging, name), testaudio.MP3Spec{Title: name, Album: "Guess " + name, Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(ctx, assignedRequest(t, staging, map[string]int{"z.mp3": 1, "x.mp3": 2, "y.mp3": 3}))
	if err != nil {
		t.Fatal(err)
	}
	wantPlanned(t, planned(t, plan), map[string]string{"z.mp3": "Dune/Dune - 01.mp3", "x.mp3": "Dune/Dune - 02.mp3", "y.mp3": "Dune/Dune - 03.mp3"})
	for _, a := range plan.Actions {
		if a.Assign == nil || a.Assign.Position == nil || *a.Assign.Position != a.Position || a.Unit != plan.Actions[0].Unit {
			t.Errorf("%s: assign %+v at %d in unit %q, want its place assigned in one unit", filepath.Base(a.Src), a.Assign, a.Position, a.Unit)
		}
	}
}

// TestAnAssignedImportLeavesTheFolderRuleOut: a numbered part with no album in a staging
// folder whose other files name another book plans into the assigned book with them, rather
// than joining the book its folder holds.
func TestAnAssignedImportLeavesTheFolderRuleOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	for i := 1; i <= 2; i++ {
		stage(t, filepath.Join(staging, "Other", "0"+string(rune('0'+i))+".mp3"), testaudio.MP3Spec{Title: "Chapter", Artist: "Writer",
			Album: "Other", Track: i, TXXX: narrated, Audio: testaudio.AudioWithSeed(byte(i))})
	}
	stage(t, filepath.Join(staging, "Other", "03.mp3"), testaudio.MP3Spec{Track: 3, Audio: testaudio.AudioWithSeed(3)})
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(ctx, assignedRequest(t, staging,
		map[string]int{"Other/01.mp3": 1, "Other/02.mp3": 2, "Other/03.mp3": 3}))
	if err != nil {
		t.Fatal(err)
	}
	wantPlanned(t, planned(t, plan), map[string]string{"01.mp3": "Dune {Reader}/Dune - 01.mp3", "02.mp3": "Dune {Reader}/Dune - 02.mp3",
		"03.mp3": "Dune {Reader}/Dune - 03.mp3"})
	for _, a := range plan.Actions {
		if a.Joined || a.Album != "" {
			t.Errorf("%s: joined %v album %q, want the folder rule left out", filepath.Base(a.Src), a.Joined, a.Album)
		}
	}
}

// TestAnAssignedImportHoldsItsBook: an assigned book one of whose parts cannot land stays
// where it is whole.
func TestAnAssignedImportHoldsItsBook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	for i, name := range []string{"x.mp3", "y.mp3", "z.mp3"} {
		stage(t, filepath.Join(staging, name), testaudio.MP3Spec{Title: name, Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	svc := inbox.New(emptyStore{}, nil, nil, nil)
	req := assignedRequest(t, staging, map[string]int{"x.mp3": 1, "y.mp3": 2, "z.mp3": 3})
	plan, err := svc.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var taken string
	for _, a := range plan.Actions {
		if filepath.Base(a.Src) == "y.mp3" {
			taken = a.Dst
		}
	}
	if err := os.MkdirAll(filepath.Dir(taken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taken, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	if plan, err = svc.Plan(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if a.Outcome != inbox.OutcomeQuarantine {
			t.Errorf("%s: %s, want the book held whole", filepath.Base(a.Src), a.Outcome)
		}
	}
}
