package sqlite

import (
	"context"
	"io/fs"
	"os"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

const (
	rekeyR1 = "4e2b1b2a-0000-4000-8000-0000000000d1"
	rekeyR2 = "4e2b1b2a-0000-4000-8000-0000000000d2"
)

// rekeyFixture is a store over a library on disk, and put writes one encoding of the
// recording "One" there and reads it the way a scan does: the file at rel, its audio's
// essence, the bytes on disk (a retag changes them), the codec, and the recording id its
// tags name ("" for none). The file's size and mtime are the ones on disk. retag changes
// a file's bytes without reading it, as a tagger does before the next scan.
func rekeyFixture(t *testing.T) (*Store, func(rel, essence, content, codec, rec string) *model.ScanItemResult, func(rel, content string)) {
	t.Helper()
	st, _ := entityFixture(t)
	lib, file := diskLibrary(t, st)
	paths := map[string]string{}
	retag := func(rel, content string) {
		t.Helper()
		if paths[rel] == "" {
			paths[rel] = file(rel, content)
			return
		}
		if err := os.WriteFile(paths[rel], []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return st, func(rel, essence, content, codec, rec string) *model.ScanItemResult {
		t.Helper()
		retag(rel, content)
		info, err := os.Stat(paths[rel])
		if err != nil {
			t.Fatal(err)
		}
		in := trackSpecInput(lib.ID, trackSpec{path: paths[rel], essence: essence, content: content, title: "One",
			artist: "A", album: "T", mbRecording: rec, durationMS: 1000})
		in.File.Codec, in.File.SampleRate = codec, 44100
		in.File.Size, in.File.MTimeNS = info.Size(), info.ModTime().UnixNano()
		return putTrackInput(t, st, in)
	}, retag
}

// TestARecordingIDRetaggedByHandKeepsTheItem: a track's file is tagged with a recording
// id, the id is changed by hand, then stripped. The item stays the one the file backs,
// keyed each time on what the file says.
func TestARecordingIDRetaggedByHandKeepsTheItem(t *testing.T) {
	t.Parallel()
	st, put, _ := rekeyFixture(t)
	item := put("A/one.flac", "rk-1", "c1", "flac", rekeyR1).ItemPID
	for _, step := range []struct{ content, rec, key string }{
		{"c2", rekeyR2, "mbid:" + rekeyR2},
		{"c3", "", "essence:rk-1"},
		{"c4", rekeyR1, "mbid:" + rekeyR1},
	} {
		if got := put("A/one.flac", "rk-1", step.content, "flac", step.rec).ItemPID; got != item {
			t.Errorf("retagged %q: the file backs %s, want %s kept", step.rec, got, item)
		}
		if got := identityKeyOf(t, st, item); got != step.key {
			t.Errorf("retagged %q: key = %q, want %q", step.rec, got, step.key)
		}
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item"); n != 1 {
		t.Errorf("items = %d, want the one", n)
	}
	assertVerifyClean(t, st)
}

// TestARecordingIDNamingAnotherItemStillJoinsIt: a file retagged with the recording id
// another item holds joins that item, as it always has.
func TestARecordingIDNamingAnotherItemStillJoinsIt(t *testing.T) {
	t.Parallel()
	st, put, _ := rekeyFixture(t)
	other := put("B/two.flac", "rk-2", "d1", "flac", rekeyR2).ItemPID
	put("A/one.mp3", "rk-1", "c1", "mp3", rekeyR1)
	if got := put("A/one.mp3", "rk-1", "c2", "mp3", rekeyR2).ItemPID; got != other {
		t.Errorf("the retagged file backs %s, want the item holding the id, %s", got, other)
	}
	assertVerifyClean(t, st)
}

// TestACopyKeepsItsItemWhicheverFileIsReadFirst: an item's primary and a copy of the same
// audio are both retagged with a new recording id. Whichever is read first, both end on
// the item, keyed on the new id. A copy whose id is stripped stays where it is.
func TestACopyKeepsItsItemWhicheverFileIsReadFirst(t *testing.T) {
	t.Parallel()
	for _, copyFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "copy first", false: "primary first"}[copyFirst], func(t *testing.T) {
			t.Parallel()
			st, put, _ := rekeyFixture(t)
			item := put("A/one.flac", "rk-1", "c1", "flac", rekeyR1).ItemPID
			if res := put("B/one.flac", "rk-1", "c1", "flac", rekeyR1); res.ItemPID != item || !res.AttachedAsCopy {
				t.Fatalf("the copy = %+v, want it on %s", res, item)
			}
			reads := []func() *model.ScanItemResult{
				func() *model.ScanItemResult { return put("A/one.flac", "rk-1", "c2", "flac", rekeyR2) },
				func() *model.ScanItemResult { return put("B/one.flac", "rk-1", "c2", "flac", rekeyR2) },
			}
			if copyFirst {
				reads[0], reads[1] = reads[1], reads[0]
			}
			for _, read := range reads {
				if got := read().ItemPID; got != item {
					t.Errorf("a retagged file backs %s, want %s", got, item)
				}
			}
			if got := identityKeyOf(t, st, item); got != "mbid:"+rekeyR2 {
				t.Errorf("key = %q, want the new id", got)
			}
			if got := put("B/one.flac", "rk-1", "c3", "flac", "").ItemPID; got != item {
				t.Errorf("the stripped copy backs %s, want it kept on %s", got, item)
			}
			if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item"); n != 1 {
				t.Errorf("items = %d, want the one", n)
			}
			assertVerifyClean(t, st)
		})
	}
}

