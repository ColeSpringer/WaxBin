package pathx

import "strings"

// reservedBases are the names Windows opens a device for instead of a file, in upper
// case: CON, PRN, AUX, NUL, CONIN$, CONOUT$, and COM and LPT with a digit from 1 to 9 or
// a superscript one, two or three.
var reservedBases = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}
	for _, d := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00b9", "\u00b2", "\u00b3"} {
		m["COM"+d], m["LPT"+d] = true, true
	}
	return m
}()

// ReservedName reports whether a path segment names a Windows device. Windows reads the
// name up to its first dot or colon, less trailing spaces, so "nul.txt" and "NUL .mp3"
// open the device too, at least before Windows 11. This is the standard library's Windows
// rule, applied on every platform so a tree stays portable to Windows.
func ReservedName(seg string) bool {
	return reservedBase(seg[:reservedStem(seg)])
}

// EscapeReserved disarms a reserved device name by putting '_' after its stem and keeping
// the rest: "con.mp3" becomes "con_.mp3", "NUL" becomes "NUL_", "Aux .mp3" becomes
// "Aux_ .mp3". Any other segment comes back unchanged.
func EscapeReserved(seg string) string {
	n := reservedStem(seg)
	if !reservedBase(seg[:n]) {
		return seg
	}
	return seg[:n] + "_" + seg[n:]
}

// reservedStem is the length of the part of seg Windows matches against the device names.
func reservedStem(seg string) int {
	end := len(seg)
	if i := strings.IndexAny(seg, ".:"); i >= 0 {
		end = i
	}
	return len(strings.TrimRight(seg[:end], " "))
}

func reservedBase(s string) bool {
	if len(s) < 3 || len(s) > len("CONOUT$") {
		return false
	}
	// Only ASCII letters fold, as in the standard library's check.
	return reservedBases[strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' {
			return r - ('a' - 'A')
		}
		return r
	}, s)]
}
