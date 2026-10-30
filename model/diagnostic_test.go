package model

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCapDetail verifies the length bound and that truncation lands on a rune
// boundary. Upstream sanitizes a warning message but does not bound it, and a
// message can embed a file-derived snippet, and sanitizing defends against
// injection rather than against size.
func TestCapDetail(t *testing.T) {
	if got := CapDetail("short"); got != "short" {
		t.Errorf("CapDetail(short) = %q", got)
	}
	long := strings.Repeat("a", 1<<20)
	if got := CapDetail(long); len(got) != MaxDetailBytes {
		t.Errorf("len = %d, want %d", len(got), MaxDetailBytes)
	}
	// Multi-byte runes: the cap must not slice one in half.
	multi := strings.Repeat("é", 1<<20) // 2 bytes each
	got := CapDetail(multi)
	if len(got) > MaxDetailBytes {
		t.Errorf("len = %d, want <= %d", len(got), MaxDetailBytes)
	}
	if !utf8.ValidString(got) {
		t.Error("capped detail is not valid UTF-8")
	}
	emoji := strings.Repeat("🎵", 1<<20) // 4 bytes each; 512 is not a multiple of 4
	got = CapDetail(emoji)
	if !utf8.ValidString(got) {
		t.Error("capped emoji detail is not valid UTF-8")
	}
	if len(got) > MaxDetailBytes {
		t.Errorf("len = %d, want <= %d", len(got), MaxDetailBytes)
	}
}

// TestCapDetailWithTail: the body gives way so the tail survives whole, still on a
// rune boundary.
func TestCapDetailWithTail(t *testing.T) {
	if got := CapDetailWithTail("short", "; tail"); got != "short; tail" {
		t.Errorf("CapDetailWithTail(short) = %q", got)
	}
	got := CapDetailWithTail(strings.Repeat("é", 1<<10), "; tail")
	if len(got) > MaxDetailBytes || !strings.HasSuffix(got, "é; tail") || !utf8.ValidString(got) {
		t.Errorf("capped = %d bytes ending %q, want <= %d ending in a whole rune and the tail",
			len(got), got[len(got)-10:], MaxDetailBytes)
	}
}

// TestCapDetailEscapesWhatATerminalActsOn: a detail is one line of terminal output, and
// its text can come from the file (a decoder's error, a line of a cue sheet). Controls,
// bytes that are not UTF-8, bidirectional overrides and line separators come out as
// escapes, while the joiners emoji need stay.
func TestCapDetailEscapesWhatATerminalActsOn(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"bad\x1b[31m frame", `bad\x1b[31m frame`},
		{"one\ntwo\tthree\r", `one\x0atwo\x09three\x0d`},
		{"del\x7f c1\u0085", `del\x7f c1\u0085`},
		{"rtl \u202egnp.exe", `rtl \u202egnp.exe`},
		{"zero\u200bwidth\ufeff sep\u2028", `zero\u200bwidth\ufeff sep\u2028`},
		{"not utf-8 \xff\xfe", `not utf-8 \xff\xfe`},
		{"family \U0001F468\u200d\U0001F469 é", "family \U0001F468\u200d\U0001F469 é"},
	} {
		if got := CapDetail(c.in); got != c.want {
			t.Errorf("CapDetail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := CapDetailWithTail("a\nb", "; tail"); got != `a\x0ab; tail` {
		t.Errorf("CapDetailWithTail = %q, want the body escaped", got)
	}
}

// TestCapText: a stored line of feed text reads as one line, cut at a count of
// characters, with what a terminal would act on escaped (bidirectional embeddings,
// overrides and isolates among it) and the right-to-left marks and joiners kept.
func TestCapText(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{strings.Repeat("é", 1001), 1000, strings.Repeat("é", 1000)},
		{"Episode 12:\n   The Return", 100, "Episode 12: The Return"},
		{"tab\there\r\nand\u2028there", 100, "tab here and there"},
		{"next\u0085line", 100, "next line"},
		{"two  spaces", 100, "two  spaces"},
		{"ansi\x1b[31m red", 100, `ansi\x1b[31m red`},
		{"del\x7f c1\u009b", 100, `del\x7f c1\u009b`},
		{"\xff not utf-8", 100, `\xff not utf-8`},
		{"rtl \u200fmark \u200eltr \u061calm", 100, "rtl \u200fmark \u200eltr \u061calm"},
		{"embed \u202b override \u202e isolate \u2067x\u2069", 100, `embed \u202b override \u202e isolate \u2067x\u2069`},
		{"emoji \U0001F469\u200d\U0001F4BB", 100, "emoji \U0001F469\u200d\U0001F4BB"},
		{"two words", 4, "two"},
		{"cut\x1b", 5, "cut"},
		{"     ", 2, ""},
		{"ends spaced ", 100, "ends spaced "},
	} {
		if got := CapText(c.in, c.max); got != c.want {
			t.Errorf("CapText(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}
