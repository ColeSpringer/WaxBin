package enrich

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
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

// mbRelease is the subset of a MusicBrainz release we consume, for audiobooks and
// for the album release match. ReleaseGroup is present on a search document and is
// what the release match verifies against, rather than trusting that the query's
// rgid term was honored.
//
// Country, Media, and ReleaseEvents back the edition tier. A browse document carries
// all three but no Score and no ReleaseGroup, which is why that tier does not reuse
// soleRelease. Counts are deliberately not modeled.
type mbRelease struct {
	ID            string           `json:"id"`
	Title         string           `json:"title"`
	ASIN          string           `json:"asin"`
	Barcode       string           `json:"barcode"` // often the ISBN/EAN for books
	Score         int              `json:"score"`
	Country       string           `json:"country"`
	Media         []mbMedium       `json:"media"`
	ReleaseEvents []mbReleaseEvent `json:"release-events"`
	LabelInfo     []mbLabelInfo    `json:"label-info"`
	ReleaseGroup  *mbReleaseGroup  `json:"release-group"`
}

// mbMedium is one disc of a release. Only the format is read; counts stay out of the
// matcher by design (see release.go).
type mbMedium struct {
	Format string `json:"format"`
}

// mbReleaseEvent is one release of an edition in one area. Their ISO codes union with
// the top-level country into the set the edition tier compares against.
type mbReleaseEvent struct {
	Area *mbArea `json:"area"`
}

type mbArea struct {
	ISO31661Codes []string `json:"iso-3166-1-codes"`
}

type mbLabelInfo struct {
	CatalogNumber string `json:"catalog-number"`
	Label         struct {
		Name string `json:"name"`
	} `json:"label"`
}

// artistSearchResult / releaseGroupSearchResult / releaseSearchResult wrap the
// search list responses.
type artistSearchResult struct {
	Artists []mbArtist `json:"artists"`
}
type releaseGroupSearchResult struct {
	ReleaseGroups []mbReleaseGroup `json:"release-groups"`
}
type releaseSearchResult struct {
	Releases []mbRelease `json:"releases"`
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

// searchReleaseByIdentifier finds the releases of one release group carrying an
// identifier, as a Lucene query against the search server. field is a release-search
// index field ("barcode", "catno") and values are the spellings to accept, OR-ed, so
// a barcode can ask for the padded and unpadded forms in one request.
//
// Searching rather than browsing the group is deliberate. A release browse caps at
// 100 per page, and the groups that exceed it are exactly the popular records a
// barcode most often identifies, so a browse fails on the population this exists to
// serve. Note also that the obvious REST spelling does not exist: barcode is not a
// browse parameter, and inc= does not apply to search, so it has to be query=.
//
// rgMBID is interpolated unescaped and must be a validated UUID: escapeLucene
// escapes '-', which every UUID is built from.
func (m *musicBrainz) searchReleaseByIdentifier(ctx context.Context, force bool, rgMBID, field string, values []string) ([]mbRelease, error) {
	if rgMBID == "" || len(values) == 0 {
		return nil, nil
	}
	terms := make([]string, 0, len(values))
	for _, v := range values {
		terms = append(terms, field+`:"`+escapeLucene(v)+`"`)
	}
	q := "rgid:" + rgMBID + " AND (" + strings.Join(terms, " OR ") + ")"
	var res releaseSearchResult
	if err := m.get(ctx, force, "mb:release-ident:"+rgMBID+"\x1f"+field+"\x1f"+strings.Join(values, "|"),
		"/release?query="+url.QueryEscape(q)+"&limit=25&fmt=json", &res); err != nil {
		return nil, err
	}
	return res.Releases, nil
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
	if !force {
		if payload, ok, err := m.cache.get(ctx, cacheKey); err != nil {
			return err
		} else if ok {
			if err := json.Unmarshal(payload, out); err != nil {
				return waxerr.Wrap(waxerr.CodeInvalid, "enrich.musicbrainz", err)
			}
			return nil
		}
	}
	body, err := m.fetchJSON(ctx, pathAndQuery, out)
	if err != nil {
		return err
	}
	return m.cache.put(ctx, cacheKey, body)
}

// fetchJSON issues one GET and decodes it into out, returning the raw body so a caller
// decides what to cache. Touching no cache itself is what lets the group browse store a
// projection of several pages under one key.
//
// Validating by unmarshaling first is load-bearing: the MIME allow-list permits
// application/octet-stream, so a 2xx-but-garbage body would otherwise be cached and
// wedge every non-forced resume until --force.
func (m *musicBrainz) fetchJSON(ctx context.Context, pathAndQuery string, out any) ([]byte, error) {
	const op = "enrich.musicbrainz"
	resp, err := m.client.Do(ctx, netsafe.Request{
		URL:        m.baseURL + pathAndQuery,
		AcceptMIME: jsonMIME,
	})
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "parsing %s", pathAndQuery)
	}
	return resp.Body, nil
}

// maxGroupReleases caps how many editions the edition tier enumerates. Media and country
// cannot be unique among more than this, so a bigger group refuses whatever the pages
// say, and stopping after page one keeps a famous record cheap.
const maxGroupReleases = 300

