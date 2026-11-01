package sqlite_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/playlist"
	"github.com/colespringer/waxbin/store/sqlite"
)

func pathTitles(t *testing.T, st *sqlite.Store, p string) []string {
	t.Helper()
	got, err := st.ItemsByPlaylistPaths(context.Background(), []string{p}, 100)
	if err != nil {
		t.Fatalf("items by %s: %v", p, err)
	}
	var out []string
	for _, it := range got[p].Items {
		out = append(out, it.Title)
	}
	slices.Sort(out)
	return out
}

// everyEdgeFixture holds a copy pair, a three-track cue rip and a two-part book, and
// returns the root, the copy's path, the rip's path and the parts' paths.
func everyEdgeFixture(t *testing.T, st *sqlite.Store, lib *model.Library, root string) (copied, rip string, parts []string) {
	t.Helper()
	ctx := context.Background()
	original := filepath.Join(root, "A", "song.flac")
	copied = filepath.Join(root, "B", "copy.flac")
	putCopyPair(t, st, lib, original, copied)

	rip = filepath.Join(root, "Rip", "rip.flac")
	touch(t, rip)
	if _, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, rip, "sha256:RE", "sha256:RC", 8000,
		[][2]int64{{0, 300}, {300, 600}, {600, 900}})); err != nil {
		t.Fatalf("put rip: %v", err)
	}
	for i, name := range []string{"p1.m4b", "p2.m4b"} {
		p := filepath.Join(root, "Book", name)
		touch(t, p)
		in := bookIn(lib.ID, p, "sha256:BE"+name, "Long Book", "Author X", "", "", "")
		in.Position = i
		if _, err := st.PutScannedBook(ctx, in); err != nil {
			t.Fatalf("put part %s: %v", name, err)
		}
		parts = append(parts, p)
	}
	return copied, rip, parts
}

