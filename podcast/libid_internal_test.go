package podcast

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/store/sqlite"
)

// libIDStore answers the podcast library id the test sets and records the id each
// attach was given, standing in for the catalog a hand-off replaced.
type libIDStore struct {
	Store
	id       int64
	attached []int64
}

func (s *libIDStore) EnsurePodcastLibrary(context.Context, string) (int64, error) { return s.id, nil }

func (s *libIDStore) AttachEpisodeFile(_ context.Context, in model.AttachEpisodeFileInput) (model.PID, error) {
	s.attached = append(s.attached, in.LibraryID)
	return model.NewPID(), nil
}

// handoffLeaser lands a hand-off on a restored catalog before each commit tail: the
// podcast library has a new id, and the service has forgotten the one it cached.
type handoffLeaser struct {
	svc   *Service
	store *libIDStore
}

func (l handoffLeaser) handoff() { l.store.id++; l.svc.ForgetLibrary() }
func (l handoffLeaser) Lease(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
func (l handoffLeaser) LeaseWait(ctx context.Context, fn func(context.Context) error) error {
	l.handoff()
	return fn(ctx)
}
func (l handoffLeaser) LeaseImport(ctx context.Context, _ string, _ bool, fn func(context.Context) error) error {
	l.handoff()
	return fn(ctx)
}

// TestCommitUsesTheLibraryIDOfTheCatalogItLandsIn: a download or an episode import
// that spans a hand-off commits into the podcast library of the catalog it finishes
// against, not the one it started against.
func TestCommitUsesTheLibraryIDOfTheCatalogItLandsIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "catalog.db"), Owner: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	store := &libIDStore{Store: st, id: 1}
	yt := &source.Mock{Type: model.SourceYouTube, IdentityKey: "youtube:channel:c1",
		Feed: &model.Feed{Title: "Chan", Episodes: []model.FeedEpisode{
			{Title: "One", GUID: "youtube:video:v1", EnclosureURL: "yt://v1"},
			{Title: "Two", GUID: "youtube:video:v2", EnclosureURL: "yt://v2"},
		}},
		Payload: []byte("audio")}
	svc := New(store, meta.NewReader(), Config{Dir: t.TempDir(), Providers: []source.Provider{yt}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.cfg.Leaser = handoffLeaser{svc: svc, store: store}
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v)", len(eps), err)
	}

	if _, err := svc.Download(ctx, eps[0].PID); err != nil {
		t.Fatalf("download: %v", err)
	}
	src := filepath.Join(t.TempDir(), "two.mp3")
	if err := os.WriteFile(src, []byte("audio two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportEpisodeFile(ctx, eps[1].PID, src, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(store.attached) != 2 || store.attached[0] != 2 || store.attached[1] != 3 {
		t.Fatalf("attached into libraries %v, want [2 3]: each commit in the catalog it landed in", store.attached)
	}
}
