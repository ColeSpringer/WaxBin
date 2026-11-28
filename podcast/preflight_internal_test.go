package podcast

import (
	"errors"
	"io/fs"
	"math"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/internal/diskfree"
	"github.com/colespringer/waxbin/waxerr"
)

// TestPreflightSpace: a download needs the size its feed declares plus the reserve, an
// undeclared or absurd size and a missing probe included.
func TestPreflightSpace(t *testing.T) {
	t.Parallel()
	gone := &fs.PathError{Op: "statfs", Path: "/podcasts", Err: fs.ErrNotExist}
	tests := []struct {
		name          string
		reserve, size int64
		free          int64
		err           error
		refused       bool
		probed        bool
	}{
		{name: "room for the enclosure and the reserve", reserve: 100, size: 400, free: 500, probed: true},
		{name: "short by one byte", reserve: 100, size: 400, free: 499, refused: true, probed: true},
		{name: "an undeclared size checks the reserve", reserve: 100, size: 0, free: 99, refused: true, probed: true},
		{name: "a negative size checks the reserve", reserve: 100, size: -5, free: 100, probed: true},
		{name: "no reserve and no size asks nothing", reserve: 0, size: 0},
		{name: "a negative reserve takes nothing off the size", reserve: -300, size: 400, free: 399, refused: true, probed: true},
		{name: "a size past any disk is refused", reserve: 100, size: math.MaxInt64, free: 1 << 50, refused: true, probed: true},
		{name: "no probe on this platform", reserve: 100, size: 400, err: diskfree.ErrUnsupported, probed: true},
		{name: "a folder that cannot be probed", reserve: 100, size: 400, err: gone, refused: true, probed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probed := false
			svc := &Service{cfg: Config{Dir: "/podcasts", ReserveBytes: tc.reserve}, freeSpace: func(dir string) (int64, error) {
				probed = true
				return tc.free, tc.err
			}}
			err := svc.preflightSpace("/podcasts/Show", tc.size)
			if (err != nil) != tc.refused {
				t.Fatalf("err = %v, want refused %v", err, tc.refused)
			}
			if probed != tc.probed {
				t.Errorf("probed = %v, want %v", probed, tc.probed)
			}
			if err == nil {
				return
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

// TestPreflightSpaceMeasuresTheShowFolder: a show can sit on a disk of its own, so its
// folder is the one measured. Where a prune took the folder before the fetch makes it
// again, the download folder answers.
func TestPreflightSpaceMeasuresTheShowFolder(t *testing.T) {
	t.Parallel()
	const dir, show = "/podcasts", "/podcasts/Show"
	gone := &fs.PathError{Op: "statfs", Path: show, Err: fs.ErrNotExist}
	tests := []struct {
		name    string
		free    map[string]int64 // a folder not listed is not there
		refused bool
		probed  []string
	}{
		{name: "the show's own disk has room", free: map[string]int64{show: 500, dir: 0}, probed: []string{show}},
		{name: "the show's own disk is short", free: map[string]int64{show: 499, dir: 1 << 40}, refused: true, probed: []string{show}},
		{name: "a pruned show folder leaves the download folder to answer", free: map[string]int64{dir: 500}, probed: []string{show, dir}},
		{name: "a pruned show folder on a short disk", free: map[string]int64{dir: 499}, refused: true, probed: []string{show, dir}},
		{name: "neither folder is there", free: map[string]int64{}, refused: true, probed: []string{show, dir}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var probed []string
			svc := &Service{cfg: Config{Dir: dir, ReserveBytes: 100}, freeSpace: func(folder string) (int64, error) {
				probed = append(probed, folder)
				free, ok := tc.free[folder]
				if !ok {
					return 0, gone
				}
				return free, nil
			}}
			err := svc.preflightSpace(show, 400)
			if (err != nil) != tc.refused {
				t.Fatalf("err = %v, want refused %v", err, tc.refused)
			}
			if !slices.Equal(probed, tc.probed) {
				t.Errorf("probed %q, want %q", probed, tc.probed)
			}
		})
	}
}
