package inbox_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// emptyStore is a catalog holding nothing, so every staged file is new.
type emptyStore struct{}

func (emptyStore) FilesByEssence(context.Context, string) ([]model.FileOccupant, error) {
	return nil, nil
}
func (emptyStore) FileOccupants(context.Context, string) ([]model.FileOccupant, error) {
	return nil, nil
}

func (emptyStore) ArtistNames(context.Context, []string) (map[string]string, error) { return nil, nil }

func (emptyStore) AlbumTitles(context.Context, []string) (map[string]string, error) { return nil, nil }
func (emptyStore) CreateImportBatch(context.Context, *model.ImportBatch) error      { return nil }
func (emptyStore) UpdateImportBatch(context.Context, *model.ImportBatch) error      { return nil }
func (emptyStore) PutAcquisitionForFile(context.Context, []byte, model.AcquisitionInput, []model.PID) (model.PID, error) {
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

// TestHoldUnits: only a book's own part that will not land holds the book's other files;
// a joined file or a copy of a part that will not land holds nothing, nor does another
// book's part. An album held as a unit is held by a quarantined track, never by a
// duplicate, whose audio the catalog holds already.
func TestHoldUnits(t *testing.T) {
	t.Parallel()
	act := func(src, unit string, kind model.Kind, outcome inbox.Outcome, joined, alternate bool) inbox.Action {
		return inbox.Action{Src: src, Unit: unit, Kind: kind, Outcome: outcome, Joined: joined, Alternate: alternate, RelDst: src}
	}
	book, track := model.KindBook, model.KindTrack
	actions := []inbox.Action{
		act("a1", "A", book, inbox.OutcomeImport, false, false),
		act("a2", "A", book, inbox.OutcomeQuarantine, false, false),
		act("a3", "A", book, inbox.OutcomeImport, true, false),
		act("b1", "B", book, inbox.OutcomeImport, false, false),
		act("b2", "B", book, inbox.OutcomeQuarantine, true, false),
		act("b3", "B", book, inbox.OutcomeDuplicate, false, true),
		act("t", "", track, inbox.OutcomeImport, false, false),
		act("c1", "C", track, inbox.OutcomeImport, false, false),
		act("c2", "C", track, inbox.OutcomeQuarantine, false, false),
		act("d1", "D", track, inbox.OutcomeImport, false, false),
		act("d2", "D", track, inbox.OutcomeDuplicate, false, false),
	}
	inbox.HoldUnits(actions)
	wild := []inbox.Action{act("w", "*", "", inbox.OutcomeQuarantine, false, false), act("x1", "X", track, inbox.OutcomeImport, false, false),
		act("y1", "Y", book, inbox.OutcomeImport, false, false), act("z", "", track, inbox.OutcomeImport, false, false)}
	inbox.HoldUnits(wild)
	if wild[1].Code != inbox.HoldAlbum || wild[2].Code != inbox.HoldBook || wild[3].Outcome != inbox.OutcomeImport || wild[1].Occupant.StagedSrc != "w" {
		t.Errorf("a file whose unit cannot be told held %+v, want every file with a unit held by it", wild)
	}
	want := map[string]struct {
		code inbox.HoldCode
		by   string
	}{"a1": {inbox.HoldBook, "a2"}, "a3": {inbox.HoldBook, "a2"}, "c1": {inbox.HoldAlbum, "c2"}}
	for _, a := range actions {
		w, ok := want[a.Src]
		held := a.Code == inbox.HoldBook || a.Code == inbox.HoldAlbum
		if held != ok || held && (a.Code != w.code || a.Outcome != inbox.OutcomeQuarantine || a.RelDst != "" ||
			!strings.HasPrefix(a.Reason, "held with its") || a.Occupant == nil || a.Occupant.StagedSrc != w.by) {
			t.Errorf("%s: %s [%s] (%s) to %q by %+v, want held %v %+v", a.Src, a.Outcome, a.Code, a.Reason, a.RelDst, a.Occupant, ok, w)
		}
	}
}

// failingCataloger places nothing in the catalog: every scan fails.
type failingCataloger struct{}

func (failingCataloger) ScanFileWith(context.Context, *model.Library, string, scan.FileOptions) (*scan.Result, *model.ScanItemResult, error) {
	return nil, nil, waxerr.New(waxerr.CodeIO, "test", "catalog unavailable")
}

// TestAFailureAfterTheMoveNamesWhereTheFileLanded: an import whose file was placed but
// could not be cataloged says where the file now is, beside the error.
func TestAFailureAfterTheMoveNamesWhereTheFileLanded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	stage(t, filepath.Join(staging, "song.mp3"), testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Record",
		Track: 1, Audio: testaudio.AudioWithSeed(1)})
	svc := inbox.New(emptyStore{}, nil, failingCataloger{}, nil)
	plan, err := svc.Plan(ctx, importRequest(t, staging))
	if err != nil || plan.Importable() != 1 {
		t.Fatalf("Plan = %+v (err %v), want one import", plan, err)
	}
	rep, err := svc.Execute(ctx, plan)
	if err != nil || rep.Errored != 1 || len(rep.Failures) != 1 {
		t.Fatalf("Execute = %+v (err %v), want one failure", rep, err)
	}
	if f := rep.Failures[0]; f.Dst != plan.Actions[0].Dst || f.Dst == "" {
		t.Errorf("failure = %+v, want it to name %s", f, plan.Actions[0].Dst)
	}
	if _, err := os.Stat(plan.Actions[0].Dst); err != nil {
		t.Errorf("placed file: %v", err)
	}
}

