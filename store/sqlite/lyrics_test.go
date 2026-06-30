package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func putWithLyrics(t *testing.T, st *Store, libID int64, content string, ly *model.Lyrics) model.PID {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte("/lib/s.flac"), DisplayPath: "/lib/s.flac", RelPath: []byte("s.flac"),
			Kind: model.FileAudio, ContentHash: content, EssenceHash: "e", ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "S",
			SortKey: model.SortKey("S"), IdentityKey: "essence:e",
		},
		Track:  model.Track{Artist: "A", Album: "Al"},
		Lyrics: ly,
	}
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	return res.ItemPID
}

func TestLyricsRoundTripAndClear(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putWithLyrics(t, st, lib.ID, "c1", &model.Lyrics{
		Source:   "lrc",
		Synced:   []model.SyncedLine{{TimeMS: 0, Text: "Hello"}, {TimeMS: 1500, Text: "World"}},
		Unsynced: "Hello\nWorld",
	})

	got, err := st.LyricsByItem(ctx, pid)
	if err != nil {
		t.Fatalf("read lyrics: %v", err)
	}
	if got.Source != "lrc" || len(got.Synced) != 2 {
		t.Fatalf("lyrics = %+v, want source lrc + 2 synced lines", got)
	}
	if got.Synced[1].TimeMS != 1500 || got.Synced[1].Text != "World" {
		t.Errorf("synced[1] = %+v, want {1500 World}", got.Synced[1])
	}
	if got.Unsynced != "Hello\nWorld" {
		t.Errorf("unsynced = %q, want preserved block", got.Unsynced)
	}

	// A retag that removes lyrics (content change, nil lyrics) clears the row, so
	// the table stays sparse and a later read reports CodeNotFound.
	pid2 := putWithLyrics(t, st, lib.ID, "c2", nil)
	if pid2 != pid {
		t.Fatalf("expected same item pid across retag, got %s vs %s", pid2, pid)
	}
	_, err = st.LyricsByItem(ctx, pid)
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("after clearing, LyricsByItem err = %v, want CodeNotFound", err)
	}
}

func TestLyricsPickedUpWithoutAudioChange(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// First scan: no lyrics yet.
	pid := putWithLyrics(t, st, lib.ID, "c1", nil)
	if _, err := st.LyricsByItem(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("expected no lyrics initially, got %v", err)
	}
	// Rescan the SAME audio bytes (content "c1" unchanged) but now a .lrc sidecar
	// exists. The lyrics must be ingested even though the audio did not change.
	pid2 := putWithLyrics(t, st, lib.ID, "c1", &model.Lyrics{
		Source: "lrc", Synced: []model.SyncedLine{{TimeMS: 0, Text: "added later"}},
	})
	if pid2 != pid {
		t.Fatalf("expected the same item pid, got %s vs %s", pid2, pid)
	}
	got, err := st.LyricsByItem(ctx, pid)
	if err != nil {
		t.Fatalf("lyrics should be present after the sidecar appeared: %v", err)
	}
	if len(got.Synced) != 1 || got.Synced[0].Text != "added later" {
		t.Errorf("lyrics = %+v, want the newly-added sidecar line", got)
	}
}

func TestLyricsNotFound(t *testing.T) {
	st, lib := entityFixture(t)
	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/x.flac", essence: "ex", content: "cx", title: "X", artist: "A", album: "Al"}).ItemPID
	if _, err := st.LyricsByItem(context.Background(), pid); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("a track with no lyrics should report CodeNotFound, got %v", err)
	}
}