// TestAnAlternateEncodingEndsTheSameWhicheverIsReadFirst: an item's primary loses its
// recording id by hand, while its other encoding keeps naming it. Read before the primary
// or after it, the other encoding ends on an item of its own and the primary keeps its
// item. The put that re-keys the item names the other encoding, which the scan reads
// again in the same pass.
func TestAnAlternateEncodingEndsTheSameWhicheverIsReadFirst(t *testing.T) {
	t.Parallel()
	for _, alternateFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "alternate first", false: "primary first"}[alternateFirst], func(t *testing.T) {
			t.Parallel()
			st, put, _ := rekeyFixture(t)
			item := put("A/one.flac", "rk-flac", "c1", "flac", rekeyR1).ItemPID
			if res := put("A/one.mp3", "rk-mp3", "m1", "mp3", rekeyR1); res.ItemPID != item {
				t.Fatalf("the mp3 backs %s, want the flac's item %s", res.ItemPID, item)
			}
			if alternateFirst {
				put("A/one.mp3", "rk-mp3", "m1", "mp3", rekeyR1)
			}
			res := put("A/one.flac", "rk-flac", "c2", "flac", "")
			if res.ItemPID != item {
				t.Fatalf("the stripped primary backs %s, want %s kept", res.ItemPID, item)
			}
			if len(res.Promoted) != 1 {
				t.Fatalf("the re-keying put named %d files to read again, want the mp3", len(res.Promoted))
			}
			// The scan reads the named file again.
			mp3 := put("A/one.mp3", "rk-mp3", "m1", "mp3", rekeyR1)
			if mp3.ItemPID == item {
				t.Errorf("the mp3 still backs %s, whose recording id the files no longer share", item)
			}
			if got := identityKeyOf(t, st, item); got != "essence:rk-flac" {
				t.Errorf("key = %q, want the flac's essence", got)
			}
			assertVerifyClean(t, st)
		})
	}
}

