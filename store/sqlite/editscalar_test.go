package sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
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
	if err := st.EditItemFields(ctx, pid, edits, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
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

// TestEditTrackBPM covers the numeric bpm column end to end on the edit surface: the
// value lands in the column, reaches the projected item view, and clears to NULL. The
// CLI parses integers only, so a fractional or negative value is a usage error here
// even though the tag key itself accepts a fraction on disk.
func TestEditTrackBPM(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "bpm", "128", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set bpm: %v", err)
	}
	var bpm int
	if err := st.read.QueryRowContext(ctx,
		"SELECT bpm FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
		string(pid)).Scan(&bpm); err != nil {
		t.Fatalf("read bpm: %v", err)
	}
	if bpm != 128 {
		t.Fatalf("stored bpm = %d, want 128", bpm)
	}
	v, err := st.ItemByPID(ctx, pid)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if v.BPM != 128 {
		t.Errorf("projected BPM = %d, want 128", v.BPM)
	}

	for _, bad := range []string{"120.5", "-3", "fast"} {
		if err := st.EditItemField(ctx, pid, "bpm", bad, model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("bpm %q = %v, want CodeInvalid", bad, err)
		}
	}

	// An empty value clears the column back to NULL.
	if err := st.EditItemField(ctx, pid, "bpm", "", model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil {
		t.Fatalf("clear bpm: %v", err)
	}
	var stored sql.NullInt64
	if err := st.read.QueryRowContext(ctx,
		"SELECT bpm FROM track t JOIN playable_item pi ON pi.id=t.item_id WHERE pi.pid=?",
		string(pid)).Scan(&stored); err != nil {
		t.Fatalf("read cleared bpm: %v", err)
	}
	if stored.Valid {
		t.Errorf("bpm after clear = %d, want NULL", stored.Int64)
	}
}

// TestQueryBPMRange pins bpm as a numeric query field: a range compare narrows on the
// number rather than on text, and a track with no bpm is outside every range.
func TestQueryBPMRange(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/slow.flac", essence: "e1", content: "c1", title: "Slow", artist: "A", album: "Alp", bpm: 90,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/fast.flac", essence: "e2", content: "c2", title: "Fast", artist: "A", album: "Alp", bpm: 174,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/none.flac", essence: "e3", content: "c3", title: "None", artist: "A", album: "Alp",
	})

	cases := []struct {
		name string
		q    query.Query
		want string
	}{
		{"gte 100", query.New(query.EntityItems).Where("bpm", query.OpGte, 100).Build(), "Fast"},
		{"lt 100", query.New(query.EntityItems).Where("bpm", query.OpLt, 100).Build(), "Slow"},
		{"in the range", query.New(query.EntityItems).WhereRange("bpm", query.OpInRange, 80, 100).Build(), "Slow"},
		{"is missing", query.New(query.EntityItems).WherePresence("bpm", query.OpIsMissing).Build(), "None"},
	}
	for _, c := range cases {
		if got := joinTitles(userQueryTitles(t, st, c.q, "")); got != c.want {
			t.Errorf("%s matched %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEditTrackCompilationClear(t *testing.T) {
	st, pid := editFixture(t)
	ctx := context.Background()

	if err := st.EditItemField(ctx, pid, "compilation", "1", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set compilation: %v", err)
	}
	if err := st.EditItemField(ctx, pid, "compilation", "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), true); err != nil {
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

	if err := st.EditItemField(ctx, pid, "mbid", "not-a-uuid", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bad mbid = %v, want CodeInvalid", err)
	}
	if err := st.EditItemField(ctx, pid, "compilation", "maybe", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bad compilation = %v, want CodeInvalid", err)
	}
	// A book-only field is rejected on a track.
	if err := st.EditItemField(ctx, pid, "publisher", "x", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("publisher on track = %v, want CodeInvalid", err)
	}
}

func TestEditBookScalarFields(t *testing.T) {
	st, pid := bookEditFixture(t)
	ctx := context.Background()

	edits := map[string]string{
		"asin":        "B0067890AB",
		"isbn":        "9780000000002",
		"publisher":   "Recorded Books",
		"edition":     "Deluxe",
		"description": "A long description.",
		"mbid":        "c5e3a0f1-1111-2222-3333-444455556666",
	}
	if err := st.EditItemFields(ctx, pid, edits, model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("edit book scalars: %v", err)
	}

	var asin, isbn, publisher, edition, description, mbid string
	if err := st.read.QueryRowContext(ctx,
		`SELECT asin, isbn, publisher, edition, description, COALESCE(mbid,'')
		 FROM book b JOIN playable_item pi ON pi.id=b.item_id WHERE pi.pid=?`,
		string(pid)).Scan(&asin, &isbn, &publisher, &edition, &description, &mbid); err != nil {
		t.Fatalf("read book: %v", err)
	}
	if asin != "B0067890AB" || isbn != "9780000000002" || publisher != "Recorded Books" ||
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
