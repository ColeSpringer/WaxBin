package meta

import (
	"io"
	"os"
	"strings"
	"unicode/utf16"

	"github.com/colespringer/waxbin/model"
)

// readID3v2 parses an ID3v2.2/2.3/2.4 tag's text frames into t. It is
// best-effort: a missing or malformed tag returns nil with t left as-is.
func readID3v2(path string, t *model.Tags) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var hdr [10]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil
	}
	if string(hdr[:3]) != "ID3" {
		return nil
	}
	major := hdr[3]
	flags := hdr[5]
	size := int(syncsafe(hdr[6:10]))
	if size <= 0 {
		return nil
	}

	body := make([]byte, size)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil
	}

	pos := 0
	if flags&0x40 != 0 && len(body) >= 4 { // extended header present: skip it
		ext := beInt(body[0:4])
		if major < 4 {
			ext += 4 // v2.3 size excludes its own 4 bytes
		}
		if ext > 0 && ext <= len(body) {
			pos = ext
		}
	}

	idLen, szLen := 4, 4
	if major == 2 { // ID3v2.2 uses 3-byte ids and 3-byte sizes
		idLen, szLen = 3, 3
	}

	for pos+idLen+szLen <= len(body) {
		id := string(body[pos : pos+idLen])
		if id[0] == 0 { // padding
			break
		}
		var frameSize int
		switch {
		case major == 4:
			frameSize = int(syncsafe(body[pos+idLen : pos+idLen+4]))
		case major == 2:
			frameSize = beInt3(body[pos+idLen : pos+idLen+3])
		default:
			frameSize = beInt(body[pos+idLen : pos+idLen+4])
		}
		header := idLen + szLen
		if major >= 3 {
			header += 2 // 2 bytes of frame flags
		}
		pos += header
		if pos > len(body) {
			break
		}
		// Compare against remaining space (not pos+frameSize) so a corrupt,
		// near-2^31 frameSize cannot overflow the addition on 32-bit platforms.
		if frameSize <= 0 || frameSize > len(body)-pos {
			break
		}
		frame := body[pos : pos+frameSize]
		pos += frameSize

		if id[0] == 'T' {
			applyTextFrame(t, id, decodeTextFrame(frame))
		}
	}
	return nil
}

// applyTextFrame maps a decoded text frame onto Tags. Both the 4-char (v2.3/2.4)
// and 3-char (v2.2) frame ids are recognized.
func applyTextFrame(t *model.Tags, id, val string) {
	val = strings.TrimSpace(val)
	if val == "" {
		return
	}
	switch id {
	case "TIT2", "TT2":
		t.Title = val
	case "TPE1", "TP1":
		t.Artist = val
	case "TPE2", "TP2":
		t.AlbumArtist = val
	case "TALB", "TAL":
		t.Album = val
	case "TRCK", "TRK":
		t.TrackNo = parseLeadingInt(val)
	case "TPOS", "TPA":
		t.DiscNo = parseLeadingInt(val)
	case "TYER", "TYE", "TDRC", "TDRL":
		if y := parseLeadingInt(val); y > 0 {
			t.Year = y
		}
	case "TCON", "TCO":
		t.Genre = cleanGenre(val)
	}
}

// cleanGenre drops a bare ID3v1 numeric reference like "(17)" but keeps a plain
// genre name.
func cleanGenre(s string) string {
	if strings.HasPrefix(s, "(") {
		if i := strings.IndexByte(s, ')'); i >= 0 {
			rest := strings.TrimSpace(s[i+1:])
			if rest != "" {
				return rest
			}
			return "" // pure "(NN)" reference; no taxonomy mapping here
		}
	}
	return s
}

// decodeTextFrame decodes an ID3v2 text frame body honoring its encoding byte.
func decodeTextFrame(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	enc := b[0]
	payload := b[1:]
	var s string
	switch enc {
	case 0: // ISO-8859-1
		runes := make([]rune, len(payload))
		for i, c := range payload {
			runes[i] = rune(c)
		}
		s = string(runes)
	case 1: // UTF-16 with BOM
		s = decodeUTF16(payload, false)
	case 2: // UTF-16BE, no BOM
		s = decodeUTF16(payload, true)
	default: // 3 == UTF-8 (and unknown encodings, treated as UTF-8)
		s = string(payload)
	}
	// Frames may carry NUL-separated multi-values; take the first, trim NULs.
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, "\x00")
}

func decodeUTF16(b []byte, bigEndian bool) string {
	if len(b) >= 2 {
		switch {
		case b[0] == 0xFF && b[1] == 0xFE:
			bigEndian, b = false, b[2:]
		case b[0] == 0xFE && b[1] == 0xFF:
			bigEndian, b = true, b[2:]
		}
	}
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		if bigEndian {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	return string(utf16.Decode(u))
}

func syncsafe(b []byte) uint32 {
	return uint32(b[0]&0x7f)<<21 | uint32(b[1]&0x7f)<<14 | uint32(b[2]&0x7f)<<7 | uint32(b[3]&0x7f)
}

func beInt(b []byte) int {
	return int(uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]))
}

func beInt3(b []byte) int {
	return int(uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]))
}
