package inbox_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/waxerr"
)

// emptyStore is a catalog holding nothing, so every staged file is new.
type emptyStore struct{}

func (emptyStore) FileByEssence(context.Context, string) (*model.File, error) {
	return nil, waxerr.New(waxerr.CodeNotFound, "test", "no file")
}
func (emptyStore) DisplayPathExistsFold(context.Context, string) (bool, error) { return false, nil }
func (emptyStore) CreateImportBatch(context.Context, *model.ImportBatch) error { return nil }
func (emptyStore) UpdateImportBatch(context.Context, *model.ImportBatch) error { return nil }
func (emptyStore) PutAcquisitionForFile(context.Context, []byte, model.AcquisitionInput) (model.PID, error) {
	return "", nil
}
func (emptyStore) BookByKey(context.Context, int64, string) (*model.ItemView, []model.ItemFileRef, error) {
	return nil, nil, nil
}
func (emptyStore) RespellFolder(context.Context, string, string) (int, error) { return 0, nil }
func (emptyStore) FileByPID(context.Context, model.PID) (*model.File, error) {
	return nil, waxerr.New(waxerr.CodeNotFound, "test", "no file")
}

func stage(t *testing.T, path string, spec testaudio.MP3Spec) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, testaudio.BuildMP3FromSpec(spec), 0o644); err != nil {
		t.Fatal(err)
	}
}

func importRequest(t *testing.T, source string) inbox.Request {
	t.Helper()
	prof, err := organize.ProfileByName("waxbin-native")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	return inbox.Request{
		Source:  source,
		Profile: prof,
		Library: &model.Library{ID: 1, PID: "lib", Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged},
	}
}

// planned returns each action's destination below its book folder ("Book/Book - 01.mp3"),
// keyed by the staged file's base name, failing on any action that would not import.
func planned(t *testing.T, plan *inbox.Plan) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, a := range plan.Actions {
		if a.Outcome != inbox.OutcomeImport {
			t.Errorf("%s: %s (%s), want an import", filepath.Base(a.Src), a.Outcome, a.Reason)
			continue
		}
		out[filepath.Base(a.Src)] = filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(a.RelDst)), filepath.Base(a.RelDst)))
	}
	return out
}

func wantPlanned(t *testing.T, got, want map[string]string) {
	t.Helper()
	for src, dst := range want {
		if got[src] != dst {
			t.Errorf("%s planned to %q, want %q", src, got[src], dst)
		}
	}
}

// TestBookPartsAreNumbered: the parts of a staged book plan to numbered names in its
// folder, the way organize names them, whether a folder is imported at once or a host
// imports one file at a time.
func TestBookPartsAreNumbered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	for i, name := range []string{"one.mp3", "two.mp3"} {
		stage(t, filepath.Join(staging, name), testaudio.MP3Spec{Title: "Chapter", Artist: "Author", Album: "Second Book",
			Track: i + 1, TrackTotal: 2, Audio: testaudio.AudioWithSeed(byte(i + 1))})
	}
	svc := inbox.New(emptyStore{}, nil, nil, nil)
	req := importRequest(t, staging)
	req.ForceKind = model.KindBook
	plan, err := svc.Plan(ctx, req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := map[string]string{"one.mp3": "Second Book/Second Book - 01.mp3", "two.mp3": "Second Book/Second Book - 02.mp3"}
	wantPlanned(t, planned(t, plan), want)

	for _, name := range []string{"one.mp3", "two.mp3"} {
		plan, err := svc.PlanFile(ctx, importRequest(t, ""), filepath.Join(staging, name), model.KindBook)
		if err != nil {
			t.Fatalf("PlanFile %s: %v", name, err)
		}
		wantPlanned(t, planned(t, plan), map[string]string{name: want[name]})
	}
}