// occupiedStore is a catalog holding the files occupants lists by display path.
type occupiedStore struct {
	emptyStore
	occupants map[string][]model.FileOccupant
}

func (s occupiedStore) FileOccupants(_ context.Context, displayPath string) ([]model.FileOccupant, error) {
	var out []model.FileOccupant
	for path, occ := range s.occupants {
		if strings.EqualFold(path, displayPath) {
			out = append(out, occ...)
		}
	}
	return out, nil
}

// TestQuarantinesKeepTheirDestinationAndNameTheOccupant: a refused destination keeps the
// place the file would have taken and names what holds it: a staged file claiming it
// first, a file on disk there (with its catalog identity), the catalog's row for another
// file not on disk, and a file beside it named apart only by case, which a filesystem
// folding case takes for the same name, seen here by listing its folder.
func TestQuarantinesKeepTheirDestinationAndNameTheOccupant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	song := func(name, title string, track int, seed byte) {
		stage(t, filepath.Join(staging, name), testaudio.MP3Spec{Title: title, Artist: "Band", Album: "Rec", Track: track,
			Audio: testaudio.AudioWithSeed(seed)})
	}
	song("a.mp3", "One", 1, 1)
	song("b.mp3", "One", 1, 2)
	song("c.mp3", "Two", 2, 3)
	song("d.mp3", "Three", 3, 4)
	song("e.mp3", "Four", 4, 5)
	req := importRequest(t, staging)
	dir := filepath.Join(string(req.Library.Root), "Band", "Rec")
	onDisk := filepath.Join(dir, "02 - Two.mp3")
	stage(t, onDisk, testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(9)})
	neighbour := filepath.Join(dir, "04 - FOUR.mp3")
	stage(t, neighbour, testaudio.MP3Spec{Audio: testaudio.AudioWithSeed(10)})
	store := occupiedStore{occupants: map[string][]model.FileOccupant{
		onDisk:                               {{Path: []byte(onDisk), DisplayPath: onDisk, FilePID: "FILEC", ItemPID: "ITEMC", OnDisk: true, Essence: "x"}},
		filepath.Join(dir, "03 - Three.mp3"): {{DisplayPath: filepath.Join(dir, "03 - Three.mp3"), FilePID: "FILED", ItemPID: "ITEMD", Essence: "y"}},
	}}
	plan, err := inbox.New(store, nil, nil, nil).Plan(ctx, req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := map[string]inbox.Action{}
	for _, a := range plan.Actions {
		got[filepath.Base(a.Src)] = a
	}
	want := map[string]struct {
		code inbox.HoldCode
		dst  string
		occ  inbox.Occupant
	}{
		"b.mp3": {inbox.HoldClaimed, filepath.Join(dir, "01 - One.mp3"), inbox.Occupant{Path: filepath.Join(dir, "01 - One.mp3"), StagedSrc: filepath.Join(staging, "a.mp3")}},
		"c.mp3": {inbox.HoldExists, onDisk, inbox.Occupant{Path: onDisk, FilePID: "FILEC", ItemPID: "ITEMC"}},
		"d.mp3": {inbox.HoldCataloged, filepath.Join(dir, "03 - Three.mp3"), inbox.Occupant{Path: filepath.Join(dir, "03 - Three.mp3"), FilePID: "FILED", ItemPID: "ITEMD"}},
		"e.mp3": {inbox.HoldOccupiedCase, filepath.Join(dir, "04 - Four.mp3"), inbox.Occupant{Path: neighbour}},
	}
	if a := got["a.mp3"]; a.Outcome != inbox.OutcomeImport || a.Code != "" || a.Occupant != nil {
		t.Errorf("a.mp3 = %+v, want it imported", a)
	}
	for name, w := range want {
		a := got[name]
		if a.Outcome != inbox.OutcomeQuarantine || a.Code != w.code || a.Dst != w.dst || a.RelDst == "" || a.Occupant == nil || *a.Occupant != w.occ {
			t.Errorf("%s = %+v (occupant %+v), want %s at %s held by %+v", name, a, a.Occupant, w.code, w.dst, w.occ)
		}
	}
}

