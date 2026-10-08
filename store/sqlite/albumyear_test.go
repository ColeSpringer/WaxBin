package sqlite

import (
	"context"
	"strconv"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// albumYearOf reads the year column of the album titled title, "" when it is NULL.
func albumYearOf(t *testing.T, st *Store, title string) string {
	t.Helper()
	return scalarStr(t, st, "SELECT COALESCE(CAST(year AS TEXT),'') FROM album WHERE title = ?", title)
}

// yearTrack is one track of a test album: its number, the year it is tagged with, and
// the folder it sits in.
func yearTrack(n, year int, folder, album string) trackSpec {
	s := strconv.Itoa(n)
	return trackSpec{
		path: folder + "/" + s + ".flac", essence: album + "-e" + s, content: album + "-c" + s,
		title: album + " " + s, artist: "Anderson .Paak", albumArt: "Anderson .Paak", album: album, year: year,
	}
}

// putMalibu scans twelve tracks of one release, eleven tagged 2015 and track 7 tagged 2016,
// the odd one first so the album row is created with the minority year.
func putMalibu(t *testing.T, st *Store, libID int64) map[int]*model.ScanItemResult {
	t.Helper()
	out := map[int]*model.ScanItemResult{}
	for _, n := range []int{7, 1, 2, 3, 4, 5, 6, 8, 9, 10, 11, 12} {
		year := 2015
		if n == 7 {
			year = 2016
		}
		out[n] = putTrack(t, st, libID, yearTrack(n, year, "/lib/AP/Malibu", "Malibu"))
	}
	return out
}

// TestAlbumYearIsTheMostCommonMemberYear: an album's year is the year most of its members
// carry, whichever member was read first, and a retag that leaves the most common year
// standing writes nothing.
func TestAlbumYearIsTheMostCommonMemberYear(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putMalibu(t, st, lib.ID)
	if got := albumYearOf(t, st, "Malibu"); got != "2015" {
		t.Fatalf("album year = %q, want 2015, the year eleven of twelve members carry", got)
	}

	seq, _ := st.LatestChangeSeq(ctx)
	odd := yearTrack(7, 2015, "/lib/AP/Malibu", "Malibu")
	odd.content += "-retag"
	putTrack(t, st, lib.ID, odd)
	if got := albumYearOf(t, st, "Malibu"); got != "2015" {
		t.Errorf("album year after retagging the odd track = %q, want 2015", got)
	}
	if n := changeCount(t, st, seq, "album", model.OpUpdate); n != 0 {
		t.Errorf("album updates = %d, want none for a year that did not move", n)
	}
	assertVerifyClean(t, st)
}

// TestAlbumYearFollowsTrashedMembers: a trashed member keeps its track row, archived, but
// is no part of the album's year, so trashing the eleven 2015 tracks leaves the album the
// year of the one that remains, and says so in the change log.
func TestAlbumYearFollowsTrashedMembers(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	tracks := putMalibu(t, st, lib.ID)
	if got := albumYearOf(t, st, "Malibu"); got != "2015" {
		t.Fatalf("album year before the trash = %q, want 2015", got)
	}
	seq, _ := st.LatestChangeSeq(ctx)
	for n, res := range tracks {
		if n == 7 {
			continue
		}
		path := []byte("/trash/" + strconv.Itoa(n) + ".flac")
		if _, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: res.FilePID, TrashPath: path, TrashDisplay: string(path)}); err != nil {
			t.Fatalf("trash %d: %v", n, err)
		}
	}
	if got := albumYearOf(t, st, "Malibu"); got != "2016" {
		t.Errorf("album year = %q, want 2016, the year of the one member left", got)
	}
	if n := changeCount(t, st, seq, "album", model.OpUpdate); n < 1 {
		t.Errorf("album updates = %d, want the year change logged", n)
	}
	assertVerifyClean(t, st)
}

// TestAlbumYearTiesGoToTheEarliest: six members at 2009 and six at 1999 give the album
// 1999, the earliest of the most common years, however the members were read.
func TestAlbumYearTiesGoToTheEarliest(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for n := 1; n <= 12; n++ {
		year := 2009
		if n > 6 {
			year = 1999
		}
		putTrack(t, st, lib.ID, yearTrack(n, year, "/lib/AP/Tied", "Tied"))
	}
	if got := albumYearOf(t, st, "Tied"); got != "1999" {
		t.Errorf("album year = %q, want 1999", got)
	}
	assertVerifyClean(t, st)
}

