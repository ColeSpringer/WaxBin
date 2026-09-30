package waxbin_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// profileNames lists a library's profile names.
func profileNames(lib *waxbin.Library) []string {
	var out []string
	for _, p := range lib.Profiles() {
		out = append(out, p.Name)
	}
	return out
}

// openWithProfile opens a managed library over one scanned track, its root laid out
// by a custom profile named mine with the given music template.
func openWithProfile(t *testing.T, music string) (*waxbin.Library, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "in", "song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:   filepath.Join(t.TempDir(), "catalog.db"),
		Roots:    []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "mine"}},
		Profiles: []config.ProfileDef{{Name: "mine", Music: music}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	return lib, root
}

// planDst plans an organize of everything and returns the one action's relative path.
func planDst(t *testing.T, lib *waxbin.Library, opts waxbin.OrganizeOptions) (*organize.Plan, string) {
	t.Helper()
	plan, err := lib.PlanOrganize(context.Background(), query.New(query.EntityItems).Build(), opts)
	if err != nil {
		t.Fatalf("plan organize: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("plan = %+v, want one action", plan.Actions)
	}
	return plan, filepath.ToSlash(plan.Actions[0].RelDst)
}

// TestSetProfilesReplacesTheSet: a new set takes effect on the next plan, a set with a
// bad template is refused whole, and one that drops a profile a root uses is refused
// naming the library, both leaving the old set in place.
func TestSetProfilesReplacesTheSet(t *testing.T) {
	ctx := context.Background()
	lib, _ := openWithProfile(t, "A/{title}.{ext}")
	if _, dst := planDst(t, lib, waxbin.OrganizeOptions{}); dst != "A/Song.mp3" {
		t.Fatalf("destination = %q, want A/Song.mp3", dst)
	}

	if err := lib.SetProfiles(ctx, []config.ProfileDef{{Name: "mine", Music: "{nope}"}}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("a bad template = %v, want CodeInvalid", err)
	}
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = lib.SetProfiles(ctx, nil)
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), string(libs[0].PID)) || !strings.Contains(err.Error(), "mine") {
		t.Fatalf("dropping a used profile = %v, want CodeInvalid naming the library and the profile", err)
	}
	if _, dst := planDst(t, lib, waxbin.OrganizeOptions{}); dst != "A/Song.mp3" {
		t.Fatalf("destination after two refusals = %q, want the old set's A/Song.mp3", dst)
	}

	if err := lib.SetProfiles(ctx, []config.ProfileDef{{Name: "mine", Music: "B/{title}.{ext}"}, {Name: "other"}}); err != nil {
		t.Fatalf("set profiles: %v", err)
	}
	if _, dst := planDst(t, lib, waxbin.OrganizeOptions{}); dst != "B/Song.mp3" {
		t.Fatalf("destination = %q, want the new set's B/Song.mp3", dst)
	}
	if got := strings.Join(profileNames(lib), ","); got != "mine,other,waxbin-native" {
		t.Fatalf("profiles = %s, want mine,other,waxbin-native", got)
	}
}

// TestPlanOrganizeTakesAnAdHocProfile: a profile value lays out every library without
// joining the set, and is validated first.
func TestPlanOrganizeTakesAnAdHocProfile(t *testing.T) {
	lib, _ := openWithProfile(t, "A/{title}.{ext}")
	adhoc := organize.Profile{Name: "adhoc", Music: "Z/{title}.{ext}", Audiobook: "{title}.{ext}", Podcast: "{episode}.{ext}"}
	plan, dst := planDst(t, lib, waxbin.OrganizeOptions{Profile: &adhoc})
	if dst != "Z/Song.mp3" || plan.Profile != "adhoc" {
		t.Fatalf("ad-hoc plan = %q under %q, want Z/Song.mp3 under adhoc", dst, plan.Profile)
	}
	for _, name := range profileNames(lib) {
		if name == "adhoc" {
			t.Fatal("an ad-hoc profile joined the set")
		}
	}
	q := query.New(query.EntityItems).Build()
	if _, err := lib.PlanOrganize(context.Background(), q, waxbin.OrganizeOptions{ProfileName: "mine", Profile: &adhoc}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("a name and a value = %v, want CodeInvalid", err)
	}
	bad := adhoc
	bad.Podcast = ""
	if _, err := lib.PlanOrganize(context.Background(), q, waxbin.OrganizeOptions{Profile: &bad}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("an incomplete ad-hoc profile = %v, want CodeInvalid", err)
	}
}

