package podcast

import (
	"encoding/xml"
	"fmt"
	"slices"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// The bounds a feed's text is held to. A show's title is written into the search row of
// every one of its episodes twice, as the row's artist and album, so one without a bound
// costs its length times twice the episode count in the index.
const (
	MaxTitleChars       = 1000     // show, episode, chapter and soundbite titles, in characters
	MaxNameChars        = 256      // author, language, category, medium, media types, a person's name, role and group
	MaxDescriptionBytes = 64 << 10 // descriptions and content:encoded
	// MaxGUIDBytes bounds a GUID, which keys an episode's identity: one past it
	// identifies by its first MaxGUIDBytes bytes.
	MaxGUIDBytes = 2048
	// MaxURLBytes bounds a URL a feed names. A longer one is left out, since cutting it
	// would name another resource.
	MaxURLBytes        = 8 << 10
	MaxPersons         = 64 // person credits per show or episode, soundbites per episode, and any other tag a feed repeats
	MaxChaptersPerDoc  = 10000
	MaxEpisodesPerFeed = 50000
	MaxOPMLEntries     = 10000
)

// cloneFeed copies f deep enough that capping the copy leaves f's own slices alone.
func cloneFeed(f model.Feed) model.Feed {
	f.Persons = slices.Clone(f.Persons)
	f.Episodes = slices.Clone(f.Episodes)
	for i := range f.Episodes {
		f.Episodes[i] = cloneEpisode(f.Episodes[i])
	}
	return f
}

func cloneEpisode(e model.FeedEpisode) model.FeedEpisode {
	e.Persons = slices.Clone(e.Persons)
	e.Soundbites = slices.Clone(e.Soundbites)
	return e
}

// capFeed holds a feed's text and lists to their bounds, in place, so a caller that does
// not own f caps a cloneFeed. Episodes past MaxEpisodesPerFeed are left out and counted,
// which the parser has already done for a parsed feed.
func capFeed(f *model.Feed) {
	if n := len(f.Episodes); n > MaxEpisodesPerFeed {
		f.Episodes, f.EpisodesDropped = f.Episodes[:MaxEpisodesPerFeed], f.EpisodesDropped+n-MaxEpisodesPerFeed
	}
	f.Title = model.CapText(f.Title, MaxTitleChars)
	f.Author = model.CapText(f.Author, MaxNameChars)
	f.Description = model.CapBytes(f.Description, MaxDescriptionBytes)
	f.Link = capURL(f.Link)
	f.Language = model.CapText(f.Language, MaxNameChars)
	f.Category = model.CapText(f.Category, MaxNameChars)
	f.FundingURL = capURL(f.FundingURL)
	f.FundingMessage = model.CapText(f.FundingMessage, MaxTitleChars)
	f.Medium = model.CapText(f.Medium, MaxNameChars)
	f.GUID = model.CapBytes(f.GUID, MaxGUIDBytes)
	f.ImageURL = capURL(f.ImageURL)
	f.Persons = capPersons(f.Persons)
	for i := range f.Episodes {
		capEpisode(&f.Episodes[i])
	}
}

func capEpisode(e *model.FeedEpisode) {
	e.GUID = model.CapBytes(e.GUID, MaxGUIDBytes)
	e.Title = model.CapText(e.Title, MaxTitleChars)
	e.Description = model.CapBytes(e.Description, MaxDescriptionBytes)
	e.Link = capURL(e.Link)
	e.EnclosureURL = capURL(e.EnclosureURL)
	e.EnclosureType = model.CapText(e.EnclosureType, MaxNameChars)
	e.TranscriptURL = capURL(e.TranscriptURL)
	e.TranscriptType = model.CapText(e.TranscriptType, MaxNameChars)
	e.ChaptersURL = capURL(e.ChaptersURL)
	e.ImageURL = capURL(e.ImageURL)
	e.Persons = capPersons(e.Persons)
	e.Soundbites = e.Soundbites[:min(len(e.Soundbites), MaxPersons)]
	for i := range e.Soundbites {
		e.Soundbites[i].Title = model.CapText(e.Soundbites[i].Title, MaxTitleChars)
	}
}

func capPersons(ps []model.FeedPerson) []model.FeedPerson {
	ps = ps[:min(len(ps), MaxPersons)]
	for i := range ps {
		p := &ps[i]
		p.Name = model.CapText(p.Name, MaxNameChars)
		p.Role = model.CapText(p.Role, MaxNameChars)
		p.Group = model.CapText(p.Group, MaxNameChars)
		p.Img, p.Href = capURL(p.Img), capURL(p.Href)
	}
	return ps
}

func capURL(s string) string {
	if len(s) > MaxURLBytes {
		return ""
	}
	return s
}

// checkURLs refuses a URL a caller hands over past MaxURLBytes, which in a feed would be
// left out; urls pairs each field's name with its value.
func checkURLs(op string, urls ...string) error {
	for i := 0; i+1 < len(urls); i += 2 {
		if len(urls[i+1]) > MaxURLBytes {
			return waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("%s is longer than %d bytes", urls[i], MaxURLBytes))
		}
	}
	return nil
}

// repeated collects up to MaxPersons of a tag a feed repeats and skips the rest
// undecoded, so a feed that repeats one without end costs nothing past the bound.
type repeated[T any] struct{ list []T }

func (r *repeated[T]) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if len(r.list) >= MaxPersons {
		return d.Skip()
	}
	var v T
	if err := d.DecodeElement(&v, &start); err != nil {
		return err
	}
	r.list = append(r.list, v)
	return nil
}

// itemList collects up to MaxEpisodesPerFeed items and counts the rest, which it skips
// undecoded.
type itemList struct {
	items   []rssItem
	dropped int
}

func (l *itemList) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if len(l.items) >= MaxEpisodesPerFeed {
		l.dropped++
		return d.Skip()
	}
	var it rssItem
	if err := d.DecodeElement(&it, &start); err != nil {
		return err
	}
	l.items = append(l.items, it)
	return nil
}
