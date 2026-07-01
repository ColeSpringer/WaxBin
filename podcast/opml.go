package podcast

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// opmlDoc is the OPML structure used for podcast subscription exchange. Outlines
// can nest (feed clients group shows into folders), so an outline carries its own
// children and only the ones with an xmlUrl are subscriptions.
type opmlDoc struct {
	XMLName xml.Name    `xml:"opml"`
	Version string      `xml:"version,attr"`
	Head    opmlHead    `xml:"head"`
	Body    opmlOutline `xml:"body"`
}

type opmlHead struct {
	Title string `xml:"title"`
}

type opmlOutline struct {
	Text     string        `xml:"text,attr,omitempty"`
	Title    string        `xml:"title,attr,omitempty"`
	Type     string        `xml:"type,attr,omitempty"`
	XMLURL   string        `xml:"xmlUrl,attr,omitempty"`
	Outlines []opmlOutline `xml:"outline"`
}

// maxOPMLDepth bounds the nesting an OPML document may reach. OPML is untrusted
// input (imported from a file/URL); a hard cap rejects a pathologically nested
// document with a clean error instead of letting it drive unbounded work.
const maxOPMLDepth = 64

// ParseOPML reads an OPML document and returns every subscription it contains
// (any outline with an xmlUrl, including nested ones), de-duplicated by feed URL,
// preserving document order. It streams tokens with a depth bound rather than
// decoding into a recursive struct, so a deeply nested document cannot exhaust the
// stack.
func ParseOPML(data []byte) ([]model.OPMLEntry, error) {
	const op = "podcast.ParseOPML"
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.CharsetReader = charsetPassthrough

	var out []model.OPMLEntry
	seen := map[string]bool{}
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeInvalid, op, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxOPMLDepth {
				return nil, waxerr.New(waxerr.CodeInvalid, op, "OPML nesting exceeds the supported depth")
			}
			if !strings.EqualFold(t.Name.Local, "outline") {
				continue
			}
			var url, title, text string
			for _, a := range t.Attr {
				switch strings.ToLower(a.Name.Local) {
				case "xmlurl":
					url = strings.TrimSpace(a.Value)
				case "title":
					title = strings.TrimSpace(a.Value)
				case "text":
					text = strings.TrimSpace(a.Value)
				}
			}
			if url != "" && !seen[url] {
				seen[url] = true
				out = append(out, model.OPMLEntry{Title: firstNonEmpty(title, text), FeedURL: url})
			}
		case xml.EndElement:
			depth--
		}
	}
	return out, nil
}

// WriteOPML writes subscriptions as a flat OPML 2.0 document with the given head
// title. Each entry becomes an <outline type="rss" .../> line.
func WriteOPML(w io.Writer, title string, entries []model.OPMLEntry) error {
	const op = "podcast.WriteOPML"
	doc := opmlDoc{Version: "2.0", Head: opmlHead{Title: title}}
	for _, e := range entries {
		if strings.TrimSpace(e.FeedURL) == "" {
			continue
		}
		t := e.Title
		if t == "" {
			t = e.FeedURL
		}
		doc.Body.Outlines = append(doc.Body.Outlines, opmlOutline{
			Text: t, Title: t, Type: "rss", XMLURL: e.FeedURL,
		})
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(&doc); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return nil
}
