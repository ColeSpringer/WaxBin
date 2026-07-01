package organize

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// maxSegmentBytes caps one path component. 255 is the NAME_MAX of ext4, APFS,
// NTFS, and most others; capping here keeps a managed tree portable across all of
// them rather than letting one filesystem's longer limit produce names another
// cannot store.
const maxSegmentBytes = 255

// reservedDeviceNames are Windows reserved device names. A file named CON or NUL
// (with or without an extension) is unusable on Windows, so WaxBin avoids
// producing one on every platform to keep a managed tree portable.
var reservedDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// illegalSegmentChars is the cross-platform-illegal character set for a single
// path segment: path separators, the drive colon, and the Windows wildcard/
// redirect characters. Shared by the sanitizer and the audit's UnsafeSegmentReason
// so detection and rewriting stay in lockstep.
const illegalSegmentChars = `/\:*?"<>|`

// illegalToUnderscore folds a path-hostile rune to '_': control characters and
// the illegal set above. Shared by segment and field sanitizing so both
// neutralize the same characters.
func illegalToUnderscore(r rune) rune {
	switch {
	case r < 0x20: // control characters
		return '_'
	case strings.ContainsRune(illegalSegmentChars, r):
		return '_'
	default:
		return r
	}
}

// segmentStem returns the part of a segment before its first dot, the portion the
// Windows reserved-device-name rule applies to ("con.mp3" -> "con").
func segmentStem(seg string) string {
	if dot := strings.IndexByte(seg, '.'); dot >= 0 {
		return seg[:dot]
	}
	return seg
}

// isReservedName reports whether a segment's stem is a Windows reserved device name.
func isReservedName(seg string) bool {
	return reservedDeviceNames[strings.ToLower(segmentStem(seg))]
}

// UnsafeSegmentReason reports why a single on-disk path segment is not portable,
// or "" when it is safe. It applies the same portability rules sanitizeSegment
// enforces when organizing (control and illegal characters, Windows reserved
// device names, a leading or trailing space, a trailing dot, and the 255-byte
// segment cap), so audit can flag on-disk names organize would have to rewrite.
// NFC/NFD differences are not flagged, since both forms are valid on disk; only
// true portability hazards are. An empty segment is safe (callers filter those).
func UnsafeSegmentReason(seg string) string {
	if seg == "" {
		return ""
	}
	for _, r := range seg {
		if r < 0x20 {
			return "control character"
		}
		if strings.ContainsRune(illegalSegmentChars, r) {
			return "illegal character " + string(r)
		}
	}
	if isReservedName(seg) {
		return "Windows reserved device name"
	}
	// sanitizeSegment trims leading and trailing spaces and trailing dots, so flag
	// any of those. A leading-space name is rewritten by organize, so it is not safe.
	if strings.TrimLeft(seg, " ") != seg {
		return "leading space"
	}
	if strings.TrimRight(seg, " .") != seg {
		return "trailing space or dot"
	}
	if len(seg) > maxSegmentBytes {
		return "segment exceeds 255 bytes"
	}
	return ""
}

// sanitizeSegment makes one path segment filesystem-safe and portable:
// illegal characters become '_', surrounding spaces and trailing dots are
// trimmed, the result is Unicode NFC-normalized (so the same name folds
// identically on NFC and NFD filesystems and cannot collide with a differently
// composed twin), Windows reserved device names are escaped, and the byte length
// is capped so it fits every common filesystem.
func sanitizeSegment(seg string) string {
	seg = strings.Map(illegalToUnderscore, seg)
	// Compose to NFC. macOS stores NFD and Linux stores bytes verbatim, so a
	// precomposed "é" and a decomposed "é" would otherwise be two distinct
	// on-disk names for the same title; normalizing makes organize idempotent and
	// collision detection meaningful across platforms.
	seg = norm.NFC.String(seg)
	seg = strings.Trim(seg, " ")
	// A trailing dot or space is silently stripped by Windows, which would make the
	// catalog path disagree with the real on-disk name; strip it ourselves.
	seg = strings.TrimRight(seg, " .")
	if seg == "" {
		return "_"
	}
	seg = escapeReservedName(seg)
	return capSegmentBytes(seg)
}

// foldField neutralizes a metadata value before it is substituted into a path
// segment: path separators and other illegal characters fold to '_', the value is
// NFC-normalized, and surrounding space/dots are trimmed. Unlike sanitizeSegment
// it preserves an empty result, so an absent optional field stays empty and its
// surrounding template group drops; reserved-name and length rules apply to the
// whole assembled segment, not to a single field.
func foldField(s string) string {
	if s == "" {
		return ""
	}
	s = strings.Map(illegalToUnderscore, s)
	s = norm.NFC.String(s)
	s = strings.Trim(s, " ")
	return strings.TrimRight(s, " .")
}

// escapeReservedName disarms a Windows reserved device name by appending '_' to
// the stem (the part before the first dot, matched case-insensitively), keeping
// any extension intact: "con.mp3" -> "con_.mp3", "NUL" -> "NUL_". Every other name
// is returned unchanged.
func escapeReservedName(seg string) string {
	if !isReservedName(seg) {
		return seg
	}
	stem := segmentStem(seg)
	return stem + "_" + seg[len(stem):]
}

// capSegmentBytes truncates seg to maxSegmentBytes, preferring to keep the file
// extension and never splitting a UTF-8 rune. The base is trimmed first so
// "verylongname.flac" stays "...flac" rather than losing the extension.
func capSegmentBytes(seg string) string {
	if len(seg) <= maxSegmentBytes {
		return seg
	}
	ext := ""
	if dot := strings.LastIndexByte(seg, '.'); dot > 0 && len(seg)-dot <= 16 {
		ext = seg[dot:]
	}
	budget := maxSegmentBytes - len(ext)
	if budget <= 0 { // pathological extension: cap the whole thing
		return truncateUTF8(seg, maxSegmentBytes)
	}
	base := truncateUTF8(seg[:len(seg)-len(ext)], budget)
	base = strings.TrimRight(base, " .")
	if base == "" {
		base = "_"
	}
	return base + ext
}

// truncateUTF8 returns the longest prefix of s that is at most n bytes and ends
// on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back up to the start of the rune straddling the cut so we never emit a
	// partial multi-byte sequence.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