// foldingCataloger catalogs a file as an item that absorbed one item when it landed and
// another when a promoted alternate was read again after it.
type foldingCataloger struct{}

func (foldingCataloger) ScanFileWith(context.Context, *model.Library, string, scan.FileOptions) (*scan.Result, *model.ScanItemResult, error) {
	out := &model.ScanItemResult{ItemPID: "NEW", ItemCreated: true, Absorbed: map[model.PID]model.PID{"OLD": "NEW"}}
	res := &scan.Result{ItemsCreated: 1, Absorbed: map[model.PID]model.PID{"OLD": "NEW", "PROMOTED": "OTHER"}}
	return res, out, nil
}

// stampingStore records, for each acquisition stamp, the items it was told the put made.
type stampingStore struct {
	emptyStore
	made [][]model.PID
}

func (s *stampingStore) PutAcquisitionForFile(_ context.Context, _ []byte, _ model.AcquisitionInput, made []model.PID) (model.PID, error) {
	s.made = append(s.made, made)
	return "", nil
}

// mintingCataloger catalogs a file as an item it made from nothing.
type mintingCataloger struct{}

func (mintingCataloger) ScanFileWith(context.Context, *model.Library, string, scan.FileOptions) (*scan.Result, *model.ScanItemResult, error) {
	return &scan.Result{ItemsCreated: 1}, &model.ScanItemResult{ItemPID: "NEW", ItemCreated: true, Made: []model.PID{"NEW"}}, nil
}

// TestAnImportFillsTheRecordOfAnItemThatTookOneIn: the origin an import records is
// recorded whole only on the items its put made from nothing, so one the put made by
// taking a standing item in, which brought its own origin along, has its record filled.
func TestAnImportFillsTheRecordOfAnItemThatTookOneIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		cataloger inbox.Cataloger
		made      []model.PID
	}{{foldingCataloger{}, nil}, {mintingCataloger{}, []model.PID{"NEW"}}} {
		staging := t.TempDir()
		stage(t, filepath.Join(staging, "song.mp3"), testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Record",
			Track: 1, Audio: testaudio.AudioWithSeed(1)})
		st := &stampingStore{}
		svc := inbox.New(st, nil, tc.cataloger, nil)
		req := importRequest(t, staging)
		req.Acquisition = &model.AcquisitionInput{SourceType: model.SourceYouTube, SourceURL: "https://example.test/song"}
		plan, err := svc.Plan(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Execute(ctx, plan); err != nil {
			t.Fatal(err)
		}
		if len(st.made) != 1 || !slices.Equal(st.made[0], tc.made) {
			t.Errorf("%T: made = %v, want [%v]", tc.cataloger, st.made, tc.made)
		}
	}
}

