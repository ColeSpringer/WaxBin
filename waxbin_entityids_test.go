package waxbin_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
)

const (
	idsAlbum = "a1000000-0000-4000-8000-000000000001"
	idsGroup = "b1000000-0000-4000-8000-000000000002"
)

// idsAlbumFixture scans an album of two tracks into a managed library, and returns the
// library, its catalog file, the two files and the album's and its release group's pids.
func idsAlbumFixture(t *testing.T, extra ...config.Root) (lib *waxbin.Library, db string, files [2]string, album, group model.PID) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db = filepath.Join(t.TempDir(), "catalog.db")
	for n, title := range []string{"One", "Two"} {
		files[n] = filepath.Join(root, "Album", title+".mp3")
		writeFile(t, files[n], testaudio.BuildMP3WithAudio(title, "Artist", "Album", n+1, testaudio.AudioWithSeed(byte(80+n))))
	}
	roots := append([]config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}, extra...)
	lib, err := waxbin.OpenSeeded(t, ctx, waxbin.Options{DBPath: db, Roots: roots})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	album, group = idsChain(t, ctx, lib)
	return lib, db, files, album, group
}

// idsChain reads the album and release group every track sits on, failing unless there
// is exactly one of each.
func idsChain(t *testing.T, ctx context.Context, lib *waxbin.Library) (album, group model.PID) {
	t.Helper()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) == 0 {
		t.Fatalf("query: %d items (err %v)", len(items), err)
	}
	for _, it := range items {
		if album == "" {
			album, group = it.AlbumPID, it.ReleaseGroupPID
		}
		if it.AlbumPID != album || it.ReleaseGroupPID != group {
			t.Fatalf("items sit on albums %s and %s (groups %s, %s), want one", album, it.AlbumPID, group, it.ReleaseGroupPID)
		}
	}
	return album, group
}

// fileIDs reads the release and release-group ids a file's tags name.
func fileIDs(t *testing.T, path string) (album, group string) {
	t.Helper()
	fm, err := meta.NewReader().Read(context.Background(), path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fm.Tags.MBReleaseID, fm.Tags.MBReleaseGroupID
}

// TestAnAlbumsIDsWrittenBackKeepItsRows sets an album's and its release group's ids with
// write-back. The member files carry both, and a scan resolving every file again, and one
// resolving a single file, land them on the same rows, with no new album or group and
// nothing owed. A retag or a move is what makes a scan resolve a file again.
func TestAnAlbumsIDsWrittenBackKeepItsRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, db, files, album, group := idsAlbumFixture(t)
	opts := waxbin.EntityEditOptions{Lock: model.LockOn, WriteBack: true}
	if _, err := lib.EditEntity(ctx, model.MergeAlbum, album, map[string]string{"mbid": idsAlbum}, opts); err != nil {
		t.Fatalf("set the album's id: %v", err)
	}
	if _, err := lib.EditEntity(ctx, model.MergeReleaseGroup, group, map[string]string{"mbid": idsGroup}, opts); err != nil {
		t.Fatalf("set the group's id: %v", err)
	}
	for _, f := range files {
		if a, g := fileIDs(t, f); a != idsAlbum || g != idsGroup {
			t.Errorf("%s names %q and %q, want both ids", f, a, g)
		}
	}
	check := func(when string) {
		t.Helper()
		if a, g := idsChain(t, ctx, lib); a != album || g != group {
			t.Errorf("%s: the tracks sit on %s under %s, want %s under %s", when, a, g, album, group)
		}
		if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 1 {
			t.Errorf("%s: %d albums, want 1", when, n)
		}
		if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM release_group"); n != 1 {
			t.Errorf("%s: %d release groups, want 1", when, n)
		}
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, ReResolve: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	check("after a forced scan")
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{SubPath: files[0], Force: true, ReResolve: true}); err != nil {
		t.Fatalf("scan one file: %v", err)
	}
	check("after one file was read again")
	if ds, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{Code: model.DiagTagWriteOwed}); err != nil || len(ds) != 0 {
		t.Errorf("owed rows = %+v (err %v), want none", ds, err)
	}
}

// TestAGroupsIDAloneOnTheFilesKeepsTheAlbum writes the release group's id alone, so the
// files name a group and no release. A forced rescan lands them on the album they sat on.
func TestAGroupsIDAloneOnTheFilesKeepsTheAlbum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, files, album, group := idsAlbumFixture(t)
	if _, err := lib.EditEntity(ctx, model.MergeReleaseGroup, group, map[string]string{"mbid": idsGroup},
		waxbin.EntityEditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("set the group's id: %v", err)
	}
	if a, g := fileIDs(t, files[1]); a != "" || g != idsGroup {
		t.Fatalf("the file names %q and %q, want the group's id alone", a, g)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, ReResolve: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if a, g := idsChain(t, ctx, lib); a != album || g != group {
		t.Errorf("the tracks sit on %s under %s, want %s under %s", a, g, album, group)
	}
}

