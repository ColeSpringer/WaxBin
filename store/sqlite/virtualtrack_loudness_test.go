package sqlite

import (
	"context"
	"math"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// TestAlbumGainDedupsSharedRipFile: an album that mixes a single-file rip (one file,
// N virtual tracks) with a separate per-track file must weight the rip's file ONCE in
// the duration-weighted album gain, not once per virtual track. Counting it per
// virtual track over-weights the rip and skews the aggregate.
func TestAlbumGainDedupsSharedRipFile(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// A 200 ms rip (file A, 15 CD frames) carved into two virtual tracks, plus a
	// separate 100 ms track (file B) in the same album (same album artist / album /
	// folder).
	vt := model.PutScannedVirtualTracksInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/Band/Album/rip.flac"), DisplayPath: "/lib/Band/Album/rip.flac",
			RelPath: []byte("rip.flac"), Kind: model.FileAudio, Size: 5, MTimeNS: 1,
			ContentHash: "cripA", EssenceHash: "eripA", DurationMS: 200, ScanState: model.ScanIndexed,
		},
		Tracks: []model.VirtualTrack{
			{
				Item:        model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: "R1", SortKey: model.SortKey("R1"), IdentityKey: identity.VirtualTrackKey("eripA", 1, 0)},
				Track:       model.Track{Artist: "Band", ArtistSort: model.SortKey("Band"), AlbumArtist: "Band", Album: "Album", TrackNo: 1},
				StartFrames: 0, EndFrames: 8,
			},
			{
				Item:        model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: "R2", SortKey: model.SortKey("R2"), IdentityKey: identity.VirtualTrackKey("eripA", 2, 8)},
				Track:       model.Track{Artist: "Band", ArtistSort: model.SortKey("Band"), AlbumArtist: "Band", Album: "Album", TrackNo: 2},
				StartFrames: 8, EndFrames: 15,
			},
		},
	}
	resA, err := st.PutScannedVirtualTracks(ctx, vt)
	if err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}
	rB := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Album/02.flac", essence: "eB", content: "cB", title: "Two",
		artist: "Band", albumArt: "Band", album: "Album", durationMS: 100,
	})

	putLoudness(t, st, resA.FilePID, "eripA", -6.0, 0.8)
	putLoudness(t, st, rB.FilePID, "eB", -12.0, 0.5)

	if err := st.RefreshAlbumGain(ctx); err != nil {
		t.Fatalf("refresh album gain: %v", err)
	}

	// Correct: file A (gain -6, dur 200) weighted once, file B (gain -12, dur 100)
	// once. Over-weighting A twice would give ~-8.03 dB instead of ~-9.00 dB.
	wantGain := -10 * math.Log10((200*math.Pow(10, 0.6)+100*math.Pow(10, 1.2))/300)
	l, err := st.LoudnessByItem(ctx, rB.ItemPID)
	if err != nil {
		t.Fatalf("loudness: %v", err)
	}
	if !l.HasAlbum {
		t.Fatal("album gain not set after refresh")
	}
	if math.Abs(l.AlbumGainDB-wantGain) > 0.01 {
		t.Errorf("album gain = %.3f dB, want %.3f dB (rip file weighted once, not per virtual track)", l.AlbumGainDB, wantGain)
	}
}