// TestAnImportReportsEveryFoldItsScanMade: an imported file's outcome names every item its
// cataloging scan folded away, the ones a promoted alternate's re-read folded included.
func TestAnImportReportsEveryFoldItsScanMade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	stage(t, filepath.Join(staging, "song.mp3"), testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Record",
		Track: 1, Audio: testaudio.AudioWithSeed(1)})
	svc := inbox.New(emptyStore{}, nil, foldingCataloger{}, nil)
	plan, err := svc.Plan(ctx, importRequest(t, staging))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Execute(ctx, plan)
	if err != nil || len(rep.Files) != 1 {
		t.Fatalf("Execute = %+v (err %v)", rep, err)
	}
	if got := rep.Files[0].Absorbed; len(got) != 2 || got["PROMOTED"] != "OTHER" {
		t.Errorf("absorbed = %v, want the re-read's fold too", got)
	}
}

// countingReader reads tags as the real reader does and counts the reads of each path.
type countingReader struct {
	meta.Reader
	mu    sync.Mutex
	reads map[string]int
}

func (r *countingReader) Read(ctx context.Context, path string) (*meta.FileMeta, error) {
	r.mu.Lock()
	r.reads[path]++
	r.mu.Unlock()
	return r.Reader.Read(ctx, path)
}

// TestPlanningAcquiredFilesReadsEachOnce: planning acquired files held as whole units reads
// each file's tags once, however many of the planner's steps look at them.
func TestPlanningAcquiredFilesReadsEachOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	var paths []string
	for i, title := range []string{"One", "Two", "Three"} {
		p := filepath.Join(dir, strconv.Itoa(i+1)+".mp3")
		stage(t, p, testaudio.MP3Spec{Title: title, Artist: "Band", Album: "Record", Track: i + 1, Audio: testaudio.AudioWithSeed(byte(i + 1))})
		paths = append(paths, p)
	}
	r := &countingReader{Reader: meta.NewReader(), reads: map[string]int{}}
	req := importRequest(t, dir)
	req.Acquired, req.HoldUnits = true, true
	plan, err := inbox.New(emptyStore{}, r, nil, nil).PlanPaths(ctx, req, paths, model.KindTrack)
	if err != nil || len(plan.Actions) != len(paths) {
		t.Fatalf("PlanPaths = %+v (err %v)", plan, err)
	}
	for _, p := range paths {
		if r.reads[p] != 1 {
			t.Errorf("%s read %d times, want once", p, r.reads[p])
		}
	}
}

// audioStore is a catalog holding the files byEssence lists for every audio, and those
// byPath lists at a display path.
type audioStore struct {
	emptyStore
	byEssence []model.FileOccupant
	byPath    map[string][]model.FileOccupant
}

func (s audioStore) FilesByEssence(context.Context, string) ([]model.FileOccupant, error) {
	return s.byEssence, nil
}

func (s audioStore) FileOccupants(_ context.Context, path string) ([]model.FileOccupant, error) {
	return s.byPath[path], nil
}

// goneFile is a cataloged file not on disk, under a reachable root, backing item.
func goneFile(item model.PID, kind model.Kind, library int64) model.FileOccupant {
	return model.FileOccupant{Path: []byte("/gone/" + item), DisplayPath: "/gone/" + string(item), FilePID: "F" + item,
		ItemPID: item, Kind: kind, LibraryID: library, Reachable: true}
}