// TestAnAlbumWithAReadOnlyCopyHoldsItsID: one member has a copy in a read-only library,
// so the album's id goes on none of the files, the write-back says why, and every file
// still owes it. Once the library is writable the same set writes all of them.
func TestAnAlbumWithAReadOnlyCopyHoldsItsID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	other := t.TempDir()
	copyPath := filepath.Join(other, "One.mp3")
	writeFile(t, copyPath, testaudio.BuildMP3WithAudio("One", "Artist", "Album", 1, testaudio.AudioWithSeed(80)))
	lib, _, files, album, _ := idsAlbumFixture(t, config.Root{Path: other, Mode: model.ModeInPlace})
	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, other), true)

	set := func() error {
		_, err := lib.EditEntity(ctx, model.MergeAlbum, album, map[string]string{"mbid": idsAlbum},
			waxbin.EntityEditOptions{Lock: model.LockOn, WriteBack: true, Force: true})
		return err
	}
	err := set()
	var wb *waxbin.WriteBackError
	if !errors.As(err, &wb) || len(wb.Failures) != 3 {
		t.Fatalf("set with a read-only copy = %v, want the id held from the three files", err)
	}
	for _, f := range wb.Failures {
		if f.Key != "MUSICBRAINZ_ALBUMID" || !strings.Contains(f.Reason, "read-only library") {
			t.Errorf("failure %+v, want the id held for the read-only copy", f)
		}
	}
	for _, f := range append(files[:], copyPath) {
		if a, _ := fileIDs(t, f); a != "" {
			t.Errorf("%s took the id %q while it was held", f, a)
		}
	}
	owed, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{Code: model.DiagTagWriteOwed})
	if err != nil || len(owed) != 3 {
		t.Fatalf("owed rows = %+v (err %v), want one per member file", owed, err)
	}

	setReadOnly(t, ctx, lib, libraryPIDFor(t, ctx, lib, other), false)
	if err := set(); err != nil {
		t.Fatalf("set once writable: %v", err)
	}
	for _, f := range append(files[:], copyPath) {
		if a, _ := fileIDs(t, f); a != idsAlbum {
			t.Errorf("%s names %q once writable, want the id", f, a)
		}
	}
	if owed, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{Code: model.DiagTagWriteOwed}); err != nil || len(owed) != 0 {
		t.Errorf("owed rows once written = %+v (err %v), want none", owed, err)
	}
}

