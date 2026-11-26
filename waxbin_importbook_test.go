package waxbin_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// stageBookPart writes one staged part of a book. album may be empty, and narrated adds
// a NARRATOR credit, the tag that makes a file a book on its own.
func stageBookPart(t *testing.T, path, album string, track int, narrated bool, seed byte) {
	t.Helper()
	spec := testaudio.MP3Spec{Artist: "Frank Herbert", Album: album, Track: track, Audio: testaudio.AudioWithSeed(seed)}
	if track > 0 {
		spec.TrackTotal = 3
	}
	if narrated {
		spec.TXXX = []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Scott Brick"}}
	}
	writeFile(t, path, testaudio.BuildMP3FromSpec(spec))
}

// importAll plans and applies an import of staging, failing unless every file imports.
func importAll(t *testing.T, ctx context.Context, lib *waxbin.Library, staging string, want int) *inbox.Plan {
	t.Helper()
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	if plan.Importable() != want {
		t.Fatalf("importable = %d, want %d: %+v", plan.Importable(), want, plan.Actions)
	}
	if rep, err := lib.ApplyImport(ctx, plan); err != nil || rep.Imported != want {
		t.Fatalf("ApplyImport: rep=%+v err=%v", rep, err)
	}
	return plan
}

// oneBook returns the catalog's only item, failing unless it is a book of parts files
// with no kind lock.
func oneBook(t *testing.T, ctx context.Context, lib *waxbin.Library, parts int) *model.ItemView {
	t.Helper()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Kind != model.KindBook {
		var got []string
		for _, it := range items {
			got = append(got, string(it.Kind)+" "+it.Title+" @ "+it.DisplayPath)
		}
		t.Fatalf("items = %v (err %v), want one book", got, err)
	}
	files, err := lib.ItemFiles(ctx, items[0].PID)
	if err != nil || len(files) != parts {
		t.Fatalf("book parts = %d (err %v), want %d", len(files), err, parts)
	}
	if row := kindRow(t, ctx, lib, items[0].PID); row != nil {
		t.Errorf("kind row = %+v, want none: the folder rule made the parts a book", row)
	}
	return items[0]
}

// TestStagedBookFolderImportsAsOneBook: a staged book folder where only part 2 carries the
// narrator imports as one book of three parts under the audiobook root, its siblings
// joining the book their album names, whether the parts sit in a folder of the source or
// the source is the book's folder.
func TestStagedBookFolderImportsAsOneBook(t *testing.T) {
	t.Parallel()
	for _, nested := range []bool{true, false} {
		ctx := context.Background()
		musicRoot, bookRoot, staging := t.TempDir(), t.TempDir(), t.TempDir()
		lib := openMediaTyped(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), musicRoot, bookRoot, t.TempDir())
		dir := staging
		if nested {
			dir = filepath.Join(staging, "Dune")
		}
		for i := 1; i <= 3; i++ {
			stageBookPart(t, filepath.Join(dir, "0"+string(rune('0'+i))+".mp3"), "Dune", i, i == 2, byte(i))
		}
		plan := importAll(t, ctx, lib, staging, 3)
		for _, a := range plan.Actions {
			if a.Kind != model.KindBook || a.KindForced || !strings.HasPrefix(a.Dst, bookRoot) {
				t.Errorf("%s planned as %s (forced %v) to %s, want an unforced book under the audiobook root", a.Src, a.Kind, a.KindForced, a.Dst)
			}
		}
		book := oneBook(t, ctx, lib, 3)
		if book.Title != "Dune" || book.Narrator != "Scott Brick" {
			t.Errorf("book = %q narrated by %q, want Dune by Scott Brick", book.Title, book.Narrator)
		}
	}
}

// TestStagedAlbumlessPartsJoinTheirBook: numbered parts with no album join the one book
// their folder holds, in a mixed library where they would otherwise stay tracks, and land
// in its folder.
func TestStagedAlbumlessPartsJoinTheirBook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	stageBookPart(t, filepath.Join(staging, "Dune", "01.mp3"), "", 0, false, 1)
	stageBookPart(t, filepath.Join(staging, "Dune", "02.mp3"), "Dune", 2, true, 2)
	stageBookPart(t, filepath.Join(staging, "Dune", "03.mp3"), "", 0, false, 3)
	importAll(t, ctx, lib, staging, 3)
	book := oneBook(t, ctx, lib, 3)
	files, _ := lib.ItemFiles(ctx, book.PID)
	for _, f := range files {
		if filepath.Dir(f.DisplayPath) != filepath.Dir(book.DisplayPath) {
			t.Errorf("part %s outside the book's folder %s", f.DisplayPath, filepath.Dir(book.DisplayPath))
		}
	}
}

// TestStagedAlbumlessBookIsNamedForItsFolder: numbered parts of a book whose tags name no
// album are one book named for their staging folder, as a scan names them.
func TestStagedAlbumlessBookIsNamedForItsFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	for i := 1; i <= 2; i++ {
		stageBookPart(t, filepath.Join(staging, "Dune", "0"+string(rune('0'+i))+".mp3"), "", 0, true, byte(i))
	}
	importAll(t, ctx, lib, staging, 2)
	if book := oneBook(t, ctx, lib, 2); book.Title != "Dune" {
		t.Errorf("book title = %q, want Dune from its folder", book.Title)
	}
}

// TestStagedTracksJoinOnlyAStrongBookTheyName: a track joins a book in its folder only
// when its album names the book and the book is one by a tag of its own (a narrator, not
// only a genre).
func TestStagedTracksJoinOnlyAStrongBookTheyName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	stageBookPart(t, filepath.Join(staging, "Mixed", "01.mp3"), "Dune", 1, true, 1)
	stageBookPart(t, filepath.Join(staging, "Mixed", "02.mp3"), "Children of Dune", 1, false, 2)
	writeFile(t, filepath.Join(staging, "Genre", "01.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Artist: "Author",
		Album: "Tome", Track: 1, TrackTotal: 2, Genre: "Audiobook", Audio: testaudio.AudioWithSeed(3)}))
	writeFile(t, filepath.Join(staging, "Genre", "02.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Artist: "Author",
		Album: "Tome", Track: 2, TrackTotal: 2, Audio: testaudio.AudioWithSeed(4)}))
	plan := importAll(t, ctx, lib, staging, 4)
	kinds := map[string]model.Kind{}
	for _, a := range plan.Actions {
		rel, _ := filepath.Rel(staging, a.Src)
		kinds[filepath.ToSlash(rel)] = a.Kind
	}
	for src, want := range map[string]model.Kind{
		"Mixed/01.mp3": model.KindBook, "Mixed/02.mp3": model.KindTrack, "Genre/01.mp3": model.KindBook, "Genre/02.mp3": model.KindTrack,
	} {
		if kinds[src] != want {
			t.Errorf("%s planned as %s, want %s", src, kinds[src], want)
		}
	}
}

// TestStagedTracksStayTracksWhereTheScanKeepsThem: a track imported into a lone library
// declared music, or one a cue sheet beside it makes a rip, joins no book, as a scan of
// its destination would leave it.
func TestStagedTracksStayTracksWhereTheScanKeepsThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	music, staging := t.TempDir(), t.TempDir()
	lib, err := waxbin.OpenSeeded(t, ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: music, Mode: model.ModeManaged, Media: model.MediaMusic, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	stageBookPart(t, filepath.Join(staging, "Dune", "01.mp3"), "Dune", 1, false, 1)
	stageBookPart(t, filepath.Join(staging, "Dune", "02.mp3"), "Dune", 2, true, 2)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	for _, a := range plan.Actions {
		if want := map[string]model.Kind{"01.mp3": model.KindTrack, "02.mp3": model.KindBook}[filepath.Base(a.Src)]; a.Kind != want {
			t.Errorf("music library: %s planned as %s, want %s", filepath.Base(a.Src), a.Kind, want)
		}
	}

	mixed := openManaged(t, ctx, filepath.Join(t.TempDir(), "mixed.db"), t.TempDir())
	staging = t.TempDir()
	stageBookPart(t, filepath.Join(staging, "Dune", "01.mp3"), "Dune", 1, false, 3)
	stageBookPart(t, filepath.Join(staging, "Dune", "02.mp3"), "Dune", 2, true, 4)
	writeFile(t, filepath.Join(staging, "Dune", "01.cue"), []byte("FILE \"01.mp3\" MP3\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"))
	plan, err = mixed.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	for _, a := range plan.Actions {
		if want := map[string]model.Kind{"01.mp3": model.KindTrack, "02.mp3": model.KindBook}[filepath.Base(a.Src)]; a.Kind != want {
			t.Errorf("beside a cue sheet: %s planned as %s, want %s", filepath.Base(a.Src), a.Kind, want)
		}
	}
}

