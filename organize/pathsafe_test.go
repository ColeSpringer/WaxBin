package organize

import (
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestUnsafeSegmentReasonMatchesSanitizer(t *testing.T) {
	// The audit detector must agree with the sanitizer: a name organize would
	// rewrite is flagged unsafe, and a name it leaves alone is reported safe. This
	// locks the two together (they drifted on leading spaces, since sanitizeSegment trims
	// both sides while the detector only checked trailing). NFC/diacritic folding is
	// excluded: sanitizeSegment NFC-normalizes but both forms are valid on disk, so
	// the seeds below are all already-NFC to keep the comparison about portability.
	segs := []string{
		"Normal Title.mp3", // safe
		"track 02.flac",    // safe
		" leading.mp3",     // rewritten: leading space
		"trailing .mp3",    // safe: the space is mid-name (before the extension dot)
		"trailing. ",       // rewritten: trailing space/dot
		"what?.flac",       // rewritten: illegal character
		"con.mp3",          // rewritten: reserved device name
		"a/b.flac",         // rewritten: separator
	}
	for _, seg := range segs {
		safe := UnsafeSegmentReason(seg) == ""
		rewritten := sanitizeSegment(seg) != seg
		if safe == rewritten {
			t.Errorf("detector/sanitizer disagree for %q: safe=%v, sanitizeSegment=%q",
				seg, safe, sanitizeSegment(seg))
		}
	}
}

func TestSanitizeSegmentReservedDeviceNames(t *testing.T) {
	cases := map[string]string{
		"CON":       "CON_",
		"nul":       "nul_",
		"con.mp3":   "con_.mp3",
		"COM1":      "COM1_",
		"lpt9.flac": "lpt9_.flac",
		"console":   "console", // only exact device names are reserved
		"NULL":      "NULL",
	}
	for in, want := range cases {
		if got := sanitizeSegment(in); got != want {
			t.Errorf("sanitizeSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeSegmentTrailingDotsAndSpaces(t *testing.T) {
	// Windows silently strips trailing dots/spaces, so the catalog path would
	// disagree with disk; WaxBin strips them itself.
	if got := sanitizeSegment("Album.  "); got != "Album" {
		t.Errorf("trailing dot/space not stripped: %q", got)
	}
	if got := sanitizeSegment("  spaced  "); got != "spaced" {
		t.Errorf("surrounding space not trimmed: %q", got)
	}
}

func TestSanitizeSegmentNFC(t *testing.T) {
	// A decomposed "é" (e + combining acute) must compose to the single NFC code
	// point so it cannot collide with a precomposed twin on a byte-preserving FS.
	decomposed := "Caf" + "é"
	got := sanitizeSegment(decomposed)
	if !norm.NFC.IsNormalString(got) {
		t.Fatalf("segment not NFC-normalized: %q", got)
	}
	if got != "Café" {
		t.Fatalf("NFC fold = %q, want precomposed Café", got)
	}
}

func TestCapSegmentBytesKeepsExtension(t *testing.T) {
	long := strings.Repeat("a", 400) + ".flac"
	got := sanitizeSegment(long)
	if len(got) > maxSegmentBytes {
		t.Fatalf("segment not capped: %d bytes", len(got))
	}
	if !strings.HasSuffix(got, ".flac") {
		t.Fatalf("extension lost when capping: %q", got)
	}
}

func TestTruncateUTF8RuneBoundary(t *testing.T) {
	// A 3-byte rune repeated; cutting at a non-multiple of 3 must back up to a
	// boundary rather than emit a partial sequence.
	s := strings.Repeat("世", 200) // 600 bytes
	got := capSegmentBytes(s)
	if len(got) > maxSegmentBytes {
		t.Fatalf("not capped: %d", len(got))
	}
	if !norm.NFC.IsNormalString(got) || strings.ContainsRune(got, '�') {
		t.Fatalf("capping split a rune: %q", got)
	}
}

func TestFoldFieldPreservesEmpty(t *testing.T) {
	// foldField must return "" for empty input (so optional groups drop), unlike
	// sanitizeSegment which returns "_".
	if got := foldField(""); got != "" {
		t.Fatalf("foldField(\"\") = %q, want empty", got)
	}
	if got := foldField("AC/DC"); got != "AC_DC" {
		t.Fatalf("foldField separator fold = %q", got)
	}
}
