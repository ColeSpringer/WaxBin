package meta

import (
	"encoding/binary"
	"io"
	"os"
	"strings"

	"github.com/colespringer/waxbin/model"
)

const (
	flacBlockStreamInfo = 0
	flacBlockVorbisCmt  = 4
	flacMaxComment      = 1 << 20 // sanity cap on a single comment block (1 MiB)
)

// readFLAC parses STREAMINFO (audio properties) and the Vorbis comment block
// (tags) from a FLAC file into t. Best-effort: malformed input returns nil.
func readFLAC(path string, t *model.Tags) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var marker [4]byte
	if _, err := io.ReadFull(f, marker[:]); err != nil || string(marker[:]) != "fLaC" {
		return nil
	}

	for {
		var bh [4]byte
		if _, err := io.ReadFull(f, bh[:]); err != nil {
			return nil
		}
		last := bh[0]&0x80 != 0
		blockType := bh[0] & 0x7f
		blockLen := int(bh[1])<<16 | int(bh[2])<<8 | int(bh[3])

		switch blockType {
		case flacBlockStreamInfo:
			buf := make([]byte, blockLen)
			if _, err := io.ReadFull(f, buf); err != nil {
				return nil
			}
			parseStreamInfo(buf, t)
		case flacBlockVorbisCmt:
			if blockLen < 0 || blockLen > flacMaxComment {
				return nil
			}
			buf := make([]byte, blockLen)
			if _, err := io.ReadFull(f, buf); err != nil {
				return nil
			}
			parseVorbisComments(buf, t)
		default:
			if _, err := f.Seek(int64(blockLen), io.SeekCurrent); err != nil {
				return nil
			}
		}
		if last {
			break
		}
	}
	return nil
}

// parseStreamInfo decodes the packed STREAMINFO fields (sample rate, channels,
// bit depth, total samples -> duration).
func parseStreamInfo(b []byte, t *model.Tags) {
	if len(b) < 18 {
		return
	}
	sampleRate := int(b[10])<<12 | int(b[11])<<4 | int(b[12])>>4
	channels := int(b[12]>>1)&0x07 + 1
	bitDepth := (int(b[12]&0x01)<<4 | int(b[13])>>4) + 1
	totalSamples := int64(b[13]&0x0f)<<32 | int64(b[14])<<24 | int64(b[15])<<16 | int64(b[16])<<8 | int64(b[17])

	t.SampleRate = sampleRate
	t.Channels = channels
	t.BitDepth = bitDepth
	if sampleRate > 0 && totalSamples > 0 {
		t.DurationMS = totalSamples * 1000 / int64(sampleRate)
	}
}

// parseVorbisComments decodes the little-endian comment list into t.
func parseVorbisComments(b []byte, t *model.Tags) {
	r := bytesReader{b: b}
	vendorLen, ok := r.u32()
	if !ok || !r.skip(int(vendorLen)) {
		return
	}
	count, ok := r.u32()
	if !ok {
		return
	}
	for i := uint32(0); i < count; i++ {
		n, ok := r.u32()
		if !ok {
			return
		}
		field, ok := r.bytes(int(n))
		if !ok {
			return
		}
		applyVorbisComment(t, string(field))
	}
}

func applyVorbisComment(t *model.Tags, kv string) {
	eq := strings.IndexByte(kv, '=')
	if eq < 0 {
		return
	}
	key := strings.ToUpper(strings.TrimSpace(kv[:eq]))
	val := strings.TrimSpace(kv[eq+1:])
	if val == "" {
		return
	}
	switch key {
	case "TITLE":
		t.Title = val
	case "ARTIST":
		t.Artist = val
	case "ALBUMARTIST", "ALBUM ARTIST":
		t.AlbumArtist = val
	case "ALBUM":
		t.Album = val
	case "TRACKNUMBER":
		t.TrackNo = parseLeadingInt(val)
	case "DISCNUMBER":
		t.DiscNo = parseLeadingInt(val)
	case "DATE", "YEAR":
		if y := parseLeadingInt(val); y > 0 {
			t.Year = y
		}
	case "GENRE":
		t.Genre = val
	case "MUSICBRAINZ_TRACKID":
		t.MBID = val
	}
}

// bytesReader is a tiny bounds-checked little-endian cursor over a byte slice.
type bytesReader struct {
	b   []byte
	pos int
}

func (r *bytesReader) u32() (uint32, bool) {
	if r.pos+4 > len(r.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v, true
}

func (r *bytesReader) bytes(n int) ([]byte, bool) {
	if n < 0 || r.pos+n > len(r.b) {
		return nil, false
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v, true
}

func (r *bytesReader) skip(n int) bool {
	if n < 0 || r.pos+n > len(r.b) {
		return false
	}
	r.pos += n
	return true
}
