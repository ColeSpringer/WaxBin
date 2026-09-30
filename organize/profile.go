// Package organize renders managed-library paths and applies file relocations.
// Profiles choose a template per media kind; execution records each move in the
// organize journal and carries sidecars with the audio.
package organize

import (
	"sort"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Profile is a named library layout: one path template per media type plus an
// optional tag write-back policy.
type Profile struct {
	Name      string
	Music     string // track template
	Audiobook string // book template
	Podcast   string // episode template
	TagWrite  bool   // optional lock-respecting tag write-back (off by default)
}

// templateFor returns the path template for an item's media kind.
func (p Profile) templateFor(kind model.Kind) string {
	switch kind {
	case model.KindBook:
		return p.Audiobook
	case model.KindEpisode:
		return p.Podcast
	default:
		return p.Music
	}
}

// nativeProfile is the default layout. Music uses an album-artist/album/track
// shape compatible with common servers and taggers. Audiobooks include series,
// sequence, narrator, and ASIN when present. Podcast paths stay readable while
// stable episode identity remains in the catalog.
var nativeProfile = Profile{
	Name:      "waxbin-native",
	Music:     `{albumartist}/{album}< ({year})>/<{disc}->{track:02} - {title}.{ext}`,
	Audiobook: `{authorsort}/<{series}/><{seq} - ><{year} - >{title}< - {subtitle}>< \{{narrator}\}>< [{asin}]>/{title}.{ext}`,
	Podcast:   `{podcast}/<{season}/><{pubdate} - >{episode}.{ext}`,
}

// builtins are the profiles every build ships.
var builtins = map[string]Profile{nativeProfile.Name: nativeProfile}

// DefaultProfileName is the profile a managed root uses unless configured
// otherwise.
const DefaultProfileName = "waxbin-native"

// ProfileSet resolves profile names against the built-ins plus any user-defined
// profiles, which override a built-in of the same name. The zero value is not
// usable; build one with NewProfileSet.
type ProfileSet struct {
	byName map[string]Profile
}

// Validate checks that p can lay out every media kind: it has a name, and each
// template is present and parses against the known fields. A bad template
// (unbalanced groups or braces, an unknown field) is CodeInvalid.
func (p Profile) Validate() error {
	const op = "organize.Validate"
	if p.Name == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "profile has no name")
	}
	for _, t := range []struct{ kind, tmpl string }{
		{"music", p.Music}, {"audiobook", p.Audiobook}, {"podcast", p.Podcast},
	} {
		if t.tmpl == "" {
			return waxerr.New(waxerr.CodeInvalid, op, "profile "+p.Name+" has no "+t.kind+" template")
		}
		if err := validateTemplate(t.tmpl); err != nil {
			return waxerr.Wrapf(waxerr.CodeInvalid, op, err, "profile %s %s template", p.Name, t.kind)
		}
	}
	return nil
}

// NewProfileSet validates each custom profile's templates and returns a set that
// resolves them ahead of the built-ins. A custom profile with the same name as a
// built-in overrides it; an empty template field inherits the built-in's. A bad
// template (unbalanced groups/braces, unknown field) is rejected here so the
// failure surfaces at config load, not at the first organize.
func NewProfileSet(custom []Profile) (*ProfileSet, error) {
	set := &ProfileSet{byName: make(map[string]Profile, len(builtins)+len(custom))}
	for name, p := range builtins {
		set.byName[name] = p
	}
	for _, p := range custom {
		// Inherit unspecified templates from the built-in of the same name, or from
		// the native profile for a brand-new profile, so a user can override just one
		// media type and still get sensible defaults for the rest.
		base, ok := set.byName[p.Name]
		if !ok {
			base = nativeProfile
		}
		merged := Profile{
			Name:      p.Name,
			Music:     firstNonEmpty(p.Music, base.Music),
			Audiobook: firstNonEmpty(p.Audiobook, base.Audiobook),
			Podcast:   firstNonEmpty(p.Podcast, base.Podcast),
			TagWrite:  p.TagWrite,
		}
		if err := merged.Validate(); err != nil {
			return nil, err
		}
		set.byName[p.Name] = merged
	}
	return set, nil
}

// ByName returns a profile, custom-first then built-in.
func (s *ProfileSet) ByName(name string) (Profile, error) {
	if s == nil {
		s = mustDefaultSet()
	}
	p, ok := s.byName[name]
	if !ok {
		return Profile{}, waxerr.New(waxerr.CodeNotFound, "organize.ProfileByName",
			"no such organization profile: "+name)
	}
	return p, nil
}

// Names lists the set's profile names, sorted.
func (s *ProfileSet) Names() []string {
	if s == nil {
		s = mustDefaultSet()
	}
	names := make([]string, 0, len(s.byName))
	for n := range s.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// All returns every profile the set resolves, as ByName returns it, sorted by name.
func (s *ProfileSet) All() []Profile {
	names := s.Names()
	if s == nil {
		s = mustDefaultSet()
	}
	out := make([]Profile, len(names))
	for i, n := range names {
		out[i] = s.byName[n]
	}
	return out
}

func mustDefaultSet() *ProfileSet {
	set, _ := NewProfileSet(nil)
	return set
}

// ProfileByName returns a built-in profile by name. It is the convenience entry
// for callers with no custom profiles; the facade resolves through a ProfileSet
// built from config.
func ProfileByName(name string) (Profile, error) {
	return mustDefaultSet().ByName(name)
}

// Profiles lists the built-in profile names, sorted.
func Profiles() []string { return mustDefaultSet().Names() }
