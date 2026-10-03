package podcast

import (
	"strings"
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
    <podcast:funding url="https://example.com/support">Support the show</podcast:funding>
    <podcast:medium>Podcast</podcast:medium>
    <podcast:person role="Host" group="Cast" img="https://example.com/jane.jpg" href="https://example.com/jane">Jane Host</podcast:person>
    <podcast:person>Sam Producer</podcast:person>
    <podcast:person role="guest">   </podcast:person>
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
      <podcast:chapters url="https://example.com/1.json" type="application/json+chapters"/>
      <podcast:transcript url="https://example.com/1.srt" type="application/srt"/>
      <podcast:transcript url="https://example.com/1.html" type="text/html"/>
      <podcast:person role="Guest">Alex Guest</podcast:person>
      <podcast:soundbite startTime="73.0" duration="60.5">The best bit</podcast:soundbite>
      <podcast:soundbite startTime="-5" duration="10">negative start dropped</podcast:soundbite>
      <podcast:soundbite startTime="10" duration="0">zero duration dropped</podcast:soundbite>
      <podcast:soundbite startTime="abc" duration="10">unparseable dropped</podcast:soundbite>
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
	t.Parallel()
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

func TestParseFeedPodcasting20Extras(t *testing.T) {
	t.Parallel()
	feed, err := ParseFeed([]byte(sampleFeed))
	if err != nil {
		t.Fatalf("ParseFeed: %v", err)
	}
	if feed.FundingURL != "https://example.com/support" || feed.FundingMessage != "Support the show" {
		t.Fatalf("funding = %q / %q", feed.FundingURL, feed.FundingMessage)
	}
	// Medium is lowercased on parse.
	if feed.Medium != "podcast" {
		t.Fatalf("medium = %q", feed.Medium)
	}
	// The nameless third person tag is skipped; role/group are lowercased; the
	// role-less credit keeps role "".
	if len(feed.Persons) != 2 {
		t.Fatalf("channel persons = %+v, want 2", feed.Persons)
	}
	jane := feed.Persons[0]
	if jane.Name != "Jane Host" || jane.Role != "host" || jane.Group != "cast" ||
		jane.Img != "https://example.com/jane.jpg" || jane.Href != "https://example.com/jane" {
		t.Fatalf("jane = %+v", jane)
	}
	if feed.Persons[1].Name != "Sam Producer" || feed.Persons[1].Role != "" {
		t.Fatalf("sam = %+v", feed.Persons[1])
	}

	e1 := feed.Episodes[0]
	if len(e1.Persons) != 1 || e1.Persons[0].Name != "Alex Guest" || e1.Persons[0].Role != "guest" {
		t.Fatalf("e1 persons = %+v", e1.Persons)
	}
	// The three malformed soundbites are dropped; the valid one converts to ms.
	if len(e1.Soundbites) != 1 {
		t.Fatalf("e1 soundbites = %+v, want 1", e1.Soundbites)
	}
	sb := e1.Soundbites[0]
	if sb.StartMS != 73000 || sb.DurationMS != 60500 || sb.Title != "The best bit" {
		t.Fatalf("soundbite = %+v", sb)
	}
	// The second episode carries no extras.
	if len(feed.Episodes[1].Persons) != 0 || len(feed.Episodes[1].Soundbites) != 0 {
		t.Fatalf("e2 extras should be empty: %+v", feed.Episodes[1])
	}
}

// TestParseFeedBindsNamespaceSpellings: the extension elements bind under the namespace
// spellings feeds use besides the canonical URIs: the Podcasting 2.0 namespace at its old
// GitHub address, the iTunes DTD over https or in Apple's mixed-case path, and the bare
// prefixes of a feed that never declares them.
func TestParseFeedBindsNamespaceSpellings(t *testing.T) {
	t.Parallel()
	const (
		itunes  = `xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"`
		podcast = `xmlns:podcast="https://podcastindex.org/namespace/1.0"`
		content = `xmlns:content="http://purl.org/rss/1.0/modules/content/"`
	)
	for name, r := range map[string]*strings.Replacer{
		"legacy podcast namespace": strings.NewReplacer(podcast,
			`xmlns:podcast="https://github.com/Podcastindex-org/podcast-namespace/blob/main/docs/1.0.md"`),
		"itunes over https":     strings.NewReplacer(itunes, `xmlns:itunes="https://www.itunes.com/dtds/podcast-1.0.dtd"`),
		"itunes mixed-case dtd": strings.NewReplacer(itunes, `xmlns:itunes="http://www.itunes.com/DTDs/Podcast-1.0.dtd"`),
		"itunes without www":    strings.NewReplacer(itunes, `xmlns:itunes="http://itunes.com/dtds/podcast-1.0.dtd"`),
		"undeclared prefixes":   strings.NewReplacer(itunes, "", podcast, "", content, ""),
	} {
		t.Run(name, func(t *testing.T) {
			feed, err := ParseFeed([]byte(r.Replace(sampleFeed)))
			if err != nil {
				t.Fatalf("ParseFeed: %v", err)
			}
			if feed.Author != "Jane Host" || feed.ImageURL != "https://example.com/art.jpg" || feed.Category != "Technology" {
				t.Errorf("itunes channel fields = %q, %q, %q", feed.Author, feed.ImageURL, feed.Category)
			}
			if feed.GUID != "show-guid-001" || feed.FundingURL != "https://example.com/support" ||
				feed.Medium != "podcast" || len(feed.Persons) != 2 {
				t.Errorf("podcast channel fields = guid %q, funding %q, medium %q, %d persons",
					feed.GUID, feed.FundingURL, feed.Medium, len(feed.Persons))
			}
			e1 := feed.Episodes[0]
			if e1.DurationMS != 3723*1000 || e1.Description != "<p>First episode, expanded.</p>" {
				t.Errorf("itunes and content item fields = %d ms, %q", e1.DurationMS, e1.Description)
			}
			if e1.ChaptersURL != "https://example.com/1.json" || e1.TranscriptURL != "https://example.com/1.srt" ||
				len(e1.Persons) != 1 || len(e1.Soundbites) != 1 {
				t.Errorf("podcast item fields = chapters %q, transcript %q, %d persons, %d soundbites",
					e1.ChaptersURL, e1.TranscriptURL, len(e1.Persons), len(e1.Soundbites))
			}
		})
	}
}

// TestParseFeedToleratesMisNestedTags: the permissive parse repairs an element left open
// (an atom:link, a media:thumbnail, a <br> in a description under a default namespace)
// the way it always has, so a feed that parsed before the namespace rewrite still does.
func TestParseFeedToleratesMisNestedTags(t *testing.T) {
	t.Parallel()
	for name, feed := range map[string]string{
		"unclosed atom:link": `<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom"><channel><title>S</title>
			<item><title>E</title><guid>g</guid></item>
			<atom:link href="https://h/feed" rel="self" type="application/rss+xml">
			</channel></rss>`,
		"unclosed media:thumbnail": `<rss version="2.0" xmlns:media="http://search.yahoo.com/mrss/"><channel><title>S</title>
			<item><title>E</title><guid>g</guid><media:thumbnail url="https://h/t.jpg"></item>
			</channel></rss>`,
		"br under a default namespace": `<rss version="2.0" xmlns="http://backend.userland.com/rss2"><channel><title>S</title>
			<item><title>E</title><guid>g</guid><description>a<br>b</description></item>
			</channel></rss>`,
	} {
		t.Run(name, func(t *testing.T) {
			f, err := ParseFeed([]byte(feed))
			if err != nil {
				t.Fatalf("ParseFeed: %v", err)
			}
			if f.Title != "S" || len(f.Episodes) != 1 || f.Episodes[0].Title != "E" {
				t.Fatalf("feed = %q with %+v, want S with episode E", f.Title, f.Episodes)
			}
		})
	}
}

func TestParseFeedMultipleFundingKeepsFirstWithURL(t *testing.T) {
	t.Parallel()
	feed, err := ParseFeed([]byte(`<rss xmlns:podcast="https://podcastindex.org/namespace/1.0"><channel>
		<title>S</title>
		<podcast:funding>no url here</podcast:funding>
		<podcast:funding url="https://a/support">First</podcast:funding>
		<podcast:funding url="https://b/support">Second</podcast:funding>
	</channel></rss>`))
	if err != nil {
		t.Fatalf("ParseFeed: %v", err)
	}
	if feed.FundingURL != "https://a/support" || feed.FundingMessage != "First" {
		t.Fatalf("funding = %q / %q, want the first tag with a url", feed.FundingURL, feed.FundingMessage)
	}
}

func TestParseFeedRejectsNonFeed(t *testing.T) {
	t.Parallel()
	if _, err := ParseFeed([]byte(`<html><body>not a feed</body></html>`)); err == nil {
		t.Fatal("expected error on non-feed XML")
	}
}

func TestParseDurationForms(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