// TestAGoneFileRestoresOnlyItsOwnKindInTheTargetLibrary: a staged file's audio held only by
// gone files restores the item of its kind in the library it lands in, whatever gone files of
// another kind or library hold it too; with no such item, those hold the audio, so it is a
// duplicate under DupSkip.
func TestAGoneFileRestoresOnlyItsOwnKindInTheTargetLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		occ      []model.FileOccupant
		outcome  inbox.Outcome
		restores model.PID
	}{
		{"its own kind beside another", []model.FileOccupant{goneFile("B", model.KindBook, 1), goneFile("X", model.KindTrack, 1)}, inbox.OutcomeImport, "X"},
		{"another library", []model.FileOccupant{goneFile("X", model.KindTrack, 2)}, inbox.OutcomeDuplicate, ""},
		{"another kind alone", []model.FileOccupant{goneFile("B", model.KindBook, 1)}, inbox.OutcomeDuplicate, ""},
	} {
		staging := t.TempDir()
		stage(t, filepath.Join(staging, "song.mp3"), testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Record",
			Track: 1, Audio: testaudio.AudioWithSeed(1)})
		plan, err := inbox.New(audioStore{byEssence: tc.occ}, nil, nil, nil).Plan(ctx, importRequest(t, staging))
		if err != nil {
			t.Fatal(err)
		}
		if a := plan.Actions[0]; a.Outcome != tc.outcome || a.Restores != tc.restores {
			t.Errorf("%s: %s restoring %q, want %s restoring %q", tc.name, a.Outcome, a.Restores, tc.outcome, tc.restores)
		}
	}
}

// TestARefusedActionRestoresNothing: an action that will not land, its destination taken,
// names no item it would restore.
func TestARefusedActionRestoresNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	stage(t, filepath.Join(staging, "song.mp3"), testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Record",
		Track: 1, Audio: testaudio.AudioWithSeed(1)})
	req := importRequest(t, staging)
	svc := inbox.New(audioStore{byEssence: []model.FileOccupant{goneFile("X", model.KindTrack, 1)}}, nil, nil, nil)
	first, err := svc.Plan(ctx, req)
	if err != nil || first.Actions[0].Restores != "X" {
		t.Fatalf("plan = %+v (err %v), want a restore of X", first, err)
	}
	stage(t, first.Actions[0].Dst, testaudio.MP3Spec{Title: "Other", Audio: testaudio.AudioWithSeed(2)})
	plan, err := svc.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if a := plan.Actions[0]; a.Code != inbox.HoldExists || a.Restores != "" {
		t.Errorf("action = %s [%s] restoring %q, want held as exists restoring nothing", a.Outcome, a.Code, a.Restores)
	}
}

// landingCataloger catalogs every file onto the item it names, "" standing for a rip.
type landingCataloger struct{ item model.PID }

func (c landingCataloger) ScanFileWith(context.Context, *model.Library, string, scan.FileOptions) (*scan.Result, *model.ScanItemResult, error) {
	return &scan.Result{}, &model.ScanItemResult{ItemPID: c.item, ItemCreated: c.item == "N"}, nil
}

// TestARestoreIsReportedOnlyWhereTheFileLanded: an import planned to restore an item says it
// did only when the file landed on that item, a rip's file (whose put names no one item) by
// the item its file now backs.
func TestARestoreIsReportedOnlyWhereTheFileLanded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		landed, backs, want model.PID
	}{{"X", "", "X"}, {"N", "", ""}, {"", "X", "X"}, {"", "Y", ""}} {
		staging := t.TempDir()
		stage(t, filepath.Join(staging, "song.mp3"), testaudio.MP3Spec{Title: "Song", Artist: "Band", Album: "Record",
			Track: 1, Audio: testaudio.AudioWithSeed(1)})
		st := audioStore{byEssence: []model.FileOccupant{goneFile("X", model.KindTrack, 1)}, byPath: map[string][]model.FileOccupant{}}
		svc := inbox.New(st, nil, landingCataloger{tc.landed}, nil)
		plan, err := svc.Plan(ctx, importRequest(t, staging))
		if err != nil || plan.Actions[0].Restores != "X" {
			t.Fatalf("plan = %+v (err %v), want a restore of X", plan, err)
		}
		if tc.backs != "" {
			st.byPath[plan.Actions[0].Dst] = []model.FileOccupant{{DisplayPath: plan.Actions[0].Dst, ItemPID: tc.backs, OnDisk: true, Reachable: true}}
		}
		rep, err := svc.Execute(ctx, plan)
		if err != nil || len(rep.Files) != 1 {
			t.Fatalf("Execute = %+v (err %v)", rep, err)
		}
		if got := rep.Files[0].Restores; got != tc.want {
			t.Errorf("landed on %q backing %q: restores %q, want %q", tc.landed, tc.backs, got, tc.want)
		}
	}
}

