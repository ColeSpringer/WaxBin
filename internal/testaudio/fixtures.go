package testaudio

import (
	"embed"
	"testing"
)

//go:embed testdata
var fixtures embed.FS

// Fixture returns one of the checked-in files under testdata by name. They are the
// formats WaxFlow decodes but cannot encode, so EncodeAs cannot make them:
//
//	mono-8k.wma              WaxFlow container/asf/testdata at 192e0e1: a 3.7 KB WMAv2
//	                         stream, 8 kHz mono.
//	chapters.wma             WaxFlow container/asf/testdata at f635256: WMAv2, 8 kHz mono,
//	                         2 s, with Marker Object chapters Intro, Mïddle, and Coda at
//	                         0, 500, and 1250 ms.
//	ref-2s-sv7.mpc           ReferenceSignal(44100, 2s) through mppenc 1.16 --thumb:
//	                         Musepack SV7, which that encoder writes as two channels.
//	ref-2s-sv8-chapters.mpc  the same signal through mpcenc r475 --thumb: Musepack SV8,
//	                         mono, with chapter packets Intro, Middle, and Coda at 0,
//	                         750, and 1500 ms written by mpcchap.
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
