package pathx

import "testing"

// TestReservedNameReadsTheStemAsWindowsDoes: a device name is reserved with any extension,
// with spaces before its dot, in any ASCII case, and in the forms the old rule missed (the
// superscript digits, CONIN$ and CONOUT$). Escaping puts '_' on the stem, which disarms it.
func TestReservedNameReadsTheStemAsWindowsDoes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		seg, escaped string
	}{
		{"CON", "CON_"},
		{"nul", "nul_"},
		{"con.mp3", "con_.mp3"},
		{"lpt9.flac", "lpt9_.flac"},
		{"Aux. Radio", "Aux_. Radio"},
		{"nul .mp3", "nul_ .mp3"},
		{"COM1  .flac", "COM1_  .flac"},
		{"COM\u00b9", "COM\u00b9_"},
		{"lpt\u00b2.mp3", "lpt\u00b2_.mp3"},
		{"CONIN$", "CONIN$_"},
		{"conout$.log", "conout$_.log"},
		{"aux:stream", "aux_:stream"},
		// Not device names.
		{"console", "console"},
		{"NULL", "NULL"},
		{"COM0", "COM0"},
		{"COM10.mp3", "COM10.mp3"},
		{" con.mp3", " con.mp3"},
		{"con_.mp3", "con_.mp3"},
		{"con\u0131n$", "con\u0131n$"}, // a dotless i upper-cases to I outside ASCII
		{"", ""},
	}
	for _, c := range cases {
		reserved := c.escaped != c.seg
		if got := ReservedName(c.seg); got != reserved {
			t.Errorf("ReservedName(%q) = %v, want %v", c.seg, got, reserved)
		}
		got := EscapeReserved(c.seg)
		if got != c.escaped {
			t.Errorf("EscapeReserved(%q) = %q, want %q", c.seg, got, c.escaped)
		}
		if ReservedName(got) {
			t.Errorf("EscapeReserved(%q) = %q is still reserved", c.seg, got)
		}
	}
}
