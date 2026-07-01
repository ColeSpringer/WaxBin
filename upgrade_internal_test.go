package waxbin

import "testing"

func TestSortByQuality(t *testing.T) {
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
