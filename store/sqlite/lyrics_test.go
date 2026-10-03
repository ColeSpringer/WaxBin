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
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putWithLyrics(t, st, lib.ID, "c1", &model.Lyrics{
		Source:   model.SourceSidecar,
		Synced:   []model.SyncedLine{{TimeMS: 0, Text: "Hello"}, {TimeMS: 1500, Text: "World"}},
		Unsynced: "Hello\nWorld",
	})

	got, err := st.LyricsByItem(ctx, pid)
	if err != nil {
		t.Fatalf("read lyrics: %v", err)
	}
	if got.Source != model.SourceSidecar || len(got.Synced) != 2 {
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
	t.Parallel()
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
		Source: model.SourceSidecar, Synced: []model.SyncedLine{{TimeMS: 0, Text: "added later"}},
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
	t.Parallel()
	st, lib := entityFixture(t)
	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/x.flac", essence: "ex", content: "cx", title: "X", artist: "A", album: "Al"}).ItemPID
	if _, err := st.LyricsByItem(context.Background(), pid); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("a track with no lyrics should report CodeNotFound, got %v", err)
	}
}

// TestCoverageCountsHeldLyrics: every present track counts toward the lyrics tile
// whatever supplied its lyrics, and one with no lyrics whose lookup answered that there
// are none counts as asked. A lookup still owed its answer, or one that found lyrics
// since removed, counts as neither. Missing tracks and books are not tracks the tile
// counts.
func TestCoverageCountsHeldLyrics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	track := func(title string, ly *model.Lyrics) (int64, model.PID) {
		t.Helper()
		path := "/lib/" + title + ".flac"
		res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
			LibraryID: lib.ID,
			File: model.File{
				Path: []byte(path), DisplayPath: path, RelPath: []byte(title + ".flac"),
				Kind: model.FileAudio, ContentHash: "c-" + title, EssenceHash: "e-" + title, ScanState: model.ScanIndexed,
			},
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent, Title: title,
				SortKey: model.SortKey(title), IdentityKey: "essence:e-" + title,
			},
			Track:  model.Track{Artist: "A", Album: "Al"},
			Lyrics: ly,
		})
		if err != nil {
			t.Fatalf("put %s: %v", title, err)
		}
		return itemID(t, st, title), res.ItemPID
	}
	apply := func(in model.LyricsEnrichment) {
		t.Helper()
		if err := st.ApplyLyricsEnrichment(ctx, in); err != nil {
			t.Fatalf("apply lyrics for %d: %v", in.ItemID, err)
		}
	}

	track("Tagged", &model.Lyrics{Source: model.SourceTag, Unsynced: "from the tag"})
	fetched, fetchedPID := track("Fetched", nil)
	apply(model.LyricsEnrichment{ItemID: fetched, PID: fetchedPID, Matched: true, Provider: "lrclib",
		Lyrics: &model.Lyrics{Source: model.SourceEnrichment, Provider: "lrclib", Unsynced: "fetched"}})
	missed, missedPID := track("Missed", nil)
	apply(model.LyricsEnrichment{ItemID: missed, PID: missedPID, Provider: "lrclib"})
	owed, owedPID := track("Owed", nil)
	apply(model.LyricsEnrichment{ItemID: owed, PID: owedPID, Provider: "lrclib", Incomplete: true})
	cleared, clearedPID := track("Cleared", nil)
	apply(model.LyricsEnrichment{ItemID: cleared, PID: clearedPID, Matched: true, Provider: "lrclib",
		Lyrics: &model.Lyrics{Source: model.SourceEnrichment, Provider: "lrclib", Unsynced: "since removed"}})
	if _, err := st.write.ExecContext(ctx, "DELETE FROM lyrics WHERE item_id = ?", cleared); err != nil {
		t.Fatalf("remove lyrics: %v", err)
	}
	track("Unasked", nil)
	gone, _ := track("Gone", &model.Lyrics{Source: model.SourceSidecar, Unsynced: "still held"})
	if _, err := st.write.ExecContext(ctx, "UPDATE playable_item SET state = 'missing' WHERE id = ?", gone); err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b.m4b", essence: "eb", content: "cb", title: "Spoken", author: "Au"})
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO lyrics(item_id, source, unsynced, updated_at) VALUES (?, 'user', 'a transcript', 1)",
		itemID(t, st, "Spoken")); err != nil {
		t.Fatalf("book lyrics: %v", err)
	}

	cov, err := st.EnrichmentCoverage(ctx)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	// Only the settled miss counts as asked: an owed lookup has no answer yet, and a found
	// one whose lyrics were removed did not find none.
	if cov.Tracks != 6 || cov.TracksWithLyrics != 2 || cov.TracksLyricsAsked != 1 {
		t.Errorf("lyrics coverage = %d tracks, %d held, %d asked; want 6, 2, 1",
			cov.Tracks, cov.TracksWithLyrics, cov.TracksLyricsAsked)
	}
	if cov.Matched != 0 {
		t.Errorf("Matched = %d, want 0 (the lyrics markers are not identity matches)", cov.Matched)
	}
}
