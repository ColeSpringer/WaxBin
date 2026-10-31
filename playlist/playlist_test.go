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
	lookups   int                          // paths looked up
	calls     int                          // lookup calls
	asked     []string
	limits    []int // each call's limit
	playlists map[model.PID]*model.Playlist
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
		playlists: map[model.PID]*model.Playlist{},
	}
}

func (f *fakeStore) add(path string, pid model.PID, title, artist string) {
	f.byPath[path] = &model.ItemView{PID: pid, Title: title, Artist: artist, DisplayPath: path, DurationMS: 200000}
}

func (f *fakeStore) CreatePlaylist(_ context.Context, name, _ string, _ model.PID, kind model.PlaylistKind, _ model.PlaylistVisibility, _ *query.Query) (model.PID, error) {
	f.nextID++
	pid := model.PID("pl" + string(rune('0'+f.nextID)))
	f.createdAs[pid] = kind
	f.members[pid] = nil
	return pid, nil
}

func (f *fakeStore) CreatePlaylistWithItems(ctx context.Context, name, description string, owner model.PID, vis model.PlaylistVisibility, itemPIDs []model.PID) (model.PID, error) {
	if f.failSet != nil {
		return "", f.failSet
	}
	pid, _ := f.CreatePlaylist(ctx, name, description, owner, model.PlaylistStatic, vis, nil)
	f.members[pid] = append([]model.PID(nil), itemPIDs...)
	return pid, nil
}

