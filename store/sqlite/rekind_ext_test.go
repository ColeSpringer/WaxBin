package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestKindLockRefusedOnAVirtualTrack: a cue track's kind follows its rip, which no lock on
// one track can pin, so the lock is refused.
func TestKindLockRefusedOnAVirtualTrack(t *testing.T) {
	st, lib := openTestStore(t)
	ctx := context.Background()
	res, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, "/lib/rip.flac", "vk1", "vkc1", 10000, [][2]int64{{0, 300}, {300, 0}}))
	if err != nil {
		t.Fatalf("put rip: %v", err)
	}
	_ = res
	items := vtItems(t, st)
	if len(items) != 2 {
		t.Fatalf("virtual tracks = %d, want 2", len(items))
	}
	if err := st.LockField(ctx, items[0].PID, model.KindLockField); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("kind lock on a virtual track = %v, want refused", err)
	}
}

// TestRekindKeepsAnEncodingAlternate: another encoding of a track's recording stays an
// alternate when the track turns into a book and the encoding is read as one, rather than
// becoming a second part of the same audio.
func TestRekindKeepsAnEncodingAlternate(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	mp3, flac := filepath.Join(root, "mp3", "1.mp3"), filepath.Join(root, "flac", "1.flac")
	touch(t, mp3)
	touch(t, flac)
	encoding := func(path, essence, codec string, bitrate, depth int) model.PutScannedTrackInput {
		in := input(lib.ID, path, essence, "sha256:C"+essence, "Chapter")
		in.Item.IdentityKey, in.Track.MBID = "mbid:rec-1", "rec-1"
		in.File.Codec, in.File.Bitrate, in.File.SampleRate, in.File.BitDepth, in.File.DurationMS = codec, bitrate, 44100, depth, 1000
		return in
	}
	track := mustPut(t, st, encoding(flac, "EFLAC", "flac", 900000, 16)).ItemPID
	alt := mustPut(t, st, encoding(mp3, "EMP3", "mp3", 320000, 0)).FilePID
	book := func(path, essence, codec string) model.PutScannedBookInput {
		return model.PutScannedBookInput{
			LibraryID: lib.ID,
			File: model.File{Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
				Kind: model.FileAudio, Size: 5, MTimeNS: 1, ContentHash: "sha256:C" + essence, EssenceHash: essence,
				ScanState: model.ScanIndexed, Codec: codec, DurationMS: 1000},
			Item: model.PlayableItem{Kind: model.KindBook, State: model.StatePresent, Title: "Tome",
				SortKey: model.SortKey("Tome"), IdentityKey: "book:author\x1ftome\x1f"},
			Book:     model.Book{Author: "Author"},
			Position: 1,
			Chapters: []model.Chapter{{Title: "Chapter"}}, ChapterSource: "synthetic",
		}
	}
	if r, err := st.PutScannedBook(ctx, book(flac, "EFLAC", "flac")); err != nil || r.ItemPID != track {
		t.Fatalf("flac as a book = %+v (err %v), want the track re-kinded", r, err)
	}
	if _, err := st.PutScannedBook(ctx, book(mp3, "EMP3", "mp3")); err != nil {
		t.Fatalf("mp3 as a book: %v", err)
	}
	if role := rolesOf(t, st, track)[alt]; role != "alternate" {
		t.Errorf("mp3 role = %q, want it kept an alternate", role)
	}
	if b, err := st.BookByPID(ctx, track); err != nil || b.TotalDurationMS != 1000 {
		t.Errorf("book duration = %v (err %v), want one part's 1000 ms", b, err)
	}
}