// stageJoinedParts stages a book folder whose part 1 is a narrated book and whose parts
// 2 and 3 carry the tags of other (none for C; the album, a track, and the narrator
// credited as the artist for D).
func stageJoinedParts(t *testing.T, staging string, other testaudio.MP3Spec) {
	t.Helper()
	stageBookPart(t, filepath.Join(staging, "Dune", "01.mp3"), "Dune", 1, true, 1)
	for i := 2; i <= 3; i++ {
		spec := other
		spec.Audio = testaudio.AudioWithSeed(byte(i))
		if spec.Track != 0 {
			spec.Track = i
		}
		writeFile(t, filepath.Join(staging, "Dune", "0"+string(rune('0'+i))+".mp3"), testaudio.BuildMP3FromSpec(spec))
	}
}

// TestJoinedPartsLandWithTheirBook: parts the folder rule joins to a staged book land in
// its folder after it, whatever author their own tags give, so the book they join is the
// one their destination catalogs them into: one book of three parts in a mixed library,
// for parts with no tags and for parts whose album names the book under another artist.
// Bound for a library declared audiobook, untagged parts join it the same way, while parts
// whose own album and artist key another book are cataloged as that book there.
func TestJoinedPartsLandWithTheirBook(t *testing.T) {
	t.Parallel()
	untagged := testaudio.MP3Spec{}
	misattributed := testaudio.MP3Spec{Album: "Dune", Artist: "Scott Brick", Track: 2, TrackTotal: 3}
	for name, other := range map[string]testaudio.MP3Spec{"untagged": untagged, "another artist": misattributed} {
		t.Run(name+" into a mixed library", func(t *testing.T) {
			ctx := context.Background()
			staging := t.TempDir()
			lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
			stageJoinedParts(t, staging, other)
			importAll(t, ctx, lib, staging, 3)
			if book := oneBook(t, ctx, lib, 3); book.Artist != "Frank Herbert" {
				t.Errorf("book by %q, want Frank Herbert's", book.Artist)
			}
		})
	}
	t.Run("untagged into an audiobook library", func(t *testing.T) {
		ctx := context.Background()
		music, books, staging := t.TempDir(), t.TempDir(), t.TempDir()
		lib := openMediaTyped(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), music, books, t.TempDir())
		stageJoinedParts(t, staging, untagged)
		importAll(t, ctx, lib, staging, 3)
		if book := oneBook(t, ctx, lib, 3); !strings.HasPrefix(book.DisplayPath, books) {
			t.Errorf("book at %s, want it under the audiobook root", book.DisplayPath)
		}
	})
	t.Run("another artist into an audiobook library", func(t *testing.T) {
		ctx := context.Background()
		music, books, staging := t.TempDir(), t.TempDir(), t.TempDir()
		lib := openMediaTyped(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), music, books, t.TempDir())
		stageJoinedParts(t, staging, misattributed)
		importAll(t, ctx, lib, staging, 3)
		items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
		if err != nil {
			t.Fatal(err)
		}
		parts := map[string]int{}
		for _, it := range items {
			files, _ := lib.ItemFiles(ctx, it.PID)
			if it.Kind != model.KindBook || !strings.HasPrefix(it.DisplayPath, books) {
				t.Errorf("%s %q at %s, want a book under the audiobook root", it.Kind, it.Title, it.DisplayPath)
			}
			parts[it.Artist] = len(files)
		}
		if parts["Frank Herbert"] != 1 || parts["Scott Brick"] != 2 {
			t.Errorf("books by author = %v, want Frank Herbert's of one part and Scott Brick's of two", parts)
		}
		if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
			t.Errorf("organize after the import would move %v", moves)
		}
	})
}

// TestStagedTrackJoinsABookOfSeveralParts: a book two staged parts make is strong enough
// to take in a track whose album names it, as a scan of its folder joins one.
func TestStagedTrackJoinsABookOfSeveralParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	for i := 1; i <= 3; i++ {
		spec := testaudio.MP3Spec{Artist: "Author", Album: "Tome", Track: i, TrackTotal: 3, Audio: testaudio.AudioWithSeed(byte(i))}
		if i < 3 {
			spec.Genre = "Audiobook"
		}
		writeFile(t, filepath.Join(staging, "Tome", "0"+string(rune('0'+i))+".mp3"), testaudio.BuildMP3FromSpec(spec))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	importAll(t, ctx, lib, staging, 3)
	oneBook(t, ctx, lib, 3)
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the import would move %v", moves)
	}
}

// TestAcquiredDiscPartsKeepTheirDiscs: a part imported alone from a disc folder takes the
// folder's disc, so the first part of each disc lands apart, and one with no album keeps
// its disc though the folder above names no book.
func TestAcquiredDiscPartsKeepTheirDiscs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, acq := t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	importOne := func(src string) *inbox.Action {
		t.Helper()
		res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: src}, model.KindBook, waxbin.AcquiredMeta{})
		if err != nil {
			t.Fatalf("ImportAcquired %s: %v", src, err)
		}
		if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 1 {
			t.Fatalf("ApplyImport %s: %+v, %v (actions %+v)", src, rep, err, res.Plan.Actions)
		}
		return &res.Plan.Actions[0]
	}
	for disc := 1; disc <= 2; disc++ {
		src := filepath.Join(acq, "Disc Book", "CD"+string(rune('0'+disc)), "01.mp3")
		writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Artist: "Author", Album: "Disc Book", Track: 1,
			TrackTotal: 2, Audio: testaudio.AudioWithSeed(byte(disc))}))
		if a := importOne(src); filepath.Base(a.RelDst) != "Disc Book - "+string(rune('0'+disc))+"-01.mp3" {
			t.Errorf("disc %d part planned to %s, want Disc Book - %d-01.mp3", disc, a.RelDst, disc)
		}
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the discs would move %v", moves)
	}
	// Disc tags and no totals: the first disc's part is the book's own name until more
	// arrive, and the second disc's is numbered, so the two land apart.
	for disc := 1; disc <= 2; disc++ {
		src := filepath.Join(acq, "Tagged", "d"+string(rune('0'+disc))+".mp3")
		writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Artist: "Author", Album: "Tagged Book", Disc: disc,
			Track: 1, Audio: testaudio.AudioWithSeed(byte(20 + disc))}))
		importOne(src)
	}
	src := filepath.Join(acq, "Dune", "CD2", "03.mp3")
	stageBookPart(t, src, "", 0, true, 9)
	if a := importOne(src); strings.Contains(a.RelDst, "Dune") || !strings.HasSuffix(a.RelDst, " - 2-03.mp3") {
		t.Errorf("album-less disc part planned to %s, want disc 2 part 3 named for no folder", a.RelDst)
	}
}

