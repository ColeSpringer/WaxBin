package enrich

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// lrclib supplies a recording's lyrics from LRCLIB, a key-free public lyrics
// database. It keys on the track's title, artist, album, and duration (LRCLIB
// disambiguates near-duplicate tracks by duration), advertising CapLyrics. It is the
// flagship built-in lyrics provider and, like the other built-ins, does not cache:
// the per-recording enrichment marker keeps a re-run from re-querying a track.
type lrclib struct {
	client  *netsafe.Client
	baseURL string // e.g. https://lrclib.net
}

func (l *lrclib) Name() string             { return providerLRCLIB }
func (l *lrclib) Capabilities() Capability { return CapLyrics }

// lrclibResponse is the subset of LRCLIB's /api/get response we consume. An
// instrumental track carries no lyrics; a match carries synced (LRC) and/or plain
// lyrics.
type lrclibResponse struct {
	Instrumental bool   `json:"instrumental"`
	PlainLyrics  string `json:"plainLyrics"`
	SyncedLyrics string `json:"syncedLyrics"`
}

// Enrich looks up lyrics for one recording. It needs at least a title and artist;
// without them, or on a genuine 404, it returns a clean no-match. Synced lyrics win
// over plain; an instrumental match yields no lyrics.
//
// /api/get is duration-disambiguated: it matches the given duration within a small
// tolerance, so a track whose stored duration drifts from LRCLIB's (a different master
// or a slightly mis-probed length) 404s even when the lyrics exist. Because a no-match
// writes a marker that suppresses the track until a forced re-run, a first, duration-
// keyed attempt that 404s is retried by name alone. The retry stays on /api/get, so it
// still requires an exact title/artist(/album) match (the same song under a different
// length yields the same lyrics) rather than falling back to a fuzzy search that could
// adopt a cover or remix.
func (l *lrclib) Enrich(ctx context.Context, req Request) (*Candidate, error) {
	if req.Type != TargetRecording {
		return nil, nil
	}
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Artist) == "" {
		return nil, nil
	}
	params := func(withDuration bool) url.Values {
		q := url.Values{}
		q.Set("track_name", req.Title)
		q.Set("artist_name", req.Artist)
		if req.Album != "" {
			q.Set("album_name", req.Album)
		}
		if withDuration && req.DurationSec > 0 {
			q.Set("duration", strconv.Itoa(req.DurationSec))
		}
		return q
	}

	out, err := l.get(ctx, params(true))
	if waxerr.Is(err, waxerr.CodeNotFound) && req.DurationSec > 0 {
		out, err = l.get(ctx, params(false)) // duration drift: retry by name alone
	}
	if err != nil {
		if waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, nil // no lyrics for this track
		}
		return nil, err
	}
	if out.Instrumental {
		return nil, nil
	}
	ly := &model.Lyrics{Source: providerLRCLIB}
	if synced := parseLRC(out.SyncedLyrics); len(synced) > 0 {
		ly.Synced = synced
	}
	if strings.TrimSpace(out.PlainLyrics) != "" {
		ly.Unsynced = out.PlainLyrics
	}
	if !ly.HasContent() {
		return nil, nil
	}
	return &Candidate{Lyrics: ly}, nil
}

// get issues one /api/get lookup and decodes the response. A 404 surfaces as
// CodeNotFound so the caller can distinguish "no match" from a transport error.
func (l *lrclib) get(ctx context.Context, q url.Values) (*lrclibResponse, error) {
	resp, err := l.client.Do(ctx, netsafe.Request{
		URL:        l.baseURL + "/api/get?" + q.Encode(),
		AcceptMIME: jsonMIME,
	})
	if err != nil {
		return nil, err
	}
	var out lrclibResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalid, "enrich.lrclib", err)
	}
	return &out, nil
}

// lrcTimeRe matches one leading LRC timestamp, "[mm:ss]" or "[mm:ss.xx]" (the
// fraction is tenths, centiseconds, or milliseconds). A line may carry several
// timestamps before its text ("[00:12.00][00:15.00]la la"), so the parser strips them
// one at a time. The minute field allows up to four digits (a long audiobook/DJ track),
// and the fraction consumes any number of digits so an over-precise source still parses
// rather than dropping the whole line; excess fraction precision is truncated to
// milliseconds. A metadata tag like "[ar:Artist]" has a non-numeric first field and
// does not match, so it is dropped.
var lrcTimeRe = regexp.MustCompile(`^\[(\d{1,4}):(\d{1,2})(?:[.:](\d+))?\]`)

// parseLRC parses LRCLIB synced-lyric text into WaxBin synced lines in time order.
// Untimed lines (metadata, blanks) are dropped; the plain-lyrics field carries the
// unsynced fallback separately.
func parseLRC(text string) []model.SyncedLine {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []model.SyncedLine
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		var times []int64
		rest := line
		for {
			m := lrcTimeRe.FindStringSubmatch(rest)
			if m == nil {
				break
			}
			times = append(times, lrcTimestampMS(m[1], m[2], m[3]))
			rest = rest[len(m[0]):]
		}
		if len(times) == 0 {
			continue
		}
		lineText := strings.TrimSpace(rest)
		for _, t := range times {
			out = append(out, model.SyncedLine{TimeMS: t, Text: lineText})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TimeMS < out[j].TimeMS })
	return out
}

// lrcTimestampMS converts an LRC timestamp's minute/second/fraction fields to
// milliseconds. The fraction is centiseconds (two digits, the LRC norm) or
// milliseconds (three digits); a single digit is read as tenths of a second, and any
// digits beyond the third are excess precision that truncates to milliseconds.
func lrcTimestampMS(mm, ss, frac string) int64 {
	m, _ := strconv.Atoi(mm)
	s, _ := strconv.Atoi(ss)
	ms := int64(m)*60000 + int64(s)*1000
	switch {
	case len(frac) == 1:
		f, _ := strconv.Atoi(frac)
		ms += int64(f) * 100
	case len(frac) == 2:
		f, _ := strconv.Atoi(frac)
		ms += int64(f) * 10
	case len(frac) >= 3:
		f, _ := strconv.Atoi(frac[:3])
		ms += int64(f)
	}
	return ms
}
