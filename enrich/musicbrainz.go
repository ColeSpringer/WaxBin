package enrich

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/waxerr"
)

// musicBrainz wraps the MusicBrainz web service v2. Lookups are MBID-first; a
// text search is the fallback when an entity has no MBID yet. Responses are JSON.
// The mandatory contact User-Agent and the 1 req/sec pacing are applied by the
// shared netsafe client the Service builds.
type musicBrainz struct {
	client  *netsafe.Client
	baseURL string // e.g. https://musicbrainz.org/ws/2
	cache   cache
}

// jsonMIME is the response allow-list for the JSON web service.
var jsonMIME = []string{"application/json", "text/json", "application/octet-stream"}

// mbArtist is the subset of a MusicBrainz artist we consume.
type mbArtist struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	SortName  string       `json:"sort-name"`
	Score     int          `json:"score"` // present on search results (0-100)
	Aliases   []mbAlias    `json:"aliases"`
	Relations []mbRelation `json:"relations"`
	Genres    []mbGenre    `json:"genres"`
}

type mbAlias struct {
	Name     string `json:"name"`
	SortName string `json:"sort-name"`
	Primary  *bool  `json:"primary"`
}

type mbRelation struct {
	Type      string    `json:"type"`      // "member of band", "is person", "collaboration", ...
	Direction string    `json:"direction"` // forward|backward
	Artist    *mbArtist `json:"artist"`
}

type mbGenre struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// mbReleaseGroup is the subset of a MusicBrainz release group we consume.
type mbReleaseGroup struct {
	ID             string         `json:"id"`
	Title          string         `json:"title"`
	PrimaryType    string         `json:"primary-type"`
	SecondaryTypes []string       `json:"secondary-types"`
	Score          int            `json:"score"`
	ArtistCredit   []mbArtistCred `json:"artist-credit"`
	Genres         []mbGenre      `json:"genres"`
}

type mbArtistCred struct {
	Artist mbArtist `json:"artist"`
}

// mbRelease is the subset of a MusicBrainz release we consume for audiobooks.
type mbRelease struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	ASIN      string        `json:"asin"`
	Barcode   string        `json:"barcode"` // often the ISBN/EAN for books
	Score     int           `json:"score"`
	LabelInfo []mbLabelInfo `json:"label-info"`
}

type mbLabelInfo struct {
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
}

// artistSearchResult / releaseGroupSearchResult wrap the search list responses.
type artistSearchResult struct {
	Artists []mbArtist `json:"artists"`
}
type releaseGroupSearchResult struct {
	ReleaseGroups []mbReleaseGroup `json:"release-groups"`
}

// minMatchScore is the search score required to accept a text-search hit. It is
// deliberately high, and paired with a normalized-name equality check, so
// enrichment never adopts a wrong MBID from a fuzzy match.
const minMatchScore = 90

