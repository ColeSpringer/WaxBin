package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

func putTrackCustom(t *testing.T, st *Store, libID int64, path, essence, content, title string, custom map[string][]string, preserveLocks bool) *model.ScanItemResult {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 1,
			ContentHash: content, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence,
		},
		Track:         model.Track{Artist: "Artist", Album: "Album"},
		CustomTags:    custom,
		PreserveLocks: preserveLocks,
	}
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
	return res
}

func tagValues(t *testing.T, st *Store, pid model.PID, key string) []string {
	t.Helper()
	tags, err := st.ItemTags(context.Background(), pid)
	if err != nil {
		t.Fatalf("item tags: %v", err)
	}
	for _, tg := range tags {
		if tg.Key == key {
			return tg.Values
		}
	}
	return nil
}

func TestSetItemTag(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One", nil, true)
	pid := res.ItemPID

	// Set a multi-valued custom tag (key normalized to uppercase).
	stored, n, err := st.SetItemTag(ctx, pid, "mood", []string{"chill", "upbeat"}, model.SourceUser, true, false)
	if err != nil {
		t.Fatalf("set tag: %v", err)
	}
	if stored != "MOOD" || n != 2 {
		t.Fatalf("SetItemTag = (%q,%d), want (MOOD,2)", stored, n)
	}
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 2 || got[0] != "chill" || got[1] != "upbeat" {
		t.Fatalf("MOOD values = %v", got)
	}
	// The tag.<KEY> lock is recorded.
	locked, err := st.IsFieldLocked(ctx, pid, "tag.MOOD")
	if err != nil || !locked {
		t.Fatalf("tag.MOOD should be locked: locked=%v err=%v", locked, err)
	}

	// A reserved key is rejected, directing the caller to the right surface.
	if _, _, err := st.SetItemTag(ctx, pid, "artist", []string{"x"}, model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("reserved key should be CodeInvalid, got %v", err)
	}
	// An invalid key is rejected.
	if _, _, err := st.SetItemTag(ctx, pid, "bad=key", []string{"x"}, model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("invalid key should be CodeInvalid, got %v", err)
	}

	// A whitespace-only value trims to empty, so it clears (reporting 0 stored) rather
	// than reading as a set of one value. Even with lock on (the default), a clear must
	// NOT leave a locked-empty tag behind (force is needed to clear the locked MOOD).
	if _, n, err := st.SetItemTag(ctx, pid, "MOOD", []string{"   "}, model.SourceUser, true, true); err != nil || n != 0 {
		t.Fatalf("whitespace-only value should clear (n=0), got n=%d err=%v", n, err)
	}
	if got := tagValues(t, st, pid, "MOOD"); got != nil {
		t.Fatalf("MOOD should be cleared by a whitespace-only value, got %v", got)
	}
	if locked, _ := st.IsFieldLocked(ctx, pid, "tag.MOOD"); locked {
		t.Fatalf("a clear must drop the tag.MOOD lock, not leave a locked-empty tag")
	}
	// Because the clear dropped the lock, a re-set needs no force.
	if _, n, err := st.SetItemTag(ctx, pid, "MOOD", []string{"reset"}, model.SourceUser, true, false); err != nil || n != 1 {
		t.Fatalf("re-set after a clear should succeed without force: n=%d err=%v", n, err)
	}
}

func TestScanPreservesLockedCustomTag(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// First scan carries a custom MOOD frame; it is persisted.
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One",
		map[string][]string{"MOOD": {"chill"}}, true)
	pid := res.ItemPID
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "chill" {
		t.Fatalf("scan should persist custom MOOD, got %v", got)
	}

	// User curates and locks MOOD.
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"locked-mood"}, model.SourceUser, true, true); err != nil {
		t.Fatalf("set tag: %v", err)
	}

	// A forced rescan (PreserveLocks) must not clobber the locked tag, but a new
	// unlocked tag is still ingested.
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c2", "One",
		map[string][]string{"MOOD": {"from-file"}, "TEMPO": {"fast"}}, true)
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "locked-mood" {
		t.Fatalf("locked MOOD was clobbered by scan: %v", got)
	}
	if got := tagValues(t, st, pid, "TEMPO"); len(got) != 1 || got[0] != "fast" {
		t.Fatalf("unlocked TEMPO should be ingested: %v", got)
	}

	// An --ignore-locks rescan re-derives the tag from the file.
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c3", "One",
		map[string][]string{"MOOD": {"re-derived"}}, false)
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "re-derived" {
		t.Fatalf("--ignore-locks should re-derive MOOD: %v", got)
	}
	// TEMPO was not in the file this time and is not locked, so it is dropped.
	if got := tagValues(t, st, pid, "TEMPO"); got != nil {
		t.Fatalf("TEMPO should be dropped when absent from the file: %v", got)
	}
}

func TestScanRefreshesFTSOnTagChangeWithoutAudioChange(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// First scan: the file carries MOOD=filevalue.
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One",
		map[string][]string{"MOOD": {"filevalue"}}, true)
	pid := res.ItemPID

	// User overrides MOOD (unlocked) to a searchable value.
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"editvalue"}, model.SourceUser, false, false); err != nil {
		t.Fatalf("set tag: %v", err)
	}
	sr, _ := st.Search(ctx, "editvalue", read.SearchOptions{})
	if len(sr.Tracks) != 1 {
		t.Fatalf("edit value should be searchable, got %+v", sr.Tracks)
	}

	// A rescan with IDENTICAL audio bytes (no content change) re-derives MOOD=filevalue
	// from the file (the tag is unlocked). The search row must follow even though the
	// entity/audio-change path did not run.
	putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One",
		map[string][]string{"MOOD": {"filevalue"}}, true)
	if got := tagValues(t, st, pid, "MOOD"); len(got) != 1 || got[0] != "filevalue" {
		t.Fatalf("MOOD should be re-derived to filevalue, got %v", got)
	}
	after, _ := st.Search(ctx, "filevalue", read.SearchOptions{})
	if len(after.Tracks) != 1 {
		t.Fatalf("re-derived value should be searchable after a no-audio-change rescan, got %+v", after.Tracks)
	}
	stale, _ := st.Search(ctx, "editvalue", read.SearchOptions{})
	if len(stale.Tracks) != 0 {
		t.Fatalf("stale edit value should no longer be searchable, got %+v", stale.Tracks)
	}
}

func TestItemTagsNotFound(t *testing.T) {
	st, _ := entityFixture(t)
	if _, err := st.ItemTags(context.Background(), "does-not-exist"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("ItemTags for a missing item should be CodeNotFound, got %v", err)
	}
}

func TestCustomTagIsSearchable(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrackCustom(t, st, lib.ID, "/lib/1.flac", "e1", "c1", "One", nil, true)
	pid := res.ItemPID

	// A distinctive custom-tag value should surface the item in full-text search.
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"melancholic"}, model.SourceUser, true, false); err != nil {
		t.Fatalf("set tag: %v", err)
	}
	sr, err := st.Search(ctx, "melancholic", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(sr.Tracks) != 1 || sr.Tracks[0].PID != pid {
		t.Fatalf("custom tag value should be searchable, got %+v", sr.Tracks)
	}
}
