// Package testaudio synthesizes minimal tagged audio files for tests. It is not
// part of the public API; it lives under internal/ so identity, store, and
// facade tests can share one MP3 builder.
package testaudio

import "strconv"

// DefaultAudio returns a fixed, deterministic blob standing in for MP3 audio
// frames. It begins with an MPEG-1 Layer III sync word so it is recognized as
// audio essence even without an ID3 tag.
func DefaultAudio() []byte {
	b := make([]byte, 512)
	b[0], b[1] = 0xFF, 0xFB // MPEG audio frame sync
	for i := 2; i < len(b); i++ {
		b[i] = byte(i*7 + 3)
	}
	return b
}

// BuildMP3 builds an MP3 with an ID3v2.3 tag (TIT2/TPE1/TPE2/TALB/TRCK) over the
// default audio payload.
func BuildMP3(title, artist, album string, track int) []byte {
	return BuildMP3WithAudio(title, artist, album, track, DefaultAudio())
}

// BuildMP3WithAudio is BuildMP3 with a caller-supplied audio essence payload, so
// a test can vary tags while holding the essence constant (or vice versa).
func BuildMP3WithAudio(title, artist, album string, track int, audio []byte) []byte {
	var frames []byte
	frames = append(frames, id3v23TextFrame("TIT2", title)...)
	frames = append(frames, id3v23TextFrame("TPE1", artist)...)
	frames = append(frames, id3v23TextFrame("TPE2", artist)...) // album artist
	frames = append(frames, id3v23TextFrame("TALB", album)...)
	frames = append(frames, id3v23TextFrame("TRCK", strconv.Itoa(track))...)

	size := len(frames)
	out := []byte{'I', 'D', '3', 3, 0, 0,
		byte((size >> 21) & 0x7f), byte((size >> 14) & 0x7f),
		byte((size >> 7) & 0x7f), byte(size & 0x7f)}
	out = append(out, frames...)
	out = append(out, audio...)
	return out
}

// id3v23TextFrame builds a UTF-8 text frame (ID3v2.3 uses a plain big-endian
// 32-bit size, not synchsafe).
func id3v23TextFrame(id, text string) []byte {
	body := append([]byte{0x03}, []byte(text)...) // 0x03 == UTF-8
	size := len(body)
	frame := append([]byte(id),
		byte(size>>24), byte(size>>16), byte(size>>8), byte(size),
		0, 0) // flags
	return append(frame, body...)
}