// TestItemsByPlaylistPathsReadEveryEdge: an M3U8 path names the items behind its file
// through any edge, so a copy's path names its item, a later book part's path its book,
// and a cue rip's file every track carved from it.
func TestItemsByPlaylistPathsReadEveryEdge(t *testing.T) {
	t.Parallel()
	st, lib, root := openCopyStore(t)
	copied, rip, parts := everyEdgeFixture(t, st, lib, root)

	for _, c := range []struct {
		path string
		want []string
	}{
		{copied, []string{"Original"}},
		{"B/copy.flac", []string{"Original"}},
		{rip, []string{"Track 1", "Track 2", "Track 3"}},
		{parts[1], []string{"Long Book"}},
		{"Book/p2.m4b", []string{"Long Book"}},
		{filepath.Join(root, "nothing.flac"), nil},
		{strings.Repeat("a/", 30000) + "x.flac", nil},
	} {
		if got := pathTitles(t, st, c.path); !slices.Equal(got, c.want) {
			t.Errorf("%.60s = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestM3U8RoundTripsCueTracks: tracks of one cue rip share a path, so an exported
// playlist of them names each by its #EXTINF label, and resolving or importing the
// document gives the same tracks back.
func TestM3U8RoundTripsCueTracks(t *testing.T) {
	t.Parallel()
	st, lib, root := openCopyStore(t)
	ctx := context.Background()
	rip := filepath.Join(root, "Rip", "rip.flac")
	touch(t, rip)
	if _, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, rip, "sha256:RE", "sha256:RC", 8000,
		[][2]int64{{0, 300}, {300, 600}, {600, 900}})); err != nil {
		t.Fatalf("put rip: %v", err)
	}
	byTitle := map[string]model.PID{}
	for _, it := range vtItems(t, st) {
		byTitle[it.Title] = it.PID
	}
	svc := playlist.New(st)
	pl, err := svc.CreateStatic(ctx, "Rip mix", "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Set(ctx, pl, []model.PID{byTitle["Track 2"], byTitle["Track 3"], byTitle["Track 2"]}); err != nil {
		t.Fatalf("set: %v", err)
	}
	var doc bytes.Buffer
	if _, err := svc.ExportM3U8(ctx, pl, &doc, ""); err != nil {
		t.Fatalf("export: %v", err)
	}

	matches, err := svc.ResolveM3U8(ctx, bytes.NewReader(doc.Bytes()))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var got []string
	for _, m := range matches {
		if m.Item == nil {
			got = append(got, "<none>")
			continue
		}
		got = append(got, m.Item.Title)
	}
	if want := []string{"Track 2", "Track 3", "Track 2"}; !slices.Equal(got, want) {
		t.Fatalf("resolved %v, want %v", got, want)
	}
	res, err := svc.ImportM3U8(ctx, "Again", "", "", "", bytes.NewReader(doc.Bytes()))
	if err != nil || res.Matched != 3 || res.Unmatched != 0 {
		t.Fatalf("import = %+v (err %v), want three matched", res, err)
	}

	// The rip's path with no label, or a label naming no track of it, names no track.
	bare := "#EXTM3U\n" + rip + "\n#EXTINF:3,Somebody - Elsewhere\n" + rip + "\n"
	matches, err = svc.ResolveM3U8(ctx, strings.NewReader(bare))
	if err != nil || len(matches) != 2 || matches[0].Item != nil || matches[1].Item != nil {
		t.Errorf("unlabeled and mislabeled rip entries = %+v (err %v), want no items", matches, err)
	}
}

// TestM3U8ReadsAWindowsRootedLineAlikeEverywhere: a line written on Windows from a
// drive's root, backslashes and no drive, names a 120-track rip past the relative cap on
// every platform, and each label picks its track: Windows reads the line below a drive's
// root, and elsewhere its backslashes are read as separators, which makes it absolute.
func TestM3U8ReadsAWindowsRootedLineAlikeEverywhere(t *testing.T) {
	t.Parallel()
	st, lib, root := openCopyStore(t)
	ctx := context.Background()
	rip := filepath.Join(root, "Rip", "rip.flac")
	touch(t, rip)
	windows := make([][2]int64, 120)
	for i := range windows {
		windows[i] = [2]int64{int64(i) * 300, int64(i+1) * 300}
	}
	if _, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, rip, "sha256:WR", "sha256:WC", 360000, windows)); err != nil {
		t.Fatalf("put rip: %v", err)
	}
	line := strings.ReplaceAll(filepath.ToSlash(strings.TrimPrefix(rip, filepath.VolumeName(rip))), "/", `\`)
	doc := "#EXTINF:-1,Rip Artist - Track 1\n" + line + "\n#EXTINF:-1,Rip Artist - Track 120\n" + line + "\n"
	matches, err := playlist.New(st).ResolveM3U8(ctx, strings.NewReader(doc))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var got []string
	for _, m := range matches {
		title := "<none>"
		if m.Item != nil {
			title = m.Item.Title
		}
		got = append(got, title)
	}
	if want := []string{"Track 1", "Track 120"}; !slices.Equal(got, want) {
		t.Errorf("%q resolved to %v, want %v", line, got, want)
	}
}

// TestM3U8ImportListsABookOnce: another player lists a book by its part files, each of
// which names the book, and the import adds the book once.
func TestM3U8ImportListsABookOnce(t *testing.T) {
	t.Parallel()
	st, lib, root := openCopyStore(t)
	ctx := context.Background()
	var parts []string
	for i, name := range []string{"p1.m4b", "p2.m4b", "p3.m4b"} {
		p := filepath.Join(root, "Book", name)
		touch(t, p)
		in := bookIn(lib.ID, p, "sha256:LB"+name, "Long Book", "Author X", "", "", "")
		in.Position = i
		if _, err := st.PutScannedBook(ctx, in); err != nil {
			t.Fatalf("put part %s: %v", name, err)
		}
		parts = append(parts, p)
	}
	doc := strings.Join(parts, "\n") + "\n"
	res, err := playlist.New(st).ImportM3U8(ctx, "Book", "", "", "", strings.NewReader(doc))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	items, err := st.PlaylistItems(ctx, res.PlaylistPID, "")
	if err != nil || len(items) != 1 || items[0].Title != "Long Book" {
		t.Fatalf("playlist = %d items (err %v), want the book once", len(items), err)
	}
	if res.Matched != 1 || res.Merged != 2 || res.Unmatched != 0 {
		t.Errorf("result = %+v, want 1 matched and 2 merged", res)
	}
}

// TestItemsByPlaylistPathsMatchesWhatTheLikeDid: on fixtures naming at most 100 items a
// path, the name-seeded lookup answers every path as the LIKE over every file did, in a
// document that names each path in several spellings.
func TestItemsByPlaylistPathsMatchesWhatTheLikeDid(t *testing.T) {
	t.Parallel()
	st, lib, root := openCopyStore(t)
	ctx := context.Background()
	copied, rip, parts := everyEdgeFixture(t, st, lib, root)
	for _, rel := range []string{"x/dup.flac", "y/dup.flac", "y/DUP.flac", "deep/x/dup.flac", "Café/é.flac"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		touch(t, p)
		in := input(lib.ID, p, "sha256:E"+rel, "sha256:C"+rel, "Title "+rel)
		mustPut(t, st, in)
	}
	lines := []string{
		copied, "B/copy.flac", "b/COPY.FLAC", "./B/copy.flac", "B/./copy.flac", "x/../B/copy.flac", "copy.flac",
		rip, "Rip/rip.flac", "rip.flac", parts[1], "Book/p2.m4b", "p2.m4b", "ook/p2.m4b",
		"dup.flac", "x/dup.flac", "X/DUP.FLAC", "deep/x/dup.flac", "Café/é.flac", "café/é.flac", "CAFÉ/é.flac",
		filepath.Join(root, "nothing.flac"), "nothing.flac", "", ".", "..", "../dup.flac",
		filepath.ToSlash(strings.TrimPrefix(copied, filepath.VolumeName(copied))),
	}
	got, err := st.ItemsByPlaylistPaths(ctx, lines, 1000)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	for _, line := range lines {
		want, err := st.ItemsByPlaylistPathLike(ctx, line)
		if err != nil {
			t.Fatalf("reference %q: %v", line, err)
		}
		if g := got[line]; g.Overflow || !slices.Equal(viewPIDs(g.Items), viewPIDs(want)) {
			t.Errorf("%q = %v (overflow %v), the LIKE named %v", line, viewPIDs(g.Items), g.Overflow, viewPIDs(want))
		}
	}
}

// TestItemsByPlaylistPathsSeedsOnBothSeparators: a file name holding a backslash, which
// the name column cuts at as it cuts at a slash, is still found by a relative path naming
// it, since the seed is cut the same way.
func TestItemsByPlaylistPathsSeedsOnBothSeparators(t *testing.T) {
	t.Parallel()
	if filepath.Separator == '\\' {
		t.Skip("a backslash is a separator here, so no file name holds one")
	}
	st, lib, root := openCopyStore(t)
	p := filepath.Join(root, "x", `a\b.flac`)
	touch(t, p)
	mustPut(t, st, input(lib.ID, p, "sha256:EB", "sha256:CB", "Slashed"))
	for _, line := range []string{`x/a\b.flac`, `a\b.flac`, p} {
		if got := pathTitles(t, st, line); !slices.Equal(got, []string{"Slashed"}) {
			t.Errorf("%q = %v, want Slashed", line, got)
		}
	}
	if got := pathTitles(t, st, "b.flac"); got != nil {
		t.Errorf("the name past the backslash = %v, want nothing (the LIKE wants it whole)", got)
	}
}

func viewPIDs(items []*model.ItemView) []model.PID {
	var out []model.PID
	for _, it := range items {
		out = append(out, it.PID)
	}
	return out
}

// TestItemsByPlaylistPathsReadsAWindowsLine: a relative line written on Windows, its
// folders split by backslashes, names the file it would there when it names nothing as
// written, while a file whose own name holds a backslash is still found by its name.
func TestItemsByPlaylistPathsReadsAWindowsLine(t *testing.T) {
	t.Parallel()
	if filepath.Separator == '\\' {
		t.Skip("a backslash is already a separator here")
	}
	st, lib, root := openCopyStore(t)
	p := filepath.Join(root, "Artist", "Album", "01.flac")
	touch(t, p)
	mustPut(t, st, input(lib.ID, p, "sha256:EW", "sha256:CW", "Windows Line"))
	for _, line := range []string{`Artist\Album\01.flac`, `Album\01.flac`, `.\Album\01.flac`} {
		if got := pathTitles(t, st, line); !slices.Equal(got, []string{"Windows Line"}) {
			t.Errorf("%q = %v, want Windows Line", line, got)
		}
	}
	if got := pathTitles(t, st, `Other\01.flac`); got != nil {
		t.Errorf("a line naming another folder = %v, want nothing", got)
	}
}

// TestItemsByPlaylistPathsReadsALineWithNoVolume: a line naming a file from the root of
// its volume, but not the volume, names that file and not a deeper one ending the same
// way, on every platform: on POSIX the line is the absolute path, and Windows reads it as
// that path below a drive's root.
func TestItemsByPlaylistPathsReadsALineWithNoVolume(t *testing.T) {
	t.Parallel()
	st, lib, root := openCopyStore(t)
	p := filepath.Join(root, "Artist", "Album", "01.flac")
	line := filepath.ToSlash(strings.TrimPrefix(p, filepath.VolumeName(p)))
	deeper := filepath.Join(root, "Nested", filepath.FromSlash(line))
	for i, put := range []struct{ path, title string }{{p, "No Volume"}, {deeper, "Deeper"}} {
		touch(t, put.path)
		mustPut(t, st, input(lib.ID, put.path, fmt.Sprintf("sha256:EV%d", i), fmt.Sprintf("sha256:CV%d", i), put.title))
	}
	if got := pathTitles(t, st, line); !slices.Equal(got, []string{"No Volume"}) {
		t.Errorf("%q = %v, want No Volume alone", line, got)
	}
}