func (f *fakeStore) PlaylistItems(ctx context.Context, pid model.PID, _ model.PID) ([]*model.ItemView, error) {
	var out []*model.ItemView
	for _, ip := range f.members[pid] {
		items, _ := f.ItemsByPIDs(ctx, []model.PID{ip})
		out = append(out, items...)
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

func (f *fakeStore) ItemsByPlaylistPaths(_ context.Context, paths []string, limit int) (map[string]model.PathCandidates, error) {
	f.calls++
	f.limits = append(f.limits, limit)
	out := map[string]model.PathCandidates{}
	for _, path := range paths {
		f.lookups++
		f.asked = append(f.asked, path)
		items := f.shared[path]
		if it, ok := f.byPath[path]; ok && items == nil {
			items = []*model.ItemView{it}
		}
		if len(items) > limit {
			out[path] = model.PathCandidates{Overflow: true}
			continue
		}
		out[path] = model.PathCandidates{Items: items}
	}
	return out, nil
}

// ItemsByPIDs reads as the store's does: input order, unknown pids skipped, a repeat
// collapsed to its first place.
func (f *fakeStore) ItemsByPIDs(_ context.Context, pids []model.PID) ([]*model.ItemView, error) {
	var out []*model.ItemView
	seen := map[model.PID]bool{}
	for _, pid := range pids {
		for _, it := range f.byPath {
			if it.PID == pid && !seen[pid] {
				seen[pid] = true
				out = append(out, it)
			}
		}
		for _, items := range f.shared {
			for _, it := range items {
				if it.PID == pid && !seen[pid] {
					seen[pid] = true
					out = append(out, it)
				}
			}
		}
	}
	return out, nil
}

func (f *fakeStore) PlaylistByPID(_ context.Context, pid model.PID) (*model.Playlist, error) {
	if pl, ok := f.playlists[pid]; ok {
		return pl, nil
	}
	return nil, waxerr.New(waxerr.CodeNotFound, "fake.PlaylistByPID", "no such playlist: "+string(pid))
}

// QueryNeedsUser compiles against a two-field map, one field per-user, as the store
// compiles against its own.
func (f *fakeStore) QueryNeedsUser(q query.Query) (bool, error) {
	c, err := query.Compile(q, query.FieldMap{
		"title":  {Expr: "title"},
		"rating": {Expr: "rating", Kind: query.KindInt, NeedsUser: true},
	})
	if err != nil {
		return false, err
	}
	return c.NeedsUser, nil
}

// Unused-by-these-tests methods.
func (f *fakeStore) ListPlaylists(context.Context, model.PID) ([]*model.Playlist, error) {
	return nil, nil
}
func (f *fakeStore) DeletePlaylist(context.Context, model.PID) error         { return nil }
func (f *fakeStore) RenamePlaylist(context.Context, model.PID, string) error { return nil }
func (f *fakeStore) SetPlaylistDescription(context.Context, model.PID, string) error {
	return nil
}
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

	src, _ := svc.CreateStatic(ctx, "src", "", "", "")
	if err := svc.Set(ctx, src, []model.PID{"ia", "ib"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	var buf bytes.Buffer
	if _, err := svc.ExportM3U8(ctx, src, &buf, ""); err != nil {
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
	res, err := svc.ImportM3U8(ctx, "copy", "", "", "", strings.NewReader(text))
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
	src, _ := svc.CreateStatic(ctx, "s", "", "", "")
	if err := svc.Set(ctx, src, []model.PID{"ia"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := svc.ExportM3U8(ctx, src, &bytes.Buffer{}, ""); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("exporting a newline path err = %v, want CodeInvalid (refuse, not corrupt)", err)
	}
}

func TestExportM3U8FoldsMetadataNewlines(t *testing.T) {
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "Title\nWith\nBreaks", "The\rArtist")
	svc := New(fs)
	ctx := context.Background()
	src, _ := svc.CreateStatic(ctx, "s", "", "", "")
	if err := svc.Set(ctx, src, []model.PID{"ia"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	var buf bytes.Buffer
	if _, err := svc.ExportM3U8(ctx, src, &buf, ""); err != nil {
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
	res, err := svc.ImportM3U8(ctx, "p", "", "", "", strings.NewReader(doc))
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
		if err := svc.CheckImport(ctx, c.name, "", c.owner, c.vis); !waxerr.Is(err, c.code) {
			t.Errorf("check %+v: err = %v, want %s", c, err, c.code)
		}
		if _, err := svc.ImportM3U8(ctx, c.name, "", c.owner, c.vis, strings.NewReader("/music/a.flac\n")); !waxerr.Is(err, c.code) {
			t.Errorf("import %+v: err = %v, want %s", c, err, c.code)
		}
	}
	if fs.lookups != 0 {
		t.Errorf("refused imports looked up %d entries, want none", fs.lookups)
	}
	if err := svc.CheckImport(ctx, "p", "", "bob", model.VisibilityShared); err != nil {
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
	_, err := New(fs).ImportM3U8(context.Background(), "p", "", "", "", strings.NewReader("/music/a.flac\n"))
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
	res, err := svc.ImportM3U8(ctx, "p", "", "", "", strings.NewReader(doc))
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

// TestServiceReadsUserState: a smart playlist reads per-user state when its rule does, a
// static one never does, since its members are the same for every reader, and an unknown
// playlist is the store's error.
func TestServiceReadsUserState(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	rated := query.New(query.EntityItems).Where("rating", query.OpGte, 80).Build()
	titled := query.New(query.EntityItems).Where("title", query.OpContains, "a").Build()
	fs.playlists["rated"] = &model.Playlist{PID: "rated", Kind: model.PlaylistSmart, Rule: &rated}
	fs.playlists["titled"] = &model.Playlist{PID: "titled", Kind: model.PlaylistSmart, Rule: &titled}
	fs.playlists["static"] = &model.Playlist{PID: "static", Kind: model.PlaylistStatic}
	svc := New(fs)
	ctx := context.Background()
	for pid, want := range map[model.PID]bool{"rated": true, "titled": false, "static": false} {
		got, err := svc.ReadsUserState(ctx, pid)
		if err != nil || got != want {
			t.Errorf("ReadsUserState(%s) = %v, %v; want %v", pid, got, err, want)
		}
	}
	if _, err := svc.ReadsUserState(ctx, "nope"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("ReadsUserState(unknown) = %v, want CodeNotFound", err)
	}
}

// TestResolveM3U8LooksEachDistinctPathUpOnce: a document is resolved in one lookup that
// names each distinct path once, its first spelling standing for the ones that clean to
// it, and every line is still answered.
func TestResolveM3U8LooksEachDistinctPathUpOnce(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.add("a.flac", "ia", "A", "X")
	fs.add("b.flac", "ib", "B", "X")
	doc := "a.flac\nb.flac\n./a.flac\na.flac\nmissing.flac\nx/../b.flac\nmissing.flac\n"
	matches, err := New(fs).ResolveM3U8(context.Background(), strings.NewReader(doc))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if fs.calls != 1 || fmt.Sprint(fs.asked) != "[a.flac b.flac missing.flac]" {
		t.Errorf("lookups = %d calls of %v, want one of [a.flac b.flac missing.flac]", fs.calls, fs.asked)
	}
	var got []string
	for _, m := range matches {
		pid := "-"
		if m.Item != nil {
			pid = string(m.Item.PID)
		}
		got = append(got, pid)
	}
	if strings.Join(got, " ") != "ia ib ia ia - ib -" {
		t.Errorf("matches = %v, want ia ib ia ia - ib -", got)
	}
	if _, err := New(fs).ResolveM3U8(context.Background(), strings.NewReader("#EXTM3U\n")); err != nil || fs.calls != 1 {
		t.Errorf("a document with no paths: err %v, %d calls, want no lookup", err, fs.calls)
	}
}

// TestResolveM3U8AcceptPicksTheOnlyVisibleCandidate: a path naming 150 items the caller
// may not hold and one it may names that one, while unscoped it names too many to pick
// from and names nothing; an import scoped the same way lists it.
func TestResolveM3U8AcceptPicksTheOnlyVisibleCandidate(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	var items []*model.ItemView
	for i := range 150 {
		items = append(items, &model.ItemView{PID: model.PID(fmt.Sprintf("hidden%03d", i)), Title: "Song"})
	}
	fs.shared["song.flac"] = append(items, &model.ItemView{PID: "mine", Title: "Song"})
	svc := New(fs)
	ctx := context.Background()
	mine := ResolveOptions{Accept: func(v *model.ItemView) bool { return v.PID == "mine" }}
	if matches, err := svc.ResolveM3U8(ctx, strings.NewReader("song.flac\n")); err != nil || matches[0].Item != nil {
		t.Errorf("unscoped = %+v (err %v), want no item among 151", matches, err)
	}
	matches, err := svc.ResolveM3U8With(ctx, strings.NewReader("song.flac\n"), mine)
	if err != nil || len(matches) != 1 || matches[0].Item == nil || matches[0].Item.PID != "mine" {
		t.Errorf("scoped = %+v (err %v), want the one visible item", matches, err)
	}
	res, err := svc.ImportM3U8With(ctx, "Mine", "", "", "", strings.NewReader("song.flac\n"), mine)
	if err != nil || res.Matched != 1 || fmt.Sprint(fs.members[res.PlaylistPID]) != "[mine]" {
		t.Errorf("scoped import = %+v (err %v), want the one visible item listed", res, err)
	}
}

// TestResolveM3U8AnOverflowedPathNamesNothing: a path the lookup could not read whole
// names nothing, even when the part it read holds one item the caller may hold, since
// what it did not read could hold another.
func TestResolveM3U8AnOverflowedPathNamesNothing(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	var items []*model.ItemView
	for i := range 1001 {
		items = append(items, &model.ItemView{PID: model.PID(fmt.Sprintf("i%04d", i)), Title: "Song"})
	}
	fs.shared["01.flac"] = items
	fs.add("other.flac", "io", "Other", "X")
	only := ResolveOptions{Accept: func(v *model.ItemView) bool { return v.PID == "i0000" || v.PID == "io" }}
	matches, err := New(fs).ResolveM3U8With(context.Background(), strings.NewReader("01.flac\nother.flac\n"), only)
	if err != nil || len(matches) != 2 || matches[0].Item != nil || matches[1].Item == nil {
		t.Errorf("matches = %+v (err %v), want the overflowed path unmatched and the other matched", matches, err)
	}
}

// TestResolveM3U8MergedRunsStillMerge: a book listed by its parts merges into one entry
// when each part's path is looked up once, a part path that also names a copy the caller
// may not hold still names the book, and a part given again in another spelling is the
// listener's repeat, a new entry the next part merges into.
func TestResolveM3U8MergedRunsStillMerge(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	book := &model.ItemView{PID: "bk", Title: "Book", Artist: "Author", DurationMS: 200000}
	for _, part := range []string{"/b/p1.m4b", "/b/p3.m4b"} {
		fs.shared[part] = []*model.ItemView{book}
	}
	fs.shared["/b/p2.m4b"] = []*model.ItemView{book, {PID: "theirs", Title: "Book", Artist: "Author"}}
	doc := "/b/p1.m4b\n/b/p2.m4b\n/b/p3.m4b\n/b/./p3.m4b\n/b/p1.m4b\n"
	mine := ResolveOptions{Accept: func(v *model.ItemView) bool { return v.PID != "theirs" }}
	res, err := New(fs).ImportM3U8With(context.Background(), "Book", "", "", "", strings.NewReader(doc), mine)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := fs.members[res.PlaylistPID]; fmt.Sprint(got) != "[bk bk]" || res.Matched != 2 || res.Merged != 3 {
		t.Errorf("entries = %v, result %+v; want [bk bk], 2 matched and 3 merged", got, res)
	}
	if fs.calls != 1 || len(fs.asked) != 3 {
		t.Errorf("lookups = %d calls of %v, want one of the three parts", fs.calls, fs.asked)
	}
}

// TestResolveM3U8RefusesALineTooLongAsInvalid: a document whose line runs past what a
// path line can be is refused as invalid input, not as a failure to read it.
func TestResolveM3U8RefusesALineTooLongAsInvalid(t *testing.T) {
	t.Parallel()
	doc := strings.Repeat("a", 2<<20) + "\n"
	if _, err := New(newFakeStore()).ResolveM3U8(context.Background(), strings.NewReader(doc)); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("a 2 MiB line: err = %v, want CodeInvalid", err)
	}
}

// TestExportM3U8ForKeepsOrderAndDuplicatesAndSkipsFileless: a list exports in its order,
// a repeat written each time, while an item with no file and a pid naming no item are
// left out and named in their places, with no line written for either.
func TestExportM3U8ForKeepsOrderAndDuplicatesAndSkipsFileless(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "A", "X")
	fs.add("/music/b.flac", "ib", "B", "Y")
	fs.shared["(no file)"] = []*model.ItemView{{PID: "ep", Title: "Episode", Artist: "Show"}}
	var buf bytes.Buffer
	res, err := New(fs).ExportM3U8For(context.Background(), []model.PID{"ib", "ep", "ia", "nope", "ib", "ep"}, &buf)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := "#EXTM3U\n#EXTINF:200,Y - B\n/music/b.flac\n#EXTINF:200,X - A\n/music/a.flac\n#EXTINF:200,Y - B\n/music/b.flac\n"
	if buf.String() != want {
		t.Errorf("document =\n%s\nwant\n%s", buf.String(), want)
	}
	if res.Written != 3 || fmt.Sprint(res.Skipped) != "[ep nope ep]" {
		t.Errorf("result = %+v, want 3 written and [ep nope ep] skipped", res)
	}
}

// TestExportM3U8NoLongerWritesBlankPaths: a playlist holding an item with no file writes
// no #EXTINF and no empty path line for it, so a re-import does not hand that item's
// label to the entry after it.
func TestExportM3U8NoLongerWritesBlankPaths(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.add("/music/a.flac", "ia", "A", "X")
	fs.shared["(no file)"] = []*model.ItemView{{PID: "ep", Title: "Episode", Artist: "Show"}}
	svc := New(fs)
	ctx := context.Background()
	pl, _ := svc.CreateStatic(ctx, "p", "", "", "")
	fs.members[pl] = []model.PID{"ep", "ia"}
	var buf bytes.Buffer
	res, err := svc.ExportM3U8(ctx, pl, &buf, "")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if buf.String() != "#EXTM3U\n#EXTINF:200,X - A\n/music/a.flac\n" || res.Written != 1 || fmt.Sprint(res.Skipped) != "[ep]" {
		t.Errorf("export = %q, %+v; want only A written and ep skipped", buf.String(), res)
	}
}

// ripOf lists n windows of one file, Track 1 to Track n by Artist.
func ripOf(n int) []*model.ItemView {
	var out []*model.ItemView
	for i := 1; i <= n; i++ {
		out = append(out, &model.ItemView{PID: model.PID(fmt.Sprintf("t%03d", i)), Title: fmt.Sprintf("Track %d", i),
			Artist: "Artist", DurationMS: 200000})
	}
	return out
}

// TestResolveM3U8ScopedLabelStillHasToMatch: a path naming several items, one of which the
// caller may hold, still needs its label to name that one; a label naming a hidden item
// matches nothing, and a line with no label takes the one the caller may hold.
func TestResolveM3U8ScopedLabelStillHasToMatch(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.shared["/music/rip.flac"] = ripOf(12)
	seven := ResolveOptions{Accept: func(v *model.ItemView) bool { return v.PID == "t007" }}
	doc := "#EXTINF:200,Artist - Track 1\n/music/rip.flac\n#EXTINF:200,Artist - Track 7\n/music/rip.flac\n/music/rip.flac\n"
	matches, err := New(fs).ResolveM3U8With(context.Background(), strings.NewReader(doc), seven)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range matches {
		pid := "-"
		if m.Item != nil {
			pid = string(m.Item.PID)
		}
		got = append(got, pid)
	}
	if strings.Join(got, " ") != "- t007 t007" {
		t.Errorf("matches = %v, want - t007 t007 (Track 1 hidden, Track 7 named, the unlabeled line the one held)", got)
	}
}

// TestResolveM3U8AnAbsolutePathTakesNoCap: an exported rip's absolute path names every
// window behind it however many there are, and each line's label picks its window, while
// a relative suffix naming that many still names nothing.
func TestResolveM3U8AnAbsolutePathTakesNoCap(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fs.shared["/music/big.flac"] = ripOf(120)
	fs.shared["big.flac"] = ripOf(120)
	doc := "#EXTINF:200,Artist - Track 1\n/music/big.flac\n#EXTINF:200,Artist - Track 120\n/music/big.flac\n" +
		"#EXTINF:200,Artist - Track 1\nbig.flac\n"
	matches, err := New(fs).ResolveM3U8(context.Background(), strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 3 || matches[0].Item == nil || matches[0].Item.PID != "t001" ||
		matches[1].Item == nil || matches[1].Item.PID != "t120" || matches[2].Item != nil {
		t.Errorf("matches = %+v, want t001 and t120 by label and nothing for the relative suffix", matches)
	}
}

// TestResolveM3U8FetchesNoMoreThanItCanUse: unscoped, a relative path is read only as far
// as the cap, since more names nothing; a scoped document and an absolute path read to the
// fetch bound.
func TestResolveM3U8FetchesNoMoreThanItCanUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct {
		name string
		doc  string
		opts ResolveOptions
		want string
	}{
		{"relative", "a.flac\nb.flac\n", ResolveOptions{}, "[100]"},
		{"absolute", "/m/a.flac\n", ResolveOptions{}, "[1000]"},
		{"both", "a.flac\n/m/a.flac\n", ResolveOptions{}, "[1000 100]"},
		{"scoped", "a.flac\n/m/a.flac\n", ResolveOptions{Accept: func(*model.ItemView) bool { return true }}, "[1000]"},
	} {
		fs := newFakeStore()
		if _, err := New(fs).ResolveM3U8With(ctx, strings.NewReader(c.doc), c.opts); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(fs.limits); got != c.want {
			t.Errorf("%s: limits %s, want %s", c.name, got, c.want)
		}
	}
}