// TestBookDiscsAreNumbered: a book on several discs leads each part's number with its
// disc, whether the tags or a disc folder give it.
func TestBookDiscsAreNumbered(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	seed := byte(1)
	for disc := 1; disc <= 2; disc++ {
		for track := 1; track <= 3; track++ {
			stage(t, filepath.Join(staging, "Tagged", "d"+string(rune('0'+disc))+"t"+string(rune('0'+track))+".mp3"),
				testaudio.MP3Spec{Title: "Chapter", Artist: "Author", Album: "Disc Book", Disc: disc, DiscTotal: 2,
					Track: track, TrackTotal: 3, Audio: testaudio.AudioWithSeed(seed)})
			seed++
		}
		stage(t, filepath.Join(staging, "Foldered", "CD"+string(rune('0'+disc)), "01.mp3"),
			testaudio.MP3Spec{Title: "Chapter", Artist: "Author", Album: "Folder Book", Track: 1, Audio: testaudio.AudioWithSeed(seed)})
		seed++
	}
	req := importRequest(t, staging)
	req.ForceKind = model.KindBook
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := map[string]string{}
	for _, a := range plan.Actions {
		if a.Outcome != inbox.OutcomeImport {
			t.Fatalf("%s: %s (%s), want an import", a.Src, a.Outcome, a.Reason)
		}
		rel, _ := filepath.Rel(staging, a.Src)
		got[filepath.ToSlash(rel)] = filepath.Base(a.RelDst)
	}
	for src, dst := range map[string]string{
		"Tagged/d1t3.mp3": "Disc Book - 1-03.mp3", "Tagged/d2t1.mp3": "Disc Book - 2-01.mp3",
		"Foldered/CD1/01.mp3": "Folder Book - 1-01.mp3", "Foldered/CD2/01.mp3": "Folder Book - 2-01.mp3",
	} {
		if got[src] != dst {
			t.Errorf("%s planned to %q, want %q", src, got[src], dst)
		}
	}
}

// TestUnnumberedBookPartsTakeReadingOrder: parts whose tags give no place are numbered in
// the order the scan reads them, and a part that names its place in its title keeps it.
func TestUnnumberedBookPartsTakeReadingOrder(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	for i, name := range []string{"c.mp3", "a.mp3", "b.mp3"} {
		stage(t, filepath.Join(staging, "Third", name), testaudio.MP3Spec{Title: "Untitled Stretch", Artist: "Author",
			Album: "Third Book", Audio: testaudio.AudioWithSeed(byte(10 + i))})
	}
	for i, title := range []string{"Epilogue", "Chapter 2", "Prologue", "Chapter 1"} {
		stage(t, filepath.Join(staging, "Fourth", string(rune('a'+i))+".mp3"), testaudio.MP3Spec{Title: title,
			Artist: "Author", Album: "Fourth Book", Audio: testaudio.AudioWithSeed(byte(20 + i))})
	}
	req := importRequest(t, staging)
	req.ForceKind = model.KindBook
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := map[string]string{}
	for _, a := range plan.Actions {
		if a.Outcome != inbox.OutcomeImport {
			t.Fatalf("%s: %s (%s), want an import", a.Src, a.Outcome, a.Reason)
		}
		got[filepath.Base(filepath.Dir(a.Src))+"/"+filepath.Base(a.Src)] = filepath.Base(a.RelDst)
	}
	for src, dst := range map[string]string{
		"Third/a.mp3": "Third Book - 01.mp3", "Third/b.mp3": "Third Book - 02.mp3", "Third/c.mp3": "Third Book - 03.mp3",
		"Fourth/c.mp3": "Fourth Book - 01.mp3", "Fourth/d.mp3": "Fourth Book - 02.mp3",
		"Fourth/b.mp3": "Fourth Book - 03.mp3", "Fourth/a.mp3": "Fourth Book - 04.mp3",
	} {
		if got[src] != dst {
			t.Errorf("%s planned to %q, want %q", src, got[src], dst)
		}
	}
}