// browsePageSize is the browse endpoint's per-page maximum.
const browsePageSize = 100

// releaseBrowseResult is one page of the release browse; release-count is the group's
// total, which is how paging terminates and how over-cap is seen from page one.
type releaseBrowseResult struct {
	Count    int         `json:"release-count"`
	Releases []mbRelease `json:"releases"`
}

// releaseEditions enumerates a release group's releases, projected to the facts the
// matcher compares and cached under one key per group.
//
// It browses where searchReleaseByIdentifier searches, and that function's reason does
// not carry over: whole-group enumeration is N pages either way, and the search endpoint
// caps at 100 with no paging, so it would refuse on exactly the popular groups this tier
// serves. The browse also reads the live database, not the Lucene index.
//
// inc=media is the only inc needed and the only correct one: the default document already
// carries country, release-events[].area.iso-3166-1-codes, barcode, date, and status, and
// inc=release-events answers HTTP 400 (not a valid inc for the release resource).
//
// Two failure shapes stay distinct. Over cap is a stable fact, cached with a nil edition
// list after one request. A short read (fewer unique releases than release-count) is
// transient: it is never cached, and it reports ok=false so the caller leaves the album
// unmarked and a later run retries it. Caching alone would not be enough, because the
// no-match marker the caller writes would keep the album out of the queue anyway. A page
// error propagates and aborts the pass, marking nothing.
//
// rgMBID must be a validated UUID, the contract searchReleaseByIdentifier states;
// enrichAlbumRelease checks model.IsMBID before either is reached.
//
// force bypasses the cache read, so the caller is responsible for not forcing the same
// group twice in one run: the point of forcing is to refresh a stale answer, and the
// second album's fetch would refresh what the first just wrote. matchAlbumRelease keeps
// that per-run memo.
func (m *musicBrainz) releaseEditions(ctx context.Context, force bool, rgMBID string) (releaseGroupEditions, bool, error) {
	const op = "enrich.musicbrainz"
	var out releaseGroupEditions
	if rgMBID == "" {
		return out, false, nil
	}
	cacheKey := "mb:rg-editions:" + rgMBID
	if !force {
		if payload, ok, err := m.cache.get(ctx, cacheKey); err != nil {
			return out, false, err
		} else if ok {
			if err := json.Unmarshal(payload, &out); err != nil {
				return out, false, waxerr.Wrap(waxerr.CodeInvalid, op, err)
			}
			return out, true, nil
		}
	}

	// Offset paging walks a set that can shift, so a release can repeat or be skipped.
	// Deduping by id makes completeness "at least count distinct ids seen".
	seen := make(map[string]bool)
	var editions []releaseEdition
	count := -1
	for offset := 0; ; offset += browsePageSize {
		var page releaseBrowseResult
		q := "/release?release-group=" + url.QueryEscape(rgMBID) +
			"&inc=media&limit=" + strconv.Itoa(browsePageSize) +
			"&offset=" + strconv.Itoa(offset) + "&fmt=json"
		if _, err := m.fetchJSON(ctx, q, &page); err != nil {
			// A stale or merged-away release-group id browses to 404. That is a fact about
			// the group, not a failed run, and every other MusicBrainz call here degrades
			// the same way rather than aborting the pass over one album's bad tag.
			if waxerr.Is(err, waxerr.CodeNotFound) {
				return releaseGroupEditions{}, true, nil
			}
			return releaseGroupEditions{}, false, err
		}
		if count < 0 {
			count = page.Count
			// A page carrying releases but no usable release-count cannot be judged
			// complete: taking count as 0 would satisfy every completeness test below and
			// hand the matcher whatever fraction of the group this page held. Treat it as
			// the short read it is. A genuinely empty group reports 0 with no releases.
			if count <= 0 && len(page.Releases) > 0 {
				return releaseGroupEditions{}, false, nil
			}
			if count > maxGroupReleases {
				over := releaseGroupEditions{Count: count}
				return over, true, m.cacheEditions(ctx, cacheKey, over)
			}
		}
		for i := range page.Releases {
			r := &page.Releases[i]
			if r.ID == "" || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			editions = append(editions, editionOf(r))
		}
		if len(seen) >= count || len(page.Releases) == 0 || offset+browsePageSize >= maxGroupReleases {
			break
		}
	}
	if len(seen) < count {
		// Refuse and cache nothing, so the next run retries rather than inheriting a set
		// the matcher would judge uniqueness over.
		return releaseGroupEditions{}, false, nil
	}
	full := releaseGroupEditions{Count: count, Editions: editions}
	return full, true, m.cacheEditions(ctx, cacheKey, full)
}

// cacheEditions stores a projected edition set. The projection is ~30x smaller than the
// raw pages and is exactly what the matcher consumes, which matters because
// enrichment_cache has no TTL and no prune.
func (m *musicBrainz) cacheEditions(ctx context.Context, key string, g releaseGroupEditions) error {
	payload, err := json.Marshal(g)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeInvalid, "enrich.musicbrainz", err)
	}
	return m.cache.put(ctx, key, payload)
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
