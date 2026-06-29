// Package organize lays out managed files. A path-template engine selects
// destinations, and a plan-then-execute mover applies them while recording each
// move in organize_journal through model.Catalog. Selection comes from the
// shared query engine.
package organize

import (
	"sort"

	"github.com/colespringer/waxbin/waxerr"
)

// Profile is a named layout with a track path template.
type Profile struct {
	Name          string
	TrackTemplate string
}

// builtins are the profiles available in this build.
var builtins = map[string]Profile{
	"waxbin-native": {
		Name:          "waxbin-native",
		TrackTemplate: "{albumartist}/{album}/{track:02} - {title}.{ext}",
	},
	"plex-music": {
		Name:          "plex-music",
		TrackTemplate: "{albumartist}/{album} ({year})/{track:02} - {title}.{ext}",
	},
}

// ProfileByName returns a built-in profile.
func ProfileByName(name string) (Profile, error) {
	p, ok := builtins[name]
	if !ok {
		return Profile{}, waxerr.New(waxerr.CodeNotFound, "organize.ProfileByName",
			"no such organization profile: "+name)
	}
	return p, nil
}

// Profiles lists the built-in profile names, sorted.
func Profiles() []string {
	names := make([]string, 0, len(builtins))
	for n := range builtins {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
