package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func TestSetItemCreditsMusicRoles(t *testing.T) {
	st, pid := editFixture(t) // one track: artist Alpha, composer "Writer"
	ctx := context.Background()

	// Set producers on the track.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"Prod One", "Prod Two"}, model.SourceUser, true, false); err != nil {
		t.Fatalf("set producers: %v", err)
	}
	// Set composers via the credit API; the denormalized track.composer follows.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"Comp A", "Comp B"}, model.SourceUser, true, false); err != nil {
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
	if _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"Prod Solo"}, model.SourceUser, true, true); err != nil {
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
	if _, err := st.SetItemCredits(ctx, pid, model.RoleNarrator, []string{"X"}, model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("narrator on track = %v, want CodeInvalid", err)
	}
	// An unknown role is rejected.
	if _, err := st.SetItemCredits(ctx, pid, model.ContributorRole("bogus"), []string{"X"}, model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bogus role = %v, want CodeInvalid", err)
	}
}

func TestSetItemCreditsBookAuthorSyncsDenorm(t *testing.T) {
	st, bpid := bookEditFixture(t)
	ctx := context.Background()

	if _, err := st.SetItemCredits(ctx, bpid, model.RoleAuthor, []string{"New Author"}, model.SourceUser, true, false); err != nil {
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
	if _, err := st.SetItemCredits(ctx, bpid, model.RoleDJMixer, []string{"X"}, model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("djmixer on book = %v, want CodeInvalid", err)
	}
}

func TestSetItemCreditsDedupAndCount(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	// "Bob"/"bob" fold to one artist (match key), and "" / "!!!" resolve to nothing.
	stored, err := st.SetItemCredits(ctx, pid, model.RoleComposer,
		[]string{"Bob", "bob", "", "!!!"}, model.SourceUser, true, false)
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
	stored, err = st.SetItemCredits(ctx, pid, model.RoleComposer, []string{"!!!"}, model.SourceUser, true, true)
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
		if err := st.SetFieldProvenance(ctx, pid, f, model.SourceUser, "x", false); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Fatalf("SetFieldProvenance(%q) = %v, want CodeInvalid", f, err)
		}
	}
	// A scalar field still works.
	if err := st.SetFieldProvenance(ctx, pid, "comment", model.SourceUser, "hi", false); err != nil {
		t.Fatalf("scalar SetFieldProvenance: %v", err)
	}
}

func TestLockCreditRole(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	// A credit role is lockable via the field lock path.
	if err := st.LockField(ctx, pid, "credit.producer"); err != nil {
		t.Fatalf("lock credit.producer: %v", err)
	}
	// Setting the locked role without force is refused.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"X"}, model.SourceUser, true, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("set locked credit = %v, want CodeLocked", err)
	}
	// Force overrides it.
	if _, err := st.SetItemCredits(ctx, pid, model.RoleProducer, []string{"X"}, model.SourceUser, true, true); err != nil {
		t.Fatalf("forced set: %v", err)
	}
	// A book-only credit role cannot be locked on a track.
	if err := st.LockField(ctx, pid, "credit.narrator"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("lock credit.narrator on track = %v, want CodeInvalid", err)
	}
}
