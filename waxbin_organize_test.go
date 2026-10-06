package waxbin_test

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestOrganizeFilesSpellingsOfOneEntityTogether: tracks tagged with spellings of one artist
// (as their artist, or as their album artist alone) and of one album file under the
// spelling the catalog keeps for each, so one artist and one album get one folder, while a
// joint credit, which is not that artist's name, keeps its own.
func TestOrganizeFilesSpellingsOfOneEntityTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, spec := range []testaudio.MP3Spec{
		{Title: "One", Artist: "Amir Obe", AlbumArtist: "Amir Obe", Album: "Night", Track: 1},
		{Title: "Two", Artist: "Amir Obè", AlbumArtist: "Amir Obè", Album: "NIGHT!", Track: 2},
		{Title: "Three", Artist: "Amir Obe & Friend", AlbumArtist: "Amir Obe & Friend", Album: "Duets", Track: 1},
		{Title: "Four", Artist: "Guest Star", AlbumArtist: "AMIR OBE", Album: "Night", Track: 4},
		{Title: "Five", Artist: "Guest Star", AlbumArtist: "DJ Night", Album: "Mix", Track: 1},
		{Title: "Six", Artist: "Other Guest", AlbumArtist: "dj night", Album: "Mix", Track: 2},
	} {
		// Named for their order, so the first spelling scanned, which the catalog keeps, is
		// One's.
		spec.Audio = testaudio.AudioWithSeed(byte(i + 1))
		writeFile(t, filepath.Join(root, "incoming", strconv.Itoa(i+1)+".mp3"), testaudio.BuildMP3FromSpec(spec))
	}
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Actions) != 6 {
		t.Fatalf("actions = %+v, want 6", plan.Actions)
	}
	want := map[string]string{
		"1.mp3": filepath.Join("Amir Obe", "Night", "01 - One.mp3"),
		"2.mp3": filepath.Join("Amir Obe", "Night", "02 - Two.mp3"),
		"3.mp3": filepath.Join("Amir Obe & Friend", "Duets", "01 - Three.mp3"),
		"4.mp3": filepath.Join("Amir Obe", "Night", "04 - Four.mp3"),
		"5.mp3": filepath.Join("DJ Night", "Mix", "01 - Five.mp3"),
		"6.mp3": filepath.Join("DJ Night", "Mix", "02 - Six.mp3"),
	}
	for _, a := range plan.Actions {
		if w := want[filepath.Base(a.Src)]; a.RelDst != w {
			t.Errorf("%s plans to %s, want %s", filepath.Base(a.Src), a.RelDst, w)
		}
	}
}

// TestImportFilesSpellingsOfOneAlbumTogether: a staged album whose tracks spell its artist
// and title two ways lands in one folder under the first spellings, which the catalog then
// keeps, so it stays one album; an artist's first spelling holds across the import.
func TestImportFilesSpellingsOfOneAlbumTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	for i, s := range []struct {
		rel  string
		spec testaudio.MP3Spec
	}{
		{filepath.Join("Night", "1.mp3"), testaudio.MP3Spec{Title: "One", Artist: "Amir Obe", AlbumArtist: "Amir Obe", Album: "Night", Track: 1}},
		{filepath.Join("Night", "2.mp3"), testaudio.MP3Spec{Title: "Two", Artist: "Amir Obè", AlbumArtist: "Amir Obè", Album: "NIGHT!", Track: 2}},
		{filepath.Join("Solo", "1.mp3"), testaudio.MP3Spec{Title: "Alone", Artist: "AMIR OBE", AlbumArtist: "AMIR OBE", Album: "Solo", Track: 1}},
		{filepath.Join("Third", "1.mp3"), testaudio.MP3Spec{Title: "Late", Artist: "amir obè", Album: "Third", Track: 1}},
	} {
		s.spec.Audio = testaudio.AudioWithSeed(byte(i + 1))
		writeFile(t, filepath.Join(staging, s.rel), testaudio.BuildMP3FromSpec(s.spec))
	}
	plan := importAll(t, ctx, lib, staging, 4)
	want := map[string]string{
		"Night": filepath.Join(root, "Amir Obe", "Night"),
		"Solo":  filepath.Join(root, "Amir Obe", "Solo"),
		"Third": filepath.Join(root, "Amir Obe", "Third"),
	}
	for _, a := range plan.Actions {
		if w := want[filepath.Base(filepath.Dir(a.Src))]; filepath.Dir(a.Dst) != w {
			t.Errorf("%s lands in %s, want %s", a.Src, filepath.Dir(a.Dst), w)
		}
	}
	all, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatal(err)
	}
	albums := map[model.PID]int{}
	for _, it := range all {
		albums[it.AlbumPID]++
	}
	if len(all) != 4 || len(albums) != 3 {
		t.Errorf("imported %d tracks on %d albums, want 4 on Night, Solo and Third", len(all), len(albums))
	}
}