// TestStagedEncodingOfAPartTakesItsNumber: another encoding of a staged part is that
// part's alternate once cataloged, so it takes the part's number rather than one of its
// own, and organize finds every part where the import put it.
func TestStagedEncodingOfAPartTakesItsNumber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	books, staging := t.TempDir(), t.TempDir()
	const rate = 22050
	one := testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 21)
	two := testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 22)
	for _, f := range []struct {
		rel, format string
		samples     []float32
		track       string
	}{{"Dune/01.flac", "flac", one, "1"}, {"Dune/02.flac", "flac", two, "2"}, {"Dune/01.mp3", "mp3", one, "1"}} {
		path := filepath.Join(staging, f.rel)
		writeFile(t, path, testaudio.EncodeAs(t, f.format, "", rate, f.samples))
		if _, err := meta.NewWriter().Apply(ctx, path, []meta.TagEdit{
			{Key: "ALBUM", Values: []string{"Dune"}}, {Key: "ARTIST", Values: []string{"Frank Herbert"}},
			{Key: "TRACKNUMBER", Values: []string{f.track}},
		}); err != nil {
			t.Fatalf("tag %s: %v", f.rel, err)
		}
	}
	lib, err := waxbin.OpenSeeded(t, ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: books, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	plan := importAll(t, ctx, lib, staging, 3)
	got := map[string]string{}
	for _, a := range plan.Actions {
		got[filepath.Base(a.Src)] = filepath.Base(a.RelDst)
	}
	for src, want := range map[string]string{"01.flac": "Dune - 01.flac", "02.flac": "Dune - 02.flac", "01.mp3": "Dune - 01.mp3"} {
		if got[src] != want {
			t.Errorf("%s planned to %q, want %q", src, got[src], want)
		}
	}
	book := oneBook(t, ctx, lib, 3)
	files, _ := lib.ItemFiles(ctx, book.PID)
	if files[2].Role != "alternate" || filepath.Base(files[2].DisplayPath) != "Dune - 01.mp3" {
		t.Errorf("book files = %+v, want the MP3 an alternate of part 1", files)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the import would move %v", moves)
	}
}

// TestStagedEncodingOfACatalogedPartTakesItsNumber: a staged encoding of a part the
// catalog already holds is that part's other file, so it is named by the part's number
// beside it rather than numbered as one more part.
func TestStagedEncodingOfACatalogedPartTakesItsNumber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	books := t.TempDir()
	const rate = 22050
	samples := [][]float32{testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 41), testaudio.RichSignal(rate, 2, testaudio.MusicalPartials, 42)}
	stageSet := func(format string) string {
		staging := t.TempDir()
		for i, part := range samples {
			path := filepath.Join(staging, "Dune", fmt.Sprintf("%02d.%s", i+1, format))
			writeFile(t, path, testaudio.EncodeAs(t, format, "", rate, part))
			if _, err := meta.NewWriter().Apply(ctx, path, []meta.TagEdit{
				{Key: "ALBUM", Values: []string{"Dune"}}, {Key: "ARTIST", Values: []string{"Frank Herbert"}},
				{Key: "TRACKNUMBER", Values: []string{strconv.Itoa(i + 1)}},
			}); err != nil {
				t.Fatalf("tag %s: %v", path, err)
			}
		}
		return staging
	}
	lib, err := waxbin.OpenSeeded(t, ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: books, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	importAll(t, ctx, lib, stageSet("mp3"), 2)
	plan := importAll(t, ctx, lib, stageSet("flac"), 2)
	for _, a := range plan.Actions {
		if want := strings.TrimSuffix(filepath.Base(a.Src), ".flac"); filepath.Base(a.RelDst) != "Dune - "+want+".flac" {
			t.Errorf("%s planned to %s, want Dune - %s.flac", filepath.Base(a.Src), a.RelDst, want)
		}
	}
	oneBook(t, ctx, lib, 4)
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the import would move %v", moves)
	}
}

// TestStagedCopyOfACatalogedPartIsNamedBesideIt: an import allowed to bring in audio the
// catalog holds names a copy of a cataloged part beside it, by the part's number, past
// the copies already there.
func TestStagedCopyOfACatalogedPartIsNamedBesideIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	stage := func() string {
		staging := t.TempDir()
		for i := 1; i <= 2; i++ {
			stageBookPart(t, filepath.Join(staging, "Dune", fmt.Sprintf("%02d.mp3", i)), "Dune", i, true, byte(i))
		}
		return staging
	}
	importAll(t, ctx, lib, stage(), 2)
	for n := 2; n <= 3; n++ {
		plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: stage(), DupPolicy: model.DupAllow})
		if err != nil {
			t.Fatalf("PlanImport: %v", err)
		}
		for _, a := range plan.Actions {
			if want := fmt.Sprintf("Dune - %s (%d).mp3", strings.TrimSuffix(filepath.Base(a.Src), ".mp3"), n); filepath.Base(a.RelDst) != want {
				t.Errorf("%s planned to %s, want %s", filepath.Base(a.Src), a.RelDst, want)
			}
		}
		if rep, err := lib.ApplyImport(ctx, plan); err != nil || rep.Imported != 2 {
			t.Fatalf("ApplyImport: %+v, %v", rep, err)
		}
	}
	oneBook(t, ctx, lib, 6)
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the import would move %v", moves)
	}
}

// TestConfiguredInboxNamesNoBook: a configured inbox folder holds staged imports rather
// than one book, so numbered parts with no album straight in it are not named for it,
// while a book folder inside it still names its parts' book.
func TestConfiguredInboxNamesNoBook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, inboxDir := t.TempDir(), t.TempDir()
	lib, err := waxbin.OpenSeeded(t, ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Inbox:  []string{inboxDir},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	stageBookPart(t, filepath.Join(inboxDir, "01.mp3"), "", 0, true, 1)
	stageBookPart(t, filepath.Join(inboxDir, "Dune", "01.mp3"), "", 0, true, 2)
	stageBookPart(t, filepath.Join(inboxDir, "Dune", "02.mp3"), "", 0, true, 3)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inboxDir})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	for _, a := range plan.Actions {
		inDune := filepath.Base(filepath.Dir(a.Src)) == "Dune"
		if named := strings.Contains(a.RelDst, filepath.Base(inboxDir)); named || (inDune && a.Album != "Dune") {
			t.Errorf("%s planned to %s (book %q), want only the Dune parts named, for Dune", a.Src, a.RelDst, a.Album)
		}
	}
}

// TestImportedAlbumlessBookKeepsItsTitle: a book whose tags name no album, titled for the
// folder it was staged in, keeps that title through a forced rescan in the managed library,
// where its folder and file name are the ones WaxBin gave it.
func TestImportedAlbumlessBookKeepsItsTitle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	stageBookPart(t, filepath.Join(staging, "Dune", "02.mp3"), "", 2, true, 1)
	importAll(t, ctx, lib, staging, 1)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if book := oneBook(t, ctx, lib, 1); book.Title != "Dune" {
		t.Errorf("book title after a forced rescan = %q, want Dune", book.Title)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize would move %v", moves)
	}
}

// TestStagedTrackJoinsBetweenWeakParts: a track whose album names a book two genre-tagged
// staged parts make lands after both of them, numbered between them, so its destination
// already holds a book of several parts to join.
func TestStagedTrackJoinsBetweenWeakParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging := t.TempDir()
	for i := 1; i <= 3; i++ {
		spec := testaudio.MP3Spec{Artist: "Author", Album: "Tome", Track: i, TrackTotal: 3, Audio: testaudio.AudioWithSeed(byte(i))}
		if i != 2 {
			spec.Genre = "Audiobook"
		}
		writeFile(t, filepath.Join(staging, "Tome", "0"+string(rune('0'+i))+".mp3"), testaudio.BuildMP3FromSpec(spec))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	importAll(t, ctx, lib, staging, 3)
	oneBook(t, ctx, lib, 3)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	oneBook(t, ctx, lib, 3)
}

// TestImportOfTheWorkingFolder: an import handed "." reads it as the folder it names, so a
// book folder imported from inside is named for it.
func TestImportOfTheWorkingFolder(t *testing.T) {
	ctx := context.Background()
	staging := t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	for i := 1; i <= 2; i++ {
		stageBookPart(t, filepath.Join(staging, "Dune", "0"+string(rune('0'+i))+".mp3"), "", 0, true, byte(i))
	}
	t.Chdir(filepath.Join(staging, "Dune"))
	importAll(t, ctx, lib, ".", 2)
	if book := oneBook(t, ctx, lib, 2); book.Title != "Dune" {
		t.Errorf("book title = %q, want Dune", book.Title)
	}
}

