package organize

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzSanitizeSegment asserts the path-segment sanitizer's invariants for any
// input: the result is non-empty, within the byte cap, free of separators and
// control characters, itself reported safe, and idempotent (organizing an
// already-organized name is a no-op).
func FuzzSanitizeSegment(f *testing.F) {
	for _, s := range []string{
		"normal.mp3", "CON.mp3", "aux", "a/b\\c", "  trailing . ", "..", ".", "",
		"\x00\x01\x1f", "日本語のタイトル", "café", "é", strings.Repeat("x", 400),
		"nul", "com9.flac", "<>:\"|?*",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, seg string) {
		out := sanitizeSegment(seg)
		if out == "" {
			t.Fatalf("sanitizeSegment(%q) is empty", seg)
		}
		if len(out) > maxSegmentBytes {
			t.Fatalf("sanitizeSegment(%q) = %q exceeds byte cap (%d)", seg, out, len(out))
		}
		if strings.ContainsAny(out, `/\`) {
			t.Fatalf("sanitized %q still has a path separator: %q", seg, out)
		}
		for _, r := range out {
			if r < 0x20 {
				t.Fatalf("sanitized %q still has a control character: %q", seg, out)
			}
		}
		if !utf8.ValidString(out) {
			t.Fatalf("sanitized %q is not valid UTF-8: %q", seg, out)
		}
		if reason := UnsafeSegmentReason(out); reason != "" {
			t.Fatalf("sanitized %q = %q is still unsafe: %s", seg, out, reason)
		}
		if again := sanitizeSegment(out); again != out {
			t.Fatalf("sanitizeSegment not idempotent: %q -> %q -> %q", seg, out, again)
		}
	})
}

// FuzzRenderTemplate feeds arbitrary template strings through the path template
// renderer to prove it never panics or fails to terminate on malformed markup
// (unbalanced groups/braces, stray delimiters).
func FuzzRenderTemplate(f *testing.F) {
	for _, s := range []string{
		"{artist}/{album} ({year})/{track:02} - {title}",
		"{disc?}{track}", "<{missing}>literal", "}}}{{{", "{unterminated",
		"<><<>>", "{track:99}", "{}", "{artist", "artist}", "{a}<{b}<{c}>>",
	} {
		f.Add(s)
	}
	fields := map[string]fieldVal{
		"artist": {s: "The Artist"},
		"album":  {s: "Album"},
		"title":  {s: "Title"},
		"year":   {n: 1999, isNum: true},
		"track":  {n: 3, isNum: true},
		"disc":   {}, // empty optional
	}
	f.Fuzz(func(t *testing.T, tmpl string) {
		// A malformed template must error or render, never panic or loop forever.
		_, _ = renderTemplate(tmpl, fields)
	})
}
