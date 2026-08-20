package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// putLyricTrack seeds one track carrying scanned lyrics, re-running for the same
// essence to model a rescan.
func putLyricTrack(t *testing.T, st *sqlite.Store, libID int64, essence, content string, ly *model.Lyrics) model.PID {
	t.Helper()
	path := "/lib/" + essence + ".flac"
	res, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1, DurationMS: 1000,
			ContentHash: content, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "T-" + essence,
			SortKey: model.SortKey("T-" + essence), IdentityKey: "essence:" + essence,
		},
		Track:  model.Track{Artist: "Alpha", AlbumArtist: "Alpha", Album: "A", TrackNo: 1},
		Lyrics: ly,
	})
	if err != nil {
		t.Fatalf("PutScannedTrack %s: %v", essence, err)
	}
	return res.ItemPID
}

// TestLyricsProvenanceRoundTrip: each of the four sources survives the write and comes
// back naming itself, with the provider only where one supplied the words.
func TestLyricsProvenanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	// tag: embedded USLT read at scan time.
	embedded := putLyricTrack(t, st, lib.ID, "ess-tag", "c1",
		&model.Lyrics{Source: model.SourceTag, Unsynced: "embedded block"})
	if got, err := st.LyricsByItem(ctx, embedded); err != nil || got.Source != model.SourceTag || got.Provider != "" {
		t.Errorf("embedded lyrics = %+v (err %v), want a tag row with no provider", got, err)
	}

	// sidecar: a .lrc beside the audio.
	sidecar := putLyricTrack(t, st, lib.ID, "ess-sidecar", "c2",
		&model.Lyrics{Source: model.SourceSidecar, Synced: []model.SyncedLine{{TimeMS: 0, Text: "hi"}}})
	if got, err := st.LyricsByItem(ctx, sidecar); err != nil || got.Source != model.SourceSidecar {
		t.Errorf("sidecar lyrics = %+v (err %v), want a sidecar row", got, err)
	}

	// enrichment: a provider supplied them, and is named separately from the source.
	fetched := putLyricTrack(t, st, lib.ID, "ess-fetched", "c3", nil)
	err := st.ApplyLyricsEnrichment(ctx, model.LyricsEnrichment{
		ItemID: itemRowID(t, db, fetched), PID: fetched, Matched: true,
		Provider: "lrclib",
		Lyrics:   &model.Lyrics{Source: model.SourceEnrichment, Provider: "lrclib", Unsynced: "fetched words"},
	})
	if err != nil {
		t.Fatalf("ApplyLyricsEnrichment: %v", err)
	}
	got, err := st.LyricsByItem(ctx, fetched)
	if err != nil || got.Source != model.SourceEnrichment || got.Provider != "lrclib" {
		t.Errorf("fetched lyrics = %+v (err %v), want enrichment/lrclib", got, err)
	}

	// user: a curation edit that names no origin is recorded as a hand edit, and the
	// provider that came with the words it replaces goes with them.
	if err := st.SetItemLyrics(ctx, fetched, &model.Lyrics{Unsynced: "my words"},
		model.LockOf(true), false); err != nil {
		t.Fatalf("SetItemLyrics: %v", err)
	}
	got, err = st.LyricsByItem(ctx, fetched)
	if err != nil || got.Source != model.SourceUser || got.Provider != "" {
		t.Errorf("user lyrics = %+v (err %v), want a user row with no provider", got, err)
	}

	// And a curation write that does name an origin keeps it, which is what lets an
	// embedder that fetched the words record who fetched them.
	if err := st.SetItemLyrics(ctx, fetched, &model.Lyrics{
		Source: model.SourceEnrichment, Provider: "lrclib", Unsynced: "their words",
	}, model.LockOf(true), true); err != nil {
		t.Fatalf("SetItemLyrics stamped: %v", err)
	}
	got, err = st.LyricsByItem(ctx, fetched)
	if err != nil || got.Source != model.SourceEnrichment || got.Provider != "lrclib" {
		t.Errorf("stamped curation lyrics = %+v (err %v), want enrichment/lrclib", got, err)
	}
}