// TestRunOrganizeTakesItsProfileAtTheCall: the job lays out under the profile as it was
// when RunOrganize was called, so a caller reusing its struct afterwards changes
// nothing, and a profile that does not validate is refused before any job starts.
func TestRunOrganizeTakesItsProfileAtTheCall(t *testing.T) {
	ctx := context.Background()
	lib, root := openWithProfile(t, "A/{title}.{ext}")
	q := query.New(query.EntityItems).Build()
	bad := organize.Profile{Name: "bad", Music: "{title}.{ext}", Audiobook: "{title}.{ext}"}
	if pid, err := lib.RunOrganize(ctx, q, waxbin.OrganizeOptions{Profile: &bad}); !waxerr.Is(err, waxerr.CodeInvalid) || pid != "" {
		t.Fatalf("run with an incomplete profile = %q (err %v), want CodeInvalid and no job", pid, err)
	}
	prof := organize.Profile{Name: "adhoc", Music: "Z/{title}.{ext}", Audiobook: "{title}.{ext}", Podcast: "{episode}.{ext}"}
	pid, err := lib.RunOrganize(ctx, q, waxbin.OrganizeOptions{Profile: &prof})
	if err != nil {
		t.Fatalf("run organize: %v", err)
	}
	prof.Music = "Y/{title}.{ext}"
	if job := waitForJobDone(t, ctx, lib, pid); job.State != model.JobDone {
		t.Fatalf("organize job = %+v, want done", job)
	}
	if !fileExists(filepath.Join(root, "Z", "Song.mp3")) {
		t.Fatal("the job did not lay the file out under the profile it was called with")
	}
}

// TestRootsMustNameAKnownProfile: a root naming a profile the set lacks is refused
// when it is added at runtime and when it is configured at Open.
func TestRootsMustNameAKnownProfile(t *testing.T) {
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	added := t.TempDir()
	_, err := lib.AddRoot(ctx, config.Root{Path: added, Mode: model.ModeManaged, Profile: "nope"})
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "no such organization profile") ||
		!strings.Contains(err.Error(), added) {
		t.Fatalf("add a root under an unknown profile = %v, want CodeInvalid naming the root", err)
	}
	configured := t.TempDir()
	_, err = waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "other.db"),
		Roots:  []config.Root{{Path: configured, Mode: model.ModeManaged, Profile: "nope"}},
	})
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "no such organization profile") ||
		!strings.Contains(err.Error(), configured) {
		t.Fatalf("open with a root under an unknown profile = %v, want CodeInvalid naming the root", err)
	}
}

// TestInPlaceRootsKeepAnyProfileName: WaxBin never lays out an in-place root, so the
// profile it names is not checked. A config that still names a removed profile there
// opens, adds, and lets the profile go.
func TestInPlaceRootsKeepAnyProfileName(t *testing.T) {
	ctx := context.Background()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:   filepath.Join(t.TempDir(), "catalog.db"),
		Roots:    []config.Root{{Path: t.TempDir(), Mode: model.ModeInPlace, Profile: "gone"}},
		Profiles: []config.ProfileDef{{Name: "mine"}},
	})
	if err != nil {
		t.Fatalf("open with an in-place root naming an unknown profile: %v", err)
	}
	defer lib.Close()
	if _, err := lib.AddRoot(ctx, config.Root{Path: t.TempDir(), Mode: model.ModeInPlace, Profile: "mine"}); err != nil {
		t.Fatalf("add an in-place root: %v", err)
	}
	if err := lib.SetProfiles(ctx, nil); err != nil {
		t.Fatalf("dropping a profile only an in-place root names: %v", err)
	}
}