// TestARefusedCopyOfAPartHoldsNoBook: in a plan that holds units, a copy of a book's part
// refused before the book is numbered holds nothing, as a copy of a part that will not land
// never does, while a refused part of its own holds the book.
func TestARefusedCopyOfAPartHoldsNoBook(t *testing.T) {
	t.Parallel()
	for refused, want := range map[string]int{"01 copy.mp3": 0, "02.mp3": 1} {
		staging := t.TempDir()
		for i := 1; i <= 2; i++ {
			stage(t, filepath.Join(staging, "Dune", "0"+strconv.Itoa(i)+".mp3"), testaudio.MP3Spec{Artist: "Author", Album: "Dune",
				Track: i, TrackTotal: 2, Audio: testaudio.AudioWithSeed(byte(i))})
		}
		stage(t, filepath.Join(staging, "Dune", "01 copy.mp3"), testaudio.MP3Spec{Artist: "Author", Album: "Dune", Track: 1,
			TrackTotal: 2, Audio: testaudio.AudioWithSeed(1)})
		req := importRequest(t, staging)
		req.ForceKind, req.HoldUnits = model.KindBook, true
		req.Hold = func(path string) string {
			if filepath.Base(path) == refused {
				return "read-only"
			}
			return ""
		}
		plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		held := 0
		for _, a := range plan.Actions {
			if a.Code == inbox.HoldBook {
				held++
			}
		}
		if held != want {
			t.Errorf("%s refused: %d held with the book, want %d", refused, held, want)
		}
	}
}

// TestAnAlbumHoldsAsOneUnitHoweverItsTracksAreTagged: the tracks of one album held as a
// unit share it whether or not each carries the release's MusicBrainz id, so a refused
// track without one holds the tracks that have it.
func TestAnAlbumHoldsAsOneUnitHoweverItsTracksAreTagged(t *testing.T) {
	t.Parallel()
	staging := t.TempDir()
	for i := 1; i <= 3; i++ {
		spec := testaudio.MP3Spec{Title: "Song " + strconv.Itoa(i), Artist: "Band", AlbumArtist: "Band", Album: "Record",
			Track: i, Audio: testaudio.AudioWithSeed(byte(i))}
		if i < 3 {
			spec.TXXX = []testaudio.TXXXFrame{{Desc: "MusicBrainz Album Id", Value: "14141414-1414-1414-1414-141414141414"}}
		}
		stage(t, filepath.Join(staging, "Record", "0"+strconv.Itoa(i)+".mp3"), spec)
	}
	req := importRequest(t, staging)
	req.HoldUnits = true
	req.Hold = func(path string) string {
		if filepath.Base(path) == "03.mp3" {
			return "read-only"
		}
		return ""
	}
	plan, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if filepath.Base(a.Src) != "03.mp3" && a.Code != inbox.HoldAlbum {
			t.Errorf("%s: %s [%s], want it held with its album", filepath.Base(a.Src), a.Outcome, a.Code)
		}
	}
}

// bookStore is a catalog holding one book, whatever its key, with one part.
type bookStore struct {
	emptyStore
	part model.File
}

