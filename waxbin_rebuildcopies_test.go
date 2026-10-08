package waxbin_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// TestRebuildKeepsTheStampedPIDOverACopyReadFirst: an unstamped copy the walk reaches
// before its stamped original makes the item, and the original arriving after it hands
// the item the pid it carries, whether the two sit in one root or in two.
func TestRebuildKeepsTheStampedPIDOverACopyReadFirst(t *testing.T) {
	t.Parallel()
	for _, split := range []bool{false, true} {
		ctx := context.Background()
		rootA, rootB := t.TempDir(), t.TempDir()
		stampedRoot := rootA
		if split {
			stampedRoot = rootB
		}
		stamp := model.NewPID()
		audio := testaudio.AudioWithSeed(41)
		// "A Copy" sorts before "B Original", so the copy is read first.
		writeFile(t, filepath.Join(rootA, "A Copy", "song.mp3"), testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, audio))
		writeFile(t, filepath.Join(stampedRoot, "B Original", "song.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: "Song", Artist: "Artist", Album: "Album", Track: 1, Audio: audio,
			TXXX: []testaudio.TXXXFrame{{Desc: model.TagWaxbinItemPID, Value: string(stamp)}},
		}))
		roots := []config.Root{{Path: rootA, Mode: model.ModeInPlace}}
		if split {
			roots = append(roots, config.Root{Path: rootB, Mode: model.ModeInPlace})
		}
		lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"), Roots: roots})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{AdoptStampedPIDs: true}); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
		if err != nil || len(items) != 1 {
			t.Fatalf("split=%v: items = %d (err %v), want the copy and the original as one", split, len(items), err)
		}
		if items[0].PID != stamp {
			t.Errorf("split=%v: rebuilt item pid = %s, want the stamped %s", split, items[0].PID, stamp)
		}
		files, err := lib.ItemFiles(ctx, items[0].PID)
		if err != nil || len(files) != 2 {
			t.Errorf("split=%v: item files = %+v (err %v), want both", split, files, err)
		}
		_ = lib.Close()
	}
}

// TestRebuildAdoptionKeepsAFolderBookWhole: a stamped part handing the book its pid
// mid-walk leaves the folder rule still knowing it as one book, so a numbered part with no
// ALBUM read after it joins that book.
func TestRebuildAdoptionKeepsAFolderBookWhole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	stamp := model.NewPID()
	stamped := []testaudio.TXXXFrame{{Desc: model.TagWaxbinItemPID, Value: string(stamp)}}
	for n, album := range []string{"The Book", "The Book", ""} {
		var tx []testaudio.TXXXFrame
		if n > 0 {
			tx = stamped
		}
		writeFile(t, filepath.Join(root, "Book", "0"+string(rune('1'+n))+".mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: "Part", Artist: "Author", Album: album, Track: n + 1, Audio: testaudio.AudioWithSeed(byte(70 + n)), TXXX: tx}))
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace, Media: model.MediaAudiobook}}})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{AdoptStampedPIDs: true}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v (err %v), want the one book", items, err)
	}
	if items[0].PID != stamp {
		t.Errorf("book pid = %s, want the stamped %s", items[0].PID, stamp)
	}
	if files, err := lib.ItemFiles(ctx, items[0].PID); err != nil || len(files) != 3 {
		t.Errorf("book files = %d (err %v), want the three parts", len(files), err)
	}
}
