package enrich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/waxerr"
)

// acoustID looks up recordings by Chromaprint fingerprint. AcoustID is
// Chromaprint-only, so it requires fpcalc to have produced the compressed
// fingerprint, and it is off unless an API key is configured. A result yields
// MusicBrainz recording and release-group ids, which enrichment then resolves
// through MusicBrainz proper.
type acoustID struct {
	client  *netsafe.Client
	baseURL string // e.g. https://api.acoustid.org
	key     string
}

// acoustIDResponse is the subset of the AcoustID v2 lookup response we consume.
type acoustIDResponse struct {
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	Results []struct {
		ID         string  `json:"id"`
		Score      float64 `json:"score"`
		Recordings []struct {
			ID            string `json:"id"` // recording MBID
			ReleaseGroups []struct {
				ID string `json:"id"` // release-group MBID
			} `json:"releasegroups"`
		} `json:"recordings"`
	} `json:"results"`
}

// acoustMatch is one AcoustID hit reduced to the MBIDs enrichment can act on.
type acoustMatch struct {
	Score            float64
	RecordingMBID    string
	ReleaseGroupMBID string
}

// minAcoustScore is the AcoustID match score required to trust a fingerprint hit.
const minAcoustScore = 0.85

// lookup queries AcoustID with a compressed fingerprint and duration, returning
// the best match's recording and release-group MBIDs, or (nil, nil) when nothing
// clears the score threshold.
func (a *acoustID) lookup(ctx context.Context, fingerprint string, durationSec int) (*acoustMatch, error) {
	const op = "enrich.acoustid"
	if a.key == "" {
		return nil, waxerr.New(waxerr.CodeUnsupported, op, "no acoustid api key configured")
	}
	// AcoustID recommends POST for lookups: a compressed fingerprint is multi-kilobyte
	// and would risk a 414 in a GET URL. The parameters ride in a form-encoded body.
	// meta is space-separated ("recordingids releasegroupids"); url.Values encodes the
	// space as "+", which AcoustID decodes back to a separator (a literal "+" would be
	// sent as %2B and misread as one token).
	form := url.Values{}
	form.Set("client", a.key)
	form.Set("duration", strconv.Itoa(durationSec))
	form.Set("fingerprint", fingerprint)
	form.Set("meta", "recordingids releasegroupids")
	form.Set("format", "json")

	resp, err := a.client.Do(ctx, netsafe.Request{
		URL:         a.baseURL + "/v2/lookup",
		Method:      http.MethodPost,
		Body:        []byte(form.Encode()),
		ContentType: "application/x-www-form-urlencoded",
		AcceptMIME:  jsonMIME,
	})
	if err != nil {
		return nil, err
	}
	var out acoustIDResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalid, op, err)
	}
	if out.Error != nil {
		return nil, waxerr.New(waxerr.CodeIO, op, "acoustid: "+out.Error.Message)
	}
	var best *acoustMatch
	for _, r := range out.Results {
		if r.Score < minAcoustScore {
			continue
		}
		for _, rec := range r.Recordings {
			m := &acoustMatch{Score: r.Score, RecordingMBID: rec.ID}
			if len(rec.ReleaseGroups) > 0 {
				m.ReleaseGroupMBID = rec.ReleaseGroups[0].ID
			}
			// Prefer a higher score, and at an equal score prefer a recording that
			// actually carries a release group: all recordings in one result share the
			// score, and AcoustID's first recording often lacks releasegroups, so a
			// strict `>` would lock onto it and drop a same-score sibling that resolves.
			if best == nil || m.Score > best.Score ||
				(m.Score == best.Score && m.ReleaseGroupMBID != "" && best.ReleaseGroupMBID == "") {
				best = m
			}
		}
	}
	return best, nil
}
