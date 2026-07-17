package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func TestEditTrackScalarIdentifiers(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	edits := map[string]string{
		"isrc":        "USRC17607839",
		"mbid":        "b1a9c0e9-d987-4042-ae91-78d6a3267d69",
		"compilation": "true",
	}
	if err := st.EditItemFields(ctx, pid, edits, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit scalars: %v", err)
	}

	var isrc, mbid string
	var comp int
	if err := st.read.QueryRowContext(ctx,
		"SELECT isrc, mbid, compilation FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
		string(pid)).Scan(&isrc, &mbid, &comp); err != nil {
		t.Fatalf("read track: %v", err)
	}
	if isrc != "USRC17607839" || mbid != "b1a9c0e9-d987-4042-ae91-78d6a3267d69" || comp != 1 {
		t.Fatalf("track = isrc %q mbid %q compilation %d", isrc, mbid, comp)
	}

	// The recording MBID becomes a cross-catalog resolution anchor.
	if v, err := st.ItemByRecordingMBID(ctx, "b1a9c0e9-d987-4042-ae91-78d6a3267d69"); err != nil || v.PID != pid {
		t.Fatalf("resolve by recording mbid = %v, %v", v, err)
	}
}

func TestEditTrackCompilationClear(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "compilation", "1", model.SourceUser, false, false); err != nil {
		t.Fatalf("set compilation: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "compilation", "", model.SourceUser, false, true); err != nil {
		t.Fatalf("clear compilation: %v", err)
	}
	var comp int
	if err := st.read.QueryRowContext(ctx,
		"SELECT compilation FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
		string(pid)).Scan(&comp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if comp != 0 {
		t.Fatalf("compilation = %d, want 0", comp)
	}
}

func TestEditTrackBadValues(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "mbid", "not-a-uuid", model.SourceUser, false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bad mbid = %v, want CodeInvalid", err)
	}
	if err := st.EditItemField(ctx, pid, "compilation", "maybe", model.SourceUser, false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bad compilation = %v, want CodeInvalid", err)
	}
	// A book-only field is rejected on a track.
	if err := st.EditItemField(ctx, pid, "publisher", "x", model.SourceUser, false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("publisher on track = %v, want CodeInvalid", err)
	}
}

func TestEditBookScalarFields(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	edits := map[string]string{
		"asin":        "B0067890AB",
		"isbn":        "9780000000001",
		"publisher":   "Recorded Books",
		"edition":     "Deluxe",
		"description": "A long description.",
		"mbid":        "c5e3a0f1-1111-2222-3333-444455556666",
	}
	if err := st.EditItemFields(ctx, pid, edits, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit book scalars: %v", err)
	}

	var asin, isbn, publisher, edition, description, mbid string
	if err := st.read.QueryRowContext(ctx,
		`SELECT asin, isbn, publisher, edition, description, COALESCE(mbid,'')
		 FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`,
		string(pid)).Scan(&asin, &isbn, &publisher, &edition, &description, &mbid); err != nil {
		t.Fatalf("read book: %v", err)
	}
	if asin != "B0067890AB" || isbn != "9780000000001" || publisher != "Recorded Books" ||
		edition != "Deluxe" || description != "A long description." ||
		mbid != "c5e3a0f1-1111-2222-3333-444455556666" {
		t.Fatalf("book = asin %q isbn %q pub %q ed %q desc %q mbid %q",
			asin, isbn, publisher, edition, description, mbid)
	}

	// A book edit preserves its other fields (author survives the re-upsert).
	v, err := st.ItemByPID(ctx, pid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.Artist != "Jane Author" {
		t.Fatalf("author after scalar edit = %q, want Jane Author", v.Artist)
	}
}
