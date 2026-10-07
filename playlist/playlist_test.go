package playlist

import (
	"bytes"
	"context"
	"fmt"
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
	rules     map[model.PID]query.Query
	removed   []removal
	owners    map[model.PID]model.PID
	failSet   error
	shared    map[string][]*model.ItemView // paths behind several items, a cue rip's file
	lookups   int
}

// removal records one by-index removal the service handed the store.
type removal struct {
	pid     model.PID
	indexes []int
	expect  []model.PID
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byPath: map[string]*model.ItemView{}, members: map[model.PID][]model.PID{},
		createdAs: map[model.PID]model.PlaylistKind{}, rules: map[model.PID]query.Query{},
		owners: map[model.PID]model.PID{}, shared: map[string][]*model.ItemView{},
	}
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

func (f *fakeStore) CreatePlaylistWithItems(ctx context.Context, name string, owner model.PID, vis model.PlaylistVisibility, itemPIDs []model.PID) (model.PID, error) {
	if f.failSet != nil {
		return "", f.failSet
	}
	pid, _ := f.CreatePlaylist(ctx, name, owner, model.PlaylistStatic, vis, nil)
	f.members[pid] = append([]model.PID(nil), itemPIDs...)
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

// CountPlaylistItems ignores narrow: the fake has no query engine, and the service
// method is a straight delegation, so the narrowing semantics are pinned in
// store/sqlite where they live.
func (f *fakeStore) CountPlaylistItems(ctx context.Context, pid model.PID, userPID model.PID, _ query.Node) (int, error) {
	items, err := f.PlaylistItems(ctx, pid, userPID)
	return len(items), err
}

func (f *fakeStore) SetPlaylistItems(_ context.Context, pid model.PID, itemPIDs []model.PID) error {
	f.members[pid] = append([]model.PID(nil), itemPIDs...)
	return nil
}

func (f *fakeStore) UserByPID(_ context.Context, pid model.PID) (*model.User, error) {
	switch pid {
	case "", "ann", "bob":
		return &model.User{PID: pid}, nil
	}
	return nil, waxerr.New(waxerr.CodeNotFound, "fake", "no such user")
}

func (f *fakeStore) ItemsByPlaylistPath(_ context.Context, path string) ([]*model.ItemView, error) {
	f.lookups++
	if items, ok := f.shared[path]; ok {
		return items, nil
	}
	if it, ok := f.byPath[path]; ok {
		return []*model.ItemView{it}, nil
	}
	return nil, nil
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
func (f *fakeStore) RemovePlaylistItemAt(_ context.Context, pid model.PID, index int, expect model.PID) error {
	f.removed = append(f.removed, removal{pid, []int{index}, []model.PID{expect}})
	return nil
}
func (f *fakeStore) RemovePlaylistItemsAt(_ context.Context, pid model.PID, indexes []int, expect []model.PID) error {
	f.removed = append(f.removed, removal{pid, indexes, expect})
	return nil
}
func (f *fakeStore) SetPlaylistOwner(_ context.Context, pid, owner model.PID) error {
	f.owners[pid] = owner
	return nil
}
func (f *fakeStore) TransferPlaylists(_ context.Context, from, to model.PID) (int, error) {
	n := 0
	for pl, owner := range f.owners {
		if owner == from {
			f.owners[pl] = to
			n++
		}
	}
	return n, nil
}
func (f *fakeStore) SetPlaylistRule(_ context.Context, pid model.PID, rule query.Query) error {
	f.rules[pid] = rule
	return nil
}

// TestServiceSetRule confirms the service dispatches a rule replacement to the
// store unchanged (validation and the no-op contract live store-side).
func TestServiceSetRule(t *testing.T) {
	fs := newFakeStore()
	svc := New(fs)
	rule := query.New(query.EntityItems).Where("year", query.OpGte, 2000).Limit(3).Build()
	if err := svc.SetRule(context.Background(), "pl1", rule); err != nil {
		t.Fatalf("set rule: %v", err)
	}
	got, ok := fs.rules["pl1"]
	if !ok || got.Limit != 3 {
		t.Errorf("stored rule = %+v (ok %v), want the passed rule under pl1", got, ok)
	}
}

// TestServiceRemovesByIndex: the by-index removals reach the store with their indexes
// and guards as given.
func TestServiceRemovesByIndex(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	svc := New(fs)
	ctx := context.Background()
	if err := svc.RemoveAt(ctx, "pl1", 2, "ia"); err != nil {
		t.Fatalf("remove at: %v", err)
	}
	if err := svc.RemoveAtMany(ctx, "pl1", []int{3, 1}, []model.PID{"ic", ""}); err != nil {
		t.Fatalf("remove many: %v", err)
	}
	if len(fs.removed) != 2 {
		t.Fatalf("store saw %d removals, want 2", len(fs.removed))
	}
	one, many := fs.removed[0], fs.removed[1]
	if one.pid != "pl1" || len(one.indexes) != 1 || one.indexes[0] != 2 || one.expect[0] != "ia" {
		t.Errorf("remove at reached the store as %+v", one)
	}
	if many.pid != "pl1" || len(many.indexes) != 2 || many.indexes[0] != 3 || many.indexes[1] != 1 ||
		len(many.expect) != 2 || many.expect[0] != "ic" {
		t.Errorf("remove many reached the store as %+v", many)
	}
}

// TestServiceMovesOwners: an owner change and a transfer reach the store as given.
func TestServiceMovesOwners(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	svc := New(fs)
	ctx := context.Background()
	if err := svc.SetOwner(ctx, "pl1", "ann"); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	fs.owners["pl2"] = "ann"
	n, err := svc.TransferPlaylists(ctx, "ann", "bob")
	if err != nil || n != 2 || fs.owners["pl1"] != "bob" || fs.owners["pl2"] != "bob" {
		t.Errorf("transfer = %d (err %v), owners %v; want both of ann's moved to bob", n, err, fs.owners)
	}
}

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

// TestResolveM3U8MatchesEachLine: every path line gets one match in document order, a
// path that names no item (or more than one) included with no item, and a line given
// twice twice.
func TestResolveM3U8MatchesEachLine(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "A", "X")
	fs.add("/music/b.flac", "ib", "B", "X")
	doc := "#EXTM3U\n/music/a.flac\n/music/missing.flac\n#EXTINF:1,X - B\n/music/b.flac\n/music/a.flac\n"
	matches, err := New(fs).ResolveM3U8(context.Background(), strings.NewReader(doc))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []struct {
		path string
		item model.PID
	}{{"/music/a.flac", "ia"}, {"/music/missing.flac", ""}, {"/music/b.flac", "ib"}, {"/music/a.flac", "ia"}}
	if len(matches) != len(want) {
		t.Fatalf("matches = %d, want %d", len(matches), len(want))
	}
	for i, w := range want {
		m := matches[i]
		var got model.PID
		if m.Item != nil {
			got = m.Item.PID
		}
		if m.Path != w.path || got != w.item {
			t.Errorf("match %d = %s -> %q, want %s -> %q", i, m.Path, got, w.path, w.item)
		}
	}
}

// TestResolveM3U8PicksByTheEntryLabel: among the items one path names, the entry's
// #EXTINF label picks one (as "artist - title" or the title alone), its length breaking
// a tie between two of one name; a missing or matching-nothing label picks none.
func TestResolveM3U8PicksByTheEntryLabel(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.shared["/rip.flac"] = []*model.ItemView{
		{PID: "t1", Title: "Intro", Artist: "Band", DurationMS: 60000},
		{PID: "t2", Title: "Song", Artist: "Band", DurationMS: 200000},
		{PID: "t3", Title: "Song", Artist: "Band", DurationMS: 300000},
	}
	doc := "#EXTM3U\n#EXTINF:60,Band - Intro\n/rip.flac\n#EXTINF:200,Song\n/rip.flac\n" +
		"#EXTINF:300.4,Band - Song\n/rip.flac\n#EXTINF:-1,Band - Song\n/rip.flac\n/rip.flac\n" +
		"#EXTINF:60,Other - Intro\n/rip.flac\n"
	matches, err := New(fs).ResolveM3U8(context.Background(), strings.NewReader(doc))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var got []string
	for _, m := range matches {
		if m.Item == nil {
			got = append(got, "-")
			continue
		}
		got = append(got, string(m.Item.PID))
	}
	if strings.Join(got, " ") != "t1 t2 t3 - - -" {
		t.Errorf("picked %v, want t1 t2 t3 - - -", got)
	}
}

// TestImportM3U8MergesARunOfOneItemsFiles: consecutive lines naming one item through
// different files (a book's parts) are one entry, while a path given twice is the
// listener's repeat and a later return to the item a new entry.
func TestImportM3U8MergesARunOfOneItemsFiles(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	for _, part := range []string{"/b/p1.m4b", "/b/p2.m4b", "/b/p3.m4b"} {
		fs.add(part, "bk", "Book", "Author")
	}
	fs.add("/s.flac", "s", "Song", "X")
	doc := "/b/p1.m4b\n/b/p2.m4b\n/b/p3.m4b\n/s.flac\n/s.flac\n/b/p1.m4b\n/b/p1.m4b\n"
	svc := New(fs)
	ctx := context.Background()
	matches, err := svc.ResolveM3U8(ctx, strings.NewReader(doc))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var merged []bool
	for _, m := range matches {
		merged = append(merged, m.Merged)
	}
	if want := []bool{false, true, true, false, false, false, false}; fmt.Sprint(merged) != fmt.Sprint(want) {
		t.Errorf("merged = %v, want %v", merged, want)
	}
	res, err := svc.ImportM3U8(ctx, "p", "", "", strings.NewReader(doc))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := fs.members[res.PlaylistPID]; fmt.Sprint(got) != "[bk s s bk bk]" {
		t.Errorf("entries = %v, want [bk s s bk bk]", got)
	}
	if res.Matched != 5 || res.Merged != 2 || res.Unmatched != 0 {
		t.Errorf("result = %+v, want 5 matched, 2 merged, none unmatched", res)
	}
}

// TestParseM3U8StripsAByteOrderMark: a document saved with a byte-order mark reads its
// first line as written, a header or a path.
func TestParseM3U8StripsAByteOrderMark(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "A", "X")
	svc := New(fs)
	for _, doc := range []string{"\ufeff#EXTM3U\n/music/a.flac\n", "\ufeff/music/a.flac\n"} {
		matches, err := svc.ResolveM3U8(context.Background(), strings.NewReader(doc))
		if err != nil {
			t.Fatalf("resolve %q: %v", doc, err)
		}
		if len(matches) != 1 || matches[0].Item == nil || matches[0].Item.PID != "ia" {
			t.Errorf("%q resolved to %+v, want the one entry A", doc, matches)
		}
	}
}