func (s bookStore) BookByKey(context.Context, int64, string) (*model.ItemView, []model.ItemFileRef, error) {
	return &model.ItemView{PID: "B", Kind: model.KindBook, Title: "Dune", Artist: "Author", Album: "Dune"},
		[]model.ItemFileRef{{FilePID: s.part.PID, Path: s.part.Path, DisplayPath: s.part.DisplayPath, Role: "primary", Position: 1}}, nil
}

func (s bookStore) FileByPID(context.Context, model.PID) (*model.File, error) {
	part := s.part
	return &part, nil
}

// TestARefusedCopyOfACatalogedPartHoldsNoBook: a refused copy of a part the catalog holds
// holds nothing, so the book's new part staged with it still lands.
func TestARefusedCopyOfACatalogedPartHoldsNoBook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	stage(t, filepath.Join(staging, "Dune", "01 copy.mp3"), testaudio.MP3Spec{Artist: "Author", Album: "Dune", Track: 1,
		TrackTotal: 2, Audio: testaudio.AudioWithSeed(1)})
	stage(t, filepath.Join(staging, "Dune", "02.mp3"), testaudio.MP3Spec{Artist: "Author", Album: "Dune", Track: 2,
		TrackTotal: 2, Audio: testaudio.AudioWithSeed(2)})
	req := importRequest(t, staging)
	req.ForceKind, req.HoldUnits = model.KindBook, true
	req.Hold = func(path string) string {
		if filepath.Base(path) == "01 copy.mp3" {
			return "read-only"
		}
		return ""
	}
	first, err := inbox.New(emptyStore{}, nil, nil, nil).Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var essence string
	for _, a := range first.Actions {
		if filepath.Base(a.Src) == "01 copy.mp3" {
			essence = a.Essence
		}
	}
	part := filepath.Join(string(req.Library.Root), "Author", "Dune", "Dune - 01.mp3")
	stage(t, part, testaudio.MP3Spec{Artist: "Author", Album: "Dune", Track: 1, TrackTotal: 2, Audio: testaudio.AudioWithSeed(1)})
	st := bookStore{part: model.File{PID: "P1", Path: []byte(part), DisplayPath: part, EssenceHash: essence}}
	plan, err := inbox.New(st, nil, nil, nil).Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if filepath.Base(a.Src) == "02.mp3" && a.Outcome != inbox.OutcomeImport {
			t.Errorf("part 2: %s [%s] (%s), want it imported", a.Outcome, a.Code, a.Reason)
		}
	}
}

// TestARefusalKeepsADestinationOnlyWhereItHolds: a refused action restores nothing and
// keeps the destination it rendered only when held there or by its library or source; a
// duplicate, a file held with its unit, and one with no destination keep none.
func TestARefusalKeepsADestinationOnlyWhereItHolds(t *testing.T) {
	t.Parallel()
	for code, keeps := range map[inbox.HoldCode]bool{
		inbox.HoldClaimed: true, inbox.HoldExists: true, inbox.HoldOccupiedCase: true, inbox.HoldCataloged: true,
		inbox.HoldCollisionCheck: true, inbox.HoldReadOnlySource: true, inbox.HoldReadOnlyLibrary: true,
		inbox.HoldDuplicateCatalog: false, inbox.HoldDuplicateBatch: false, inbox.HoldLandedSince: false,
		inbox.HoldBook: false, inbox.HoldAlbum: false, inbox.HoldNoDestination: false, inbox.HoldNoLibrary: false,
	} {
		a := inbox.Action{Outcome: inbox.OutcomeImport, Dst: "/lib/a.mp3", RelDst: "a.mp3", Restores: "X"}
		a.Refuse(inbox.OutcomeQuarantine, code, "held", nil)
		if a.Restores != "" || (a.Dst != "") != keeps || (a.RelDst != "") != keeps {
			t.Errorf("%s: dst %q rel %q restores %q, want the destination kept %v and nothing restored", code, a.Dst, a.RelDst, a.Restores, keeps)
		}
	}
}
