package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func TestSetItemCreditsMusicRoles(t *testing.T) {
	st, pid := editFixture(t) // one track: artist Alpha, composer "Writer"
	ctx := context.Background()

	// Set producers on the track.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"Prod One", "Prod Two"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set producers: %v", err)
	}
	// Set composers via the credit API; the denormalized track.composer follows.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Comp A", "Comp B"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set composers: %v", err)
	}

	credits, err := st.ItemCredits(ctx, pid)
	if err != nil {
		t.Fatalf("read credits: %v", err)
	}
	roles := map[model.ContributorRole]int{}
	for _, c := range credits {
		roles[c.Role]++
	}
	if roles[model.RoleProducer] != 2 || roles[model.RoleComposer] != 2 {
		t.Fatalf("credits = %+v", roles)
	}

	// Composer denormalization uses "; ".
	var composer string
	if err := st.read.QueryRowContext(ctx,
		"SELECT composer FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?", string(pid)).Scan(&composer); err != nil {
		t.Fatalf("read composer: %v", err)
	}
	if composer != "Comp A; Comp B" {
		t.Fatalf("track.composer = %q, want %q", composer, "Comp A; Comp B")
	}

	// Setting producers again rewrites ONLY that role, so composers survive. The first
	// set locked credit.producer, so this uses force.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"Prod Solo"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil {
		t.Fatalf("reset producers: %v", err)
	}
	credits, _ = st.ItemCredits(ctx, pid)
	roles = map[model.ContributorRole]int{}
	for _, c := range credits {
		roles[c.Role]++
	}
	if roles[model.RoleProducer] != 1 || roles[model.RoleComposer] != 2 {
		t.Fatalf("after rewrite producers = %+v, want producer 1 composer 2", roles)
	}

	// Provenance carries a locked credit.producer row.
	prov, _ := st.FieldProvenance(ctx, pid)
	found := false
	for _, p := range prov {
		if p.Field == "credit.producer" {
			found = true
			if !p.Locked || p.Source != model.SourceUser {
				t.Fatalf("credit.producer provenance = %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("no credit.producer provenance row")
	}

	// db verify stays clean (new contributor artists have zero rollup rows).
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", r, err)
	}
}

func TestSetItemCreditsRoleKindValidation(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	// A book role on a track is rejected.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleNarrator, []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("narrator on track = %v, want CodeInvalid", err)
	}
	// An unknown role is rejected.
	if _, err := st.SetItemCredits(ctx, pid, model.ContributorRole("bogus"), []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bogus role = %v, want CodeInvalid", err)
	}
}