// TestAcquiredUnnumberedPartsImportOneAtATime: parts with no number anywhere, imported one
// at a time, land beside the parts their book already holds, after them, rather than on
// the name the first one took.
func TestAcquiredUnnumberedPartsImportOneAtATime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	acq := t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	for i, title := range []string{"Intro", "Body"} {
		src := filepath.Join(acq, title+".mp3")
		writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: title, Artist: "Author", Album: "Tome",
			TXXX: narrated, Audio: testaudio.AudioWithSeed(byte(i + 1))}))
		res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: src}, model.KindBook, waxbin.AcquiredMeta{})
		if err != nil {
			t.Fatalf("ImportAcquired %s: %v", title, err)
		}
		if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 1 {
			t.Fatalf("ApplyImport %s: %+v, %v (actions %+v)", title, rep, err, res.Plan.Actions)
		}
	}
	book := oneBook(t, ctx, lib, 2)
	files, _ := lib.ItemFiles(ctx, book.PID)
	if filepath.Base(files[0].DisplayPath) != "Tome.mp3" || filepath.Base(files[1].DisplayPath) != "Tome - 02.mp3" {
		t.Errorf("parts in reading order = %s, %s; want the intro then the body", files[0].DisplayPath, files[1].DisplayPath)
	}
}

// TestAcquiredFileNamesNoBookForItsFolder: a file imported alone is named for no book by
// the folder a host staged it in.
func TestAcquiredFileNamesNoBookForItsFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	acq := t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	src := filepath.Join(acq, "uploads", "3f2a9c", "02.mp3")
	stageBookPart(t, src, "", 2, true, 1)
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: src}, model.KindBook, waxbin.AcquiredMeta{})
	if err != nil {
		t.Fatalf("ImportAcquired: %v", err)
	}
	if a := res.Plan.Actions[0]; a.Album != "" || strings.Contains(a.RelDst, "3f2a9c") {
		t.Errorf("planned %s for book %q, want nothing named for the upload folder", a.RelDst, a.Album)
	}
	if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 1 {
		t.Fatalf("ApplyImport: rep=%+v err=%v", rep, err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Kind != model.KindBook || strings.Contains(items[0].Title, "3f2a9c") {
		t.Errorf("items = %+v (err %v), want one book not titled for the upload folder", items, err)
	}
}

// TestBookHeldWhenItsPartBecomesADuplicate: a part whose audio reached the catalog since
// the plan holds the rest of its book in place, laid out as they were around it, for the
// import to be planned again around what the catalog now holds.
func TestBookHeldWhenItsPartBecomesADuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	staging, acq := t.TempDir(), t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	for i := 1; i <= 3; i++ {
		stageBookPart(t, filepath.Join(staging, "Dune", "0"+string(rune('0'+i))+".mp3"), "Dune", i, i == 1, byte(i))
	}
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	src := filepath.Join(acq, "01.mp3")
	stageBookPart(t, src, "Dune", 1, true, 1)
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: src}, model.KindBook, waxbin.AcquiredMeta{})
	if err != nil {
		t.Fatalf("ImportAcquired: %v", err)
	}
	if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 1 {
		t.Fatalf("ApplyImport of part 1: %+v, %v", rep, err)
	}
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil || rep.Imported != 0 || rep.Duplicates != 1 || rep.Quarantined != 2 {
		t.Fatalf("ApplyImport of the stale plan: %+v, %v; want part 1 a duplicate and the rest held", rep, err)
	}
	codes := map[string]inbox.HoldCode{}
	for _, h := range rep.Holds {
		codes[filepath.Base(h.Src)] = h.Code
	}
	if len(rep.Holds) != 3 || codes["01.mp3"] != inbox.HoldLandedSince || codes["02.mp3"] != inbox.HoldBook || codes["03.mp3"] != inbox.HoldBook {
		t.Errorf("holds = %+v, want part 1 landed since the plan and its book's other parts held with it", rep.Holds)
	}
	importAll(t, ctx, lib, staging, 2)
	oneBook(t, ctx, lib, 3)
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the second plan would move %v", moves)
	}
}

// TestImportReportsAPartJoiningAnExistingBook: a part imported after its book stands says
// it joined that book and names it, where the first part said it created the book.
func TestImportReportsAPartJoiningAnExistingBook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	apply := func(track int) inbox.FileOutcome {
		t.Helper()
		staging := t.TempDir()
		stageBookPart(t, filepath.Join(staging, "Dune", fmt.Sprintf("%02d.mp3", track)), "Dune", track, true, byte(track))
		plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
		if err != nil {
			t.Fatalf("PlanImport: %v", err)
		}
		rep, err := lib.ApplyImport(ctx, plan)
		if err != nil || len(rep.Files) != 1 {
			t.Fatalf("ApplyImport: %+v, %v", rep, err)
		}
		return rep.Files[0]
	}
	first := apply(1)
	book := oneBook(t, ctx, lib, 1)
	if first.Verdict != scan.VerdictCreated || first.ItemPID != book.PID {
		t.Errorf("part 1 = %+v, want it to have created the book %s", first, book.PID)
	}
	if second := apply(2); second.Verdict != scan.VerdictJoined || second.ItemPID != book.PID {
		t.Errorf("part 2 = %+v, want it to have joined the book %s", second, book.PID)
	}
	oneBook(t, ctx, lib, 2)
}

// TestAcquiredUnitPlansItsPartsTogether: a book's parts handed over as one unit are planned
// in one pass, numbered among each other as a folder import numbers them, and land as one
// book; one at a time, the first takes the book's bare name (the contrast in
// TestAcquiredUnnumberedPartsImportOneAtATime).
func TestAcquiredUnitPlansItsPartsTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	acq := t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	var files []waxbin.AcquiredFile
	for i, title := range []string{"Intro", "Body", "End"} {
		src := filepath.Join(acq, fmt.Sprintf("%c %s.mp3", 'a'+i, title))
		writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: title, Artist: "Author", Album: "Tome",
			TXXX: narrated, Audio: testaudio.AudioWithSeed(byte(i + 1))}))
		files = append(files, waxbin.AcquiredFile{Path: src})
	}
	res, err := lib.ImportAcquiredUnit(ctx, files, model.KindBook, waxbin.AcquiredMeta{})
	if err != nil {
		t.Fatalf("ImportAcquiredUnit: %v", err)
	}
	if len(res.Files) != 3 || len(res.Plan.Actions) != 3 || res.Plan.Importable() != 3 {
		t.Fatalf("unit = %+v (actions %+v), want three parts planned to import", res.Files, res.Plan.Actions)
	}
	for i, f := range res.Files {
		if f.Path != files[i].Path || f.Outcome != inbox.OutcomeImport {
			t.Errorf("file %d = %+v, want %s to import", i, f, files[i].Path)
		}
	}
	if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 3 {
		t.Fatalf("ApplyImport: %+v, %v", rep, err)
	}
	book := oneBook(t, ctx, lib, 3)
	parts, _ := lib.ItemFiles(ctx, book.PID)
	for i, want := range []string{"Tome - 01.mp3", "Tome - 02.mp3", "Tome - 03.mp3"} {
		if filepath.Base(parts[i].DisplayPath) != want {
			t.Errorf("part %d = %s, want %s", i+1, parts[i].DisplayPath, want)
		}
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the import would move %v", moves)
	}
}

