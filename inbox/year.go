package inbox

import (
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
)

// dateAlbums renders each staged track under the year its album's tracks give together
// (model.AlbumYear), the year the catalog will then show for the album, so a track tagged
// a year apart from the rest lands in its album's folder rather than in one the scan would
// key as an album of its own. A staged album is what the scan would key as one if the
// files stayed where they are staged, so two releases of one name staged in folders of
// their own keep their own years.
func dateAlbums(req Request, files []*staged) {
	groups := map[string][]*staged{}
	var keys []string
	for _, f := range files {
		if f.Outcome != OutcomeImport || f.Kind != model.KindTrack {
			continue
		}
		anchor := f.tags.AlbumArtist
		if strings.TrimSpace(anchor) == "" {
			anchor = f.tags.Artist
			if len(f.tags.Artists) > 0 {
				anchor = f.tags.Artists[0]
			}
		}
		rg := identity.ReleaseGroupKey(f.tags.MBReleaseGroupID, identity.MatchKey(anchor), f.tags.Album)
		album := identity.AlbumKey(f.tags.MBReleaseID, rg, 0, identity.AlbumFolder(stagingRoot(req.Source, f.Src), f.Src))
		if album == "" {
			continue
		}
		key := strconv.FormatInt(f.Library.ID, 10) + "\x00" + album
		if groups[key] == nil {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], f)
	}
	for _, key := range keys {
		group := groups[key]
		years := make([]int, len(group))
		for i, f := range group {
			years[i] = f.tags.Year
		}
		year := model.AlbumYear(years)
		for _, f := range group {
			if year == 0 || f.tags.Year == year {
				continue
			}
			view := acquiredItemView(f.tags, f.Src, f.Kind)
			view.Year = year
			rel, err := organize.RenderRelPath(f.prof, view)
			if err != nil {
				f.Outcome, f.Reason = OutcomeQuarantine, "no destination: "+err.Error()
				continue
			}
			f.rel, f.RelDst = rel, rel
		}
	}
}
