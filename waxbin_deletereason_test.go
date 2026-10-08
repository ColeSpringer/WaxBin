package waxbin_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestDeleteReasonIsRecorded: a caller's reason lands on every trash row its delete
// writes, through each of the three planners, and a plan without one records the
// mode's. A reason too long or carrying a control, format or line-breaking character, or one beside a mode that
// bypasses the trash, is refused at plan time, and again at apply when a caller set it on
// the plan by hand.
func TestDeleteReasonIsRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, title := range []string{"A", "B", "C", "D", "E"} {
		writeFile(t, filepath.Join(root, title+".mp3"),
			testaudio.BuildMP3WithAudio(title, "Artist", "Album", i+1, testaudio.AudioWithSeed(byte(i+1))))
	}
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := map[string]model.PID{}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 5 {
		t.Fatalf("items = %d (err %v), want five", len(items), err)
	}
	for _, it := range items {
		pid[it.Title] = it.PID
	}
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{pid["A"], pid["B"]}, model.DeleteTrash, waxbin.DeleteReason("book-merge"))
	if err != nil {
		t.Fatalf("plan with a reason: %v", err)
	}
	if plan.Reason != "book-merge" {
		t.Errorf("plan reason = %q, want book-merge", plan.Reason)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	plan, err = lib.PlanDeletePIDs(ctx, []model.PID{pid["C"]}, model.DeleteTrash)
	if err != nil {
		t.Fatalf("plan without a reason: %v", err)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	files, err := lib.ItemFiles(ctx, pid["D"])
	if err != nil || len(files) != 1 {
		t.Fatalf("D's files = %+v (err %v)", files, err)
	}
	plan, err = lib.PlanDeleteFiles(ctx, []model.PID{files[0].FilePID}, model.DeleteTrash, waxbin.DeleteReason("replaced"))
	if err != nil {
		t.Fatalf("plan files with a reason: %v", err)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	q := query.New(query.EntityItems).Where("title", query.OpIs, "E").Build()
	plan, err = lib.PlanDelete(ctx, q, model.DeleteTrash, waxbin.DeleteReason(" retention "))
	if err != nil {
		t.Fatalf("plan a sweep with a reason: %v", err)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	entries, err := lib.Trash(ctx, false, 0)
	if err != nil {
		t.Fatalf("trash: %v", err)
	}
	got := map[model.PID]string{}
	for _, e := range entries {
		got[e.ItemPID] = e.Reason
	}
	want := map[model.PID]string{
		pid["A"]: "book-merge", pid["B"]: "book-merge", pid["C"]: "user", pid["D"]: "replaced", pid["E"]: "retention",
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("trash reason for %s = %q, want %q (all: %v)", p, got[p], w, got)
		}
	}

	writeFile(t, filepath.Join(root, "F.mp3"), testaudio.BuildMP3WithAudio("F", "Artist", "Album", 6, testaudio.AudioWithSeed(6)))
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := itemPIDByTitle(t, ctx, lib, "F")
	for _, bad := range []string{strings.Repeat("x", 65), "book\nmerge", "tab\there", "del\x7f", "bad\xffbyte",
		"rtl\u202eoverride", "zero\u200bwidth", "joined\u200dname", "line\u2028break", "para\u2029break"} {
		if _, err := lib.PlanDeletePIDs(ctx, []model.PID{f}, model.DeleteTrash, waxbin.DeleteReason(bad)); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("reason %q planned with err %v, want CodeInvalid", bad, err)
		}
	}
	if _, err := lib.PlanDeletePIDs(ctx, []model.PID{f}, model.DeleteTrash, waxbin.DeleteReason(strings.Repeat("x", 64))); err != nil {
		t.Errorf("a 64-byte reason: %v, want it accepted", err)
	}
	// A delete that bypasses the trash writes no row to record a reason on.
	for _, mode := range []model.DeleteMode{model.DeletePermanent, model.DeletePrune} {
		if _, err := lib.PlanDeletePIDs(ctx, []model.PID{f}, mode, waxbin.DeleteReason("book-merge")); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("a reason with a %s delete planned with err %v, want CodeInvalid", mode, err)
		}
	}
	bypass, err := lib.PlanDeletePIDs(ctx, []model.PID{f}, model.DeletePermanent)
	if err != nil {
		t.Fatalf("plan F permanently: %v", err)
	}
	bypass.Reason = "by hand"
	if _, err := lib.ApplyDelete(ctx, bypass); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("apply of a permanent plan with a hand-set reason = %v, want CodeInvalid", err)
	}
	plan, err = lib.PlanDeletePIDs(ctx, []model.PID{f}, model.DeleteTrash)
	if err != nil {
		t.Fatalf("plan F: %v", err)
	}
	plan.Reason = "set\x00by hand"
	if _, err := lib.ApplyDelete(ctx, plan); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("apply with a hand-set bad reason = %v, want CodeInvalid", err)
	}
	if it, err := lib.Get(ctx, f); err != nil || it.State != model.StatePresent {
		t.Errorf("F after the refused apply = %+v (err %v), want it still present", it, err)
	}
}