// TestReuploadOfABooksGonePartRestoresItInPlace: a book part whose file went missing, its
// own audio staged again, is planned at the part's own name rather than as a copy beside
// it, lands there, and the report says it restored the book's file.
func TestReuploadOfABooksGonePartRestoresItInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	staging := t.TempDir()
	stageBookPart(t, filepath.Join(staging, "Dune", "01.mp3"), "Dune", 1, true, 1)
	stageBookPart(t, filepath.Join(staging, "Dune", "02.mp3"), "Dune", 2, true, 2)
	importAll(t, ctx, lib, staging, 2)
	book := oneBook(t, ctx, lib, 2)
	parts, err := lib.ItemFiles(ctx, book.PID)
	if err != nil {
		t.Fatal(err)
	}
	gone := parts[1]
	if err := os.Remove(gone.DisplayPath); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	again := t.TempDir()
	stageBookPart(t, filepath.Join(again, "Dune", "02.mp3"), "Dune", 2, true, 2)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: again})
	if err != nil {
		t.Fatal(err)
	}
	if a := plan.Actions[0]; a.Outcome != inbox.OutcomeImport || a.Dst != gone.DisplayPath || a.Alternate || a.Restores != book.PID {
		t.Fatalf("action = %+v, want the part restored at %s", a, gone.DisplayPath)
	}
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil || len(rep.Files) != 1 || rep.Files[0].Restores != book.PID {
		t.Fatalf("apply = %+v (err %v), want the report to name the book it restored", rep, err)
	}
	after, err := lib.ItemFiles(ctx, book.PID)
	if err != nil || len(after) != 2 || after[1].FilePID != gone.FilePID || after[1].Role != gone.Role {
		t.Errorf("parts = %+v (err %v), want part 2 back on its own row", after, err)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the restore would move %v", moves)
	}
}

// acquirePart is one file an acquired book hands over: its tags, and the place the host
// gives it.
type acquirePart struct {
	spec       testaudio.MP3Spec
	disc, part int
}

// titled is a part whose tags hold a title and nothing else, which on its own keys a book
// of that title.
func titled(title string, seed byte) testaudio.MP3Spec {
	return testaudio.MP3Spec{Title: title, Audio: testaudio.AudioWithSeed(seed)}
}

// acquireBook writes the parts to a folder of their own and imports them as one book under
// book, failing unless every one lands.
func acquireBook(t *testing.T, ctx context.Context, lib *waxbin.Library, book waxbin.AcquiredBook, parts ...acquirePart) *waxbin.AcquiredUnitResult {
	t.Helper()
	dir := t.TempDir()
	files := make([]waxbin.AcquiredFile, len(parts))
	for i, p := range parts {
		path := filepath.Join(dir, fmt.Sprintf("upload-%c.mp3", 'a'+i))
		writeFile(t, path, testaudio.BuildMP3FromSpec(p.spec))
		files[i] = waxbin.AcquiredFile{Path: path, Disc: p.disc, Part: p.part}
	}
	res, err := lib.ImportAcquiredUnit(ctx, files, model.KindBook, waxbin.AcquiredMeta{Book: &book})
	if err != nil {
		t.Fatalf("ImportAcquiredUnit: %v", err)
	}
	if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != len(parts) {
		t.Fatalf("ApplyImport = %+v (err %v), want %d imported; plan %+v", rep, err, len(parts), res.Plan.Actions)
	}
	return res
}

// chapterTitles lists a book's chapter titles in reading order, one per untitled-chapter
// part.
func chapterTitles(t *testing.T, ctx context.Context, lib *waxbin.Library, book model.PID) []string {
	t.Helper()
	d, err := lib.Book(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range d.Chapters {
		out = append(out, c.Title)
	}
	return out
}

// verifyClean fails unless the catalog's derived data is consistent and no assignment is
// stranded.
func verifyClean(t *testing.T, ctx context.Context, lib *waxbin.Library) {
	t.Helper()
	rep, err := lib.VerifyDerived(ctx)
	if err != nil || !rep.Consistent() || rep.StrandedAssignments != 0 {
		t.Errorf("db verify = %+v (err %v), want clean", rep, err)
	}
}

// acquiredBook returns the catalog's only item, failing unless it is a book of parts files.
// An acquired book whose parts carry no book signal holds the kind lock the import's forced
// kind sets, so unlike oneBook it does not look for one.
func acquiredBook(t *testing.T, ctx context.Context, lib *waxbin.Library, parts int) *model.ItemView {
	t.Helper()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Kind != model.KindBook {
		t.Fatalf("items = %+v (err %v), want one book", items, err)
	}
	if files, err := lib.ItemFiles(ctx, items[0].PID); err != nil || len(files) != parts {
		t.Fatalf("book files = %d (err %v), want %d", len(files), err, parts)
	}
	return items[0]
}

// dune is the book the assigned imports below name.
var dune = waxbin.AcquiredBook{Title: "Dune", Author: "Frank Herbert", Parts: 3}

// TestAnAcquiredBookFilesItsPartsAsOne: parts that key three books of their own, by their
// titles, by three other albums, or by three ASINs, acquired as one book land as that book in
// the order assigned, which takes none of their identifiers, and a forced rescan keeps it.
func TestAnAcquiredBookFilesItsPartsAsOne(t *testing.T) {
	t.Parallel()
	asin := func(spec testaudio.MP3Spec, id string) testaudio.MP3Spec {
		spec.TXXX = []testaudio.TXXXFrame{{Desc: "ASIN", Value: id}}
		return spec
	}
	album := func(spec testaudio.MP3Spec, name string) testaudio.MP3Spec {
		spec.Album, spec.Artist, spec.AlbumArtist = name, "Tagger "+name, "Tagger "+name
		return spec
	}
	for name, parts := range map[string][]testaudio.MP3Spec{
		"titles": {titled("Three", 3), titled("One", 1), titled("Two", 2)},
		"albums": {album(titled("Three", 3), "Gamma"), album(titled("One", 1), "Alpha"), album(titled("Two", 2), "Beta")},
		"asins":  {asin(titled("Three", 3), "B000000003"), asin(titled("One", 1), "B000000001"), asin(titled("Two", 2), "B000000002")},
	} {
		ctx := context.Background()
		lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
		res := acquireBook(t, ctx, lib, dune, acquirePart{spec: parts[0], part: 3}, acquirePart{spec: parts[1], part: 1},
			acquirePart{spec: parts[2], part: 2})
		for _, f := range res.Files {
			if !f.Assigned {
				t.Errorf("%s: %s not assigned", name, f.Path)
			}
		}
		check := func(when string) {
			t.Helper()
			book := acquiredBook(t, ctx, lib, 3)
			d, err := lib.Book(ctx, book.PID)
			if err != nil {
				t.Fatal(err)
			}
			if d.Item.Title != "Dune" || d.Item.Artist != "Frank Herbert" || d.ASIN != "" || !d.IdentityHeld {
				t.Errorf("%s %s: book %q by %q asin %q held %v, want Dune by Frank Herbert with no ASIN, held",
					name, when, d.Item.Title, d.Item.Artist, d.ASIN, d.IdentityHeld)
			}
			if got := strings.Join(chapterTitles(t, ctx, lib, book.PID), ","); got != "One,Two,Three" {
				t.Errorf("%s %s: parts read %s, want One,Two,Three", name, when, got)
			}
		}
		check("imported")
		if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
			t.Errorf("%s: organize after the import would move %v", name, moves)
		}
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
			t.Fatal(err)
		}
		check("after a forced scan")
		verifyClean(t, ctx, lib)
	}
}

// TestAnAcquiredPartThatSaysItAllGetsNoRow: a part whose own tags already give the assigned
// book, place and count of parts imports as it would have, with no assignment, beside parts
// that need one.
func TestAnAcquiredPartThatSaysItAllGetsNoRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	tagged := testaudio.MP3Spec{Title: "Two", Artist: "Frank Herbert", AlbumArtist: "Frank Herbert", Album: "Dune", Track: 2, TrackTotal: 3,
		TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Scott Brick"}}, Audio: testaudio.AudioWithSeed(2)}
	res := acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("One", 1), part: 1}, acquirePart{spec: tagged, part: 2},
		acquirePart{spec: titled("Three", 3), part: 3})
	if !res.Files[0].Assigned || res.Files[1].Assigned || !res.Files[2].Assigned {
		t.Errorf("assigned = %v %v %v, want the tagged part alone left to its tags", res.Files[0].Assigned, res.Files[1].Assigned, res.Files[2].Assigned)
	}
	book := acquiredBook(t, ctx, lib, 3)
	d, err := lib.Book(ctx, book.PID)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false, true} {
		if d.Files[i].Assigned != want {
			t.Errorf("part %d assigned %v, want %v", i+1, d.Files[i].Assigned, want)
		}
	}
	verifyClean(t, ctx, lib)
}

