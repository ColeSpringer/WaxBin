package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestOwedRowsAreNoWritersRows: a replace leaves the owed ledger alone, so a file holding
// only owed rows has nothing for an edit write-back's clean replace to remove, and the
// guard that skips that write transaction has to read the file that way.
func TestOwedRowsAreNoWritersRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := entityFixture(t)
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/A/One/01.flac", essence: "e1", content: "c1",
		title: "Song", artist: "Alpha", albumArt: "Alpha", album: "One",
	})
	if err := st.EditItemField(ctx, res.ItemPID, "genre", "Jazz", model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if owed, err := owedKeysOf(ctx, st, res.FilePID); err != nil || len(owed) != 1 {
		t.Fatalf("owed rows = %v (err %v), want the one the edit left", owed, err)
	}
	has, err := st.hasFileDiagnostics(ctx, res.FilePID, model.OriginEdit)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("hasFileDiagnostics = true for a file holding only an owed row, want false")
	}

	if err := st.AddFileDiagnostic(ctx, res.FilePID, model.OriginEdit, model.FileDiagnostic{
		Code: model.DiagTagWriteUnsynced, Severity: model.SeverityWarn, Detail: "write failed",
	}); err != nil {
		t.Fatal(err)
	}
	if has, err = st.hasFileDiagnostics(ctx, res.FilePID, model.OriginEdit); err != nil || !has {
		t.Errorf("hasFileDiagnostics = %v (err %v) beside a drift row of the writer's own, want true", has, err)
	}
}

// owedKeysOf lists a file's owed keys through a read of its own.
func owedKeysOf(ctx context.Context, st *Store, filePID model.PID) ([]string, error) {
	diags, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: filePID, Code: model.DiagTagWriteOwed})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range diags {
		out = append(out, d.TagKey)
	}
	return out, nil
}
