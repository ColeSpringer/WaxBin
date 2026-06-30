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

// illegalToUnderscore folds a path-hostile rune to '_': control characters and
// the cross-platform-illegal set (path separators, drive colon, the Windows
// wildcard/redirect characters). Shared by segment and field sanitizing so both
// neutralize the same characters.
func illegalToUnderscore(r rune) rune {
	switch {
	case r < 0x20: // control characters
		return '_'
	case strings.ContainsRune(`/\:*?"<>|`, r):
		return '_'
	default:
		return r
	}
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
	stem, ext := seg, ""
	if dot := strings.IndexByte(seg, '.'); dot >= 0 {
		stem, ext = seg[:dot], seg[dot:]
	}
	if reservedDeviceNames[strings.ToLower(stem)] {
		return stem + "_" + ext
	}
	return seg
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
