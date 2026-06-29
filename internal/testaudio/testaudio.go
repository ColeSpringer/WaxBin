// Package testaudio synthesizes minimal tagged audio files for tests. It is not
// part of the public API; it lives under internal/ so identity, store, and
// facade tests can share one MP3 builder.
package testaudio

import "strconv"

// mp3FrameLen is the byte length of a 128 kbps, 44100 Hz MPEG-1 Layer III frame
// (144 * 128000 / 44100 = 417).
const mp3FrameLen = 417

// mp3Frame builds one valid MPEG-1 Layer III frame: a real 4-byte frame header
// (sync, MPEG1/L3, 128 kbps, 44100 Hz, stereo) followed by main-data filled with
// fill. A real header matters because WaxLabel's essence hash covers the decoded
// frames, so a fake sync word alone yields "no audio essence". main-data is part
// of the hashed region, so two frames with different fill have different essence.
func mp3Frame(fill byte) []byte {
	f := make([]byte, mp3FrameLen)
	f[0], f[1], f[2], f[3] = 0xFF, 0xFB, 0x90, 0x00
	for i := 4; i < len(f); i++ {
		f[i] = fill
	}
	return f
}

// frames concatenates n MPEG frames whose fill bytes derive from seed, so a
// distinct seed yields distinct (but deterministic) audio essence.
func frames(n int, seed byte) []byte {
	out := make([]byte, 0, n*mp3FrameLen)
	for i := 0; i < n; i++ {
		out = append(out, mp3Frame(seed+byte(i*7+3))...)
	}
	return out
}

const defaultFrames = 20

// DefaultAudio returns a fixed, deterministic run of valid MPEG-1 Layer III
// frames standing in for MP3 audio. Because the frames are real, a tag reader
// can compute a genuine tag-independent essence hash over them, and two files
// built with this payload share essence (they dedup to one item).
func DefaultAudio() []byte { return frames(defaultFrames, 0) }

// AudioWithSeed is DefaultAudio with seed-varied main-data, for when a test needs
// two files with distinct essence (distinct items) rather than the shared payload.
func AudioWithSeed(seed byte) []byte { return frames(defaultFrames, seed) }

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

// MP3Spec describes the tags to stamp into a richer fixture for exercising the
// full music model (composer, genre, disc, year, compilation). Zero-valued
// fields are omitted so the resulting file carries only the frames a test sets.
type MP3Spec struct {
	Title, Artist, AlbumArtist, Album string
	Genre, Composer                   string
	Track, Disc, Year                 int
	Compilation                       bool
	Audio                             []byte // nil uses DefaultAudio
}

// BuildMP3FromSpec builds an ID3v2.3-tagged MP3 from spec over valid MPEG frames.
func BuildMP3FromSpec(s MP3Spec) []byte {
	audio := s.Audio
	if audio == nil {
		audio = DefaultAudio()
	}
	var frames []byte
	add := func(id, text string) {
		if text != "" {
			frames = append(frames, id3v23TextFrame(id, text)...)
		}
	}
	add("TIT2", s.Title)
	add("TPE1", s.Artist)
	add("TPE2", s.AlbumArtist)
	add("TALB", s.Album)
	add("TCON", s.Genre)
	add("TCOM", s.Composer)
	if s.Track > 0 {
		add("TRCK", strconv.Itoa(s.Track))
	}
	if s.Disc > 0 {
		add("TPOS", strconv.Itoa(s.Disc))
	}
	if s.Year > 0 {
		add("TYER", strconv.Itoa(s.Year))
	}
	if s.Compilation {
		add("TCMP", "1") // iTunes compilation flag
	}

	size := len(frames)
	out := []byte{'I', 'D', '3', 3, 0, 0,
		byte((size >> 21) & 0x7f), byte((size >> 14) & 0x7f),
		byte((size >> 7) & 0x7f), byte(size & 0x7f)}
	out = append(out, frames...)
	return append(out, audio...)
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