// TestLoneBookPartKeepsTheBareName: one file tagged track 1 with no total is what a tagger
// writes on a single-file book, so it keeps the book's own name; a lone part past the
// first is numbered.
func TestLoneBookPartKeepsTheBareName(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	stage(t, filepath.Join(staging, "lone.mp3"), testaudio.MP3Spec{Title: "Whole", Artist: "Author", Album: "Lone Book",
		Track: 1, Audio: testaudio.AudioWithSeed(1)})
	stage(t, filepath.Join(staging, "third.mp3"), testaudio.MP3Spec{Title: "Chapter", Artist: "Author", Album: "Part Book",
		Track: 3, Audio: testaudio.AudioWithSeed(2)})
	svc := inbox.New(emptyStore{}, nil, nil, nil)
	for name, want := range map[string]string{"lone.mp3": "Lone Book/Lone Book.mp3", "third.mp3": "Part Book/Part Book - 03.mp3"} {
		plan, err := svc.PlanFile(context.Background(), importRequest(t, ""), filepath.Join(staging, name), model.KindBook)
		if err != nil {
			t.Fatalf("PlanFile: %v", err)
		}
		if got := planned(t, plan)[name]; !strings.EqualFold(got, want) {
			t.Errorf("%s planned to %q, want %q", name, got, want)
		}
	}
}

// TestBookPartsLandUnderOneLayout: a book's parts render under the layout of the part with
// the strongest book signal, which lands first so the catalog makes it the book's primary
// file, the one whose tags organize renders the folder from.
func TestBookPartsLandUnderOneLayout(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	for i := 1; i <= 3; i++ {
		spec := testaudio.MP3Spec{Title: "Chapter", Artist: "Author", Album: "Tome", Track: i, TrackTotal: 3,
			Audio: testaudio.AudioWithSeed(byte(i))}
		if i == 2 {
			spec.TXXX = []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
		}
		stage(t, filepath.Join(staging, "Tome", "0"+string(rune('0'+i))+".mp3"), spec)
	}
	req := importRequest(t, staging)
	req.ForceKind = model.KindBook
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Actions) != 3 || filepath.Base(plan.Actions[0].Src) != "02.mp3" {
		t.Fatalf("actions = %+v, want the narrated part first", plan.Actions)
	}
	wantPlanned(t, planned(t, plan), map[string]string{
		"01.mp3": "Tome {Reader}/Tome - 01.mp3", "02.mp3": "Tome {Reader}/Tome - 02.mp3", "03.mp3": "Tome {Reader}/Tome - 03.mp3",
	})
}

// TestBookPartsPadToTheirTaggedTotal: parts numbered by their tags pad to the total the
// tags give, as a host importing them one at a time names them.
func TestBookPartsPadToTheirTaggedTotal(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	for i := 1; i <= 2; i++ {
		stage(t, filepath.Join(staging, "Long", "0"+string(rune('0'+i))+".mp3"), testaudio.MP3Spec{Title: "Chapter",
			Artist: "Author", Album: "Long Book", Track: i, TrackTotal: 120, Audio: testaudio.AudioWithSeed(byte(i))})
	}
	req := importRequest(t, staging)
	req.ForceKind = model.KindBook
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	wantPlanned(t, planned(t, plan), map[string]string{"01.mp3": "Long Book/Long Book - 001.mp3", "02.mp3": "Long Book/Long Book - 002.mp3"})
}