// TestUnchangedLyricsRescanWritesNothing: the idempotence compare now includes the
// provider, so a rescan that re-reads the same words stays silent and a re-attribution
// does not.
func TestUnchangedLyricsRescanWritesNothing(t *testing.T) {
	st, dbPath, lib := openStoreAt(t)
	ly := func() *model.Lyrics {
		return &model.Lyrics{Source: model.SourceSidecar, Synced: []model.SyncedLine{{TimeMS: 0, Text: "hi"}}}
	}
	putLyricTrack(t, st, lib.ID, "ess-a", "c1", ly())

	db := roConn(t, dbPath)
	before := scalarInt64(t, db, "SELECT updated_at FROM lyrics")
	putLyricTrack(t, st, lib.ID, "ess-a", "c2", ly())
	if after := scalarInt64(t, db, "SELECT updated_at FROM lyrics"); after != before {
		t.Errorf("an unchanged rescan rewrote the lyrics row (%d -> %d)", before, after)
	}

	// The same words under a different attribution is a real change.
	reattributed := ly()
	reattributed.Source = model.SourceTag
	putLyricTrack(t, st, lib.ID, "ess-a", "c3", reattributed)
	if got := scalarQueryStr(t, db, "SELECT source FROM lyrics"); got != string(model.SourceTag) {
		t.Errorf("re-attributed lyrics source = %q, want tag", got)
	}
}

// TestFieldProvenanceOverlaysLyrics: the lyrics row answers with the attribution the
// lyrics table holds, not the one the lock writer invented. LockField records "tag" on
// any field it locks, and enrichment records no provenance row at all.
func TestFieldProvenanceOverlaysLyrics(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)

	pid := putLyricTrack(t, st, lib.ID, "ess-a", "c1", nil)
	if err := st.ApplyLyricsEnrichment(ctx, model.LyricsEnrichment{
		ItemID: itemRowID(t, db, pid), PID: pid, Matched: true, Provider: "lrclib",
		Lyrics: &model.Lyrics{Source: model.SourceEnrichment, Provider: "lrclib", Unsynced: "words"},
	}); err != nil {
		t.Fatalf("ApplyLyricsEnrichment: %v", err)
	}

	row := func() (model.FieldProvenance, bool) {
		t.Helper()
		rows, err := st.FieldProvenance(ctx, pid)
		if err != nil {
			t.Fatalf("FieldProvenance: %v", err)
		}
		for _, r := range rows {
			if r.Field == "lyrics" {
				return r, true
			}
		}
		return model.FieldProvenance{}, false
	}

	// Fetched lyrics with no provenance row at all still report themselves.
	got, ok := row()
	if !ok || got.Source != model.SourceEnrichment || got.Provider != "lrclib" || got.Locked {
		t.Errorf("lyrics row = %+v (found %v), want an unlocked enrichment/lrclib row", got, ok)
	}

	// Locking writes "tag" into field_provenance, which the overlay corrects while
	// keeping the lock.
	if err := st.LockField(ctx, pid, "lyrics"); err != nil {
		t.Fatalf("LockField: %v", err)
	}
	if stored := scalarQueryStr(t, db, `SELECT fp.source FROM field_provenance fp
		JOIN playable_item pi ON pi.id = fp.item_id WHERE pi.pid = ? AND fp.field = 'lyrics'`,
		string(pid)); stored != "tag" {
		t.Fatalf("lock row source = %q, want the tag LockField writes", stored)
	}
	got, ok = row()
	if !ok || got.Source != model.SourceEnrichment || got.Provider != "lrclib" || !got.Locked {
		t.Errorf("locked lyrics row = %+v (found %v), want a locked enrichment/lrclib row", got, ok)
	}

	// A lock with no lyrics at all is a real state and survives on its own.
	bare := putLyricTrack(t, st, lib.ID, "ess-b", "c2", nil)
	if err := st.LockField(ctx, bare, "lyrics"); err != nil {
		t.Fatalf("LockField bare: %v", err)
	}
	rows, err := st.FieldProvenance(ctx, bare)
	if err != nil || len(rows) != 1 || rows[0].Field != "lyrics" || !rows[0].Locked {
		t.Errorf("bare lock rows = %+v (err %v), want one locked lyrics row", rows, err)
	}
}
