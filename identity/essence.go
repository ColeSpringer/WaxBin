package identity

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/waxerr"
)

const (
	id3v1Len    = 128 // trailing ID3v1 tag length
	id3v2Header = 10  // leading ID3v2 header length
)

// EssenceHash returns the decoder-independent essence hash for the file at path.
// Dispatch is by extension: MP3 and FLAC get tag-stripped essence hashes, and
// every other format returns contentHash with no extra read or PCM decode.
// Extension-driven routing, rather than magic sniffing, keeps ADTS .aac out of
// the MP3 path even though its frame sync overlaps the MPEG sync word.
func EssenceHash(path, contentHash string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".mp3" && ext != ".flac" {
		return contentHash, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", waxerr.Wrap(waxerr.CodeIO, "identity.EssenceHash", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", waxerr.Wrap(waxerr.CodeIO, "identity.EssenceHash", err)
	}
	size := st.Size()

	if ext == ".mp3" {
		return hashMP3Essence(f, size)
	}
	return hashFLACEssence(f, size)
}

// hashMP3Essence hashes an MP3's audio frames, skipping a leading ID3v2 tag (and
// its optional footer) and a trailing ID3v1 tag.
func hashMP3Essence(f *os.File, size int64) (string, error) {
	start := int64(0)
	var hdr [id3v2Header]byte
	if _, err := f.ReadAt(hdr[:], 0); err == nil && string(hdr[:3]) == "ID3" {
		tagSize := syncsafe(hdr[6:10])
		start = id3v2Header + int64(tagSize)
		if hdr[5]&0x10 != 0 { // footer-present flag
			start += id3v2Header
		}
	}

	end := size
	if size >= id3v1Len {
		var tail [3]byte
		if _, err := f.ReadAt(tail[:], size-id3v1Len); err == nil && string(tail[:]) == "TAG" {
			end = size - id3v1Len
		}
	}

	if start < 0 || end < start || start > size {
		start, end = 0, size // corrupt offsets: fall back to whole file
	}
	return hashRegion(f, start, end-start)
}

// hashFLACEssence hashes a FLAC's audio frames, skipping the "fLaC" marker and
// all metadata blocks.
func hashFLACEssence(f *os.File, size int64) (string, error) {
	off := int64(4) // past the "fLaC" marker
	for {
		var bh [4]byte
		if _, err := f.ReadAt(bh[:], off); err != nil {
			return hashRegion(f, 0, size) // truncated header: hash whole file
		}
		last := bh[0]&0x80 != 0
		blockLen := int64(bh[1])<<16 | int64(bh[2])<<8 | int64(bh[3])
		off += 4 + blockLen
		if last {
			break
		}
		if off >= size {
			return hashRegion(f, 0, size)
		}
	}
	if off > size {
		off = 0
	}
	return hashRegion(f, off, size-off)
}

// hashRegion hashes length bytes starting at offset.
func hashRegion(f *os.File, offset, length int64) (string, error) {
	if length < 0 {
		length = 0
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, offset, length)); err != nil {
		return "", waxerr.Wrap(waxerr.CodeIO, "identity.EssenceHash", err)
	}
	return tag(h), nil
}

// syncsafe decodes a 4-byte ID3v2 synchsafe integer (7 significant bits each).
func syncsafe(b []byte) uint32 {
	return uint32(b[0]&0x7f)<<21 | uint32(b[1]&0x7f)<<14 | uint32(b[2]&0x7f)<<7 | uint32(b[3]&0x7f)
}
