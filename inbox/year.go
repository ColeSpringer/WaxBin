package inbox

import (
	"cmp"
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
)

// alignAlbums renders each staged track the way the catalog will file it once it has
// landed, so a track tagged a year or a spelling apart from the rest lands in its album's
// folder: each artist under the name the catalog keeps for it, else the first spelling the
// import carries, and each staged album under the year its tracks give together
// (model.AlbumYear) and the title of the album the catalog holds at its destination, else
// its first track's. A staged album is what the scan would key as one if the files stayed
// where they are staged, so two releases of one name staged in folders of their own keep
// their own years.
func (s *Service) alignAlbums(ctx context.Context, req Request, files []*staged, spelling map[string]string) error {
	var tracks []*staged
	for _, f := range files {
		if f.Outcome == OutcomeImport && f.Kind == model.KindTrack {
			tracks = append(tracks, f)
		}
	}
	if len(tracks) == 0 {
		return nil
	}

	groups := map[string][]*staged{}
	var order []string
	for _, f := range tracks {
		album := identity.AlbumKey(f.tags.MBReleaseID, releaseGroupKey(f.tags), 0, identity.AlbumFolder(stagingRoot(req.Source, f.Src), f.Src))
		if album == "" {
			continue
		}
		key := strconv.FormatInt(f.Library.ID, 10) + "\x00" + album
		if groups[key] == nil {
			order = append(order, key)
		}
		groups[key] = append(groups[key], f)
	}
	views := make(map[*staged]*model.ItemView, len(tracks))
	for _, f := range tracks {
		view := acquiredItemView(f.tags, f.Src, f.Kind)
		if name := spelling[identity.MatchKey(view.AlbumArtist)]; name != "" {
			view.AlbumArtist = name
		}
		if name := spelling[identity.MatchKey(view.Artist)]; name != "" {
			view.Artist = name
		}
		views[f] = view
	}
	for _, key := range order {
		group := groups[key]
		years := make([]int, len(group))
		for i, f := range group {
			years[i] = f.tags.Year
		}
		year := model.AlbumYear(years)
		for _, f := range group {
			if year != 0 {
				views[f].Year = year
			}
			views[f].Album = group[0].tags.Album
		}
	}

	// The album the catalog holds where a staged album lands names it; the folder in its
	// key folds case and accents, so the staged spelling finds it.
	dest := map[*staged]string{}
	var albums []string
	for _, key := range order {
		for _, f := range groups[key] {
			rel, err := organize.RenderRelPath(f.prof, views[f])
			if err != nil {
				continue
			}
			root := string(f.Library.Root)
			if k := identity.AlbumKey(f.tags.MBReleaseID, releaseGroupKey(f.tags), 0, identity.AlbumFolder(root, filepath.Join(root, rel))); k != "" {
				dest[f] = k
				albums = append(albums, k)
			}
		}
	}
	titles, err := s.store.AlbumTitles(ctx, albums)
	if err != nil {
		return err
	}
	for _, f := range tracks {
		view := views[f]
		if title := titles[dest[f]]; title != "" {
			view.Album = title
		}
		if view.Year == f.tags.Year && view.Album == f.tags.Album && view.AlbumArtist == f.tags.AlbumArtist && view.Artist == f.tags.Artist {
			continue
		}
		rel, err := organize.RenderRelPath(f.prof, view)
		if err != nil {
			f.Outcome, f.Reason = OutcomeQuarantine, "no destination: "+err.Error()
			continue
		}
		f.rel, f.RelDst = rel, rel
	}
	return nil
}

// spellings maps each artist an import's tracks and books name, by match key, to the name
// the catalog keeps for it, else to the first spelling the import carries, the name the
// catalog will keep for a new one.
func (s *Service) spellings(ctx context.Context, files []*staged) (map[string]string, error) {
	var names, keys []string
	for _, f := range files {
		if f.Outcome != OutcomeImport {
			continue
		}
		switch f.Kind {
		case model.KindTrack:
			names = append(names, f.tags.AlbumArtist, f.tags.Artist)
		case model.KindBook:
			names = append(names, firstNonEmpty(f.tags.AlbumArtist, f.tags.Artist))
		}
	}
	for _, name := range names {
		if k := identity.MatchKey(name); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	known, err := s.store.ArtistNames(ctx, keys)
	if err != nil {
		return nil, err
	}
	spelling := map[string]string{}
	for _, name := range names {
		if k := identity.MatchKey(name); k != "" && spelling[k] == "" {
			spelling[k] = cmp.Or(known[k], strings.TrimSpace(name))
		}
	}
	return spelling, nil
}

// spellAuthor gives a book's view the spelling its author takes, reporting whether that
// changed it.
func spellAuthor(v *model.ItemView, spelling map[string]string) bool {
	name := spelling[identity.MatchKey(v.Artist)]
	if name == "" || name == v.Artist {
		return false
	}
	v.Artist, v.AlbumArtist = name, name
	return true
}

// releaseGroupKey is the release group the scan will key a staged track's album under:
// its album artist, else its first stated artist, and its album title.
func releaseGroupKey(tags model.Tags) string {
	anchor := tags.AlbumArtist
	if strings.TrimSpace(anchor) == "" {
		anchor = tags.Artist
		if len(tags.Artists) > 0 {
			anchor = tags.Artists[0]
		}
	}
	return identity.ReleaseGroupKey(tags.MBReleaseGroupID, identity.MatchKey(anchor), tags.Album)
}
