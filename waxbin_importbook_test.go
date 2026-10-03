package waxbin_test

import (
	"context"
	"fmt"
	"path/filepath"
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
	lib, err := waxbin.Open(ctx, waxbin.Options{
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
	lib, err := waxbin.Open(ctx, waxbin.Options{
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
	lib, err := waxbin.Open(ctx, waxbin.Options{
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
	lib, err := waxbin.Open(ctx, waxbin.Options{
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
	importAll(t, ctx, lib, staging, 2)
	oneBook(t, ctx, lib, 3)
	if moves := organizeMoves(t, ctx, lib); len(moves) != 0 {
		t.Errorf("organize after the second plan would move %v", moves)
	}
}
