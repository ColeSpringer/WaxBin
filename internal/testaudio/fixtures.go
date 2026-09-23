package testaudio

import (
	"embed"
	"testing"
)

//go:embed testdata
var fixtures embed.FS

// Fixture returns one of the checked-in files under testdata by name. They are
// formats EncodeAs cannot make, mostly ones WaxFlow decodes but cannot encode:
//
//	mono-8k.wma              WaxFlow container/asf/testdata at 192e0e1: a 3.7 KB WMAv2
//	                         stream, 8 kHz mono.
//	chapters.wma             WaxFlow container/asf/testdata at f635256: WMAv2, 8 kHz mono,
//	                         2 s, with Marker Object chapters Intro, Mïddle, and Coda at
//	                         0, 500, and 1250 ms.
//	lossless-s16.wma         WaxFlow container/asf/testdata at 05f3032: WMA Lossless;
//	                         recipe in WaxFlow docs/notes/wma-lossless-oracle-corpus.md.
//	pro-s16.wma              WaxFlow container/asf/testdata at 05f3032: WMA Pro; recipe in
//	                         WaxFlow docs/notes/wma-pro-oracle-corpus.md.
//	voice-mono.wma           WaxFlow container/asf/testdata at 05f3032: WMA Voice; recipe
//	                         in WaxFlow docs/notes/wma-voice-oracle-corpus.md.
//	ref-1s-alaw.wav          ReferenceSignal(8000, 1s) through EncodeWAV16 and ffmpeg
//	                         8.0.1 -c:a pcm_alaw: G.711 A-law in RIFF.
//	ref-1s-mulaw.wav         the same source WAV through ffmpeg -c:a pcm_mulaw: G.711
//	                         mu-law in RIFF.
//	ref-1s-ima-adpcm.wav     ReferenceSignal(44100, 1s) through EncodeWAV16 and ffmpeg
//	                         -c:a adpcm_ima_wav: IMA ADPCM in RIFF.
//	ref-1s-ms-adpcm.wav      the same source WAV through ffmpeg -c:a adpcm_ms: Microsoft
//	                         ADPCM in RIFF.
//	ref-1s-layer2.mp2        the same source WAV through ffmpeg -ac 1 -c:a mp2 -b:a 64k:
//	                         MPEG-1 Layer II, which WaxLabel names and WaxFlow does not
//	                         decode.
//	ref-2s-sv7.mpc           ReferenceSignal(44100, 2s) through mppenc 1.16 --thumb:
//	                         Musepack SV7, which that encoder writes as two channels.
//	ref-2s-sv8-chapters.mpc  the same signal through mpcenc r475 --thumb: Musepack SV8,
//	                         mono, with chapter packets Intro, Middle, and Coda at 0,
//	                         750, and 1500 ms written by mpcchap.
//	sample.m4a               WaxLabel v1.6.2 testdata/sample.m4a: a 10 KB AAC file tagged
//	                         by ffmpeg (title, artist, album, genre, track 2 of 10), the
//	                         one MP4 container here, for the freeform-atom tag paths the
//	                         MP3-bytes-named-.m4b fixtures never reach.
//
// The Musepack tools are the reference encoders WaxFlow builds with make mpc-tools.
func Fixture(tb testing.TB, name string) []byte {
	tb.Helper()
	data, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		tb.Fatalf("fixture %s: %v", name, err)
	}
	return data
}