// TestStagedCopyOfAPartIsNoPartOfItsOwn: a second staged copy of a part's audio is a
// duplicate and takes no number, so the book's parts keep theirs.
func TestStagedCopyOfAPartIsNoPartOfItsOwn(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	one := testaudio.MP3Spec{Title: "Chapter", Artist: "Author", Album: "Tome", Track: 1, TrackTotal: 2, Audio: testaudio.AudioWithSeed(1)}
	stage(t, filepath.Join(staging, "Tome", "a.mp3"), one)
	stage(t, filepath.Join(staging, "Tome", "b.mp3"), one)
	stage(t, filepath.Join(staging, "Tome", "c.mp3"), testaudio.MP3Spec{Title: "Chapter", Artist: "Author", Album: "Tome",
		Track: 2, TrackTotal: 2, Audio: testaudio.AudioWithSeed(2)})
	req := importRequest(t, staging)
	req.ForceKind = model.KindBook
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := map[string]string{}
	for _, a := range plan.Actions {
		got[filepath.Base(a.Src)] = string(a.Outcome)
		if a.RelDst != "" {
			got[filepath.Base(a.Src)] += " " + filepath.Base(a.RelDst)
		}
	}
	for src, want := range map[string]string{"a.mp3": "import Tome - 01.mp3", "b.mp3": "duplicate", "c.mp3": "import Tome - 02.mp3"} {
		if got[src] != want {
			t.Errorf("%s = %q, want %q", src, got[src], want)
		}
	}
}

// TestImportedBookTitleDropsTheAbridgedMarker: a book renders under the title the scan
// gives it, without a trailing "(Unabridged)".
func TestImportedBookTitleDropsTheAbridgedMarker(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	stage(t, filepath.Join(staging, "tome.mp3"), testaudio.MP3Spec{Title: "Chapter", Artist: "Author",
		Album: "Tome (Unabridged)", Audio: testaudio.AudioWithSeed(1)})
	req := importRequest(t, staging)
	req.ForceKind = model.KindBook
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	wantPlanned(t, planned(t, plan), map[string]string{"tome.mp3": "Tome/Tome.mp3"})
}

// TestStagedCopiesOfAPartAreItsAlternates: under DupAllow a staged copy of a part's audio
// imports as that part's alternate beside it, numbered like it and told apart, and so
// does another encoding in the same container; a long title gives way to keep the names
// apart and within the limit.
func TestStagedCopiesOfAPartAreItsAlternates(t *testing.T) {
	t.Parallel()
	const rate = 22050
	samples := testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 31)
	staging := t.TempDir()
	long := strings.Repeat("x", 251)
	write := func(name string, data []byte, album string, track int) {
		path := filepath.Join(staging, "Tome", name)
		stage(t, path, testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(1)})
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		edits := []meta.TagEdit{{Key: "ALBUM", Values: []string{album}}, {Key: "ARTIST", Values: []string{"Author"}}}
		if track > 0 {
			edits = append(edits, meta.TagEdit{Key: "TRACKNUMBER", Values: []string{strconv.Itoa(track)}})
		}
		if _, err := meta.NewWriter().Apply(context.Background(), path, edits); err != nil {
			t.Fatalf("tag %s: %v", name, err)
		}
	}
	one := testaudio.EncodeAs(t, "mp3", "", rate, samples)
	write("01.mp3", one, "Tome", 1)
	write("01 copy.mp3", one, "Tome", 1)
	write("01 low.mp3", testaudio.EncodeAs(t, "mp3", "", rate/2, testaudio.RichSignal(rate/2, 2, testaudio.MusicalPartials, 31)), "Tome", 1)
	write("02.mp3", testaudio.EncodeAs(t, "mp3", "", rate, testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 32)), "Tome", 2)
	write("lone.wav", testaudio.EncodeAs(t, "wav", "", rate, samples), long, 0)
	write("lone.flac", testaudio.EncodeAs(t, "flac", "", rate, samples), long, 0)
	req := importRequest(t, staging)
	req.ForceKind, req.DupPolicy = model.KindBook, model.DupAllow
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	names := map[string]bool{}
	for _, a := range plan.Actions {
		if a.Outcome != inbox.OutcomeImport {
			t.Errorf("%s: %s (%s), want an import", filepath.Base(a.Src), a.Outcome, a.Reason)
		}
		base := filepath.Base(a.RelDst)
		if names[base] || len(base) > 255 {
			t.Errorf("%s planned to %q, want a name of its own within 255 bytes", filepath.Base(a.Src), base)
		}
		names[base] = true
	}
	for _, want := range []string{"Tome - 01.mp3", "Tome - 01 (2).mp3", "Tome - 01 (3).mp3", "Tome - 02.mp3"} {
		if !names[want] {
			t.Errorf("names = %v, want %s", names, want)
		}
	}
}