// TestAnAcquiredBookOutlivesEditsAndOrganize: an acquired book of untagged parts keeps its
// parts and order through a catalog-only title edit, an organize that renames its files, and
// a forced scan, and through an edit written back into the files and another forced scan.
func TestAnAcquiredBookOutlivesEditsAndOrganize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("One", 1), part: 1}, acquirePart{spec: titled("Two", 2), part: 2},
		acquirePart{spec: titled("Three", 3), part: 3})
	book := acquiredBook(t, ctx, lib, 3).PID
	after := func(when, title string) {
		t.Helper()
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
			t.Fatal(err)
		}
		if b := acquiredBook(t, ctx, lib, 3); b.PID != book || b.Title != title {
			t.Errorf("%s: book %s %q, want %s %q", when, b.PID, b.Title, book, title)
		}
		if got := strings.Join(chapterTitles(t, ctx, lib, book), ","); got != "One,Two,Three" {
			t.Errorf("%s: parts read %s, want One,Two,Three", when, got)
		}
	}
	if err := lib.EditFields(ctx, book, map[string]string{"title": "Dune Messiah"}, waxbin.EditOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{}); err != nil {
		t.Fatal(err)
	}
	after("a catalog-only edit and an organize", "Dune Messiah")
	if err := lib.EditFields(ctx, book, map[string]string{"title": "Children of Dune"}, waxbin.EditOptions{WriteBack: true}); err != nil {
		t.Fatal(err)
	}
	after("an edit written back", "Children of Dune")
	verifyClean(t, ctx, lib)
}

// TestAnAssignedPartsCopiesAndReencodes: a retagged copy of an assigned part joins its book
// as the part's other file, the part keeping its place; a scan that ignores locks keeps the
// assignment; and a part re-encoded where it stands stays the book's part at its place.
func TestAnAssignedPartsCopiesAndReencodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("One", 1), part: 1}, acquirePart{spec: titled("Two", 2), part: 2},
		acquirePart{spec: titled("Three", 3), part: 3})
	book := acquiredBook(t, ctx, lib, 3).PID
	parts, err := lib.ItemFiles(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "Elsewhere", "copy.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Retagged",
		Artist: "Someone", Album: "Not Dune", Audio: testaudio.AudioWithSeed(2)}))
	scanLib(t, ctx, lib)
	files, err := lib.ItemFiles(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	var alternates int
	for _, f := range files {
		if f.Role == "alternate" && f.Position == 2 {
			alternates++
		}
		if f.FilePID == parts[1].FilePID && (f.Role == "alternate" || f.Position != 2) {
			t.Errorf("part two = %+v, want it the part at 2", f)
		}
	}
	if alternates != 1 {
		t.Errorf("files = %+v, want the copy as part two's alternate", files)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, IgnoreLocks: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(chapterTitles(t, ctx, lib, book), ","); got != "One,Two,Three" {
		t.Errorf("after a scan ignoring locks the parts read %s, want One,Two,Three", got)
	}
	writeFile(t, parts[2].DisplayPath, testaudio.BuildMP3FromSpec(titled("Three Again", 33)))
	scanLib(t, ctx, lib)
	if got := strings.Join(chapterTitles(t, ctx, lib, book), ","); got != "One,Two,Three Again" {
		t.Errorf("after a re-encode in place the parts read %s, want One,Two,Three Again", got)
	}
	verifyClean(t, ctx, lib)
}

// TestAnAssignedPartReturnsFromTheTrashAndARootAddedBack: a part trashed and restored, and a
// book whose library is removed and added back, find their book and place again.
func TestAnAssignedPartReturnsFromTheTrashAndARootAddedBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	lib, err := waxbin.OpenSeeded(t, ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	managed, err := lib.AddRoot(ctx, config.Root{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"})
	if err != nil {
		t.Fatal(err)
	}
	acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("One", 1), part: 1}, acquirePart{spec: titled("Two", 2), part: 2},
		acquirePart{spec: titled("Three", 3), part: 3})
	book := acquiredBook(t, ctx, lib, 3).PID
	parts, err := lib.ItemFiles(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := lib.PlanDeleteFiles(ctx, []model.PID{parts[1].FilePID}, model.DeleteTrash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatal(err)
	}
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %+v (err %v)", entries, err)
	}
	if err := lib.RestoreTrash(ctx, entries[0].PID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(chapterTitles(t, ctx, lib, book), ","); got != "One,Two,Three" {
		t.Errorf("after a restore the parts read %s, want One,Two,Three", got)
	}
	if _, err := lib.RemoveRoot(ctx, managed.PID, waxbin.RemoveRootOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.AddRoot(ctx, config.Root{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}); err != nil {
		t.Fatal(err)
	}
	scanLib(t, ctx, lib)
	if b := acquiredBook(t, ctx, lib, 3); b.PID != book {
		t.Errorf("after the root came back the book is %s, want %s", b.PID, book)
	}
	if got := strings.Join(chapterTitles(t, ctx, lib, book), ","); got != "One,Two,Three" {
		t.Errorf("after the root came back the parts read %s, want One,Two,Three", got)
	}
	verifyClean(t, ctx, lib)
}

// TestALaterAcquisitionByPIDAddsAPart: a part acquired later for a book named by its pid
// joins it after a catalog-only title edit has left its key behind, joins a book keyed by
// its ASIN, and takes a place on a second disc.
func TestALaterAcquisitionByPIDAddsAPart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("One", 1), part: 1}, acquirePart{spec: titled("Two", 2), part: 2},
		acquirePart{spec: titled("Three", 3), part: 3})
	book := acquiredBook(t, ctx, lib, 3).PID
	if err := lib.EditFields(ctx, book, map[string]string{"title": "Dune Messiah"}, waxbin.EditOptions{}); err != nil {
		t.Fatal(err)
	}
	acquireBook(t, ctx, lib, waxbin.AcquiredBook{PID: book}, acquirePart{spec: titled("Four", 4), part: 4})
	acquireBook(t, ctx, lib, waxbin.AcquiredBook{PID: book}, acquirePart{spec: titled("Disc Two", 5), disc: 2, part: 1})
	if b := acquiredBook(t, ctx, lib, 5); b.PID != book || b.Title != "Dune Messiah" {
		t.Fatalf("book = %s %q, want %s Dune Messiah with five parts", b.PID, b.Title, book)
	}
	if got := strings.Join(chapterTitles(t, ctx, lib, book), ","); got != "One,Two,Three,Four,Disc Two" {
		t.Errorf("parts read %s, want the later parts after the first three", got)
	}

	other := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	staging := t.TempDir()
	for i := 1; i <= 2; i++ {
		writeFile(t, filepath.Join(staging, "Hyperion", fmt.Sprintf("0%d.mp3", i)), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: fmt.Sprintf("Part %d", i), Artist: "Dan Simmons", Album: "Hyperion", Track: i,
			TXXX:  []testaudio.TXXXFrame{{Desc: "ASIN", Value: "B00HYPERIO"}, {Desc: "NARRATOR", Value: "Reader"}},
			Audio: testaudio.AudioWithSeed(byte(40 + i))}))
	}
	importAll(t, ctx, other, staging, 2)
	hyperion := oneBook(t, ctx, other, 2).PID
	acquireBook(t, ctx, other, waxbin.AcquiredBook{PID: hyperion}, acquirePart{spec: titled("Part 3", 43), part: 3})
	if b := acquiredBook(t, ctx, other, 3); b.PID != hyperion {
		t.Errorf("the ASIN-keyed book is %s, want %s with the acquired part", b.PID, hyperion)
	}
	verifyClean(t, ctx, lib)
	verifyClean(t, ctx, other)
}

