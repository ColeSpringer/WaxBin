package inbox

import (
	"errors"
	"io/fs"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/diskfree"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// fakeVolumes answers the preflight from a table: the device each folder that is there
// is on, the pool each device draws on (a device with none here has no pool to read), and
// the free bytes under each folder, or the bytes each successive read finds (the last
// repeating).
type fakeVolumes struct {
	volume map[string]string
	pool   map[string]string
	peers  map[string][]string // by pool
	whole  map[string]int64    // the room each pool itself reports, by pool
	free   map[string]int64
	rooms  map[string][]int64
	err    error
	probed []string
	pooled []string
	peered []string
	asked  []string // the folders the pool itself was asked for its room by
}

func (f *fakeVolumes) probes() volumes {
	reads := map[string]int{}
	return volumes{
		free: func(dir string) (int64, error) {
			f.probed = append(f.probed, dir)
			if seq, ok := f.rooms[dir]; ok {
				n := min(reads[dir], len(seq)-1)
				reads[dir]++
				return seq[n], f.err
			}
			return f.free[dir], f.err
		},
		of: func(dir string) (string, bool) {
			id, ok := f.volume[dir]
			return id, ok
		},
		pool: func(dir string) (string, bool) {
			f.pooled = append(f.pooled, dir)
			id, ok := f.pool[f.volume[dir]]
			return id, ok
		},
		peers: func(dir string) []string {
			f.peered = append(f.peered, dir)
			return f.peers[f.pool[f.volume[dir]]]
		},
		room: func(dir string) (int64, bool) {
			f.asked = append(f.asked, dir)
			n, ok := f.whole[f.pool[f.volume[dir]]]
			return n, ok
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

// TestPreflightPlanCountsPools: devices that draw on one pool fill each other's room, and
// a member held under a limit of its own answers for what lands on it as well.
func TestPreflightPlanCountsPools(t *testing.T) {
	t.Parallel()
	p := filepath.FromSlash
	a := &model.Library{ID: 1, Root: []byte(p("/tank/a"))}
	b := &model.Library{ID: 2, Root: []byte(p("/tank/b"))}
	held := &model.Library{ID: 3, Root: []byte(p("/tank/held"))}
	c := &model.Library{ID: 4, Root: []byte(p("/pond/c"))}
	// Datasets a, b, held and inbox are devices of one pool, tank; c is on another pool,
	// and the staging folder is a disk that shares nothing.
	held2 := &model.Library{ID: 5, Root: []byte(p("/tank/held2"))}
	volume := map[string]string{
		p("/tank/a"): "da", p("/tank/b"): "db", p("/tank/held"): "dh", p("/tank/held2"): "dh2",
		p("/tank/inbox"): "di", p("/tank"): "dt", p("/pond/c"): "dc", p("/staging"): "ds",
	}
	pool := map[string]string{"da": "tank", "db": "tank", "dh": "tank", "dh2": "tank", "di": "tank", "dt": "tank", "dc": "pond"}
	to := func(lib *model.Library, src, dst string, size int64) Action {
		return Action{Src: p(src), Dst: p(dst), Size: size, Library: lib, Outcome: OutcomeImport}
	}

	tests := []struct {
		name    string
		plan    Plan
		pool    map[string]string
		peers   map[string][]string
		whole   map[string]int64
		free    map[string]int64
		rooms   map[string][]int64
		refused string
		probed  []string
	}{
		{
			name: "two roots on one pool are refused together",
			plan: Plan{Library: a, Actions: []Action{to(a, "/staging/x.flac", "/tank/a/x.flac", 60), to(b, "/staging/y.flac", "/tank/b/y.flac", 60)}},
			free: map[string]int64{p("/tank/a"): 100, p("/tank/b"): 100}, refused: p("/tank/a") + " and " + p("/tank/b") + " (one pool) for import (need 120 bytes, have 100)",
		},
		{
			name: "two roots on two pools answer each for its own",
			plan: Plan{Library: a, Actions: []Action{to(a, "/staging/x.flac", "/tank/a/x.flac", 60), to(c, "/staging/y.flac", "/pond/c/y.flac", 60)}},
			free: map[string]int64{p("/tank/a"): 100, p("/pond/c"): 100}, probed: []string{p("/pond/c"), p("/tank/a")},
		},
		{
			name: "a move inside a pool across devices needs room for its largest file",
			plan: Plan{Library: a, Actions: []Action{to(a, "/tank/inbox/x.flac", "/tank/a/x.flac", 500), to(a, "/tank/inbox/y.flac", "/tank/a/y.flac", 300)}},
			free: map[string]int64{p("/tank/a"): 500, p("/tank/inbox"): 500}, probed: []string{p("/tank/a"), p("/tank/inbox")},
		},
		{
			name: "a move inside a pool is refused when its largest file does not fit",
			plan: Plan{Library: a, Actions: []Action{to(a, "/tank/inbox/x.flac", "/tank/a/x.flac", 500), to(a, "/tank/inbox/y.flac", "/tank/a/y.flac", 300)}},
			free: map[string]int64{p("/tank/a"): 499, p("/tank/inbox"): 499}, refused: "need 500 bytes, have 499",
		},
		{
			name: "bytes from outside the pool add to the largest move inside it",
			plan: Plan{Library: a, Actions: []Action{to(a, "/tank/inbox/x.flac", "/tank/a/x.flac", 500), to(b, "/staging/y.flac", "/tank/b/y.flac", 60)}},
			free: map[string]int64{p("/tank/a"): 559, p("/tank/b"): 559, p("/tank/inbox"): 559}, refused: "need 560 bytes, have 559",
		},
		{
			name: "a copy inside a pool needs every byte",
			plan: Plan{Library: a, Copy: true, Actions: []Action{to(a, "/tank/inbox/x.flac", "/tank/a/x.flac", 500), to(a, "/tank/inbox/y.flac", "/tank/a/y.flac", 300)}},
			free: map[string]int64{p("/tank/a"): 799, p("/tank/inbox"): 799}, refused: "need 800 bytes, have 799",
		},
		{
			name: "a destination with less room than its pool answers for all that lands on it",
			plan: Plan{Library: held, Actions: []Action{to(held, "/tank/inbox/x.flac", "/tank/held/x.flac", 150)}},
			free: map[string]int64{p("/tank/held"): 100, p("/tank/inbox"): 1000}, refused: p("/tank/held") + " for import (need 150 bytes, have 100)",
		},
		{
			name: "a destination with less room than its pool takes what fits it",
			plan: Plan{Library: held, Actions: []Action{to(held, "/tank/inbox/x.flac", "/tank/held/x.flac", 100)}},
			free: map[string]int64{p("/tank/held"): 100, p("/tank/inbox"): 1000}, probed: []string{p("/tank/held"), p("/tank/inbox")},
		},
		{
			name:   "a shortfall one read shows and the next does not is no limit",
			plan:   Plan{Library: b, Actions: []Action{to(b, "/tank/inbox/x.flac", "/tank/b/x.flac", 150)}},
			rooms:  map[string][]int64{p("/tank/b"): {50, 1000}, p("/tank/inbox"): {1000}},
			probed: []string{p("/tank/b"), p("/tank/inbox"), p("/tank/b"), p("/tank/inbox")},
		},
		{
			name:    "a shortfall two reads show refuses",
			plan:    Plan{Library: b, Actions: []Action{to(b, "/tank/inbox/x.flac", "/tank/b/x.flac", 150)}},
			rooms:   map[string][]int64{p("/tank/b"): {50, 50}, p("/tank/inbox"): {1000}},
			refused: p("/tank/b") + " for import (need 150 bytes, have 50)",
		},
		{
			// The first read shows held short under its quota; by the second the pool has
			// filled, and held, with the most room left, stands for the pool.
			name:    "a shortfall that changes kind between the reads still refuses",
			plan:    Plan{Library: held, Actions: []Action{to(held, "/staging/x.flac", "/tank/held/x.flac", 100), to(a, "/tank/inbox/y.flac", "/tank/a/y.flac", 1)}},
			rooms:   map[string][]int64{p("/tank/held"): {50, 50}, p("/tank/inbox"): {1000, 40}, p("/tank/a"): {1000, 40}},
			refused: p("/tank/held") + " for import (need 100 bytes, have 50)",
		},
		{
			// /tank is the pool's top dataset, under no quota, mounted where the import can
			// read it.
			name:   "every destination under its own quota is weighed against the pool's top",
			plan:   Plan{Library: held, Actions: []Action{to(held, "/staging/x.flac", "/tank/held/x.flac", 80), to(held2, "/staging/y.flac", "/tank/held2/y.flac", 80)}},
			peers:  map[string][]string{"tank": {p("/tank")}},
			free:   map[string]int64{p("/tank/held"): 100, p("/tank/held2"): 100, p("/tank"): 1000},
			probed: []string{p("/tank/held"), p("/tank/held2"), p("/tank")},
		},
		{
			// Under quotas that happen to have as much left, they read as the pool's room
			// would; the pool itself says otherwise.
			name:   "destinations reporting alike are weighed against the pool itself",
			plan:   Plan{Library: held, Actions: []Action{to(held, "/staging/x.flac", "/tank/held/x.flac", 80), to(held2, "/staging/y.flac", "/tank/held2/y.flac", 80)}},
			whole:  map[string]int64{"tank": 1000},
			free:   map[string]int64{p("/tank/held"): 100, p("/tank/held2"): 100},
			probed: []string{p("/tank/held"), p("/tank/held2")},
		},
		{
			// One under no quota and one under a quota, which the pool itself shows filling.
			name:    "destinations disagreeing are weighed against the pool itself",
			plan:    Plan{Library: a, Actions: []Action{to(a, "/staging/x.flac", "/tank/a/x.flac", 80), to(held, "/staging/y.flac", "/tank/held/y.flac", 80)}},
			whole:   map[string]int64{"tank": 150},
			free:    map[string]int64{p("/tank/a"): 150, p("/tank/held"): 100},
			refused: "need 160 bytes, have 150",
		},
		{
			// Alike, they read as two members reporting the pool's room.
			name: "destinations reporting alike with nothing else to read stand for the pool",
			plan: Plan{Library: held, Actions: []Action{to(held, "/staging/x.flac", "/tank/held/x.flac", 80), to(held2, "/staging/y.flac", "/tank/held2/y.flac", 80)}},
			free: map[string]int64{p("/tank/held"): 100, p("/tank/held2"): 100}, refused: "need 160 bytes, have 100",
		},
		{
			// Destinations that disagree are under quotas, the largest no measure of the
			// pool, so each answers for its own bytes alone.
			name:   "destinations disagreeing with nothing else to read answer each for its own",
			plan:   Plan{Library: held, Actions: []Action{to(held, "/staging/x.flac", "/tank/held/x.flac", 80), to(held2, "/staging/y.flac", "/tank/held2/y.flac", 80)}},
			free:   map[string]int64{p("/tank/held"): 100, p("/tank/held2"): 90},
			probed: []string{p("/tank/held"), p("/tank/held2")},
		},
		{
			name: "a destination disagreeing with the rest is refused for its own bytes",
			plan: Plan{Library: held, Actions: []Action{to(held, "/staging/x.flac", "/tank/held/x.flac", 80), to(held2, "/staging/y.flac", "/tank/held2/y.flac", 80)}},
			free: map[string]int64{p("/tank/held"): 100, p("/tank/held2"): 70}, refused: p("/tank/held2") + " for import (need 80 bytes, have 70)",
		},
		{
			name: "a pool that cannot be read is its device",
			plan: Plan{Library: a, Actions: []Action{to(a, "/tank/inbox/x.flac", "/tank/a/x.flac", 500), to(a, "/tank/inbox/y.flac", "/tank/a/y.flac", 300)}},
			pool: map[string]string{},
			free: map[string]int64{p("/tank/a"): 799, p("/tank/inbox"): 799}, refused: "need 800 bytes, have 799",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeVolumes{volume: volume, pool: pool, peers: tc.peers, whole: tc.whole, free: tc.free, rooms: tc.rooms}
			if tc.pool != nil {
				f.pool = tc.pool
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
		})
	}
}

// TestPreflightPlanAsksThePoolOnlyWhenItsMembersCannotSay: the pool's own report of its
// room costs a tool run, so it is asked for only where two destinations share the pool and
// nothing read besides them shows at least the room they do.
func TestPreflightPlanAsksThePoolOnlyWhenItsMembersCannotSay(t *testing.T) {
	t.Parallel()
	p := filepath.FromSlash
	a := &model.Library{ID: 1, Root: []byte(p("/tank/a"))}
	b := &model.Library{ID: 2, Root: []byte(p("/tank/b"))}
	volume := map[string]string{p("/tank/a"): "da", p("/tank/b"): "db", p("/tank/inbox"): "di", p("/staging"): "ds"}
	pool := map[string]string{"da": "tank", "db": "tank", "di": "tank"}
	to := func(lib *model.Library, src, dst string) Action {
		return Action{Src: p(src), Dst: p(dst), Size: 10, Library: lib, Outcome: OutcomeImport}
	}
	tests := []struct {
		name  string
		plan  Plan
		free  map[string]int64
		asked int
	}{
		{"one destination", Plan{Library: a, Actions: []Action{to(a, "/staging/x.flac", "/tank/a/x.flac")}},
			map[string]int64{p("/tank/a"): 100}, 0},
		{"two destinations and a source showing as much room", Plan{Library: a, Actions: []Action{to(a, "/tank/inbox/x.flac", "/tank/a/x.flac"), to(b, "/staging/y.flac", "/tank/b/y.flac")}},
			map[string]int64{p("/tank/a"): 100, p("/tank/b"): 90, p("/tank/inbox"): 100}, 0},
		{"two destinations and nothing else", Plan{Library: a, Actions: []Action{to(a, "/staging/x.flac", "/tank/a/x.flac"), to(b, "/staging/y.flac", "/tank/b/y.flac")}},
			map[string]int64{p("/tank/a"): 100, p("/tank/b"): 90}, 1},
	}
	for _, tc := range tests {
		f := &fakeVolumes{volume: volume, pool: pool, whole: map[string]int64{"tank": 1000}, free: tc.free}
		if err := preflightPlan(&tc.plan, f.probes()); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(f.asked) != tc.asked {
			t.Errorf("%s: the pool was asked %d times (%q), want %d", tc.name, len(f.asked), f.asked, tc.asked)
		}
	}
}

// TestPreflightPlanAsksEachDevicesPoolOnce: the pool a device draws on is read once,
// however many files go to it or come from it.
func TestPreflightPlanAsksEachDevicesPoolOnce(t *testing.T) {
	t.Parallel()
	p := filepath.FromSlash
	a := &model.Library{ID: 1, Root: []byte(p("/tank/a"))}
	b := &model.Library{ID: 2, Root: []byte(p("/tank/b"))}
	f := &fakeVolumes{
		volume: map[string]string{p("/tank/a"): "da", p("/tank/b"): "db", p("/tank/inbox"): "di", p("/staging"): "ds"},
		pool:   map[string]string{"da": "tank", "db": "tank", "di": "tank"},
		free:   map[string]int64{p("/tank/a"): 1 << 30, p("/tank/b"): 1 << 30, p("/tank/inbox"): 1 << 30},
	}
	var actions []Action
	for i, lib := range []*model.Library{a, a, b, a, b} {
		src := "/tank/inbox/"
		if i == 3 {
			src = "/staging/"
		}
		name := string(rune('v'+i)) + ".flac"
		actions = append(actions, Action{Src: p(src + name), Dst: filepath.Join(string(lib.Root), name), Size: 10, Library: lib, Outcome: OutcomeImport})
	}
	if err := preflightPlan(&Plan{Library: a, Actions: actions}, f.probes()); err != nil {
		t.Fatal(err)
	}
	asked := map[string]int{}
	for _, dir := range f.pooled {
		asked[f.volume[dir]]++
	}
	if want := map[string]int{"da": 1, "db": 1, "di": 1, "ds": 1}; !maps.Equal(asked, want) {
		t.Errorf("pools asked by device %v (folders %q), want each device once", asked, f.pooled)
	}
	if len(f.peered) != 1 {
		t.Errorf("peers asked for %q, want once for the one pool", f.peered)
	}
}