// writeCueRip writes a two-track WAV rip of its own audio (seed) and its cue sheet under
// dir, its tracks titled first and second, returning the rip's path.
func writeCueRip(t *testing.T, dir string, seed int64, first, second string) string {
	t.Helper()
	const rate = 22050
	rip := filepath.Join(dir, "album.wav")
	writeFile(t, rip, testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 20, testaudio.AltPartials, seed)))
	writeFile(t, filepath.Join(dir, "album.cue"), []byte("PERFORMER \"Rip Artist\"\nTITLE \"Rip Album\"\nFILE \"album.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \""+first+"\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \""+second+"\"\n    INDEX 01 00:10:00\n"))
	return rip
}

// TestOrganizeHoldsACueRipWhole: the tracks a cue sheet carves out of one file are held as
// one line naming the file, never planned as a move of that file per track.
func TestOrganizeHoldsACueRipWhole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	rip := writeCueRip(t, filepath.Join(root, "rip"), 7, "One", "Two")
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	if items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), ""); err != nil || len(items) != 2 || !items[0].Virtual {
		t.Fatalf("items = %+v (err %v), want the rip's two virtual tracks", items, err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Code != organize.HoldVirtual || plan.Actions[0].Src != rip || plan.Pending() != 0 {
		t.Fatalf("plan = %+v, want the rip held once as virtual", plan.Actions)
	}
}

// TestTrashRefusesOneTrackOfACueRip: deleting one track a cue sheet carves out of a file
// would delete the file its sibling plays, so it is refused naming the sibling; deleting
// both trashes the file once and archives both.
func TestTrashRefusesOneTrackOfACueRip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	rip := writeCueRip(t, filepath.Join(root, "rip"), 7, "One", "Two")
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	one, two := itemPIDByTitle(t, ctx, lib, "One"), itemPIDByTitle(t, ctx, lib, "Two")

	writeCueRip(t, filepath.Join(root, "rip2"), 9, "Three", "Four")
	scanLib(t, ctx, lib)
	three, four := itemPIDByTitle(t, ctx, lib, "Three"), itemPIDByTitle(t, ctx, lib, "Four")

	_, err := lib.PlanDeletePIDs(ctx, []model.PID{one, three}, model.DeleteTrash)
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), string(two)) || !strings.Contains(err.Error(), string(four)) {
		t.Fatalf("delete of one track of each rip = %v, want CodeInvalid naming %s and %s", err, two, four)
	}
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{one, two}, model.DeleteTrash)
	if err != nil || len(plan.Actions) != 1 || plan.Actions[0].Src != rip {
		t.Fatalf("delete of the whole rip = %+v (err %v), want its file once", plan, err)
	}
	if got := plan.Actions[0].Tracks; len(got) != 2 || got[0] != one || got[1] != two {
		t.Errorf("the rip's action names tracks %v, want %s and %s", got, one, two)
	}
	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil || rep.Trashed != 1 || rep.Errored != 0 {
		t.Fatalf("apply = %+v (err %v), want the file trashed once", rep, err)
	}
	if fileExists(rip) {
		t.Error("the rip is still in place")
	}
	for _, pid := range []model.PID{one, two} {
		if it, err := lib.Get(ctx, pid); err != nil || it.State != model.StateArchived {
			t.Errorf("track %s = %+v (err %v), want archived", pid, it, err)
		}
	}
}

