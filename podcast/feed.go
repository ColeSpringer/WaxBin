// Package podcast parses RSS podcast feeds and OPML, then exposes the service for
// subscriptions, sync, downloads, transcripts, artwork, and retention. All remote
// I/O goes through internal/netsafe.
package podcast

import (
	"bytes"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Go's encoding/xml matches an element by (namespace URI, local name) regardless
// of the prefix the feed chose, so the struct tags below qualify each extension
// element with its full namespace URI and read itunes:/podcast:/content: fields no
// matter the declared prefix:
//
//	itunes  http://www.itunes.com/dtds/podcast-1.0.dtd
//	podcast https://podcastindex.org/namespace/1.0
//	content http://purl.org/rss/1.0/modules/content/

// rssDoc is the subset of an RSS 2.0 podcast feed WaxBin reads.
type rssDoc struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title          string      `xml:"title"`
	Link           string      `xml:"link"`
	Description    string      `xml:"description"`
	Language       string      `xml:"language"`
	ITunesAuthor   string      `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd author"`
	ManagingEditor string      `xml:"managingEditor"`
	ITunesExplicit string      `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd explicit"`
	ITunesImage    hrefImage   `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
	Image          rssImage    `xml:"image"`
	ITunesCategory []itunesCat `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd category"`
	PodcastGUID    string      `xml:"https://podcastindex.org/namespace/1.0 guid"`
	Items          []rssItem   `xml:"item"`
}

type rssImage struct {
	URL string `xml:"url"`
}

type hrefImage struct {
	Href string `xml:"href,attr"`
}

type itunesCat struct {
	Text string `xml:"text,attr"`
}

