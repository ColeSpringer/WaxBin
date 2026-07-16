package enrich

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/waxerr"
)

// listenBrainz supplies community genres/tags for a release group from the
// ListenBrainz metadata API. It is key-free and keys on the release-group MBID the
// identity spine resolved, so it only contributes once MusicBrainz has anchored the
// group. It advertises CapGenres and returns nothing for any other target.
//
// It does not cache: the port depends only on model/identity, so a built-in provider
// has no store handle. The per-entity enrichment marker keeps a re-run from looking a
// group up twice, so a within-run single fetch needs no response cache.
type listenBrainz struct {
	client  *netsafe.Client
	baseURL string // e.g. https://api.listenbrainz.org
}

func (l *listenBrainz) Name() string             { return providerListenBrainz }
func (l *listenBrainz) Capabilities() Capability { return CapGenres }

// lbMetadata is the subset of the ListenBrainz metadata response we consume: a map
// keyed by MBID, each carrying release-group tags under tag.release_group.
type lbMetadata map[string]struct {
	Tag struct {
		ReleaseGroup []lbTag `json:"release_group"`
	} `json:"tag"`
}

// lbTag is one community tag. GenreMBID is set when ListenBrainz recognizes the tag
// as a MusicBrainz genre (as opposed to a free-form folksonomy tag); Count is the
// community vote weight.
type lbTag struct {
	Tag       string `json:"tag"`
	Count     int    `json:"count"`
	GenreMBID string `json:"genre_mbid"`
}

// Enrich fetches a release group's community tags and returns the genre-recognized
// ones ordered by descending vote count. A group with no tags (or no recognized
// genres) is a clean no-match.
func (l *listenBrainz) Enrich(ctx context.Context, req Request) (*Candidate, error) {
	// MBIDs are case-insensitive UUIDs; MusicBrainz emits them lowercase. Normalize so
	// both the request and the response-map lookup agree regardless of a caller's casing.
	mbid := strings.ToLower(strings.TrimSpace(req.MBID))
	if req.Type != TargetReleaseGroup || mbid == "" {
		return nil, nil
	}
	q := url.Values{}
	q.Set("release_group_mbids", mbid)
	q.Set("inc", "tag")
	resp, err := l.client.Do(ctx, netsafe.Request{
		URL:        l.baseURL + "/1/metadata/release_group/?" + q.Encode(),
		AcceptMIME: jsonMIME,
	})
	if err != nil {
		if waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, nil // unknown release group
		}
		return nil, err
	}
	var meta lbMetadata
	if err := json.Unmarshal(resp.Body, &meta); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalid, "enrich.listenbrainz", err)
	}
	entry, ok := meta[mbid]
	if !ok {
		// The service keys the response by the requested mbid; match case-insensitively
		// in case it echoes a differently-cased key.
		for k, v := range meta {
			if strings.EqualFold(k, mbid) {
				entry, ok = v, true
				break
			}
		}
	}
	if !ok || len(entry.Tag.ReleaseGroup) == 0 {
		return nil, nil
	}
	genres := listenBrainzGenres(entry.Tag.ReleaseGroup)
	if len(genres) == 0 {
		return nil, nil
	}
	return &Candidate{Genres: genres}, nil
}

// listenBrainzGenres reduces community tags to genre display names ordered by
// descending vote count. It keeps only the tags ListenBrainz recognizes as MusicBrainz
// genres (those carrying a genre_mbid) and drops raw folksonomy tags entirely: a group
// tagged "seen live", "favorites", "2015", or "vinyl" contributes no genre rather than
// writing that noise into every member track's genre, where on a MusicBrainz genre-gap
// it would even become the display-primary genre. Names are kept in the provider's own
// casing; the Service's match-key dedup folds them against the MusicBrainz baseline.
func listenBrainzGenres(tags []lbTag) []string {
	recognized := make([]lbTag, 0, len(tags))
	for _, t := range tags {
		if t.GenreMBID != "" && t.Tag != "" {
			recognized = append(recognized, t)
		}
	}
	sort.SliceStable(recognized, func(i, j int) bool { return recognized[i].Count > recognized[j].Count })
	out := make([]string, 0, len(recognized))
	for _, t := range recognized {
		out = append(out, t.Tag)
	}
	return out
}
