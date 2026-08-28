package meta

import (
	"context"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
)

// TestAcquisitionTagsRoundTripAndClear is the gate the acquisition write-back rests on.
// SOURCE_URL, SOURCE_ID and ACQUISITION_DATE are read off a typed WaxLabel struct and
// have no entry in fieldTagKeys, so nothing proved the writer could address them until
// this: it writes all three, reads them back through the adapter, then clears them and
// confirms the frames are gone rather than left holding an empty string.
func TestAcquisitionTagsRoundTripAndClear(t *testing.T) {
	ctx := context.Background()
	const rate = 44100
	sig := testaudio.ReferenceSignal(rate, 1200*time.Millisecond)

	cases := []struct {
		name string
		file string
		data func() []byte
	}{
		{"mp3", "song.mp3", func() []byte { return testaudio.BuildMP3("Song", "Artist", "Album", 1) }},
		{"flac", "song.flac", func() []byte { return testaudio.EncodeAs(t, "flac", "", rate, sig) }},
		// Progressive, not the format default: a fragmented MP4 refuses a tag rewrite.
		{"mp4", "song.m4a", func() []byte { return testaudio.EncodeAs(t, "aac", "progressive", rate, sig) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeTemp(t, tc.file, tc.data())

			w, r := NewWriter(), NewReader()
			set := []TagEdit{
				{Key: "SOURCE_URL", Values: []string{"https://origin.test/x"}},
				{Key: "SOURCE_ID", Values: []string{"origin-1"}},
				// ACQUISITION_DATE is a partial-date key: day precision at best, no clock
				// and no zone. The write-back formats the stored stamp the same way.
				{Key: "ACQUISITION_DATE", Values: []string{"2026-08-28"}},
			}
			if _, err := w.Apply(ctx, p, set); err != nil {
				t.Fatalf("apply: %v", err)
			}
			fm, err := r.Read(ctx, p)
			if err != nil {
				t.Fatalf("read after set: %v", err)
			}
			acq := fm.Tags.Acquisition
			if acq.SourceURL != "https://origin.test/x" || acq.SourceID != "origin-1" {
				t.Fatalf("acquisition tags = %+v, want the written pair", acq)
			}
			if acq.AcquiredAt == 0 {
				t.Errorf("acquisition date did not round-trip: %+v", acq)
			}

			clear := []TagEdit{{Key: "SOURCE_URL"}, {Key: "SOURCE_ID"}, {Key: "ACQUISITION_DATE"}}
			res, err := w.Apply(ctx, p, clear)
			if err != nil {
				t.Fatalf("clear: %v", err)
			}
			if !res.Changed {
				t.Error("clearing three present tags reported no change")
			}
			fm, err = r.Read(ctx, p)
			if err != nil {
				t.Fatalf("read after clear: %v", err)
			}
			if fm.Tags.Acquisition.Present() || fm.Tags.Acquisition.AcquiredAt != 0 {
				t.Errorf("acquisition tags survived the clear: %+v", fm.Tags.Acquisition)
			}
			// A second clear finds nothing to do, which is what says the frames were
			// removed rather than left holding an empty value.
			if res, err := w.Apply(ctx, p, clear); err != nil || res.Changed {
				t.Errorf("re-clear = changed %v (err %v), want a no-op", res.Changed, err)
			}
		})
	}
}