type rssItem struct {
	Title             string         `xml:"title"`
	Link              string         `xml:"link"`
	Description       string         `xml:"description"`
	ContentEncoded    string         `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	ITunesSummary     string         `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd summary"`
	GUID              rssGUID        `xml:"guid"`
	PubDate           string         `xml:"pubDate"`
	Enclosure         rssEnclosure   `xml:"enclosure"`
	ITunesDuration    string         `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
	ITunesEpisode     string         `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd episode"`
	ITunesSeason      string         `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd season"`
	ITunesEpisodeType string         `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd episodeType"`
	ITunesExplicit    string         `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd explicit"`
	ITunesImage       hrefImage      `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
	Transcripts       []pcTranscript `xml:"https://podcastindex.org/namespace/1.0 transcript"`
	Chapters          pcChapters     `xml:"https://podcastindex.org/namespace/1.0 chapters"`
}

type rssGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink string `xml:"isPermaLink,attr"`
}

type rssEnclosure struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type pcTranscript struct {
	URL      string `xml:"url,attr"`
	Type     string `xml:"type,attr"`
	Language string `xml:"language,attr"`
}

type pcChapters struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

// ParseFeed parses an RSS 2.0 podcast feed (iTunes + Podcasting 2.0 extensions)
// into a normalized model.Feed. It tolerates unknown elements and namespaces and
// only fails on XML that is not a recognizable RSS document.
func ParseFeed(data []byte) (*model.Feed, error) {
	const op = "podcast.ParseFeed"
	var doc rssDoc
	dec := xml.NewDecoder(bytes.NewReader(data))
	// Some feeds declare non-UTF-8 charsets or include stray entities; be permissive
	// rather than rejecting an otherwise-parseable feed.
	dec.Strict = false
	dec.CharsetReader = charsetPassthrough
	if err := dec.Decode(&doc); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalid, op, err)
	}
	ch := doc.Channel
	if strings.TrimSpace(ch.Title) == "" && len(ch.Items) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "not a recognizable RSS podcast feed")
	}

	feed := &model.Feed{
		Title:       strings.TrimSpace(ch.Title),
		Author:      firstNonEmpty(strings.TrimSpace(ch.ITunesAuthor), strings.TrimSpace(ch.ManagingEditor)),
		Description: strings.TrimSpace(ch.Description),
		Link:        strings.TrimSpace(ch.Link),
		Language:    strings.TrimSpace(ch.Language),
		Explicit:    parseExplicit(ch.ITunesExplicit),
		GUID:        strings.TrimSpace(ch.PodcastGUID),
		ImageURL:    firstNonEmpty(strings.TrimSpace(ch.ITunesImage.Href), strings.TrimSpace(ch.Image.URL)),
	}
	if len(ch.ITunesCategory) > 0 {
		feed.Category = strings.TrimSpace(ch.ITunesCategory[0].Text)
	}

	feed.Episodes = make([]model.FeedEpisode, 0, len(ch.Items))
	for i := range ch.Items {
		feed.Episodes = append(feed.Episodes, normalizeItem(&ch.Items[i]))
	}
	return feed, nil
}

// normalizeItem converts a parsed RSS item into a normalized FeedEpisode.
func normalizeItem(it *rssItem) model.FeedEpisode {
	ep := model.FeedEpisode{
		GUID:          strings.TrimSpace(it.GUID.Value),
		Title:         strings.TrimSpace(it.Title),
		Description:   firstNonEmpty(strings.TrimSpace(it.ContentEncoded), strings.TrimSpace(it.Description), strings.TrimSpace(it.ITunesSummary)),
		Link:          strings.TrimSpace(it.Link),
		PubDateNS:     parsePubDate(it.PubDate),
		Season:        atoiSafe(it.ITunesSeason),
		EpisodeNo:     atoiSafe(it.ITunesEpisode),
		EpisodeType:   normalizeEpisodeType(it.ITunesEpisodeType),
		DurationMS:    parseDurationMS(it.ITunesDuration),
		Explicit:      parseExplicit(it.ITunesExplicit),
		EnclosureURL:  strings.TrimSpace(it.Enclosure.URL),
		EnclosureType: strings.TrimSpace(it.Enclosure.Type),
		EnclosureSize: atoi64Safe(it.Enclosure.Length),
		ImageURL:      strings.TrimSpace(it.ITunesImage.Href),
		ChaptersURL:   strings.TrimSpace(it.Chapters.URL),
	}
	ep.Year = yearOf(ep.PubDateNS)
	if t := pickTranscript(it.Transcripts); t != nil {
		ep.TranscriptURL = strings.TrimSpace(t.URL)
		ep.TranscriptType = strings.TrimSpace(t.Type)
	}
	return ep
}

// pickTranscript chooses one transcript among a feed item's <podcast:transcript>
// tags, preferring machine-friendly formats (JSON, then SRT/VTT) over HTML.
func pickTranscript(ts []pcTranscript) *pcTranscript {
	if len(ts) == 0 {
		return nil
	}
	rank := func(t string) int {
		switch {
		case strings.Contains(t, "json"):
			return 0
		case strings.Contains(t, "srt"), strings.Contains(t, "vtt"):
			return 1
		case strings.Contains(t, "plain"), strings.Contains(t, "text"):
			return 2
		default:
			return 3
		}
	}
	best := -1
	bestRank := 99
	for i := range ts {
		if strings.TrimSpace(ts[i].URL) == "" {
			continue
		}
		if r := rank(strings.ToLower(ts[i].Type)); r < bestRank {
			best, bestRank = i, r
		}
	}
	if best < 0 {
		return nil
	}
	return &ts[best]
}

// pubDateLayouts are the date formats podcast feeds use, tried in order. Only
// numeric-offset (and offset-less date) forms are listed: Go's time.Parse cannot
// resolve a named abbreviation like EST/PST that is not the process's local zone and
// silently fabricates a zero offset, so a named-zone value is instead rewritten to a
// numeric offset (replaceTZAbbrev) before parsing.
var pubDateLayouts = []string{
	time.RFC1123Z,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 -0700",
	time.RFC822Z,
	"2006-01-02T15:04:05Z07:00", // ISO 8601 (some feeds use it)
	"2006-01-02",
}

// tzOffsets maps the common English timezone abbreviations feeds use to their
// numeric offsets, so a named-zone pubDate resolves to the correct instant instead
// of Go's fabricated +0000. CST/MST resolve to the US zones (the dominant podcast
// case).
var tzOffsets = map[string]string{
	"UT": "-0000", "GMT": "-0000", "UTC": "-0000", "Z": "-0000",
	"EST": "-0500", "EDT": "-0400", "CST": "-0600", "CDT": "-0500",
	"MST": "-0700", "MDT": "-0600", "PST": "-0800", "PDT": "-0700",
}

// parsePubDate parses an RSS pubDate into unix nanoseconds, returning 0 when it is
// empty or unparseable (an undated episode sorts last rather than erroring).
func parsePubDate(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if ns, ok := tryPubDateLayouts(s); ok {
		return ns
	}
	// Fall back: rewrite a trailing named zone to its numeric offset and retry.
	if repl, ok := replaceTZAbbrev(s); ok {
		if ns, ok := tryPubDateLayouts(repl); ok {
			return ns
		}
	}
	return 0
}

func tryPubDateLayouts(s string) (int64, bool) {
	for _, layout := range pubDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixNano(), true
		}
	}
	return 0, false
}

// replaceTZAbbrev rewrites a date string whose last space-separated token is a known
// timezone abbreviation into one with the equivalent numeric offset.
func replaceTZAbbrev(s string) (string, bool) {
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return "", false
	}
	off, ok := tzOffsets[strings.ToUpper(s[i+1:])]
	if !ok {
		return "", false
	}
	return s[:i+1] + off, true
}

func yearOf(ns int64) int {
	if ns == 0 {
		return 0
	}
	return time.Unix(0, ns).UTC().Year()
}

// parseDurationMS parses an itunes:duration: plain seconds ("3600"), or a colon
// form H:MM:SS / MM:SS. Whole-second precision (a feed value) is enough, so each
// colon-separated field is floored to whole seconds before the base-60 fold.
func parseDurationMS(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if !strings.Contains(s, ":") {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			return int64(f * 1000)
		}
		return 0
	}
	var secs int64
	for _, p := range strings.Split(s, ":") {
		// Floor a fractional seconds field ("05.5" -> "05") before parsing.
		field := strings.TrimSpace(p)
		if dot := strings.IndexByte(field, '.'); dot >= 0 {
			field = field[:dot]
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return 0
		}
		secs = secs*60 + int64(n)
	}
	return secs * 1000
}

func normalizeEpisodeType(s string) model.EpisodeType {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trailer":
		return model.EpisodeTrailer
	case "bonus":
		return model.EpisodeBonus
	default:
		return model.EpisodeFull
	}
}

// parseExplicit reads an iTunes explicit flag ("yes"/"true"/"explicit" are true).
func parseExplicit(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "true", "explicit", "1":
		return true
	default:
		return false
	}
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func atoi64Safe(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// charsetPassthrough lets the XML decoder accept a declared non-UTF-8 charset by
// reading the bytes as-is. WaxBin targets UTF-8 feeds; this avoids a hard failure
// on a mislabeled-but-actually-UTF-8 document rather than transcoding (which would
// pull in a charset dependency).
func charsetPassthrough(_ string, input io.Reader) (io.Reader, error) {
	return input, nil
}
