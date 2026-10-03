package waxbin

import (
	"context"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func TestSortByQuality(t *testing.T) {
	t.Parallel()
	cs := []UpgradeCandidate{
		{ItemPID: "lossy-hi", Codec: "mp3", Bitrate: 320, SampleRate: 44100},
		{ItemPID: "flac-cd", Codec: "flac", Lossless: true, SampleRate: 44100, BitDepth: 16},
		{ItemPID: "lossy-lo", Codec: "mp3", Bitrate: 128, SampleRate: 44100},
		{ItemPID: "flac-hires", Codec: "flac", Lossless: true, SampleRate: 96000, BitDepth: 24},
	}
	sortByQuality(cs)
	got := []string{string(cs[0].ItemPID), string(cs[1].ItemPID), string(cs[2].ItemPID), string(cs[3].ItemPID)}
	want := []string{"flac-hires", "flac-cd", "lossy-hi", "lossy-lo"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("quality order = %v, want %v", got, want)
		}
	}
}

func TestSortByQualityStableTie(t *testing.T) {
	t.Parallel()
	// Identical quality: deterministic by PID so pagination/reporting is stable.
	cs := []UpgradeCandidate{
		{ItemPID: "z", Codec: "flac", Lossless: true, SampleRate: 44100, BitDepth: 16},
		{ItemPID: "a", Codec: "flac", Lossless: true, SampleRate: 44100, BitDepth: 16},
	}
	sortByQuality(cs)
	if cs[0].ItemPID != "a" {
		t.Errorf("tie should break by PID ascending, got %s first", cs[0].ItemPID)
	}
}

// TestLosslessCodecsCoverFloatPCM: WaxLabel labels float PCM by its sample format
// rather than as "PCM", so a float WAV or MOV would rank as lossy without these
// keys, and the upgrade policy would offer an mp3 as an improvement on it.
func TestLosslessCodecsCoverFloatPCM(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"pcm", "ieee float", "ieee float64"} {
		if !model.LosslessCodec(k) {
			t.Errorf("LosslessCodec(%q) is false; uncompressed audio must outrank a lossy encoding", k)
		}
	}
}

// TestEncodingComponentsSkipAVanishedSeed: an item deleted between the listing and its
// own probe is skipped like a vanished neighbour, not an error for the whole listing.
func TestEncodingComponentsSkipAVanishedSeed(t *testing.T) {
	t.Parallel()
	items := []*model.ItemView{{PID: "gone", FilePID: "f0"}, {PID: "a", FilePID: "f1"}, {PID: "b", FilePID: "f2"}}
	alts := func(_ context.Context, pid model.PID) ([]AltEncoding, error) {
		switch pid {
		case "gone":
			return nil, waxerr.New(waxerr.CodeNotFound, "test", "no such item")
		case "a":
			return []AltEncoding{{ItemPID: "b"}}, nil
		}
		return []AltEncoding{{ItemPID: "a"}}, nil
	}
	got, err := encodingComponents(context.Background(), items, alts)
	if err != nil || len(got) != 1 || !slices.Equal(got[0], []model.PID{"a", "b"}) {
		t.Errorf("components = %v (err %v), want [[a b]]", got, err)
	}
}