// TestAnAcquiredBookIsRefused: a book named on a track or an episode, one with neither pid
// nor title, a pid that names no book or a book of another library, a number without a book,
// two files at one place, a part past the book's parts and a number past a disc's places are
// refused before anything is planned.
func TestAnAcquiredBookIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inPlace := t.TempDir()
	writeFile(t, filepath.Join(inPlace, "Elsewhere", "01.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Elsewhere",
		Artist: "A", Album: "Elsewhere", TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "R"}}, Audio: testaudio.AudioWithSeed(90)}))
	writeFile(t, filepath.Join(inPlace, "song.mp3"), testaudio.BuildMP3WithAudio("Song", "Band", "Record", 1, testaudio.AudioWithSeed(91)))
	managed := t.TempDir()
	lib, err := waxbin.OpenSeeded(t, ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:    []config.Root{{Path: managed, Mode: model.ModeManaged, Profile: "waxbin-native"}, {Path: inPlace, Mode: model.ModeInPlace}},
		Podcasts: config.PodcastConfig{Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	scanLib(t, ctx, lib)
	elsewhere, song := itemPIDByTitle(t, ctx, lib, "Elsewhere"), itemPIDByTitle(t, ctx, lib, "Song")
	file := func() string {
		p := filepath.Join(t.TempDir(), "part.mp3")
		writeFile(t, p, testaudio.BuildMP3FromSpec(titled("Part", 92)))
		return p
	}
	for _, c := range []struct {
		name  string
		kind  model.Kind
		files []waxbin.AcquiredFile
		book  *waxbin.AcquiredBook
	}{
		{"a track", model.KindTrack, []waxbin.AcquiredFile{{Path: file()}}, &dune},
		{"an episode", model.KindEpisode, []waxbin.AcquiredFile{{Path: file()}}, &dune},
		{"no pid nor title", model.KindBook, []waxbin.AcquiredFile{{Path: file()}}, &waxbin.AcquiredBook{Author: "A"}},
		{"a track's pid", model.KindBook, []waxbin.AcquiredFile{{Path: file()}}, &waxbin.AcquiredBook{PID: song}},
		{"an unknown pid", model.KindBook, []waxbin.AcquiredFile{{Path: file()}}, &waxbin.AcquiredBook{PID: model.NewPID()}},
		{"another library's book", model.KindBook, []waxbin.AcquiredFile{{Path: file()}}, &waxbin.AcquiredBook{PID: elsewhere}},
		{"a number with no book", model.KindBook, []waxbin.AcquiredFile{{Path: file(), Part: 2}}, nil},
		{"two at one place", model.KindBook, []waxbin.AcquiredFile{{Path: file(), Part: 2}, {Path: file(), Part: 2}}, &dune},
		{"past the parts", model.KindBook, []waxbin.AcquiredFile{{Path: file(), Part: 4}}, &dune},
		{"past a disc", model.KindBook, []waxbin.AcquiredFile{{Path: file(), Part: model.DiscStride}}, &waxbin.AcquiredBook{Title: "T"}},
	} {
		meta := waxbin.AcquiredMeta{Book: c.book}
		var err error
		if len(c.files) == 1 {
			_, err = lib.ImportAcquired(ctx, c.files[0], c.kind, meta)
		} else {
			_, err = lib.ImportAcquiredUnit(ctx, c.files, c.kind, meta)
		}
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s: err = %v, want CodeInvalid", c.name, err)
		}
	}
}

// TestAWithdrawnAcquiredPartStaysABook: a part of an acquired book whose own tags name
// another album, its assignment withdrawn, leaves for the book those tags name and keeps the
// kind its import forced, rather than becoming the track its tags would make.
func TestAWithdrawnAcquiredPartStaysABook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	other := testaudio.MP3Spec{Title: "Two", Artist: "Someone", Album: "Other Title", Audio: testaudio.AudioWithSeed(2)}
	acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("One", 1), part: 1}, acquirePart{spec: other, part: 2})
	book := acquiredBook(t, ctx, lib, 2).PID
	parts, err := lib.ItemFiles(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := lib.AssignBookParts(ctx, []waxbin.BookPartChange{{FilePID: parts[1].FilePID}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := lib.Get(ctx, rep.Files[parts[1].FilePID])
	if err != nil || v.Kind != model.KindBook || v.PID == book || v.Title != "Other Title" {
		t.Errorf("withdrawn part's item = %+v (err %v), want a book of the title its tags name", v, err)
	}
	verifyClean(t, ctx, lib)
}

// TestAnAssignedBooksEditsStayOwedUntilWritten: a catalog-only title edit, and a cleared
// identifier the files still carry, stay owed to the files of an assigned book through a
// forced scan, since the files' own tags do not say them; a write-back pays them.
func TestAnAssignedBooksEditsStayOwedUntilWritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	tagged := titled("One", 1)
	tagged.TXXX = []testaudio.TXXXFrame{{Desc: "ASIN", Value: "B000000001"}}
	acquireBook(t, ctx, lib, dune, acquirePart{spec: tagged, part: 1})
	book := acquiredBook(t, ctx, lib, 1).PID
	if err := lib.EditFields(ctx, book, map[string]string{"title": "Dune Messiah", "asin": "B000000009"}, waxbin.EditOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := lib.EditFields(ctx, book, map[string]string{"asin": ""}, waxbin.EditOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := owedOn(t, ctx, lib, book); !slices.Equal(got, []string{"asin", "title"}) {
		t.Fatalf("owed after the edits = %v, want asin and title", got)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := owedOn(t, ctx, lib, book); !slices.Equal(got, []string{"asin", "title"}) {
		t.Errorf("owed after a forced scan = %v, want asin and title, which the file does not state", got)
	}
	if err := lib.EditFields(ctx, book, map[string]string{"title": "Dune Messiah"}, waxbin.EditOptions{WriteBack: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := owedOn(t, ctx, lib, book); slices.Contains(got, "title") {
		t.Errorf("owed after the title was written and read back = %v, want no title", got)
	}
}

// TestAnAssignmentThatRestoresAGoneFileTakesTheNamedBook: audio whose file left the disk,
// acquired again for a book named by title and author, comes back as that book, though it
// takes back the item the audio stood as: a one-part book of another title, or a track.
func TestAnAssignmentThatRestoresAGoneFileTakesTheNamedBook(t *testing.T) {
	t.Parallel()
	for _, kind := range []model.Kind{model.KindBook, model.KindTrack} {
		ctx := context.Background()
		lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
		first := filepath.Join(t.TempDir(), "first.mp3")
		writeFile(t, first, testaudio.BuildMP3FromSpec(titled("Wrong", 1)))
		res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: first}, kind, waxbin.AcquiredMeta{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lib.ApplyImport(ctx, res.Plan); err != nil {
			t.Fatal(err)
		}
		items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
		if err != nil || len(items) != 1 {
			t.Fatalf("%s: items = %+v (err %v)", kind, items, err)
		}
		if err := os.Remove(items[0].DisplayPath); err != nil {
			t.Fatal(err)
		}
		// A track restores no book, so the audio a gone track held is a duplicate unless
		// duplicates are let in.
		again := filepath.Join(t.TempDir(), "again.mp3")
		writeFile(t, again, testaudio.BuildMP3FromSpec(titled("Wrong", 1)))
		unit, err := lib.ImportAcquiredUnit(ctx, []waxbin.AcquiredFile{{Path: again, Part: 1}}, model.KindBook,
			waxbin.AcquiredMeta{Book: &dune, DupPolicy: model.DupAllow})
		if err != nil {
			t.Fatal(err)
		}
		if rep, err := lib.ApplyImport(ctx, unit.Plan); err != nil || rep.Imported != 1 {
			t.Fatalf("%s: apply = %+v (err %v)", kind, rep, err)
		}
		book := acquiredBook(t, ctx, lib, 1)
		if book.Title != "Dune" || book.Artist != "Frank Herbert" {
			t.Errorf("%s: book = %q by %q, want Dune by Frank Herbert", kind, book.Title, book.Artist)
		}
		verifyClean(t, ctx, lib)
	}
}

// TestApplyImportHoldsAPartWhoseBookIsGone: a plan naming a standing book whose book is no
// longer one when the plan is applied lands nothing.
func TestApplyImportHoldsAPartWhoseBookIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("One", 1), part: 1})
	book := acquiredBook(t, ctx, lib, 1).PID
	later := filepath.Join(t.TempDir(), "later.mp3")
	writeFile(t, later, testaudio.BuildMP3FromSpec(titled("Two", 2)))
	unit, err := lib.ImportAcquiredUnit(ctx, []waxbin.AcquiredFile{{Path: later, Part: 2}}, model.KindBook,
		waxbin.AcquiredMeta{Book: &waxbin.AcquiredBook{PID: book}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.SetItemKind(ctx, []model.PID{book}, model.KindTrack, waxbin.KindOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	rep, err := lib.ApplyImport(ctx, unit.Plan)
	if err != nil || rep.Imported != 0 || rep.Quarantined != 1 {
		t.Fatalf("apply = %+v (err %v), want the part held", rep, err)
	}
	if _, err := os.Stat(later); err != nil {
		t.Errorf("the held part left its upload: %v", err)
	}
}

// TestAnAcquiredBookByTitleTakesOneAuthor: a book named by its title alone takes one author
// for all its parts, the first a part states, so parts that disagree still make one book.
func TestAnAcquiredBookByTitleTakesOneAuthor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	credited := titled("One", 1)
	credited.Artist = "Frank Herbert"
	acquireBook(t, ctx, lib, waxbin.AcquiredBook{Title: "Dune"}, acquirePart{spec: credited, part: 1},
		acquirePart{spec: titled("Two", 2), part: 2}, acquirePart{spec: titled("Three", 3), part: 3})
	if b := acquiredBook(t, ctx, lib, 3); b.Title != "Dune" || b.Artist != "Frank Herbert" {
		t.Errorf("book = %q by %q, want Dune by Frank Herbert", b.Title, b.Artist)
	}
	verifyClean(t, ctx, lib)
}

// TestAnAcquiredBooksPartCountOutlivesARewrite: the parts an acquisition says its book has
// stay the book's when the primary is read again and states none, so a lone part keeps the
// number organize gave it.
func TestAnAcquiredBooksPartCountOutlivesARewrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("Two", 2), part: 2})
	book := acquiredBook(t, ctx, lib, 1)
	if book.PartTotal != 3 {
		t.Fatalf("part total = %d, want 3", book.PartTotal)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true, ReResolve: true}); err != nil {
		t.Fatal(err)
	}
	if b := acquiredBook(t, ctx, lib, 1); b.PartTotal != 3 {
		t.Errorf("part total after a re-resolving scan = %d, want 3", b.PartTotal)
	}
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize would move %v", moves)
	}
}

