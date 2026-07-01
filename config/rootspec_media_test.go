package config

import (
	"testing"

	"github.com/colespringer/waxbin/model"
)

func TestParseRootSpecMedia(t *testing.T) {
	cases := []struct {
		spec    string
		path    string
		mode    model.Mode
		media   model.MediaType
		profile string
	}{
		{"/music", "/music", "", "", ""},
		{"/music:managed", "/music", model.ModeManaged, "", ""},
		{"/music:managed:waxbin-native", "/music", model.ModeManaged, "", "waxbin-native"},
		{"/music:managed:music", "/music", model.ModeManaged, model.MediaMusic, ""},
		{"/books:managed:audiobook", "/books", model.ModeManaged, model.MediaAudiobook, ""},
		{"/books:managed:audiobook:custom", "/books", model.ModeManaged, model.MediaAudiobook, "custom"},
		{"/x:in-place:mixed", "/x", model.ModeInPlace, model.MediaMixed, ""},
		// A Windows drive path with no recognized suffix keeps the whole spec as the path.
		{`C:\Music`, `C:\Music`, "", "", ""},
	}
	for _, c := range cases {
		r, err := ParseRootSpec(c.spec)
		if err != nil {
			t.Fatalf("%q: %v", c.spec, err)
		}
		if r.Path != c.path || r.Mode != c.mode || r.Media != c.media || r.Profile != c.profile {
			t.Errorf("%q -> %+v, want path=%q mode=%q media=%q profile=%q",
				c.spec, r, c.path, c.mode, c.media, c.profile)
		}
	}
}

func TestValidateMediaDefaultAndReject(t *testing.T) {
	cfg := &Config{DBPath: "/db", Roots: []Root{{Path: "/m", Mode: model.ModeManaged}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Roots[0].Media != model.MediaMixed {
		t.Errorf("media default = %q, want mixed", cfg.Roots[0].Media)
	}

	bad := &Config{DBPath: "/db", Roots: []Root{{Path: "/m", Mode: model.ModeManaged, Media: "vinyl"}}}
	if err := bad.Validate(); err == nil {
		t.Error("expected an error for an invalid media type")
	}
}
