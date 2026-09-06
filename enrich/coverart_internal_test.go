package enrich

import "testing"

// TestReleaseMBIDFromURL pins the one shape a release id is read from. The trap it
// guards is the request path: it carries the group's own UUID, so a fetch that ended
// where it started must yield nothing rather than record the group as a release.
func TestReleaseMBIDFromURL(t *testing.T) {
	const (
		group   = "b0000000-0000-4000-8000-000000000002"
		release = "e0000000-0000-4000-8000-00000000000a"
		req     = "https://coverartarchive.org/release-group/" + group + "/front"
	)
	cases := []struct {
		name       string
		req, final string
		want       string
	}{
		{"archive download item", req,
			"https://archive.org/download/mbid-" + release + "/mbid-" + release + "-123.jpg", release},
		{"mirror item path", req,
			"https://ia800.us.archive.org/0/items/mbid-" + release + "/mbid-" + release + "-123.jpg", release},
		{"uppercase folds", req,
			"https://archive.org/download/MBID-E0000000-0000-4000-8000-00000000000A/x.jpg", release},
		{"no redirect", req, req, ""},
		{"empty final", req, "", ""},
		{"bare uuid segment", req,
			"https://coverartarchive.org/release/" + release + "/123.jpg", ""},
		{"no uuid at all", req, "https://archive.org/download/something/else.jpg", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := releaseMBIDFromURL(tc.req, tc.final); got != tc.want {
				t.Errorf("releaseMBIDFromURL(%q) = %q, want %q", tc.final, got, tc.want)
			}
		})
	}
}