// TestAColumnMovedByAnEditLeavesNoStaleKey: an edit moves a track's recording id in the
// catalog alone, and the file is tagged to match later. The scan joins the item through
// its column and re-keys it, so a file tagged with the old id afterwards does not land on
// it.
func TestAColumnMovedByAnEditLeavesNoStaleKey(t *testing.T) {
	t.Parallel()
	st, put, _ := rekeyFixture(t)
	ctx := context.Background()
	item := put("A/one.flac", "rk-1", "c1", "flac", rekeyR1).ItemPID
	if err := st.EditItemFields(ctx, item, map[string]string{"mbid": rekeyR2},
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := put("A/one.flac", "rk-1", "c2", "flac", rekeyR2).ItemPID; got != item {
		t.Fatalf("the file tagged to match backs %s, want %s", got, item)
	}
	if got := identityKeyOf(t, st, item); got != "mbid:"+rekeyR2 {
		t.Errorf("key = %q, want the id the file and the column share", got)
	}
	if got := put("B/two.flac", "rk-2", "d1", "flac", rekeyR1).ItemPID; got == item {
		t.Error("a file tagged with the old id landed on the item")
	}
	assertVerifyClean(t, st)
}

// TestACatalogOnlyIDGathersAFileTaggedWithIt: a track's recording id was set in the
// catalog alone. Another encoding whose tags name that id joins the item through the
// column, the album rule one rung down.
func TestACatalogOnlyIDGathersAFileTaggedWithIt(t *testing.T) {
	t.Parallel()
	st, put, _ := rekeyFixture(t)
	ctx := context.Background()
	item := put("A/one.flac", "rk-1", "c1", "flac", "").ItemPID
	if err := st.EditItemFields(ctx, item, map[string]string{"mbid": rekeyR1},
		model.Attribution{Source: model.SourceUser}, model.LockOff, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got := put("A/one.mp3", "rk-2", "m1", "mp3", rekeyR1).ItemPID; got != item {
		t.Errorf("the mp3 naming the id backs %s, want %s", got, item)
	}
	assertVerifyClean(t, st)
}

// TestACueTrackIsNeverAdopted: a track of a cue rip holds a recording id in its column.
// A whole file naming that id gets an item of its own rather than joining a window of
// another file.
func TestACueTrackIsNeverAdopted(t *testing.T) {
	t.Parallel()
	st, put, _ := rekeyFixture(t)
	ctx := context.Background()
	lib := int64(scalarInt(t, st, "SELECT id FROM library ORDER BY id DESC LIMIT 1"))
	var vts []model.VirtualTrack
	for i, title := range []string{"Rip 1", "Rip 2"} {
		start := int64(i) * 750
		vts = append(vts, model.VirtualTrack{
			Item: model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: title,
				IdentityKey: identity.VirtualTrackKey("rip-e", i+1, start)},
			Track:       model.Track{Artist: "A", AlbumArtist: "A", Album: "Rip", TrackNo: i + 1, MBID: []string{rekeyR1, ""}[i]},
			StartFrames: start, EndFrames: []int64{750, 0}[i],
		})
	}
	if _, err := st.PutScannedVirtualTracks(ctx, model.PutScannedVirtualTracksInput{
		LibraryID: lib,
		File: model.File{Path: []byte("/rip.flac"), DisplayPath: "/rip.flac", RelPath: []byte("rip.flac"),
			Kind: model.FileAudio, Size: 1, MTimeNS: 1, ContentHash: "rip-c", EssenceHash: "rip-e",
			DurationMS: 30_000, ScanState: model.ScanIndexed},
		Tracks: vts,
	}); err != nil {
		t.Fatalf("put the rip: %v", err)
	}
	rip := model.PID(scalarStr(t, st, "SELECT pi.pid FROM playable_item pi JOIN track t ON t.item_id = pi.id WHERE t.mbid = ?", rekeyR1))
	if got := put("A/one.flac", "rk-1", "c1", "flac", rekeyR1).ItemPID; got == rip {
		t.Error("a whole file joined a cue track through its column")
	}
}

// TestARetitledSingleFileBookKeepsItsItem: a book of one file is retitled by hand. The
// book keeps its item, keyed on the new title, where a book of several parts would be
// left to its parts.
func TestARetitledSingleFileBookKeepsItsItem(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/W/book.m4b", essence: "bk-1", content: "b1",
		title: "Old Title", author: "Writer", durationMS: 1000}).ItemPID
	res := putBook(t, st, lib.ID, bookSpec{path: "/lib/W/book.m4b", essence: "bk-1", content: "b2",
		title: "New Title", author: "Writer", durationMS: 1000})
	if res.ItemPID != book {
		t.Errorf("the retitled book is %s, want %s kept", res.ItemPID, book)
	}
	if got, want := identityKeyOf(t, st, book), identity.BookKey("", "", "Writer", "New Title", ""); got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	assertVerifyClean(t, st)
}