// TestAddRootAndSetProfilesSerialize: a root added under a profile while another
// call removes that profile ends with one of the two refused, never with a root
// naming a profile the set lacks.
func TestAddRootAndSetProfilesSerialize(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		lib, err := waxbin.Open(ctx, waxbin.Options{
			DBPath:   filepath.Join(t.TempDir(), "catalog.db"),
			Roots:    []config.Root{{Path: t.TempDir(), Mode: model.ModeManaged}},
			Profiles: []config.ProfileDef{{Name: "mine"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var addErr, setErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, addErr = lib.AddRoot(ctx, config.Root{Path: t.TempDir(), Mode: model.ModeManaged, Profile: "mine"})
		}()
		go func() {
			defer wg.Done()
			setErr = lib.SetProfiles(ctx, nil)
		}()
		wg.Wait()
		if (addErr == nil) == (setErr == nil) {
			t.Fatalf("add root err %v, set profiles err %v, want exactly one refused", addErr, setErr)
		}
		_ = lib.Close()
	}
}

// messages records every log line.
type messages struct {
	mu   sync.Mutex
	msgs []string
}

func (m *messages) Enabled(context.Context, slog.Level) bool { return true }
func (m *messages) WithAttrs([]slog.Attr) slog.Handler       { return m }
func (m *messages) WithGroup(string) slog.Handler            { return m }
func (m *messages) Handle(_ context.Context, r slog.Record) error {
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	m.mu.Lock()
	m.msgs = append(m.msgs, line)
	m.mu.Unlock()
	return nil
}

// TestAStoredRootWithAMissingProfile: a root the catalog kept but the configuration no
// longer names can lose its profile. Open warns rather than refusing, since fixing it
// needs an open catalog, and an import routed through that root names the problem
// instead of laying files out under some other profile.
func TestAStoredRootWithAMissingProfile(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	music, books := t.TempDir(), t.TempDir()
	first, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots: []config.Root{
			{Path: music, Mode: model.ModeManaged, Media: model.MediaMusic},
			{Path: books, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "mine"},
		},
		Profiles: []config.ProfileDef{{Name: "mine"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	libs, err := first.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var booksPID model.PID
	for _, l := range libs {
		if l.Profile == "mine" {
			booksPID = l.PID
		}
	}
	_ = first.Close()

	logs := &messages{}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: music, Mode: model.ModeManaged, Media: model.MediaMusic}},
		Logger: slog.New(logs),
	})
	if err != nil {
		t.Fatalf("open without the stored root's profile: %v", err)
	}
	defer lib.Close()
	warned := false
	for _, m := range logs.msgs {
		if strings.Contains(m, string(booksPID)) && strings.Contains(m, "mine") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("logs %q name neither the library nor its profile", logs.msgs)
	}

	staging := t.TempDir()
	writeFile(t, filepath.Join(staging, "song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	_, err = lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), string(booksPID)) || !strings.Contains(err.Error(), "mine") {
		t.Fatalf("import routed over the stored root = %v, want CodeInvalid naming it and its profile", err)
	}
}

// TestReopenWarnsOfAStoredRootsMissingProfile: a reopen can land on a catalog written
// elsewhere during the hand-off, so it checks the stored roots' profiles as Open does.
func TestReopenWarnsOfAStoredRootsMissingProfile(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	logs := &messages{}
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, Logger: slog.New(logs)})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if err := lib.BeginMaintenance(ctx); err != nil {
		t.Fatalf("begin maintenance: %v", err)
	}
	other, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:   db,
		Roots:    []config.Root{{Path: t.TempDir(), Mode: model.ModeManaged, Profile: "mine"}},
		Profiles: []config.ProfileDef{{Name: "mine"}},
	})
	if err != nil {
		t.Fatalf("foreground open: %v", err)
	}
	_ = other.Close()
	if err := lib.EndMaintenance(ctx); err != nil {
		t.Fatalf("end maintenance: %v", err)
	}
	for _, m := range logs.msgs {
		if strings.Contains(m, "profile=mine") {
			return
		}
	}
	t.Fatalf("logs %q do not name the stored root's missing profile", logs.msgs)
}