// TestACorrectedIDWrittenBackKeepsAnIDKeyedAlbum: the files named a release id when they
// were first read, so the album is keyed on it. A corrected id written back lands the
// files on the same row through its column, and clearing the ids afterwards strips both
// and keeps the rows' members together.
func TestACorrectedIDWrittenBackKeepsAnIDKeyedAlbum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	var files [2]string
	for n, title := range []string{"One", "Two"} {
		files[n] = filepath.Join(root, "Album", title+".mp3")
		writeFile(t, files[n], testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: title, Artist: "Artist", AlbumArtist: "Artist", Album: "Album", Track: n + 1,
			Audio: testaudio.AudioWithSeed(byte(90 + n)),
			TXXX: []testaudio.TXXXFrame{{Desc: "MusicBrainz Album Id", Value: "c1000000-0000-4000-8000-000000000003"},
				{Desc: "MusicBrainz Release Group Id", Value: "d1000000-0000-4000-8000-000000000004"}},
		}))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	album, group := idsChain(t, ctx, lib)
	opts := waxbin.EntityEditOptions{Lock: model.LockOn, WriteBack: true}
	if _, err := lib.EditEntity(ctx, model.MergeAlbum, album, map[string]string{"mbid": idsAlbum}, opts); err != nil {
		t.Fatalf("correct the album's id: %v", err)
	}
	if a, _ := fileIDs(t, files[0]); a != idsAlbum {
		t.Fatalf("the file names %q, want the corrected id", a)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, ReResolve: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if a, g := idsChain(t, ctx, lib); a != album || g != group {
		t.Errorf("after the correction the tracks sit on %s under %s, want %s under %s", a, g, album, group)
	}

	for _, et := range []model.MergeEntity{model.MergeAlbum, model.MergeReleaseGroup} {
		pid := album
		if et == model.MergeReleaseGroup {
			pid = group
		}
		clear := opts
		clear.Force = true
		if _, err := lib.EditEntity(ctx, et, pid, map[string]string{"mbid": ""}, clear); err != nil {
			t.Fatalf("clear the %s id: %v", et, err)
		}
	}
	for _, f := range files {
		if a, g := fileIDs(t, f); a != "" || g != "" {
			t.Errorf("%s names %q and %q after the clears, want neither", f, a, g)
		}
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, ReResolve: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	idsChain(t, ctx, lib)
}

// TestARecordingIDWrittenBackKeepsTheItem sets a track's recording id with write-back,
// then clears it. Each write re-keys the item on what its file now names, with no file
// read, so a file tagged with the id it gave up does not land on it, and a scan resolving
// the file again keeps the item with its lock, cover, lyrics and star.
func TestARecordingIDWrittenBackKeepsTheItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const r1, r2 = "4e2b1b2a-0000-4000-8000-0000000000e1", "4e2b1b2a-0000-4000-8000-0000000000e2"
	root := t.TempDir()
	// tagged writes a file of its own audio whose tags name the recording rec.
	tagged := func(path, title string, seed byte, rec string) {
		t.Helper()
		writeFile(t, path, testaudio.BuildMP3WithAudio(title, "Artist", "Album", 1, testaudio.AudioWithSeed(seed)))
		doc, err := waxlabel.ParseFile(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := doc.Edit().Set(tag.MBRecordingID, rec).Prepare(waxlabel.WithVerifyEssence())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
			t.Fatal(err)
		}
	}
	song := filepath.Join(root, "Album", "Song.mp3")
	tagged(song, "Song", 100, r1)
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	pid := itemPIDByTitle(t, ctx, lib, "Song")
	if err := lib.EditFields(ctx, pid, map[string]string{"genre": "Kept"}, waxbin.EditOptions{Lock: model.LockOn}); err != nil {
		t.Fatal(err)
	}
	if err := lib.SetItemArt(ctx, pid, model.ArtRoleFront, coverPNG(t), waxbin.ArtEditOptions{Lock: model.LockOff}); err != nil {
		t.Fatal(err)
	}
	if err := lib.SetLyrics(ctx, pid, &model.Lyrics{Unsynced: "la la"}, model.LockOn, false); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Playback().SetStar(ctx, "", pid, true, nil); err != nil {
		t.Fatal(err)
	}
	cover := frontOf(t, ctx, lib, model.EntityRef{Type: model.ArtTrack, PID: pid})

	stray := func(name, rec string, seed byte) model.PID {
		t.Helper()
		path := filepath.Join(root, name, "Stray.mp3")
		tagged(path, "Stray "+name, seed, rec)
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{SubPath: filepath.Dir(path)}); err != nil {
			t.Fatal(err)
		}
		return itemPIDByTitle(t, ctx, lib, "Stray "+name)
	}
	if err := lib.EditFields(ctx, pid, map[string]string{"mbid": r2}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("set the id: %v", err)
	}
	if got := stray("one", r1, 101); got == pid {
		t.Error("a file naming the id the item gave up landed on it")
	}
	if err := lib.EditFields(ctx, pid, map[string]string{"mbid": ""}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true, Force: true}); err != nil {
		t.Fatalf("clear the id: %v", err)
	}
	if fm, err := meta.NewReader().Read(ctx, song); err != nil || fm.Tags.MBID != "" {
		t.Fatalf("the file names %q after the clear (err %v), want none", fm.Tags.MBID, err)
	}
	if got := stray("two", r2, 102); got == pid {
		t.Error("a file naming the cleared id landed on the item")
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, ReResolve: true}); err != nil {
		t.Fatal(err)
	}
	it, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("the item is gone after the scan: %v", err)
	}
	if it.Genre != "Kept" {
		t.Errorf("genre = %q, want the locked value", it.Genre)
	}
	if got := frontOf(t, ctx, lib, model.EntityRef{Type: model.ArtTrack, PID: pid}); got != cover {
		t.Errorf("cover = %q, want %q", got, cover)
	}
	if ly, err := lib.Lyrics(ctx, pid); err != nil || ly == nil || ly.Unsynced != "la la" {
		t.Errorf("lyrics = %+v (err %v), want the set ones", ly, err)
	}
	if ps, err := lib.Playback().State(ctx, "", pid); err != nil || !ps.Starred {
		t.Errorf("play state = %+v (err %v), want it starred", ps, err)
	}
}

// TestAnAlbumWithAnUnwritableMemberHoldsItsID: one member's file has become one the tag
// writer reads and never writes (a WMA), so the album's id goes on none of the files,
// and the write-back says why for each.
func TestAnAlbumWithAnUnwritableMemberHoldsItsID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, files, album, _ := idsAlbumFixture(t)
	writeFile(t, files[1], testaudio.Fixture(t, "lossless-s16.wma"))
	_, err := lib.EditEntity(ctx, model.MergeAlbum, album, map[string]string{"mbid": idsAlbum},
		waxbin.EntityEditOptions{Lock: model.LockOn, WriteBack: true})
	var wb *waxbin.WriteBackError
	if !errors.As(err, &wb) || len(wb.Failures) != 2 {
		t.Fatalf("set with an unwritable member = %v, want the id held from both files", err)
	}
	for _, f := range wb.Failures {
		if f.Key != "MUSICBRAINZ_ALBUMID" || !strings.Contains(f.Reason, "Two.mp3") {
			t.Errorf("failure %+v, want the id held for the file that cannot take it", f)
		}
	}
	if a, _ := fileIDs(t, files[0]); a != "" {
		t.Errorf("%s took the id %q while it was held", files[0], a)
	}
}