// TestDeleteSweepLeavesAPartlyMatchedRip: a sweep matching some of the tracks a cue sheet
// carves out of one file leaves that rip whole and counts what it left, rather than failing
// the sweep, and deletes the rest of what it matched.
func TestDeleteSweepLeavesAPartlyMatchedRip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	rip := writeCueRip(t, filepath.Join(root, "rip"), 7, "One", "Two")
	other := filepath.Join(root, "other.mp3")
	writeFile(t, other, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Other", Artist: "A", Album: "B", Track: 1}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)

	q := query.New(query.EntityItems).WhereValues("title", query.OpIn, "One", "Other").Build()
	plan, err := lib.PlanDelete(ctx, q, model.DeleteTrash)
	if err != nil {
		t.Fatalf("sweep over part of a rip = %v, want it planned", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Src != other || plan.SkippedRipTracks != 1 {
		t.Fatalf("plan = %+v, want Other alone and one rip track left", plan)
	}
	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil || rep.Trashed != 1 || rep.SkippedRipTracks != 1 {
		t.Fatalf("apply = %+v (err %v), want Other trashed and the rip track counted", rep, err)
	}
	if !fileExists(rip) || fileExists(other) {
		t.Errorf("after the sweep: rip present %v, Other present %v; want the rip kept and Other gone", fileExists(rip), fileExists(other))
	}
}

// TestImportTakesTheCatalogsSpellings: a track imported into a library that already holds
// its artist and album under other spellings lands in the album's folder under those
// spellings, and a single with no album takes the artist's too.
func TestImportTakesTheCatalogsSpellings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "Amir Obe", "Night", "01 - One.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Amir Obe", AlbumArtist: "Amir Obe", Album: "Night", Track: 1, Audio: testaudio.AudioWithSeed(1)}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	writeFile(t, filepath.Join(staging, "Night", "2.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Amir Obè", AlbumArtist: "Amir Obè", Album: "NIGHT!", Track: 2, Audio: testaudio.AudioWithSeed(2)}))
	writeFile(t, filepath.Join(staging, "single.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Single", Artist: "AMIR OBÈ", Audio: testaudio.AudioWithSeed(3)}))
	plan := importAll(t, ctx, lib, staging, 2)
	want := map[string]string{
		"2.mp3":      filepath.Join(root, "Amir Obe", "Night", "02 - Two.mp3"),
		"single.mp3": filepath.Join(root, "Amir Obe", "Unknown Album", "Single.mp3"),
	}
	for _, a := range plan.Actions {
		if w := want[filepath.Base(a.Src)]; a.Dst != w {
			t.Errorf("%s lands at %s, want %s", filepath.Base(a.Src), a.Dst, w)
		}
	}
}

// TestOrganizePlansAndAppliesInOneJob: the direct organize plans and moves under one job,
// reports what it did and records the report on that job.
func TestOrganizePlansAndAppliesInOneJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 3))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	rr, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{})
	if err != nil || rr.Profile != "waxbin-native" || rr.Report.Moved != 1 {
		t.Fatalf("organize = %+v (err %v), want one move under waxbin-native", rr, err)
	}
	if fileExists(src) || !fileExists(filepath.Join(root, "The Foobars", "Night Moves", "03 - Midnight Drive.mp3")) {
		t.Error("the file did not move")
	}
	jobs, err := lib.Jobs(ctx, 1)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "organize" || !strings.Contains(jobs[0].Result, `"Moved":1`) {
		t.Fatalf("jobs = %+v (err %v), want the organize job with its report", jobs, err)
	}
}

// TestDeleteOfARipFileNamesItsTracks: deleting a cue rip's file by its pid archives every
// track it plays, so the plan names them.
func TestDeleteOfARipFileNamesItsTracks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeCueRip(t, filepath.Join(root, "rip"), 7, "One", "Two")
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	one, two := itemPIDByTitle(t, ctx, lib, "One"), itemPIDByTitle(t, ctx, lib, "Two")
	files, err := lib.ItemFiles(ctx, one)
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %+v (err %v), want the rip", files, err)
	}
	plan, err := lib.PlanDeleteFiles(ctx, []model.PID{files[0].FilePID}, model.DeleteTrash)
	if err != nil || len(plan.Actions) != 1 {
		t.Fatalf("plan = %+v (err %v), want the rip's file", plan, err)
	}
	if got := plan.Actions[0].Tracks; len(got) != 2 || got[0] != one || got[1] != two {
		t.Errorf("tracks = %v, want %s and %s", got, one, two)
	}
}

