package podcast

import (
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
)

const sampleFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"
     xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"
     xmlns:podcast="https://podcastindex.org/namespace/1.0"
     xmlns:content="http://purl.org/rss/1.0/modules/content/">
  <channel>
    <title>The Example Show</title>
    <link>https://example.com</link>
    <description>A show about examples.</description>
    <language>en-us</language>
    <itunes:author>Jane Host</itunes:author>
    <itunes:explicit>no</itunes:explicit>
    <itunes:image href="https://example.com/art.jpg"/>
    <itunes:category text="Technology"/>
    <podcast:guid>show-guid-001</podcast:guid>
    <item>
      <title>Episode One</title>
      <link>https://example.com/1</link>
      <description>First.</description>
      <content:encoded><![CDATA[<p>First episode, expanded.</p>]]></content:encoded>
      <guid isPermaLink="false">ep-0001</guid>
      <pubDate>Tue, 02 Jan 2024 15:04:05 +0000</pubDate>
      <itunes:duration>1:02:03</itunes:duration>
      <itunes:season>2</itunes:season>
      <itunes:episode>7</itunes:episode>
      <itunes:episodeType>full</itunes:episodeType>
      <enclosure url="https://example.com/1.mp3" length="123456" type="audio/mpeg"/>
      <podcast:transcript url="https://example.com/1.srt" type="application/srt"/>
      <podcast:transcript url="https://example.com/1.html" type="text/html"/>
    </item>
    <item>
      <title>Episode Two</title>
      <guid>ep-0002</guid>
      <pubDate>Wed, 03 Jan 2024 00:00:00 +0000</pubDate>
      <itunes:duration>305</itunes:duration>
      <enclosure url="https://example.com/2.mp3" length="222" type="audio/mpeg"/>
    </item>
  </channel>
</rss>`

func TestParseFeed(t *testing.T) {
	feed, err := ParseFeed([]byte(sampleFeed))
	if err != nil {
		t.Fatalf("ParseFeed: %v", err)
	}
	if feed.Title != "The Example Show" || feed.Author != "Jane Host" {
		t.Fatalf("channel meta: %q / %q", feed.Title, feed.Author)
	}
	if feed.GUID != "show-guid-001" {
		t.Fatalf("podcast:guid = %q", feed.GUID)
	}
	if feed.ImageURL != "https://example.com/art.jpg" {
		t.Fatalf("image = %q", feed.ImageURL)
	}
	if feed.Category != "Technology" {
		t.Fatalf("category = %q", feed.Category)
	}
	if len(feed.Episodes) != 2 {
		t.Fatalf("episodes = %d", len(feed.Episodes))
	}

	e1 := feed.Episodes[0]
	if e1.GUID != "ep-0001" || e1.Title != "Episode One" {
		t.Fatalf("e1 id/title: %q / %q", e1.GUID, e1.Title)
	}
	// content:encoded wins over description.
	if e1.Description != "<p>First episode, expanded.</p>" {
		t.Fatalf("e1 description = %q", e1.Description)
	}
	// 1:02:03 = 3723s.
	if e1.DurationMS != 3723*1000 {
		t.Fatalf("e1 duration = %d", e1.DurationMS)
	}
	if e1.Season != 2 || e1.EpisodeNo != 7 {
		t.Fatalf("e1 season/episode: %d/%d", e1.Season, e1.EpisodeNo)
	}
	if e1.EnclosureURL != "https://example.com/1.mp3" || e1.EnclosureSize != 123456 {
		t.Fatalf("e1 enclosure: %q %d", e1.EnclosureURL, e1.EnclosureSize)
	}
	if e1.Year != 2024 || e1.PubDateNS == 0 {
		t.Fatalf("e1 date: year=%d ns=%d", e1.Year, e1.PubDateNS)
	}
	// The SRT transcript is preferred over the HTML one.
	if e1.TranscriptURL != "https://example.com/1.srt" {
		t.Fatalf("e1 transcript = %q", e1.TranscriptURL)
	}
	if e1.EpisodeType != model.EpisodeFull {
		t.Fatalf("e1 type = %q", e1.EpisodeType)
	}

	// Plain-seconds duration.
	if feed.Episodes[1].DurationMS != 305*1000 {
		t.Fatalf("e2 duration = %d", feed.Episodes[1].DurationMS)
	}
}

func TestParseFeedRejectsNonFeed(t *testing.T) {
	if _, err := ParseFeed([]byte(`<html><body>not a feed</body></html>`)); err == nil {
		t.Fatal("expected error on non-feed XML")
	}
}

func TestParseDurationForms(t *testing.T) {
	cases := map[string]int64{
		"":           0,
		"90":         90000,
		"01:30":      90000,
		"1:00:00":    3600000,
		"00:00:05.5": 5000, // fractional seconds floored
		"bogus":      0,
	}
	for in, want := range cases {
		if got := parseDurationMS(in); got != want {
			t.Errorf("parseDurationMS(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParsePubDateNamedZone(t *testing.T) {
	// "EST" must resolve to -0500, not Go's fabricated +0000.
	got := parsePubDate("Tue, 10 Jun 2003 04:00:00 EST")
	want := time.Date(2003, 6, 10, 9, 0, 0, 0, time.UTC).UnixNano() // 04:00 -0500 == 09:00 UTC
	if got != want {
		t.Fatalf("named-zone pubDate = %d, want %d (04:00 EST == 09:00 UTC)", got, want)
	}
	// A numeric-offset form still parses.
	if parsePubDate("Tue, 02 Jan 2024 15:04:05 +0000") == 0 {
		t.Fatal("numeric-offset pubDate should parse")
	}
}