// lookupArtist fetches an artist by MBID with aliases and relations. (Artist
// genres are not requested: WaxBin has no artist-genre storage; item genres come
// from the release group.)
func (m *musicBrainz) lookupArtist(ctx context.Context, force bool, mbid string) (*mbArtist, error) {
	var a mbArtist
	if err := m.get(ctx, force, "mb:artist:"+mbid,
		"/artist/"+url.PathEscape(mbid)+"?inc=aliases+artist-rels&fmt=json", &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// searchArtist finds an artist by name. It returns (nil, nil) when no hit clears
// the score threshold and matches the normalized name, so a weak search never
// mis-resolves; a name that normalizes to empty (symbol-only) is not searched, so
// it cannot match another empty-normalizing name's MBID.
func (m *musicBrainz) searchArtist(ctx context.Context, force bool, name string) (*mbArtist, error) {
	want := identity.MatchKey(name)
	if want == "" {
		return nil, nil
	}
	q := `artist:"` + escapeLucene(name) + `"`
	var res artistSearchResult
	if err := m.get(ctx, force, "mb:artist-search:"+want,
		"/artist?query="+url.QueryEscape(q)+"&limit=5&fmt=json", &res); err != nil {
		return nil, err
	}
	for i := range res.Artists {
		a := &res.Artists[i]
		if a.Score >= minMatchScore && identity.MatchKey(a.Name) == want {
			return m.lookupArtist(ctx, force, a.ID)
		}
	}
	return nil, nil
}

// lookupReleaseGroup fetches a release group by MBID with artist credits and genres.
func (m *musicBrainz) lookupReleaseGroup(ctx context.Context, force bool, mbid string) (*mbReleaseGroup, error) {
	var rg mbReleaseGroup
	if err := m.get(ctx, force, "mb:rg:"+mbid,
		"/release-group/"+url.PathEscape(mbid)+"?inc=artist-credits+genres&fmt=json", &rg); err != nil {
		return nil, err
	}
	return &rg, nil
}

// searchReleaseGroup finds a release group by title and primary artist. It requires
// a strong score, a normalized-title match, AND (when an artist is given) that the
// hit's artist credit matches by normalized name, so a common title like "Greatest
// Hits" can never adopt a different artist's MBID.
func (m *musicBrainz) searchReleaseGroup(ctx context.Context, force bool, title, artist string) (*mbReleaseGroup, error) {
	wantTitle := identity.MatchKey(title)
	if wantTitle == "" {
		return nil, nil
	}
	wantArtist := identity.MatchKey(artist)
	q := `releasegroup:"` + escapeLucene(title) + `"`
	if strings.TrimSpace(artist) != "" {
		q += ` AND artist:"` + escapeLucene(artist) + `"`
	}
	var res releaseGroupSearchResult
	if err := m.get(ctx, force, "mb:rg-search:"+wantArtist+"\x1f"+wantTitle,
		"/release-group?query="+url.QueryEscape(q)+"&limit=5&fmt=json", &res); err != nil {
		return nil, err
	}
	for i := range res.ReleaseGroups {
		rg := &res.ReleaseGroups[i]
		if rg.Score < minMatchScore || identity.MatchKey(rg.Title) != wantTitle {
			continue
		}
		if wantArtist != "" && identity.MatchKey(releaseGroupArtistName(rg)) != wantArtist {
			continue // title matches but the artist does not: not this release group
		}
		return m.lookupReleaseGroup(ctx, force, rg.ID)
	}
	return nil, nil
}

// releaseGroupArtistName returns the first credited artist's name, "" when absent.
func releaseGroupArtistName(rg *mbReleaseGroup) string {
	if len(rg.ArtistCredit) == 0 {
		return ""
	}
	return rg.ArtistCredit[0].Artist.Name
}

// lookupRelease fetches a release by MBID with label info (for audiobook publisher).
func (m *musicBrainz) lookupRelease(ctx context.Context, force bool, mbid string) (*mbRelease, error) {
	var r mbRelease
	if err := m.get(ctx, force, "mb:release:"+mbid,
		"/release/"+url.PathEscape(mbid)+"?inc=labels&fmt=json", &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// get issues a GET against the web service, cached by key. A non-forced call reads
// the cache first (so a re-run or an offline run reuses prior answers); a forced
// call bypasses the read but still refreshes the cache. A 404 is reported as
// CodeNotFound so callers can treat "no such entity" as no match.
func (m *musicBrainz) get(ctx context.Context, force bool, cacheKey, pathAndQuery string, out any) error {
	const op = "enrich.musicbrainz"
	if !force {
		if payload, ok, err := m.cache.get(ctx, cacheKey); err != nil {
			return err
		} else if ok {
			if err := json.Unmarshal(payload, out); err != nil {
				return waxerr.Wrap(waxerr.CodeInvalid, op, err)
			}
			return nil
		}
	}
	resp, err := m.client.Do(ctx, netsafe.Request{
		URL:        m.baseURL + pathAndQuery,
		AcceptMIME: jsonMIME,
	})
	if err != nil {
		return err
	}
	// Validate by unmarshaling BEFORE caching: the MIME allow-list permits
	// application/octet-stream, so a 2xx-but-garbage body (proxy/captive portal)
	// would otherwise poison the cache and wedge every non-forced resume until --force.
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return waxerr.Wrapf(waxerr.CodeInvalid, op, err, "parsing %s", pathAndQuery)
	}
	return m.cache.put(ctx, cacheKey, resp.Body)
}

// escapeLucene escapes the Lucene special characters that would otherwise break a
// MusicBrainz query, plus the double quote we wrap terms in.
func escapeLucene(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '+', '-', '&', '|', '!', '(', ')', '{', '}', '[', ']', '^', '"', '~', '*', '?', ':', '\\', '/':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
