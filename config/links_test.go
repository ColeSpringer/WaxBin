package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testpath"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestOverlapThroughLinks: Validate's overlap rules applied to the folders the paths
// reach: two roots reaching one folder, a root inside the folder another reaches, an
// inbox or the podcast dir reaching a root; a candidate is judged against the rest only,
// so two registered roots that already overlap through a link do not refuse a third.
func TestOverlapThroughLinks(t *testing.T) {
	t.Parallel()
	p := func(parts ...string) string {
		return testpath.Vol() + filepath.Join(append([]string{string(filepath.Separator)}, parts...)...)
	}
	cfg := func(roots []string, inbox []string, pod string) *config.Config {
		c := &config.Config{DBPath: p("c.db"), Inbox: inbox, Podcasts: config.PodcastConfig{Dir: pod}}
		for _, r := range roots {
			c.Roots = append(c.Roots, config.Root{Path: r, Mode: model.ModeManaged})
		}
		return c
	}
	for _, tc := range []struct {
		name      string
		cfg       *config.Config
		resolved  map[string]string
		candidate string
		want      string // "" for no error, else text the error must carry
	}{
		{"two roots reaching one folder", cfg([]string{p("music"), p("srv", "music")}, nil, ""),
			map[string]string{p("srv", "music"): p("music")}, "", "library roots overlap"},
		{"a root reaching a folder inside another", cfg([]string{p("music"), p("link")}, nil, ""),
			map[string]string{p("link"): p("music", "rock")}, "", "library roots overlap"},
		{"a root reaching a folder above another", cfg([]string{p("music", "rock"), p("link")}, nil, ""),
			map[string]string{p("link"): p("music")}, "", "library roots overlap"},
		{"an inbox reaching a root", cfg([]string{p("music")}, []string{p("in")}, ""),
			map[string]string{p("in"): p("music", "drop")}, "", "inbox"},
		{"the podcast dir reaching a root", cfg([]string{p("music")}, nil, p("pod")),
			map[string]string{p("pod"): p("music", "casts")}, "", "podcast dir"},
		{"the podcast dir reaching an inbox", cfg(nil, []string{p("in")}, p("pod")),
			map[string]string{p("pod"): p("in")}, "", "podcast dir"},
		{"the same folder spelled as resolved", cfg([]string{p("music"), p("books")}, []string{p("in")}, p("pod")),
			map[string]string{p("music"): p("music"), p("books"): p("books"), p("in"): p("in"), p("pod"): p("pod")}, "", ""},
		{"a candidate beside roots that already overlap", cfg([]string{p("music"), p("srv", "music"), p("books")}, nil, ""),
			map[string]string{p("srv", "music"): p("music")}, p("books"), ""},
		{"a candidate that overlaps", cfg([]string{p("music"), p("books"), p("srv", "music")}, nil, ""),
			map[string]string{p("srv", "music"): p("music")}, p("srv", "music"), "library roots overlap"},
		{"a candidate an inbox reaches", cfg([]string{p("music"), p("drop")}, []string{p("in")}, ""),
			map[string]string{p("in"): p("drop")}, p("drop"), "inbox"},
		{"nothing resolved", cfg([]string{p("music"), p("srv", "music")}, []string{p("in")}, p("pod")), nil, "", ""},
	} {
		err := tc.cfg.OverlapThroughLinks(tc.resolved, tc.candidate)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v, want no error", tc.name, err)
		case tc.want != "" && (!waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want CodeInvalid saying %q", tc.name, err, tc.want)
		}
	}
}