// TestAnAcquiredPartTakesNoPlaceAnotherPartHolds: a part acquired for a standing book at a
// place one of its parts holds is held with the reason, and the book's parts stay as they
// were.
func TestAnAcquiredPartTakesNoPlaceAnotherPartHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("One", 1), part: 1}, acquirePart{spec: titled("Two", 2), part: 2})
	book := acquiredBook(t, ctx, lib, 2).PID
	dir := t.TempDir()
	taken, free := filepath.Join(dir, "taken.mp3"), filepath.Join(dir, "free.mp3")
	writeFile(t, taken, testaudio.BuildMP3FromSpec(titled("Other Two", 3)))
	writeFile(t, free, testaudio.BuildMP3FromSpec(titled("Three", 4)))
	unit, err := lib.ImportAcquiredUnit(ctx, []waxbin.AcquiredFile{{Path: taken, Part: 2}, {Path: free, Part: 3}}, model.KindBook,
		waxbin.AcquiredMeta{Book: &waxbin.AcquiredBook{PID: book}})
	if err != nil {
		t.Fatal(err)
	}
	if f := unit.Files[0]; f.Outcome != inbox.OutcomeQuarantine || f.Code != inbox.HoldPlaceTaken {
		t.Errorf("planned = %+v, want the part held for the place its book's part 2 holds", f)
	}
	if f := unit.Files[1]; f.Outcome != inbox.OutcomeQuarantine {
		t.Errorf("planned = %+v, want the rest of the book held with it", f)
	}
	if _, err := lib.ApplyImport(ctx, unit.Plan); err != nil {
		t.Fatal(err)
	}
	acquiredBook(t, ctx, lib, 2)
}

// TestAnAcquiredPartsCountIsTheHosts: the parts an acquisition gives its book stand over the
// count a mis-tagged part's TRACKTOTAL states, on the import and when a retag reads the part
// again.
func TestAnAcquiredPartsCountIsTheHosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	other := testaudio.MP3Spec{Title: "Four", Artist: "Someone", Album: "Other Album", Track: 4, TrackTotal: 12, Audio: testaudio.AudioWithSeed(4)}
	acquireBook(t, ctx, lib, dune, acquirePart{spec: other, part: 2})
	b := acquiredBook(t, ctx, lib, 1)
	if b.PartTotal != 3 {
		t.Fatalf("part total = %d, want the 3 the host gave", b.PartTotal)
	}
	files, err := lib.ItemFiles(ctx, b.PID)
	if err != nil {
		t.Fatal(err)
	}
	other.Genre = "Retagged"
	writeFile(t, files[0].DisplayPath, testaudio.BuildMP3FromSpec(other))
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if b := acquiredBook(t, ctx, lib, 1); b.PartTotal != 3 {
		t.Errorf("part total after a retag = %d, want 3", b.PartTotal)
	}
}

// TestAnAcquiredBookWithNoCountTakesItsFiles: a book an acquisition makes with no count of
// parts takes the count its parts' tags state.
func TestAnAcquiredBookWithNoCountTakesItsFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	one := titled("One", 1)
	one.Track, one.TrackTotal = 1, 5
	acquireBook(t, ctx, lib, waxbin.AcquiredBook{Title: "Dune", Author: "Frank Herbert"}, acquirePart{spec: one, part: 1})
	if b := acquiredBook(t, ctx, lib, 1); b.PartTotal != 5 {
		t.Errorf("part total = %d, want the 5 its part states", b.PartTotal)
	}
}

// TestAHostsCountHoldsOverAgreeingTags: parts whose own tags key the book the host names
// and number the places it gives them still take the count of parts the host gives.
func TestAHostsCountHoldsOverAgreeingTags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	part := func(title string, n int) acquirePart {
		return acquirePart{part: n, spec: testaudio.MP3Spec{Title: title, Artist: "Frank Herbert", AlbumArtist: "Frank Herbert",
			Album: "Dune", Track: n, Audio: testaudio.AudioWithSeed(byte(n))}}
	}
	acquireBook(t, ctx, lib, dune, part("One", 1), part("Two", 2))
	if b := acquiredBook(t, ctx, lib, 2); b.PartTotal != 3 {
		t.Errorf("part total = %d, want the 3 the host gave", b.PartTotal)
	}
}

// TestAnAcquiredBookIsNamedOneWay: a book named by its pid and by a title is refused, since
// only one of them can say which book it is.
func TestAnAcquiredBookIsNamedOneWay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
	acquireBook(t, ctx, lib, dune, acquirePart{spec: titled("One", 1), part: 1})
	book := acquiredBook(t, ctx, lib, 1).PID
	later := filepath.Join(t.TempDir(), "later.mp3")
	writeFile(t, later, testaudio.BuildMP3FromSpec(titled("Two", 2)))
	_, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: later, Part: 2}, model.KindBook,
		waxbin.AcquiredMeta{Book: &waxbin.AcquiredBook{PID: book, Title: "Dune Messiah"}})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("a pid and a title = %v, want refused", err)
	}
}