// TestBookHeldWhenAPartCannotLand: a book whose own part cannot land stays where it is
// whole, while a staged copy of a part that is a duplicate holds nothing.
func TestBookHeldWhenAPartCannotLand(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	for i := 1; i <= 3; i++ {
		stage(t, filepath.Join(staging, "Dune", "0"+string(rune('0'+i))+".mp3"), testaudio.MP3Spec{Artist: "Author",
			Album: "Dune", Track: i, TrackTotal: 3, Audio: testaudio.AudioWithSeed(byte(i))})
	}
	stage(t, filepath.Join(staging, "Dune", "01 copy.mp3"), testaudio.MP3Spec{Artist: "Author", Album: "Dune", Track: 1,
		TrackTotal: 3, Audio: testaudio.AudioWithSeed(1)})
	req := importRequest(t, staging)
	req.ForceKind = model.KindBook
	svc := inbox.New(emptyStore{}, nil, nil, nil)
	outcomes := func() map[string]inbox.Outcome {
		t.Helper()
		plan, err := svc.Plan(context.Background(), req)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		got := map[string]inbox.Outcome{}
		for _, a := range plan.Actions {
			got[filepath.Base(a.Src)] = a.Outcome
		}
		return got
	}
	if got := outcomes(); got["02.mp3"] != inbox.OutcomeImport || got["03.mp3"] != inbox.OutcomeImport ||
		got["01.mp3"] == got["01 copy.mp3"] || got["01.mp3"] != inbox.OutcomeImport && got["01.mp3"] != inbox.OutcomeDuplicate {
		t.Errorf("outcomes = %v, want the parts imported and one copy of part 1 a duplicate", got)
	}
	stage(t, filepath.Join(string(req.Library.Root), "Author", "Dune", "Dune - 02.mp3"), testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(9)})
	for name, outcome := range outcomes() {
		if outcome == inbox.OutcomeImport {
			t.Errorf("%s would import, want the book held while its part 2 cannot land", name)
		}
	}
}

// TestHoldBooks: only a book's own part that will not land holds the book's other files;
// a joined file or a copy of a part that will not land holds nothing, nor does another
// book's part.
func TestHoldBooks(t *testing.T) {
	t.Parallel()
	act := func(src, book string, outcome inbox.Outcome, joined, alternate bool) inbox.Action {
		return inbox.Action{Src: src, Book: book, Outcome: outcome, Joined: joined, Alternate: alternate, RelDst: src}
	}
	actions := []inbox.Action{
		act("a1", "A", inbox.OutcomeImport, false, false),
		act("a2", "A", inbox.OutcomeQuarantine, false, false),
		act("a3", "A", inbox.OutcomeImport, true, false),
		act("b1", "B", inbox.OutcomeImport, false, false),
		act("b2", "B", inbox.OutcomeQuarantine, true, false),
		act("b3", "B", inbox.OutcomeDuplicate, false, true),
		act("t", "", inbox.OutcomeImport, false, false),
	}
	inbox.HoldBooks(actions)
	for _, a := range actions {
		want := map[string]bool{"a1": true, "a3": true}[a.Src]
		if held := strings.HasPrefix(a.Reason, "held with its book"); held != want || held && (a.Outcome != inbox.OutcomeQuarantine || a.RelDst != "") {
			t.Errorf("%s: %s (%s) to %q, held %v, want held %v", a.Src, a.Outcome, a.Reason, a.RelDst, held, want)
		}
	}
}