// TestApplyDeleteLeavesARipWhoseTracksChanged: a rip that plays a track the plan did not
// name by the time the plan is applied is left alone, since deleting its file would archive
// a track no one chose.
func TestApplyDeleteLeavesARipWhoseTracksChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	rip := writeCueRip(t, filepath.Join(root, "rip"), 7, "One", "Two")
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	one, two := itemPIDByTitle(t, ctx, lib, "One"), itemPIDByTitle(t, ctx, lib, "Two")
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{one, two}, model.DeleteTrash)
	if err != nil || len(plan.Actions) != 1 {
		t.Fatalf("plan = %+v (err %v), want the rip's file", plan, err)
	}
	writeFile(t, filepath.Join(root, "rip", "album.cue"), []byte("PERFORMER \"Rip Artist\"\nTITLE \"Rip Album\"\nFILE \"album.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:10:00\n"+
		"  TRACK 03 AUDIO\n    TITLE \"Three\"\n    INDEX 01 00:15:00\n"))
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	itemPIDByTitle(t, ctx, lib, "Three")
	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil || rep.Trashed != 0 || rep.Skipped != 1 {
		t.Fatalf("apply = %+v (err %v), want the rip left alone", rep, err)
	}
	if !fileExists(rip) {
		t.Error("the rip was deleted")
	}
}

// TestImportTakesTheCatalogsAuthorSpelling: a book imported into a library that already
// spells its author otherwise lands under that spelling, as organize would file it, and so
// does a part joining a book the catalog holds under the author's other spelling.
func TestImportTakesTheCatalogsAuthorSpelling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "Frank Herbert", "Dune", "Dune.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Dune", Artist: "Frank Herbert", Album: "Dune", Genre: "Audiobook", Audio: testaudio.AudioWithSeed(1)}))
	// Scanned after Dune, so the catalog keeps Dune's spelling of the author.
	writeFile(t, filepath.Join(root, "zz", "Dune Messiah", "Dune Messiah - 01.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Dune Messiah", Artist: "FRANK HERBERT", Album: "Dune Messiah", Track: 1, TrackTotal: 2, Genre: "Audiobook", Audio: testaudio.AudioWithSeed(3)}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	writeFile(t, filepath.Join(staging, "children.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Children of Dune", Artist: "FRANK HERBERT", Album: "Children of Dune", Genre: "Audiobook", Audio: testaudio.AudioWithSeed(2)}))
	writeFile(t, filepath.Join(staging, "messiah2.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Dune Messiah", Artist: "FRANK HERBERT", Album: "Dune Messiah", Track: 2, TrackTotal: 2, Genre: "Audiobook", Audio: testaudio.AudioWithSeed(4)}))
	plan := importAll(t, ctx, lib, staging, 2)
	want := map[string]string{
		"children.mp3": filepath.Join(root, "Frank Herbert", "Children of Dune", "Children of Dune.mp3"),
		"messiah2.mp3": filepath.Join(root, "Frank Herbert", "Dune Messiah", "Dune Messiah - 02.mp3"),
	}
	for _, a := range plan.Actions {
		if w := want[filepath.Base(a.Src)]; a.Dst != w {
			t.Errorf("%s lands at %s, want %s", filepath.Base(a.Src), a.Dst, w)
		}
	}
}

// TestImportAcquiredTakesTheCatalogsSpellings: an acquired track lands under the spellings
// the catalog keeps for its artist and album, as a folder import's tracks do.
func TestImportAcquiredTakesTheCatalogsSpellings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, acq := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "Amir Obe", "Night", "01 - One.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Amir Obe", AlbumArtist: "Amir Obe", Album: "Night", Track: 1, Audio: testaudio.AudioWithSeed(1)}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	scanLib(t, ctx, lib)
	file := filepath.Join(acq, "two.mp3")
	writeFile(t, file, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Amir Obè", AlbumArtist: "Amir Obè", Album: "NIGHT!", Track: 2, Audio: testaudio.AudioWithSeed(2)}))
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: file}, model.KindTrack, waxbin.AcquiredMeta{SourceType: model.SourceManual})
	if err != nil || res.Plan == nil || len(res.Plan.Actions) != 1 {
		t.Fatalf("ImportAcquired = %+v (err %v), want one action", res, err)
	}
	if want := filepath.Join(root, "Amir Obe", "Night", "02 - Two.mp3"); res.Plan.Actions[0].Dst != want {
		t.Errorf("acquired track lands at %s, want %s", res.Plan.Actions[0].Dst, want)
	}
}
