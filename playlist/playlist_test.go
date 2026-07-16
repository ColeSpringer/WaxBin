package playlist

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// fakeStore is an in-memory Store for exercising the service's M3U8 orchestration
// without a database.
type fakeStore struct {
	byPath    map[string]*model.ItemView
	members   map[model.PID][]model.PID
	nextID    int
	createdAs map[model.PID]model.PlaylistKind
}

func newFakeStore() *fakeStore {
	return &fakeStore{byPath: map[string]*model.ItemView{}, members: map[model.PID][]model.PID{}, createdAs: map[model.PID]model.PlaylistKind{}}
}

func (f *fakeStore) add(path string, pid model.PID, title, artist string) {
	f.byPath[path] = &model.ItemView{PID: pid, Title: title, Artist: artist, DisplayPath: path, DurationMS: 200000}
}

func (f *fakeStore) CreatePlaylist(_ context.Context, name string, _ model.PID, kind model.PlaylistKind, _ model.PlaylistVisibility, _ *query.Query) (model.PID, error) {
	f.nextID++
	pid := model.PID("pl" + string(rune('0'+f.nextID)))
	f.createdAs[pid] = kind
	f.members[pid] = nil
	return pid, nil
}

func (f *fakeStore) PlaylistItems(_ context.Context, pid model.PID, _ model.PID) ([]*model.ItemView, error) {
	var out []*model.ItemView
	for _, ip := range f.members[pid] {
		for _, it := range f.byPath {
			if it.PID == ip {
				out = append(out, it)
			}
		}
	}
	return out, nil
}

func (f *fakeStore) SetPlaylistItems(_ context.Context, pid model.PID, itemPIDs []model.PID) error {
	f.members[pid] = append([]model.PID(nil), itemPIDs...)
	return nil
}

func (f *fakeStore) ItemByPlaylistPath(_ context.Context, path string) (*model.ItemView, error) {
	if it, ok := f.byPath[path]; ok {
		return it, nil
	}
	return nil, waxerr.New(waxerr.CodeNotFound, "fake", "no item at "+path)
}

// Unused-by-these-tests methods.
func (f *fakeStore) PlaylistByPID(context.Context, model.PID) (*model.Playlist, error) {
	return nil, nil
}
func (f *fakeStore) ListPlaylists(context.Context, model.PID) ([]*model.Playlist, error) {
	return nil, nil
}
func (f *fakeStore) DeletePlaylist(context.Context, model.PID) error         { return nil }
func (f *fakeStore) RenamePlaylist(context.Context, model.PID, string) error { return nil }
func (f *fakeStore) SetPlaylistVisibility(context.Context, model.PID, model.PlaylistVisibility) error {
	return nil
}
func (f *fakeStore) AddPlaylistItems(_ context.Context, pid model.PID, items []model.PID) error {
	f.members[pid] = append(f.members[pid], items...)
	return nil
}
func (f *fakeStore) RemovePlaylistItem(context.Context, model.PID, model.PID) error { return nil }
func (f *fakeStore) RemovePlaylistItemAt(context.Context, model.PID, int) error     { return nil }

func TestM3U8RoundTrip(t *testing.T) {
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "Song A", "Artist X")
	fs.add("/music/b.flac", "ib", "Song B", "Artist Y")
	svc := New(fs)
	ctx := context.Background()

	src, _ := svc.CreateStatic(ctx, "src", "", "")
	if err := svc.Set(ctx, src, []model.PID{"ia", "ib"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	var buf bytes.Buffer
	if err := svc.ExportM3U8(ctx, src, &buf, ""); err != nil {
		t.Fatalf("export: %v", err)
	}
	text := buf.String()
	if !strings.HasPrefix(text, "#EXTM3U") {
		t.Errorf("export missing #EXTM3U header:\n%s", text)
	}
	if !strings.Contains(text, "/music/a.flac") || !strings.Contains(text, "Artist X - Song A") {
		t.Errorf("export missing item lines:\n%s", text)
	}

	// Re-import the exported document: both items match by path.
	res, err := svc.ImportM3U8(ctx, "copy", "", "", strings.NewReader(text))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Matched != 2 || res.Unmatched != 0 {
		t.Errorf("import result = %+v, want matched 2 / unmatched 0", res)
	}
	items, _ := svc.Items(ctx, res.PlaylistPID, "")
	if len(items) != 2 {
		t.Errorf("imported playlist has %d items, want 2", len(items))
	}
}

func TestExportM3U8RefusesNewlinePath(t *testing.T) {
	fs := newFakeStore()
	fs.add("/music/a\nb.flac", "ia", "Title", "Artist") // path with an embedded newline
	svc := New(fs)
	ctx := context.Background()
	src, _ := svc.CreateStatic(ctx, "s", "", "")
	if err := svc.Set(ctx, src, []model.PID{"ia"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := svc.ExportM3U8(ctx, src, &bytes.Buffer{}, ""); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("exporting a newline path err = %v, want CodeInvalid (refuse, not corrupt)", err)
	}
}

func TestExportM3U8FoldsMetadataNewlines(t *testing.T) {
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "Title\nWith\nBreaks", "The\rArtist")
	svc := New(fs)
	ctx := context.Background()
	src, _ := svc.CreateStatic(ctx, "s", "", "")
	if err := svc.Set(ctx, src, []model.PID{"ia"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	var buf bytes.Buffer
	if err := svc.ExportM3U8(ctx, src, &buf, ""); err != nil {
		t.Fatalf("export: %v", err)
	}
	// The #EXTINF directive must stay one line: title/artist newlines folded to spaces.
	if !strings.Contains(buf.String(), "#EXTINF:200,The Artist - Title With Breaks\n") {
		t.Errorf("metadata newlines not folded:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "/music/a.flac\n") {
		t.Errorf("path line missing or split:\n%s", buf.String())
	}
}

func TestM3U8ImportReportsUnmatched(t *testing.T) {
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "A", "X")
	svc := New(fs)
	ctx := context.Background()

	doc := "#EXTM3U\n#EXTINF:200,X - A\n/music/a.flac\n/music/missing.flac\n"
	res, err := svc.ImportM3U8(ctx, "p", "", "", strings.NewReader(doc))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Matched != 1 || res.Unmatched != 1 {
		t.Errorf("result = %+v, want matched 1 / unmatched 1", res)
	}
	if len(res.UnmatchedPaths) != 1 || res.UnmatchedPaths[0] != "/music/missing.flac" {
		t.Errorf("unmatched paths = %v, want [/music/missing.flac]", res.UnmatchedPaths)
	}
}