// TestExtInfLabelKeepsAQuotedComma: an attribute value holding a comma does not cut the
// label short.
func TestExtInfLabelKeepsAQuotedComma(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.shared["/rip.flac"] = []*model.ItemView{
		{PID: "t1", Title: "Song", Artist: "Smith, John"},
		{PID: "t2", Title: "Other", Artist: "Smith, John"},
	}
	doc := "#EXTINF:215 tvg-name=\"Smith, John\",Smith, John - Song\n/rip.flac\n"
	matches, err := New(fs).ResolveM3U8(context.Background(), strings.NewReader(doc))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(matches) != 1 || matches[0].Item == nil || matches[0].Item.PID != "t1" {
		t.Errorf("matches = %+v, want the label to pick t1", matches)
	}
}

// TestImportM3U8ChecksBeforeResolving: a name, visibility or owner the playlist could
// never be created with is refused before any entry is looked up, and CheckImport says
// so for a preview.
func TestImportM3U8ChecksBeforeResolving(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "A", "X")
	svc := New(fs)
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		owner model.PID
		vis   model.PlaylistVisibility
		code  waxerr.Code
	}{
		{" ", "", "", waxerr.CodeInvalid},
		{"p", "", "bogus", waxerr.CodeInvalid},
		{"p", "nobody", "", waxerr.CodeNotFound},
	} {
		if err := svc.CheckImport(ctx, c.name, c.owner, c.vis); !waxerr.Is(err, c.code) {
			t.Errorf("check %+v: err = %v, want %s", c, err, c.code)
		}
		if _, err := svc.ImportM3U8(ctx, c.name, c.owner, c.vis, strings.NewReader("/music/a.flac\n")); !waxerr.Is(err, c.code) {
			t.Errorf("import %+v: err = %v, want %s", c, err, c.code)
		}
	}
	if fs.lookups != 0 {
		t.Errorf("refused imports looked up %d entries, want none", fs.lookups)
	}
	if err := svc.CheckImport(ctx, "p", "bob", model.VisibilityShared); err != nil {
		t.Errorf("check of a valid import: %v", err)
	}
}

// TestImportM3U8CreatesInOneStep: a store that refuses the entries leaves no playlist,
// since the playlist and its entries are one write.
func TestImportM3U8CreatesInOneStep(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "A", "X")
	fs.failSet = waxerr.New(waxerr.CodeNotFound, "fake", "no such item: ia")
	_, err := New(fs).ImportM3U8(context.Background(), "p", "", "", strings.NewReader("/music/a.flac\n"))
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("import err = %v, want the store's CodeNotFound", err)
	}
	if len(fs.members) != 0 {
		t.Errorf("playlists = %v, want none created", fs.members)
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