// TestEveryEncodingRetaggedKeepsTheItemWhicheverIsReadFirst: an item's primary and its
// other encoding are both retagged with a new recording id. Read the other encoding
// first, it waits on its item, since its primary has changed on disk since it was read,
// and the scan reads it again after the primary has re-keyed the item; read the primary
// first, it re-keys the item and the other encoding finds it. Either way the item is kept
// with both files.
func TestEveryEncodingRetaggedKeepsTheItemWhicheverIsReadFirst(t *testing.T) {
	t.Parallel()
	for _, alternateFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "alternate first", false: "primary first"}[alternateFirst], func(t *testing.T) {
			t.Parallel()
			st, put, retag := rekeyFixture(t)
			item := put("A/one.flac", "rk-flac", "flac one", "flac", rekeyR1).ItemPID
			if res := put("A/one.mp3", "rk-mp3", "mp3 one", "mp3", rekeyR1); res.ItemPID != item {
				t.Fatalf("the mp3 backs %s, want the flac's item %s", res.ItemPID, item)
			}
			retag("A/one.flac", "flac two")
			retag("A/one.mp3", "mp3 two")
			var reread []model.PromotedFile
			if alternateFirst {
				res := put("A/one.mp3", "rk-mp3", "mp3 two", "mp3", rekeyR2)
				if res.ItemPID != item {
					t.Errorf("the mp3 read first backs %s, want it waiting on %s", res.ItemPID, item)
				}
				reread = append(reread, res.Promoted...)
			}
			res := put("A/one.flac", "rk-flac", "flac two", "flac", rekeyR2)
			if res.ItemPID != item {
				t.Fatalf("the flac backs %s, want %s kept", res.ItemPID, item)
			}
			reread = append(reread, res.Promoted...)
			if len(reread) == 0 {
				t.Fatal("nothing was named to read again")
			}
			for _, f := range reread {
				if n := scalarInt(t, st, "SELECT mtime_ns FROM file WHERE pid = ?", string(f.FilePID)); n != -1 {
					t.Errorf("%s keeps its fast-path stamp %d, so a scan cut short would skip it", f.Path, n)
				}
			}
			// The scan reads the named file again.
			if got := put("A/one.mp3", "rk-mp3", "mp3 two", "mp3", rekeyR2).ItemPID; got != item {
				t.Errorf("the mp3 read again backs %s, want %s", got, item)
			}
			if got := identityKeyOf(t, st, item); got != "mbid:"+rekeyR2 {
				t.Errorf("key = %q, want the new id", got)
			}
			if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item"); n != 1 {
				t.Errorf("items = %d, want the one", n)
			}
			assertVerifyClean(t, st)
		})
	}
}

// TestAPrimaryIsStatedOutsideTheWriteLock: whether a retagged copy's primary changed on
// disk is a filesystem call, which a slow share can hold for long, so it runs before the
// put's transaction and never while the write lock is held.
func TestAPrimaryIsStatedOutsideTheWriteLock(t *testing.T) {
	t.Parallel()
	st, put, retag := rekeyFixture(t)
	var stated, locked int
	st.statFile = func(ctx context.Context, root, path string) (fs.FileInfo, error) {
		if st.wmu.TryLock() {
			st.wmu.Unlock()
		} else {
			locked++
		}
		stated++
		return os.Stat(path)
	}
	item := put("A/one.flac", "rk-flac", "flac one", "flac", rekeyR1).ItemPID
	put("A/one.mp3", "rk-mp3", "mp3 one", "mp3", rekeyR1)
	retag("A/one.mp3", "mp3 two")
	if res := put("A/one.mp3", "rk-mp3", "mp3 two", "mp3", rekeyR2); res.ItemPID == item {
		t.Errorf("the retagged copy stayed on %s though its primary is unchanged", item)
	}
	if stated == 0 || locked != 0 {
		t.Errorf("the primary was stated %d times, %d of them under the write lock, want it stated outside it", stated, locked)
	}
}