// TestAlbumYearFromEnrichmentStays: a provider's year lands on the members that can take
// it and the album follows them, through a member edit, a rescan of a file that still
// says nothing, and the whole-catalog repair, in one album delta. A member whose year is
// locked empty takes nothing, and an album whose members all lock it stays without a year:
// the locks are the user's say over the album's year too.
func TestAlbumYearFromEnrichmentStays(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	fill := func(title, year string) {
		t.Helper()
		if err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{
			AlbumID: int64(scalarInt(t, st, "SELECT id FROM album WHERE title = ?", title)),
			PID:     model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", title)),
			Matched: true, Provider: "test", Fields: map[string]string{"year": year},
		}); err != nil {
			t.Fatalf("album fields: %v", err)
		}
	}
	var pids []model.PID
	for n := 1; n <= 2; n++ {
		pids = append(pids, putTrack(t, st, lib.ID, yearTrack(n, 0, "/lib/AP/Venice", "Venice")).ItemPID)
	}
	if err := st.LockField(ctx, pids[0], "year"); err != nil {
		t.Fatalf("lock year: %v", err)
	}
	seq, _ := st.LatestChangeSeq(ctx)
	fill("Venice", "2014")
	if got := albumYearOf(t, st, "Venice"); got != "2014" {
		t.Fatalf("album year after the fill = %q, want 2014", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE year = 2014"); n != 1 {
		t.Fatalf("members carrying the year = %d, want the unlocked one", n)
	}
	if n := changeCount(t, st, seq, "album", model.OpUpdate); n != 1 {
		t.Errorf("album updates for the fill = %d, want 1", n)
	}

	if err := st.EditItemField(ctx, pids[0], "genre", "Soul", model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	again := yearTrack(2, 0, "/lib/AP/Venice", "Venice")
	again.content += "-retag"
	putTrack(t, st, lib.ID, again)
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := albumYearOf(t, st, "Venice"); got != "2014" {
		t.Errorf("album year = %q, want the provider's 2014 kept", got)
	}

	for n := 1; n <= 2; n++ {
		res := putTrack(t, st, lib.ID, yearTrack(n, 0, "/lib/AP/Locked", "Locked"))
		if err := st.LockField(ctx, res.ItemPID, "year"); err != nil {
			t.Fatalf("lock year: %v", err)
		}
	}
	fill("Locked", "1999")
	if got := albumYearOf(t, st, "Locked"); got != "" {
		t.Errorf("album year = %q, want none: every member locks its year empty", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_curation WHERE entity_type = 'album' AND field = 'year'"); n != 0 {
		t.Errorf("album year curation rows = %d, want none", n)
	}
	assertVerifyClean(t, st)
}

// TestVerifyReportsAlbumYearDrift: an album year that is not its members' most common
// year is drift, which db verify reports and the rollup repair puts right.
func TestVerifyReportsAlbumYearDrift(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putMalibu(t, st, lib.ID)
	if _, err := st.wdb().ExecContext(ctx, "UPDATE album SET year = 1900"); err != nil {
		t.Fatalf("set the year by hand: %v", err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.AlbumYearDrift != 1 || rep.Consistent() {
		t.Fatalf("drift = %d (consistent %t), want 1 and inconsistent", rep.AlbumYearDrift, rep.Consistent())
	}
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := albumYearOf(t, st, "Malibu"); got != "2015" {
		t.Errorf("album year after the repair = %q, want 2015", got)
	}
	assertVerifyClean(t, st)
}

// TestAlbumYearFollowsAMerge: a merged album's year is recounted over the members it took
// in.
func TestAlbumYearFollowsAMerge(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for n := 1; n <= 2; n++ {
		putTrack(t, st, lib.ID, yearTrack(n, 2001, "/lib/AP/Hits A", "Hits"))
	}
	for n := 3; n <= 5; n++ {
		putTrack(t, st, lib.ID, yearTrack(n, 1999, "/lib/AP/Hits B", "Hits"))
	}
	survivor := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE year = 2001"))
	loser := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE year = 1999"))
	if _, err := st.MergeEntity(ctx, model.MergeAlbum, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := albumYearOf(t, st, "Hits"); got != "1999" {
		t.Errorf("merged album year = %q, want 1999, the year three of its five members carry", got)
	}
	assertVerifyClean(t, st)
}

// TestAlbumYearFollowsADetach: a member detached from a MusicBrainz-keyed album leaves its
// year behind with it.
func TestAlbumYearFollowsADetach(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	const rel = "b1000000-0000-4000-8000-0000000000ab"
	var pids []model.PID
	for i, year := range []int{1999, 1999, 2001} {
		s := yearTrack(i+1, year, "/lib/AP/Release", "Release")
		s.mbRelease = rel
		pids = append(pids, putTrack(t, st, lib.ID, s).ItemPID)
	}
	if got := albumYearOf(t, st, "Release"); got != "1999" {
		t.Fatalf("album year = %q, want 1999", got)
	}
	if _, err := st.DetachItemFromMBIDAlbum(ctx, pids[0]); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got := scalarStr(t, st, "SELECT COALESCE(CAST(year AS TEXT),'') FROM album WHERE match_key = ?", "mbid:"+rel); got != "2001" {
		t.Errorf("album year after the detach = %q, want 2001: the two left disagree, so the later", got)
	}
	assertVerifyClean(t, st)
}

// TestAlbumYearFollowsAReKind: a member that becomes a book leaves its album, and the
// album's year is recounted without it.
func TestAlbumYearFollowsAReKind(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	var first *model.ScanItemResult
	for i, year := range []int{2015, 2015, 2016} {
		res := putTrack(t, st, lib.ID, yearTrack(i+1, year, "/lib/AP/Kind", "Kind"))
		if first == nil {
			first = res
		}
	}
	if got := albumYearOf(t, st, "Kind"); got != "2015" {
		t.Fatalf("album year = %q, want 2015", got)
	}
	s := yearTrack(1, 2015, "/lib/AP/Kind", "Kind")
	putBook(t, st, lib.ID, bookSpec{path: s.path, essence: s.essence, content: s.content,
		title: "A Book", author: "Anderson .Paak", year: 2015})
	if k := scalarStr(t, st, "SELECT kind FROM playable_item WHERE pid = ?", string(first.ItemPID)); k != "book" {
		t.Fatalf("re-kinded item kind = %q, want book", k)
	}
	if got := albumYearOf(t, st, "Kind"); got != "2016" {
		t.Errorf("album year = %q, want 2016: the two members left disagree, so the later", got)
	}
	assertVerifyClean(t, st)
}

// TestAlbumFieldsYearSkipsTrashedMembers: a trashed member is no part of the album's year,
// so its own year neither refuses a provider's nor takes it, and a trashed member with
// none is not filled.
func TestAlbumFieldsYearSkipsTrashedMembers(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	var res []*model.ScanItemResult
	for n, year := range []int{0, 0, 1990, 0} {
		res = append(res, putTrack(t, st, lib.ID, yearTrack(n+1, year, "/lib/AP/Oxnard", "Oxnard")))
	}
	for _, r := range res[2:] {
		path := []byte("/trash/" + string(r.FilePID) + ".flac")
		if _, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: r.FilePID, TrashPath: path, TrashDisplay: string(path)}); err != nil {
			t.Fatalf("trash: %v", err)
		}
	}
	if got := albumYearOf(t, st, "Oxnard"); got != "" {
		t.Fatalf("album year = %q, want none once its only dated member is trashed", got)
	}
	albumID := int64(scalarInt(t, st, "SELECT id FROM album"))
	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album"))
	if err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{
		AlbumID: albumID, PID: albumPID, Matched: true, Provider: "test",
		Fields: map[string]string{"year": "2018"},
	}); err != nil {
		t.Fatalf("album fields: %v", err)
	}
	memberYear := func(r *model.ScanItemResult) string {
		t.Helper()
		return scalarStr(t, st, `SELECT COALESCE(CAST(t.year AS TEXT),'') FROM track t
			JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(r.ItemPID))
	}
	for i, want := range []string{"2018", "2018", "1990", ""} {
		if got := memberYear(res[i]); got != want {
			t.Errorf("member %d year = %q, want %q", i+1, got, want)
		}
	}
	if got := albumYearOf(t, st, "Oxnard"); got != "2018" {
		t.Errorf("album year = %q, want the provider's 2018", got)
	}
	assertVerifyClean(t, st)
}

// TestAlbumYearClearsWithItsMembers: an album's year is its members', so it goes when no
// member carries one any more, whether the dated members were trashed or their years
// cleared, and a hand-set year on such an album is drift.
func TestAlbumYearClearsWithItsMembers(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	trashed := putTrack(t, st, lib.ID, yearTrack(1, 2001, "/lib/AP/Yes Lawd", "Yes Lawd"))
	putTrack(t, st, lib.ID, yearTrack(2, 0, "/lib/AP/Yes Lawd", "Yes Lawd"))
	if got := albumYearOf(t, st, "Yes Lawd"); got != "2001" {
		t.Fatalf("album year = %q, want 2001", got)
	}
	path := []byte("/trash/1.flac")
	if _, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: trashed.FilePID, TrashPath: path, TrashDisplay: string(path)}); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if got := albumYearOf(t, st, "Yes Lawd"); got != "" {
		t.Errorf("album year after trashing its only dated member = %q, want none", got)
	}

	kept := putTrack(t, st, lib.ID, yearTrack(1, 1999, "/lib/AP/Venice", "Venice"))
	if err := st.EditItemField(ctx, kept.ItemPID, "year", "", model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("clear the year: %v", err)
	}
	if got := albumYearOf(t, st, "Venice"); got != "" {
		t.Errorf("album year after its member's year was cleared = %q, want none", got)
	}

	if _, err := st.wdb().ExecContext(ctx, "UPDATE album SET year = 1900 WHERE title = 'Venice'"); err != nil {
		t.Fatal(err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AlbumYearDrift != 1 {
		t.Errorf("drift = %d, want the hand-set year on an album with no year reported", rep.AlbumYearDrift)
	}
}

// TestAlbumYearRuleMatchesTheModel: the store's maintained year and model.AlbumYear, which
// the import planner renders with, read one rule, so an album lands in the folder the
// catalog then names it by. Each case is an album in a folder of its own.
func TestAlbumYearRuleMatchesTheModel(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	cases := map[string][]int{
		"Stray":       {2015, 2015, 2015, 2016},
		"Split":       {2009, 2009, 1999, 1999},
		"Pair":        {2016, 2015},
		"Comp":        {1971, 1976, 1983, 1990, 1999},
		"Coincidence": {1971, 1975, 1975, 1983, 1990, 1999},
		"Undated":     {0, 0},
	}
	for album, years := range cases {
		for i, y := range years {
			putTrack(t, st, lib.ID, yearTrack(i+1, y, "/lib/AP/"+album, album))
		}
	}
	for album, years := range cases {
		want := ""
		if y := model.AlbumYear(years); y != 0 {
			want = strconv.Itoa(y)
		}
		if got := albumYearOf(t, st, album); got != want {
			t.Errorf("album %s (%v) year = %q, want %q as model.AlbumYear reads it", album, years, got, want)
		}
	}
	assertVerifyClean(t, st)
}

// TestCompilationOfOriginalYearsIsNoInconsistency: members that each carry their own year,
// with no year most of them share, are a compilation tagged with original years, which is
// deliberate; the album shows the newest of them and the year check stays quiet. A year
// most members share with a stray beside it still reports.
func TestCompilationOfOriginalYearsIsNoInconsistency(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for i, y := range []int{1971, 1975, 1975, 1983, 1990, 1999} {
		putTrack(t, st, lib.ID, yearTrack(i+1, y, "/lib/AP/Greatest", "Greatest"))
	}
	for i, y := range []int{2015, 2015, 2015, 2016} {
		putTrack(t, st, lib.ID, yearTrack(i+1, y, "/lib/AP/Malibu", "Malibu"))
	}
	if got := albumYearOf(t, st, "Greatest"); got != "1999" {
		t.Errorf("compilation year = %q, want its newest member's 1999", got)
	}
	issues, err := st.InconsistentAlbums(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Title != "Malibu" {
		t.Errorf("issues = %+v, want only Malibu's stray year", issues)
	}
}

// TestAlbumWalksSkipTrashedAlbums: an album whose members are all trashed has nothing to
// ask a provider about, for its fields or its art, until a member comes back, and its
// counts agree; a run scoped to it still reaches it.
func TestAlbumWalksSkipTrashedAlbums(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, album := range []string{"Gone", "Here"} {
		s := yearTrack(1, 0, "/lib/AP/"+album, album)
		s.barcode = "0075992739429" // the art walk asks by identifier
		putTrack(t, st, lib.ID, s)
	}
	gone := &model.ScanItemResult{FilePID: model.PID(scalarStr(t, st,
		"SELECT pid FROM file WHERE path = ?", []byte("/lib/AP/Gone/1.flac")))}
	path := []byte("/trash/gone.flac")
	if _, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: gone.FilePID, TrashPath: path, TrashDisplay: string(path)}); err != nil {
		t.Fatalf("trash: %v", err)
	}
	names := func(ts []model.EnrichTarget, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range ts {
			out = append(out, x.Name)
		}
		return out
	}
	var run model.EnrichQueueOptions
	slots := model.ArtSlots{Front: true, Aux: true}
	if got := names(st.AlbumsNeedingFields(ctx, run, 0, 100, nil)); len(got) != 1 || got[0] != "Here" {
		t.Errorf("fields queue = %v, want only Here", got)
	}
	if got := names(st.AlbumsNeedingArt(ctx, run, 0, 100, slots, nil)); len(got) != 1 || got[0] != "Here" {
		t.Errorf("art queue = %v, want only Here", got)
	}
	n, err := st.CountEntitiesNeedingEnrichment(ctx, run, model.EnrichCountOptions{
		Phases: []model.EnrichPhase{model.EnrichPhaseAlbumFields, model.EnrichPhaseAlbumArt}, AlbumArt: slots}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("count = %d, want Here once per walk", n)
	}
	goneID := int64(scalarInt(t, st, "SELECT id FROM album WHERE title = 'Gone'"))
	if got := names(st.AlbumsNeedingFields(ctx, run, 0, 100, []int64{goneID})); len(got) != 1 || got[0] != "Gone" {
		t.Errorf("scoped fields queue = %v, want the album it names", got)
	}
}