func TestSetItemCreditsBookAuthorSyncsDenorm(t *testing.T) {
	st, bpid := bookEditFixture(t)
	ctx := context.Background()

	if _, err := st.SetItemCredits(ctx, bpid, model.RoleAuthor, []string{"New Author"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set author: %v", err)
	}
	// The denormalized book.author and its item view follow.
	v, err := st.ItemByPID(ctx, bpid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.Artist != "New Author" {
		t.Fatalf("book author view = %q, want New Author", v.Artist)
	}
	// A music role is rejected on a book.
	if _, err := st.SetItemCredits(ctx, bpid, model.RoleDJMixer, []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("djmixer on book = %v, want CodeInvalid", err)
	}
}

func TestSetItemCreditsDedupAndCount(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	// "Bob"/"bob" fold to one artist (match key), and "" / "!!!" resolve to nothing.
	stored, err := st.SetItemCredits(ctx, pid, model.RoleComposer,
		[]string{"Bob", "bob", "", "!!!"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if len(stored) != 1 || stored[0] != "Bob" {
		t.Fatalf("stored = %v, want [Bob]", stored)
	}
	// Exactly one contributor row, and the denorm is not doubled.
	credits, _ := st.ItemCredits(ctx, pid)
	n := 0
	for _, c := range credits {
		if c.Role == model.RoleComposer {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("composer credit rows = %d, want 1", n)
	}
	var composer string
	if err := st.read.QueryRowContext(ctx,
		"SELECT composer FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?", string(pid)).Scan(&composer); err != nil {
		t.Fatalf("read: %v", err)
	}
	if composer != "Bob" {
		t.Fatalf("track.composer = %q, want %q (no phantom dup)", composer, "Bob")
	}

	// An all-unresolvable set clears the role and reports 0 stored (not a false "set").
	stored, err = st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"!!!"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("stored = %v, want empty", stored)
	}
}

func TestSetFieldProvenanceRejectsNonScalar(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()
	// art/lyrics/chapters are lockable but NOT scalar-settable, so SetFieldProvenance
	// (the scalar provenance path) must reject them instead of writing a junk row.
	for _, f := range []string{"art", "lyrics", "chapters"} {
		if err := st.SetFieldProvenance(ctx, pid, f, model.Attribution{Source: model.SourceUser}, "x", false); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Fatalf("SetFieldProvenance(%q) = %v, want CodeInvalid", f, err)
		}
	}
	// A scalar field still works.
	if err := st.SetFieldProvenance(ctx, pid, "comment", model.Attribution{Source: model.SourceUser}, "hi", false); err != nil {
		t.Fatalf("scalar SetFieldProvenance: %v", err)
	}
}

// TestSetCreditsArtistRenamesSingleRefArtistInPlace: setting an item's single-reference
// performing credit to a new name runs the rename pre-pass as a batch of one, so the
// artist entity is renamed in place (pid, curation, star, alias) rather than split off
// into a fresh row, mirroring what a whole-set EditItemFields artist edit does.
func TestSetCreditsArtistRenamesSingleRefArtistInPlace(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/one/01.flac", essence: "e1", content: "c1",
		title: "S", artist: "Alpha", album: "One", year: 2001,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S'"))
	artistID := scalarInt(t, st, "SELECT id FROM artist")
	artPID := model.PID(scalarStr(t, st, "SELECT pid FROM artist"))

	if _, err := st.EditEntityFields(ctx, model.MergeArtist, artPID,
		map[string]string{"mbid": "88888888-8888-8888-8888-888888888888"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("seed curation: %v", err)
	}
	if _, err := st.SetEntityStar(ctx, "", model.MergeArtist, artPID, true, nil); err != nil {
		t.Fatalf("seed star: %v", err)
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	if _, err := st.SetItemCredits(ctx, pid, model.RoleArtist, []string{"Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set credits: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Fatalf("artist rows = %d, want 1 (renamed in place)", n)
	}
	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", artistID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Beta" || matchKey != identity.MatchKey("Beta") || gotPID != string(artPID) {
		t.Fatalf("artist = %q/%q/%s, want Beta with kept pid %s", name, matchKey, gotPID, artPID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM artist_alias WHERE artist_id=? AND name='Alpha' AND is_primary=0", artistID); n != 1 {
		t.Errorf("alias rows = %d, want the old spelling recorded", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_play_state WHERE entity_type='artist' AND entity_id=? AND starred_at IS NOT NULL", artistID); n != 1 {
		t.Errorf("star rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type='artist' AND entity_id=? AND field='mbid' AND locked=1", artistID); n != 1 {
		t.Errorf("curation rows = %d, want 1", n)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN playable_item pi ON pi.id=ic.item_id
		WHERE pi.pid=? AND ic.role='artist' AND ic.artist_id=?`, string(pid), artistID); n != 1 {
		t.Errorf("artist contributor rows on the kept entity = %d, want 1", n)
	}
	if id := scalarInt(t, st, `SELECT t.artist_id FROM track t
		JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(pid)); id != artistID {
		t.Errorf("track artist_id = %d, want kept entity %d", id, artistID)
	}
	if n := changeCount(t, st, seq0, "artist", model.OpUpdate); n != 1 {
		t.Errorf("artist updates = %d, want 1", n)
	}
	for _, op := range []model.ChangeOp{model.OpCreate, model.OpDelete} {
		if n := changeCount(t, st, seq0, "artist", op); n != 0 {
			t.Errorf("artist %s deltas = %d, want 0", op, n)
		}
	}
	assertVerifyClean(t, st)
}

// TestSetCreditsAuthorRenamesInPlace: setting a book's author to a new name runs the
// pre-pass as a single-entry batch too, renaming the author artist in place.
func TestSetCreditsAuthorRenamesInPlace(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	artistID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	artPID := entityPID(t, st, "artist", "Jane Author")

	if _, err := st.EditEntityFields(ctx, model.MergeArtist, artPID,
		map[string]string{"mbid": "99999999-9999-9999-9999-999999999999"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("seed curation: %v", err)
	}
	if _, err := st.SetEntityStar(ctx, "", model.MergeArtist, artPID, true, nil); err != nil {
		t.Fatalf("seed star: %v", err)
	}

	seq0, _ := st.LatestChangeSeq(ctx)
	if _, err := st.SetItemCredits(ctx, pid, model.RoleAuthor, []string{"Janet Author"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set author credits: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Jane Author'"); n != 0 {
		t.Fatalf("old author rows = %d, want 0 (renamed in place)", n)
	}
	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", artistID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Janet Author" || matchKey != identity.MatchKey("Janet Author") || gotPID != string(artPID) {
		t.Fatalf("artist = %q/%q/%s, want Janet Author with kept pid %s", name, matchKey, gotPID, artPID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM artist_alias WHERE artist_id=? AND name='Jane Author' AND is_primary=0", artistID); n != 1 {
		t.Errorf("alias rows = %d, want the old spelling recorded", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_play_state WHERE entity_type='artist' AND entity_id=? AND starred_at IS NOT NULL", artistID); n != 1 {
		t.Errorf("star rows = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type='artist' AND entity_id=? AND field='mbid' AND locked=1", artistID); n != 1 {
		t.Errorf("curation rows = %d, want 1", n)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN playable_item pi ON pi.id=ic.item_id
		WHERE pi.pid=? AND ic.role='author' AND ic.artist_id=?`, string(pid), artistID); n != 1 {
		t.Errorf("author contributor rows on the kept entity = %d, want 1", n)
	}
	if id := scalarInt(t, st, `SELECT b.author_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); int64(id) != artistID {
		t.Errorf("book author_id = %d, want kept entity %d", id, artistID)
	}
	if n := changeCount(t, st, seq0, "artist", model.OpUpdate); n != 1 {
		t.Errorf("artist updates = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestSetCreditsMultiRefArtistStillSplits pins the per-item-surface residue: another
// item outside this single-entry batch still references the old artist, so coverage
// fails and the credit split behaves as it always did.
func TestSetCreditsMultiRefArtistStillSplits(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/01.flac", essence: "e1", content: "c1",
		title: "S1", artist: "Alpha", album: "One", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/01.flac", essence: "e2", content: "c2",
		title: "S2", artist: "Alpha", album: "Two", year: 2002,
	})
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S1'"))
	alphaID := entityIDByCol(t, st, "artist", "name", "Alpha")
	alphaPID := entityPID(t, st, "artist", "Alpha")

	if _, err := st.SetItemCredits(ctx, pid1, model.RoleArtist, []string{"Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set credits: %v", err)
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 2 {
		t.Fatalf("artist rows = %d, want 2 (Alpha kept, Beta split off)", n)
	}
	if pid := entityPID(t, st, "artist", "Alpha"); pid != alphaPID {
		t.Fatalf("Alpha pid = %s, want kept %s", pid, alphaPID)
	}
	betaID := entityIDByCol(t, st, "artist", "name", "Beta")
	if id := scalarInt(t, st, `SELECT t.artist_id FROM track t
		JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(pid1)); int64(id) != betaID {
		t.Errorf("track1 artist_id = %d, want the split Beta %d", id, betaID)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM track t
		JOIN playable_item pi ON pi.id=t.item_id WHERE pi.title='S2' AND t.artist_id=?`, alphaID); n != 1 {
		t.Errorf("track2 still on Alpha = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestSetCreditsTwoAuthorsDoesNotRenameOntoJoinedName: a credit set naming two people
// is a split, not a rename, so the old author keeps its name, key, and pid and the two
// names get rows of their own. The pre-pass must never see the joined display, which no
// splitter can turn back into the caller's list.
func TestSetCreditsTwoAuthorsDoesNotRenameOntoJoinedName(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	janeID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	janePID := entityPID(t, st, "artist", "Jane Author")

	if _, err := st.SetItemCredits(ctx, pid, model.RoleAuthor, []string{"Neil Gaiman", "Terry Pratchett"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set authors: %v", err)
	}

	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", janeID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Jane Author" || matchKey != identity.MatchKey("Jane Author") || gotPID != string(janePID) {
		t.Fatalf("old author = %q/%q/%s, want it untouched", name, matchKey, gotPID)
	}
	for _, n := range []string{"Neil Gaiman", "Terry Pratchett"} {
		if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name=?", n); c != 1 {
			t.Errorf("rows named %q = %d, want 1", n, c)
		}
	}
	if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Neil Gaiman, Terry Pratchett'"); c != 0 {
		t.Errorf("rows named after the joined display = %d, want 0", c)
	}
	neilID := entityIDByCol(t, st, "artist", "name", "Neil Gaiman")
	if id := scalarInt(t, st, `SELECT b.author_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); int64(id) != neilID {
		t.Errorf("book author_id = %d, want the fresh first author %d", id, neilID)
	}
	assertVerifyClean(t, st)
}

// TestSetCreditsAmpersandNameStaysWhole: one credited name containing an ampersand is
// one artist, so the pre-pass renames the old row onto the whole string instead of the
// half a book-credit split would leave.
func TestSetCreditsAmpersandNameStaysWhole(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	janeID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	janePID := entityPID(t, st, "artist", "Jane Author")

	if _, err := st.SetItemCredits(ctx, pid, model.RoleAuthor, []string{"Gaiman & Pratchett"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set author: %v", err)
	}

	if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Gaiman'"); c != 0 {
		t.Errorf("rows named after a partial split = %d, want 0", c)
	}
	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", janeID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Gaiman & Pratchett" || matchKey != identity.MatchKey("Gaiman & Pratchett") ||
		gotPID != string(janePID) {
		t.Fatalf("artist = %q/%q/%s, want the whole name with the kept pid %s", name, matchKey, gotPID, janePID)
	}
	if id := scalarInt(t, st, `SELECT b.author_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); int64(id) != janeID {
		t.Errorf("book author_id = %d, want the renamed row %d", id, janeID)
	}
	assertVerifyClean(t, st)
}

// TestSetCreditsTwoArtistsDoesNotRenameOntoJoinedName is the track twin: a performing
// credit naming two artists splits, and the old entity is left where it is rather than
// renamed onto the comma-joined display.
func TestSetCreditsTwoArtistsDoesNotRenameOntoJoinedName(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1",
		title: "S", artist: "Alpha", album: "One", year: 2001,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S'"))
	alphaID := entityIDByCol(t, st, "artist", "name", "Alpha")
	alphaPID := entityPID(t, st, "artist", "Alpha")

	if _, err := st.SetItemCredits(ctx, pid, model.RoleArtist, []string{"Beta", "Gamma"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set credits: %v", err)
	}

	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", alphaID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Alpha" || matchKey != identity.MatchKey("Alpha") || gotPID != string(alphaPID) {
		t.Fatalf("old artist = %q/%q/%s, want it untouched", name, matchKey, gotPID)
	}
	if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Beta, Gamma'"); c != 0 {
		t.Errorf("rows named after the joined display = %d, want 0", c)
	}
	betaID := entityIDByCol(t, st, "artist", "name", "Beta")
	if id := scalarInt(t, st, `SELECT t.artist_id FROM track t
		JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(pid)); int64(id) != betaID {
		t.Errorf("track artist_id = %d, want the fresh Beta %d", id, betaID)
	}
	assertVerifyClean(t, st)
}

// TestSetCreditsRefusedRenameLeavesChainKeys: a credit set runs the artist stage alone.
// Nothing re-resolves the chain behind this call, so a refused artist rename must not
// leave the release group and album keyed on a name their columns no longer spell.
func TestSetCreditsRefusedRenameLeavesChainKeys(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// No album artist tag, so the release-group anchor falls back to the track credit
	// and an artist edit moves the chain keys with it.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1",
		title: "T1", artist: "Alpha", album: "One", year: 2001,
	})
	// A second album keeps Alpha referenced outside this one-item batch, so the rename
	// is refused and the credit splits as it always did.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/Two/01.flac", essence: "e2", content: "c2",
		title: "T2", artist: "Alpha", album: "Two", year: 2002,
	})
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='T1'"))
	alphaPID := entityPID(t, st, "artist", "Alpha")
	rgID := entityIDByCol(t, st, "release_group", "title", "One")
	albumID := entityIDByCol(t, st, "album", "title", "One")
	rgKey0 := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", rgID)
	albumKey0 := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID)

	if _, err := st.SetItemCredits(ctx, pid1, model.RoleArtist, []string{"Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set credits: %v", err)
	}

	if p := entityPID(t, st, "artist", "Alpha"); p != alphaPID {
		t.Fatalf("Alpha pid = %s, want kept %s", p, alphaPID)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM release_group WHERE id=?", rgID); k != rgKey0 {
		t.Errorf("release_group match_key = %q, want the unchanged %q", k, rgKey0)
	}
	if k := scalarStr(t, st, "SELECT match_key FROM album WHERE id=?", albumID); k != albumKey0 {
		t.Errorf("album match_key = %q, want the unchanged %q", k, albumKey0)
	}
	assertVerifyClean(t, st)
}

func TestLockCreditRole(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	// A credit role is lockable via the field lock path.
	if err := st.LockField(ctx, pid, "credit.producer"); err != nil {
		t.Fatalf("lock credit.producer: %v", err)
	}
	// Setting the locked role without force is refused.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("set locked credit = %v, want CodeLocked", err)
	}
	// Force overrides it.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil {
		t.Fatalf("forced set: %v", err)
	}
	// A book-only credit role cannot be locked on a track.
	if err := st.LockField(ctx, pid, "credit.narrator"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("lock credit.narrator on track = %v, want CodeInvalid", err)
	}
}
