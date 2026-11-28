package inbox

import (
	"errors"
	"io/fs"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/diskfree"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// fakeVolumes answers the preflight from a table: the volume each folder that is there
// is on, and the free bytes under each folder.
type fakeVolumes struct {
	volume map[string]string
	free   map[string]int64
	err    error
	probed []string
}

func (f *fakeVolumes) probes() volumes {
	return volumes{
		free: func(dir string) (int64, error) {
			f.probed = append(f.probed, dir)
			return f.free[dir], f.err
		},
		of: func(dir string) (string, bool) {
			id, ok := f.volume[dir]
			return id, ok
		},
	}
}

func TestPreflightPlan(t *testing.T) {
	t.Parallel()
	p := filepath.FromSlash
	music := &model.Library{ID: 1, Root: []byte(p("/music"))}
	shelf := &model.Library{ID: 2, Root: []byte(p("/shelf"))}
	books := &model.Library{ID: 3, Root: []byte(p("/books"))}
	// /music and /shelf share a disk, /music/vault is another disk mounted below the
	// root, and /music-inbox is a staging folder on the music disk.
	volume := map[string]string{
		p("/music"): "disk1", p("/shelf"): "disk1", p("/music-inbox"): "disk1", p("/music/Artist"): "disk1",
		p("/music/vault"): "disk4", p("/books"): "disk2", p("/staging"): "disk3",
	}
	// to is an import of size bytes from a staging folder to a path under a library.
	to := func(lib *model.Library, src, dst string, size int64) Action {
		return Action{Src: p(src), Dst: p(dst), Size: size, Library: lib, Outcome: OutcomeImport}
	}

	tests := []struct {
		name    string
		plan    Plan
		volume  map[string]string
		free    map[string]int64
		err     error
		refused string   // a substring of the refusal, empty when the import may run
		probed  []string // the folders probed, when the import may run
	}{
		{
			name: "a move from another volume needs its bytes",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60), to(music, "/staging/b.mp3", "/music/b.mp3", 50)}},
			free: map[string]int64{p("/music"): 109}, refused: "need 110 bytes, have 109",
		},
		{
			name: "a move from another volume fits",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60), to(music, "/staging/b.mp3", "/music/b.mp3", 50)}},
			free: map[string]int64{p("/music"): 110}, probed: []string{p("/music")},
		},
		{
			name: "moves within a volume need room for the largest file alone",
			plan: Plan{Library: music, Actions: []Action{to(music, "/music-inbox/a.mp3", "/music/a.mp3", 500), to(music, "/music-inbox/b.mp3", "/music/b.mp3", 300)}},
			free: map[string]int64{p("/music"): 500}, probed: []string{p("/music")},
		},
		{
			name: "moves within a volume are refused when the largest file does not fit",
			plan: Plan{Library: music, Actions: []Action{to(music, "/music-inbox/a.mp3", "/music/a.mp3", 500), to(music, "/music-inbox/b.mp3", "/music/b.mp3", 300)}},
			free: map[string]int64{p("/music"): 499}, refused: "need 500 bytes, have 499",
		},
		{
			name: "copies within a volume need every byte",
			plan: Plan{Library: music, Copy: true, Actions: []Action{to(music, "/music-inbox/a.mp3", "/music/a.mp3", 500), to(music, "/music-inbox/b.mp3", "/music/b.mp3", 300)}},
			free: map[string]int64{p("/music"): 799}, refused: "need 800 bytes, have 799",
		},
		{
			name: "bytes from elsewhere add to the largest move within",
			plan: Plan{Library: music, Actions: []Action{to(music, "/music-inbox/a.mp3", "/music/a.mp3", 500), to(music, "/staging/b.mp3", "/music/b.mp3", 60)}},
			free: map[string]int64{p("/music"): 559}, refused: "need 560 bytes, have 559",
		},
		{
			name: "the reserve is added to the bytes",
			plan: Plan{Library: music, Reserve: 50, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60)}},
			free: map[string]int64{p("/music"): 109}, refused: "need 110 bytes, have 109",
		},
		{
			name: "a negative reserve takes nothing off the bytes",
			plan: Plan{Library: music, Reserve: -90, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60)}},
			free: map[string]int64{p("/music"): 59}, refused: "need 60 bytes, have 59",
		},
		{
			name: "a reserve past what an int64 holds is not met",
			plan: Plan{Library: music, Reserve: math.MaxInt64, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60)}},
			free: map[string]int64{p("/music"): 1 << 40}, refused: "insufficient free space",
		},
		{
			name: "roots on two volumes answer each for its own bytes",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60), to(books, "/staging/b.m4b", "/books/b.m4b", 900)}},
			free: map[string]int64{p("/music"): 100, p("/books"): 100}, refused: p("/books"),
		},
		{
			name: "roots on one volume answer together",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60), to(shelf, "/staging/b.m4b", "/shelf/b.m4b", 60)}},
			free: map[string]int64{p("/music"): 119, p("/shelf"): 119}, refused: "need 120 bytes, have 119",
		},
		{
			name: "roots on one volume that fit together are probed once",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60), to(shelf, "/staging/b.m4b", "/shelf/b.m4b", 60)}},
			free: map[string]int64{p("/music"): 120, p("/shelf"): 120}, probed: []string{p("/music")},
		},
		{
			name: "a destination folder not there yet lands on the folder above it",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/New/Album/a.mp3", 60)}},
			free: map[string]int64{p("/music"): 59}, refused: p("/music") + " for import",
		},
		{
			name: "a folder on the root's volume is probed at the root",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/Artist/a.mp3", 60)}},
			free: map[string]int64{p("/music"): 60}, probed: []string{p("/music")},
		},
		{
			name: "a volume mounted below the root answers for what lands on it",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/vault/Album/a.mp3", 60), to(music, "/staging/b.mp3", "/music/b.mp3", 60)}},
			free: map[string]int64{p("/music"): 1000, p("/music/vault"): 59}, refused: p("/music/vault") + " for import (need 60 bytes, have 59)",
		},
		{
			name: "a move onto a volume mounted below the root is no move within the root's",
			plan: Plan{Library: music, Actions: []Action{to(music, "/music-inbox/a.mp3", "/music/vault/a.mp3", 500), to(music, "/music-inbox/b.mp3", "/music/vault/b.mp3", 300)}},
			free: map[string]int64{p("/music"): 1000, p("/music/vault"): 799}, refused: "need 800 bytes, have 799",
		},
		{
			name: "an action without a library goes to the plan's",
			plan: Plan{Library: music, Actions: []Action{{Src: p("/staging/a.mp3"), Dst: p("/music/a.mp3"), Size: 60, Outcome: OutcomeImport}}},
			free: map[string]int64{p("/music"): 10}, refused: p("/music"),
		},
		{
			name: "an action without a destination counts against its root",
			plan: Plan{Library: music, Actions: []Action{{Src: p("/staging/a.mp3"), Size: 60, Library: music, Outcome: OutcomeImport}}},
			free: map[string]int64{p("/music"): 10}, refused: p("/music"),
		},
		{
			name: "a file not imported needs none",
			plan: Plan{Library: music, Actions: []Action{{Src: p("/staging/a.mp3"), Dst: p("/music/a.mp3"), Size: 60, Library: music, Outcome: OutcomeDuplicate}}},
			free: map[string]int64{p("/music"): 10},
		},
		{
			name: "an empty import still keeps the reserve",
			plan: Plan{Library: music, Reserve: 200},
			free: map[string]int64{p("/music"): 199}, refused: "need 200 bytes, have 199",
		},
		{
			name:   "with no volume to read each root answers alone for every byte",
			plan:   Plan{Library: music, Actions: []Action{to(music, "/music-inbox/a.mp3", "/music/a.mp3", 60), to(music, "/music-inbox/b.mp3", "/music/New/b.mp3", 50), to(shelf, "/staging/c.m4b", "/shelf/c.m4b", 70)}},
			volume: map[string]string{},
			free:   map[string]int64{p("/music"): 110, p("/shelf"): 70}, probed: []string{p("/music"), p("/shelf")},
		},
		{
			name:   "with no volume to read a root short of every byte refuses",
			plan:   Plan{Library: music, Actions: []Action{to(music, "/music-inbox/a.mp3", "/music/a.mp3", 60), to(music, "/music-inbox/b.mp3", "/music/New/b.mp3", 50)}},
			volume: map[string]string{},
			free:   map[string]int64{p("/music"): 109}, refused: "need 110 bytes, have 109",
		},
		{
			name: "a platform with no probe lets the import run",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60)}},
			err:  diskfree.ErrUnsupported, probed: []string{p("/music")},
		},
		{
			name: "a root that cannot be probed refuses the import",
			plan: Plan{Library: music, Actions: []Action{to(music, "/staging/a.mp3", "/music/a.mp3", 60)}},
			err:  &fs.PathError{Op: "statfs", Path: p("/music"), Err: fs.ErrNotExist}, refused: "statfs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeVolumes{volume: volume, free: tc.free, err: tc.err}
			if tc.volume != nil {
				f.volume = tc.volume
			}
			err := preflightPlan(&tc.plan, f.probes())
			if tc.refused == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if !slices.Equal(f.probed, tc.probed) {
					t.Errorf("probed %q, want %q", f.probed, tc.probed)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("err = %v, want one holding %q", err, tc.refused)
			}
			if waxerr.CodeOf(err) != waxerr.CodeIO {
				t.Errorf("code = %v, want io", waxerr.CodeOf(err))
			}
			if tc.err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("err = %v, want the probe's own error kept", err)
			}
		})
	}
}

// TestPreflightPlanNamesTheFirstShortVolume: with two volumes short, the refusal names
// the same one every time.
func TestPreflightPlanNamesTheFirstShortVolume(t *testing.T) {
	t.Parallel()
	a := &model.Library{ID: 1, Root: []byte(filepath.FromSlash("/a"))}
	b := &model.Library{ID: 2, Root: []byte(filepath.FromSlash("/b"))}
	plan := Plan{Library: a, Copy: true, Actions: []Action{
		{Src: "y", Size: 9, Library: b, Outcome: OutcomeImport},
		{Src: "x", Size: 9, Library: a, Outcome: OutcomeImport},
	}}
	for range 20 {
		f := &fakeVolumes{volume: map[string]string{string(a.Root): "one", string(b.Root): "two"}}
		err := preflightPlan(&plan, f.probes())
		if err == nil || !strings.Contains(err.Error(), string(a.Root)) {
			t.Fatalf("err = %v, want the refusal to name %s", err, a.Root)
		}
	}
}
