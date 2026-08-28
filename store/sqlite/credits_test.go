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
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"Prod One", "Prod Two"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("set producers: %v", err)
	}
	// Set composers via the credit API; the denormalized track.composer follows.
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Comp A", "Comp B"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
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
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"Prod Solo"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true, false); err != nil {
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
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleNarrator, []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("narrator on track = %v, want CodeInvalid", err)
	}
	// An unknown role is rejected.
	if _, _, err := st.SetItemCredits(ctx, pid, model.ContributorRole("bogus"), []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bogus role = %v, want CodeInvalid", err)
	}
}

func TestSetItemCreditsBookAuthorSyncsDenorm(t *testing.T) {
	st, bpid := bookEditFixture(t)
	ctx := context.Background()

	if _, _, err := st.SetItemCredits(ctx, bpid, model.RoleAuthor, []string{"New Author"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
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
	if _, _, err := st.SetItemCredits(ctx, bpid, model.RoleDJMixer, []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("djmixer on book = %v, want CodeInvalid", err)
	}
}

func TestSetItemCreditsDedupAndCount(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	// "Bob"/"bob" fold to one artist (match key), and "" / "!!!" resolve to nothing.
	stored, _, err := st.SetItemCredits(ctx, pid, model.RoleComposer,
		[]string{"Bob", "bob", "", "!!!"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false)
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
	stored, _, err = st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"!!!"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true, false)
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
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleArtist, []string{"Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
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
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleAuthor, []string{"Janet Author"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
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

	if _, _, err := st.SetItemCredits(ctx, pid1, model.RoleArtist, []string{"Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
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

// TestSetCreditsTwoAuthorsRenamesOntoFirst: a credit set naming two people renames the
// old author onto the FIRST of them, so the pid, curation, and star carry and the old
// spelling survives as an alias, while the remaining names get rows of their own. The
// joined display is never an entity name: the pre-pass reads the caller's list, which no
// splitter could recover from it.
func TestSetCreditsTwoAuthorsRenamesOntoFirst(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	janeID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	janePID := entityPID(t, st, "artist", "Jane Author")

	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleAuthor, []string{"Neil Gaiman", "Terry Pratchett"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("set authors: %v", err)
	}

	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", janeID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Neil Gaiman" || matchKey != identity.MatchKey("Neil Gaiman") || gotPID != string(janePID) {
		t.Fatalf("old author = %q/%q/%s, want it renamed onto the first name with pid %s kept", name, matchKey, gotPID, janePID)
	}
	if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Jane Author'"); c != 0 {
		t.Errorf("rows still named Jane Author = %d, want 0", c)
	}
	if c := scalarInt(t, st,
		"SELECT COUNT(*) FROM artist_alias WHERE artist_id=? AND name='Jane Author' AND is_primary=0", janeID); c != 1 {
		t.Errorf("alias rows = %d, want the old spelling recorded", c)
	}
	for _, n := range []string{"Neil Gaiman", "Terry Pratchett"} {
		if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name=?", n); c != 1 {
			t.Errorf("rows named %q = %d, want 1", n, c)
		}
	}
	if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Neil Gaiman, Terry Pratchett'"); c != 0 {
		t.Errorf("rows named after the joined display = %d, want 0", c)
	}
	if id := scalarInt(t, st, `SELECT b.author_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); int64(id) != janeID {
		t.Errorf("book author_id = %d, want the renamed row %d", id, janeID)
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

	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleAuthor, []string{"Gaiman & Pratchett"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
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

// TestSetCreditsTwoArtistsRenamesOntoFirst is the track twin: a performing credit
// naming two artists renames the covered entity onto the first of them and splits the
// rest off, rather than leaving it behind or naming it after the comma-joined display.
func TestSetCreditsTwoArtistsRenamesOntoFirst(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Alpha/One/01.flac", essence: "e1", content: "c1",
		title: "S", artist: "Alpha", album: "One", year: 2001,
	})
	pid := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S'"))
	alphaID := entityIDByCol(t, st, "artist", "name", "Alpha")
	alphaPID := entityPID(t, st, "artist", "Alpha")

	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleArtist, []string{"Beta", "Gamma"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("set credits: %v", err)
	}

	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", alphaID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Beta" || matchKey != identity.MatchKey("Beta") || gotPID != string(alphaPID) {
		t.Fatalf("old artist = %q/%q/%s, want it renamed onto Beta with pid %s kept", name, matchKey, gotPID, alphaPID)
	}
	if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Alpha'"); c != 0 {
		t.Errorf("rows still named Alpha = %d, want 0", c)
	}
	if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Gamma'"); c != 1 {
		t.Errorf("rows named Gamma = %d, want 1", c)
	}
	if c := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name='Beta, Gamma'"); c != 0 {
		t.Errorf("rows named after the joined display = %d, want 0", c)
	}
	if id := scalarInt(t, st, `SELECT t.artist_id FROM track t
		JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(pid)); int64(id) != alphaID {
		t.Errorf("track artist_id = %d, want the renamed row %d", id, alphaID)
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

	if _, _, err := st.SetItemCredits(ctx, pid1, model.RoleArtist, []string{"Beta"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
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

// TestSetItemCreditsBatchCoveredRenameLandsInPlace closes what the per-item surface
// could not: two items credit one artist, the batch moves both at once, so the coverage
// checks see every reference move and the entity renames in place instead of splitting.
func TestSetItemCreditsBatchCoveredRenameLandsInPlace(t *testing.T) {
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
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S2'"))
	alphaID := entityIDByCol(t, st, "artist", "name", "Alpha")
	alphaPID := entityPID(t, st, "artist", "Alpha")

	res, err := st.SetItemCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: pid1, Role: model.RoleArtist, Names: []string{"Beta"}},
		{ItemPID: pid2, Role: model.RoleArtist, Names: []string{"Beta"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Edited) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("result = %+v, want two edited entries", res)
	}
	for _, e := range res.Edited {
		if len(e.Names) != 1 || e.Names[0] != "Beta" {
			t.Errorf("stored names for %s = %v, want [Beta]", e.ItemPID, e.Names)
		}
	}

	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 1 {
		t.Fatalf("artist rows = %d, want 1 (renamed in place, nothing split off)", n)
	}
	var name, matchKey, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, match_key, pid FROM artist WHERE id=?", alphaID).
		Scan(&name, &matchKey, &gotPID); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if name != "Beta" || matchKey != identity.MatchKey("Beta") || gotPID != string(alphaPID) {
		t.Fatalf("artist = %q/%q/%s, want Beta with kept pid %s", name, matchKey, gotPID, alphaPID)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM artist_alias WHERE artist_id=? AND name='Alpha' AND is_primary=0", alphaID); n != 1 {
		t.Errorf("alias rows = %d, want the old spelling recorded", n)
	}
	for _, pid := range []model.PID{pid1, pid2} {
		if id := scalarInt(t, st, `SELECT t.artist_id FROM track t
			JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?`, string(pid)); int64(id) != alphaID {
			t.Errorf("%s artist_id = %d, want the kept entity %d", pid, id, alphaID)
		}
	}
	assertVerifyClean(t, st)
}

// TestSetItemCreditsBatchCoverageComparesTarget pins the coverage check that makes a
// batch safe: a credit row sitting on an item the batch renames to a DIFFERENT name does
// not cover, because the file behind it still spells the old name and the next rescan
// would fork it back. Alpha, whose every reference moves to Beta, still renames.
func TestSetItemCreditsBatchCoverageComparesTarget(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/01.flac", essence: "e1", content: "c1",
		title: "S1", artist: "Alpha, Shared", artists: []string{"Alpha", "Shared"}, album: "One", year: 2001,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/01.flac", essence: "e2", content: "c2",
		title: "S2", artist: "Shared", album: "Two", year: 2002,
	})
	pid1 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S1'"))
	pid2 := model.PID(scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='S2'"))
	alphaID := entityIDByCol(t, st, "artist", "name", "Alpha")
	sharedID := entityIDByCol(t, st, "artist", "name", "Shared")
	sharedPID := entityPID(t, st, "artist", "Shared")

	if _, err := st.SetItemCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: pid1, Role: model.RoleArtist, Names: []string{"Beta"}},
		{ItemPID: pid2, Role: model.RoleArtist, Names: []string{"Delta"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("batch: %v", err)
	}

	// Shared is credited on S1 too, and S1 moves to Beta, so its rename is refused.
	var name, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, pid FROM artist WHERE id=?", sharedID).
		Scan(&name, &gotPID); err != nil {
		t.Fatalf("read Shared: %v", err)
	}
	if name != "Shared" || gotPID != string(sharedPID) {
		t.Fatalf("Shared = %q/%s, want it left where it is", name, gotPID)
	}
	deltaID := entityIDByCol(t, st, "artist", "name", "Delta")
	if deltaID == sharedID {
		t.Fatalf("Delta id = %d, want a fresh row rather than the renamed Shared", deltaID)
	}
	// Alpha's only reference is S1, which moves to Beta, so it renames in place.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE id=? AND name='Beta'", alphaID); n != 1 {
		t.Errorf("Alpha renamed to Beta = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestSetItemCreditsBatchContributorRoleCardinality: a contributor role with no entity
// key of its own renames its holder in place only when exactly one artist holds the role
// and the entry names exactly one replacement. Two holders collapsing onto one name is
// not a rename, and the query behind the holders has no order, so nothing could pick
// which of them the pair meant.
func TestSetItemCreditsBatchContributorRoleCardinality(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	// Two holders, one new name: neither may be renamed.
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"Prod One", "Prod Two"},
		model.Attribution{Source: model.SourceUser}, model.LockOff, false, false); err != nil {
		t.Fatalf("seed producers: %v", err)
	}
	oneID := entityIDByCol(t, st, "artist", "name", "Prod One")
	twoID := entityIDByCol(t, st, "artist", "name", "Prod Two")
	if _, err := st.SetItemCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: pid, Role: model.RoleProducer, Names: []string{"Prod Solo"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOff, false, false); err != nil {
		t.Fatalf("collapse producers: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE id=? AND name='Prod One'", oneID); n != 1 {
		t.Errorf("Prod One rows = %d, want it untouched", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE id=? AND name='Prod Two'", twoID); n != 1 {
		t.Errorf("Prod Two rows = %d, want it untouched", n)
	}
	soloID := entityIDByCol(t, st, "artist", "name", "Prod Solo")
	if soloID == oneID || soloID == twoID {
		t.Fatalf("Prod Solo id = %d, want a fresh row rather than a renamed holder", soloID)
	}

	// One holder, one new name: that pair fires and the entity keeps its pid.
	soloPID := entityPID(t, st, "artist", "Prod Solo")
	if _, err := st.SetItemCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: pid, Role: model.RoleProducer, Names: []string{"Prod Renamed"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOff, false, false); err != nil {
		t.Fatalf("rename producer: %v", err)
	}
	var name, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, pid FROM artist WHERE id=?", soloID).
		Scan(&name, &gotPID); err != nil {
		t.Fatalf("read producer: %v", err)
	}
	if name != "Prod Renamed" || gotPID != string(soloPID) {
		t.Fatalf("producer = %q/%s, want Prod Renamed with kept pid %s", name, gotPID, soloPID)
	}
	assertVerifyClean(t, st)
}

// TestSetItemCreditsBatchTwoRolesOneItem: a batch identifies entries by the (item, role)
// pair, so one book can take an author entry and a narrator entry at once and both
// entities rename in place. The author member overlays the narrator entry's names too,
// so the fold-back guard reads the credit the batch leaves behind rather than the one it
// is replacing.
func TestSetItemCreditsBatchTwoRolesOneItem(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	janeID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	janePID := entityPID(t, st, "artist", "Jane Author")
	nedID := entityIDByCol(t, st, "artist", "name", "Ned Narrator")
	nedPID := entityPID(t, st, "artist", "Ned Narrator")

	res, err := st.SetItemCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: pid, Role: model.RoleAuthor, Names: []string{"Janet Author"}},
		{ItemPID: pid, Role: model.RoleNarrator, Names: []string{"Nina Narrator"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Edited) != 2 {
		t.Fatalf("edited = %+v, want both entries", res.Edited)
	}

	for _, c := range []struct {
		id       int64
		wantName string
		wantPID  model.PID
	}{{janeID, "Janet Author", janePID}, {nedID, "Nina Narrator", nedPID}} {
		var name, gotPID string
		if err := st.read.QueryRowContext(ctx, "SELECT name, pid FROM artist WHERE id=?", c.id).
			Scan(&name, &gotPID); err != nil {
			t.Fatalf("read artist %d: %v", c.id, err)
		}
		if name != c.wantName || gotPID != string(c.wantPID) {
			t.Errorf("artist %d = %q/%s, want %q with kept pid %s", c.id, name, gotPID, c.wantName, c.wantPID)
		}
	}
	if id := scalarInt(t, st, `SELECT b.author_id FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); int64(id) != janeID {
		t.Errorf("book author_id = %d, want the renamed row %d", id, janeID)
	}
	if s := scalarStr(t, st, `SELECT b.narrator FROM book b
		JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`, string(pid)); s != "Nina Narrator" {
		t.Errorf("book narrator = %q, want Nina Narrator", s)
	}
	assertVerifyClean(t, st)
}

// TestSetItemCreditsBatchFoldBackIntoSiblingRoleBlocksRename: a batch that renames the
// author while folding her old name into the narrator credit of the same book must not
// rename her row, since the narrator credit would then describe nobody. The fold-back
// guard reads the sibling entry's names to catch it.
func TestSetItemCreditsBatchFoldBackIntoSiblingRoleBlocksRename(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	janeID := entityIDByCol(t, st, "artist", "name", "Jane Author")
	janePID := entityPID(t, st, "artist", "Jane Author")

	if _, err := st.SetItemCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: pid, Role: model.RoleAuthor, Names: []string{"Janet Author"}},
		{ItemPID: pid, Role: model.RoleNarrator, Names: []string{"Ned Narrator", "Jane Author"}},
	}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); err != nil {
		t.Fatalf("batch: %v", err)
	}

	var name, gotPID string
	if err := st.read.QueryRowContext(ctx, "SELECT name, pid FROM artist WHERE id=?", janeID).
		Scan(&name, &gotPID); err != nil {
		t.Fatalf("read Jane: %v", err)
	}
	if name != "Jane Author" || gotPID != string(janePID) {
		t.Fatalf("Jane = %q/%s, want her left in place for the narrator credit", name, gotPID)
	}
	if id := entityIDByCol(t, st, "artist", "name", "Janet Author"); id == janeID {
		t.Fatalf("Janet id = %d, want a fresh row rather than the renamed Jane", id)
	}
	assertVerifyClean(t, st)
}

// TestSetItemCreditsBatchValidation: an empty batch, an unknown role, and a repeated
// (item, role) pair are all refused before any write, while the same item under two
// roles is not a duplicate.
func TestSetItemCreditsBatchValidation(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	attr := model.Attribution{Source: model.SourceUser}

	if _, err := st.SetItemCreditsBatch(ctx, nil, attr, model.LockOff, false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("empty batch = %v, want CodeInvalid", err)
	}
	if _, err := st.SetItemCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: pid, Role: model.ContributorRole("bogus"), Names: []string{"X"}},
	}, attr, model.LockOff, false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bogus role = %v, want CodeInvalid", err)
	}
	if _, err := st.SetItemCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: pid, Role: model.RoleAuthor, Names: []string{"A"}},
		{ItemPID: pid, Role: model.RoleAuthor, Names: []string{"B"}},
	}, attr, model.LockOff, false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("repeated pair = %v, want CodeInvalid", err)
	}
	// A music role on a book still fails the kind check.
	if _, err := st.SetItemCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: pid, Role: model.RoleDJMixer, Names: []string{"X"}},
	}, attr, model.LockOff, false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("djmixer on book = %v, want CodeInvalid", err)
	}
}

// TestSetItemCreditsBatchSkipsLocked: a locked credit role aborts the batch by default
// and is skipped and reported with skipLocked, per entry rather than per item.
func TestSetItemCreditsBatchSkipsLocked(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()
	attr := model.Attribution{Source: model.SourceUser}
	if err := st.LockField(ctx, pid, "credit.narrator"); err != nil {
		t.Fatalf("lock narrator: %v", err)
	}

	edits := []model.ItemCreditEdit{
		{ItemPID: pid, Role: model.RoleAuthor, Names: []string{"Janet Author"}},
		{ItemPID: pid, Role: model.RoleNarrator, Names: []string{"Nina Narrator"}},
	}
	if _, err := st.SetItemCreditsBatch(ctx, edits, attr, model.LockOff, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("locked entry = %v, want CodeLocked", err)
	}
	res, err := st.SetItemCreditsBatch(ctx, edits, attr, model.LockOff, false, true)
	if err != nil {
		t.Fatalf("skip-locked batch: %v", err)
	}
	if len(res.Edited) != 1 || res.Edited[0].Role != model.RoleAuthor {
		t.Fatalf("edited = %+v, want the author entry alone", res.Edited)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].ItemPID != pid || res.Skipped[0].Role != model.RoleNarrator {
		t.Fatalf("skipped = %+v, want the narrator entry once", res.Skipped)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic
		JOIN artist a ON a.id=ic.artist_id
		JOIN playable_item pi ON pi.id=ic.item_id
		WHERE pi.pid=? AND ic.role='narrator' AND a.name='Ned Narrator'`, string(pid)); n != 1 {
		t.Errorf("narrator credit rows = %d, want the locked credit untouched", n)
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
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("set locked credit = %v, want CodeLocked", err)
	}
	// Force overrides it.
	if _, _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"X"}, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true, false); err != nil {
		t.Fatalf("forced set: %v", err)
	}
	// A book-only credit role cannot be locked on a track.
	if err := st.LockField(ctx, pid, "credit.narrator"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("lock credit.narrator on track = %v, want CodeInvalid", err)
	}
}
