package sqlite_test

import (
	"bytes"
	"context"
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
	items, err := st.ItemsByPlaylistPath(context.Background(), p)
	if err != nil {
		t.Fatalf("items by %s: %v", p, err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Title)
	}
	slices.Sort(out)
	return out
}

// TestItemsByPlaylistPathReadsEveryEdge: an M3U8 path names the items behind its file
// through any edge, so a copy's path names its item, a later book part's path its book,
// and a cue rip's file every track carved from it.
func TestItemsByPlaylistPathReadsEveryEdge(t *testing.T) {
	t.Parallel()
	st, lib, root := openCopyStore(t)
	ctx := context.Background()
	original, copied := filepath.Join(root, "A", "song.flac"), filepath.Join(root, "B", "copy.flac")
	putCopyPair(t, st, lib, original, copied)

	rip := filepath.Join(root, "Rip", "rip.flac")
	touch(t, rip)
	if _, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, rip, "sha256:RE", "sha256:RC", 8000,
		[][2]int64{{0, 300}, {300, 600}, {600, 900}})); err != nil {
		t.Fatalf("put rip: %v", err)
	}
	var parts []string
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
	pl, err := svc.CreateStatic(ctx, "Rip mix", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Set(ctx, pl, []model.PID{byTitle["Track 2"], byTitle["Track 3"], byTitle["Track 2"]}); err != nil {
		t.Fatalf("set: %v", err)
	}
	var doc bytes.Buffer
	if err := svc.ExportM3U8(ctx, pl, &doc, ""); err != nil {
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
	res, err := svc.ImportM3U8(ctx, "Again", "", "", bytes.NewReader(doc.Bytes()))
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
	res, err := playlist.New(st).ImportM3U8(ctx, "Book", "", "", strings.NewReader(doc))
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
