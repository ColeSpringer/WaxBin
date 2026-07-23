package podcast

import "testing"

// FuzzParseFeed ensures the RSS/iTunes/Podcasting 2.0 feed parser never panics on
// arbitrary bytes (feeds are attacker-influenced input from the network).
func FuzzParseFeed(f *testing.F) {
	f.Add([]byte(`<rss version="2.0"><channel><title>Show</title>` +
		`<item><title>Ep</title><guid>g1</guid><enclosure url="http://h/e.mp3" type="audio/mpeg"/></item>` +
		`</channel></rss>`))
	f.Add([]byte(`<?xml version="1.0"?><rss><channel></channel></rss>`))
	f.Add([]byte(`<rss><channel><item><pubDate>not a date</pubDate></item></channel></rss>`))
	f.Add([]byte(``))
	f.Add([]byte(`not xml at all`))
	f.Add([]byte(`<rss><channel>` + string([]byte{0x00, 0x01, 0x02}) + `</channel></rss>`))
	f.Add([]byte(`<rss xmlns:podcast="https://podcastindex.org/namespace/1.0"><channel><title>S</title>` +
		`<podcast:funding url="https://h/support">Support us</podcast:funding><podcast:medium>music</podcast:medium>` +
		`<podcast:person role="Host" group="Cast" img="https://h/p.jpg" href="https://h/p">Jane</podcast:person>` +
		`<item><title>E</title><podcast:person role="guest">Joe</podcast:person>` +
		`<podcast:soundbite startTime="73.0" duration="60.5">Best bit</podcast:soundbite></item>` +
		`</channel></rss>`))
	f.Add([]byte(`<rss xmlns:podcast="https://podcastindex.org/namespace/1.0"><channel><title>S</title>` +
		`<item><podcast:soundbite startTime="-1" duration="0"></podcast:soundbite>` +
		`<podcast:soundbite startTime="nan" duration="inf">x</podcast:soundbite>` +
		`<podcast:person>   </podcast:person></item></channel></rss>`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// A malformed feed must error cleanly, never panic.
		_, _ = ParseFeed(data)
	})
}
