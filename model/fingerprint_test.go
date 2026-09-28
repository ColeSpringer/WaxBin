package model

import "testing"

func TestPeaksDataDurationMS(t *testing.T) {
	for _, c := range []struct {
		frames int64
		rate   int
		want   int64
	}{
		{88200, 44100, 2000},
		{48000 * 3600, 48000, 3_600_000},
		{47, 48000, 0}, // under a millisecond floors to zero
		{12345, 0, 0},  // no rate, no length
		{12345, -1, 0},
	} {
		pk := PeaksData{Frames: c.frames, SampleRate: c.rate}
		if got := pk.DurationMS(); got != c.want {
			t.Errorf("%d frames at %d Hz = %d ms, want %d", c.frames, c.rate, got, c.want)
		}
	}
}
