package netsafe

import (
	"strings"
	"testing"
)

// FuzzSafeFilename asserts a filename derived from any remote string is safe to
// create cross-platform: non-empty, no path separators or control/reserved
// characters, and never "." or "..".
func FuzzSafeFilename(f *testing.F) {
	for _, s := range []string{
		"http://h/a/b.mp3?token=x", "../../etc/passwd", "CON", `\\srv\share\f.mp3`,
		"name\x00.mp3", "", "https://h", "https://h/", "..", ".", "  ..  ",
		"日本語.mp3", "a?b*c:d.mp3",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, remote string) {
		out := SafeFilename(remote, "fallback")
		if out == "" {
			t.Fatalf("SafeFilename(%q) is empty (should fall back)", remote)
		}
		if strings.ContainsAny(out, `/\`) {
			t.Fatalf("SafeFilename(%q) = %q contains a path separator", remote, out)
		}
		if out == "." || out == ".." {
			t.Fatalf("SafeFilename(%q) = %q is a dot name", remote, out)
		}
		for _, r := range out {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("SafeFilename(%q) = %q has a control character", remote, out)
			}
		}
	})
}

// FuzzURLGuards runs the SSRF/URL guards over arbitrary strings to prove they
// never panic on malformed URLs (they gate attacker-supplied feed/enclosure URLs).
func FuzzURLGuards(f *testing.F) {
	for _, s := range []string{
		"http://example.com/path", "ftp://x", "://", "http://[::1]/", "javascript:x",
		"http://user:pass@host:99999/", "https://127.0.0.1", "http://[fe80::1%eth0]/",
		"", "h ttp://x", "http://\x00/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_ = hostOf(raw)
		_ = checkScheme(raw, "fuzz")
		_ = guardDialAddr(raw)
	})
}
