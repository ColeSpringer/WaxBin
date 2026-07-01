package podcast

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

func TestParseOPMLNestedAndDedup(t *testing.T) {
	doc := `<?xml version="1.0"?>
<opml version="2.0">
  <head><title>subs</title></head>
  <body>
    <outline text="Tech">
      <outline text="Show A" type="rss" xmlUrl="https://a.example/feed"/>
      <outline text="Show B" title="Show B" type="rss" xmlUrl="https://b.example/feed"/>
    </outline>
    <outline text="Show A dup" type="rss" xmlUrl="https://a.example/feed"/>
    <outline text="folder only with no url"/>
  </body>
</opml>`
	entries, err := ParseOPML([]byte(doc))
	if err != nil {
		t.Fatalf("ParseOPML: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d (want 2 after dedup, folders skipped): %+v", len(entries), entries)
	}
	if entries[0].FeedURL != "https://a.example/feed" || entries[1].FeedURL != "https://b.example/feed" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestOPMLRoundTrip(t *testing.T) {
	in := []model.OPMLEntry{
		{Title: "Show A", FeedURL: "https://a.example/feed"},
		{Title: "Show B", FeedURL: "https://b.example/feed"},
	}
	var buf bytes.Buffer
	if err := WriteOPML(&buf, "mine", in); err != nil {
		t.Fatalf("WriteOPML: %v", err)
	}
	out, err := ParseOPML(buf.Bytes())
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(out) != 2 || out[0].FeedURL != in[0].FeedURL || out[1].FeedURL != in[1].FeedURL {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestParseOPMLDepthBoundRejects(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<opml version="2.0"><body>`)
	depth := maxOPMLDepth + 50
	for i := 0; i < depth; i++ {
		b.WriteString("<outline>")
	}
	for i := 0; i < depth; i++ {
		b.WriteString("</outline>")
	}
	b.WriteString(`</body></opml>`)
	if _, err := ParseOPML([]byte(b.String())); err == nil {
		t.Fatal("expected an error on excessively nested OPML, got nil (would risk a crash)")
	}
}
