package identity

import "testing"

func TestPodcastKeyPrefersGUID(t *testing.T) {
	guidKey := PodcastKey("urn:uuid:ABC-123", "https://example.com/feed.xml")
	if guidKey != "pguid:urn:uuid:abc-123" {
		t.Fatalf("guid key = %q", guidKey)
	}
	// Without a guid, the feed URL anchors identity.
	urlKey := PodcastKey("", "https://example.com/feed.xml")
	if urlKey != "feed:http://example.com/feed.xml" {
		t.Fatalf("url key = %q", urlKey)
	}
	if PodcastKey("", "") != "" {
		t.Fatal("empty inputs should yield empty key")
	}
}

func TestPodcastKeyNormalizesURL(t *testing.T) {
	// http/https and a trailing slash fold to one identity; the host case folds but
	// the path case is preserved.
	a := PodcastKey("", "HTTPS://Example.COM/Feed/")
	b := PodcastKey("", "http://example.com/Feed")
	if a != b {
		t.Fatalf("scheme/host/slash should fold: %q vs %q", a, b)
	}
	// A different path case is a different feed (paths can be case-sensitive).
	if PodcastKey("", "http://example.com/feed") == b {
		t.Fatal("path case must not fold")
	}
}

func TestEpisodeKeyScopedToPodcast(t *testing.T) {
	podA := PodcastKey("", "http://a.example/feed")
	podB := PodcastKey("", "http://b.example/feed")
	// The same bare guid under two feeds must not collide.
	if EpisodeKey(podA, "ep-1", "", "") == EpisodeKey(podB, "ep-1", "", "") {
		t.Fatal("same guid under different podcasts must not collide")
	}
	// A guid is opaque/case-sensitive: two case-differing guids stay distinct.
	if EpisodeKey(podA, "Ep-1", "", "") == EpisodeKey(podA, "ep-1", "", "") {
		t.Fatal("guid case must be preserved")
	}
}

func TestEpisodeKeyFallbacks(t *testing.T) {
	pod := PodcastKey("", "http://a.example/feed")
	// No guid: fall back to the enclosure URL, then the title.
	if got := EpisodeKey(pod, "", "http://a.example/ep1.mp3", "Title"); got == "" {
		t.Fatal("enclosure fallback should key")
	}
	byTitle := EpisodeKey(pod, "", "", "My Episode")
	if byTitle == "" {
		t.Fatal("title fallback should key")
	}
	// Nothing to key on -> empty.
	if EpisodeKey(pod, "", "", "") != "" {
		t.Fatal("no guid/enclosure/title should yield empty key")
	}
	// No podcast scope -> empty.
	if EpisodeKey("", "ep-1", "", "") != "" {
		t.Fatal("missing podcast scope should yield empty key")
	}
}
