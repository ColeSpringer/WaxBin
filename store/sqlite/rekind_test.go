package sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// itemRows counts rows of a table keyed by an item id, for the item behind pid.
func itemRows(t *testing.T, st *Store, table, col string, pid model.PID) int {
	t.Helper()
	return scalarInt(t, st, `SELECT COUNT(*) FROM `+table+` WHERE `+col+` = (SELECT id FROM playable_item WHERE pid = ?)`, string(pid))
}

// itemChanges counts the change rows naming pid after seq, and the ones that are updates.
func itemChanges(t *testing.T, st *Store, seq int64, pid model.PID) (all, updates int) {
	t.Helper()
	changes, err := st.ChangesSince(context.Background(), seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	for _, c := range changes {
		if c.EntityType == "item" && c.EntityPID == pid {
			all++
			if c.Op == model.OpUpdate {
				updates++
			}
		}
	}
	return all, updates
}

// TestRekindTrackToBookKeepsTheItem: a track whose file is put again as a book stays the
// same item, with everything keyed by it, and turns into a book in place: the track row
// goes, the book row, its author and chapter come, and the provenance only a track's
// fields carry goes while the genre lock stays. It emits one item update.
func TestRekindTrackToBookKeepsTheItem(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const path = "/lib/Tolkien/The Hobbit/01.mp3"
	tr := putTrack(t, st, lib.ID, trackSpec{path: path, essence: "rk1", content: "rkc1", title: "Chapter One",
		artist: "Tolkien", album: "The Hobbit", genre: "Fantasy", year: 1937})
	pid := tr.ItemPID

	other, err := st.CreateUser(ctx, "other")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	for user, pos := range map[model.PID]int64{"": 5000, other.PID: 7000} {
		if err := st.SetProgress(ctx, user, pid, pos, nil); err != nil {
			t.Fatalf("progress: %v", err)
		}
	}
	if _, err := st.AddBookmark(ctx, "", pid, 1000, "mark"); err != nil {
		t.Fatalf("bookmark: %v", err)
	}
	pl, err := st.CreatePlaylist(ctx, "List", "", model.PlaylistStatic, model.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("playlist: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{pid}); err != nil {
		t.Fatalf("playlist items: %v", err)
	}
	if _, err := st.RecordSession(ctx, "", pid, "test", 1, 2, 1); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := st.PutAcquisition(ctx, pid, model.AcquisitionInput{SourceType: model.SourceManual}); err != nil {
		t.Fatalf("acquisition: %v", err)
	}
	if _, _, err := st.SetItemTag(ctx, pid, "MOOD", []string{"calm"}, model.Attribution{}, model.LockOn, false); err != nil {
		t.Fatalf("custom tag: %v", err)
	}
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, tinyPNG(t), "png", model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("art: %v", err)
	}
	for field, value := range map[string]string{"album": "Edited Album", "mbid": "4e2b1b2a-0000-4000-8000-000000000001", "title": "Edited"} {
		if err := st.SetFieldProvenance(ctx, pid, field, model.Attribution{}, value, false); err != nil {
			t.Fatalf("provenance %s: %v", field, err)
		}
	}
	if err := st.LockField(ctx, pid, "genre"); err != nil {
		t.Fatalf("lock genre: %v", err)
	}
	// What only a track holds: lyrics, a book-owned key as a custom tag, an owed album, and
	// an enrichment marker for the track's fields.
	if err := st.SetItemLyrics(ctx, pid, &model.Lyrics{Unsynced: "la la"}, model.LockUnchanged, false); err != nil {
		t.Fatalf("lyrics: %v", err)
	}
	if _, _, err := st.SetItemTag(ctx, pid, "ASIN", []string{"B000TEST"}, model.Attribution{}, model.LockOn, false); err != nil {
		t.Fatalf("asin tag: %v", err)
	}
	if err := st.NoteTagWriteOwed(ctx, []model.PID{tr.FilePID}, []string{"album", "tag.MOOD"}); err != nil {
		t.Fatalf("owed: %v", err)
	}
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		return st.markEnrichedTx(ctx, tx, "fields", int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid))), "test", true, "")
	}); err != nil {
		t.Fatalf("marker: %v", err)
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("seq: %v", err)
	}

	res := putBook(t, st, lib.ID, bookSpec{path: path, essence: "rk1", content: "rkc1", title: "The Hobbit",
		author: "Tolkien", genres: []string{"Fantasy"}, year: 1937, chapters: []model.Chapter{{Title: "Chapter One"}},
		preserveLocks: true})
	if res.ItemPID != pid || res.ItemCreated {
		t.Fatalf("put as a book: item %s created %v, want the track's item %s kept", res.ItemPID, res.ItemCreated, pid)
	}
	v, err := st.ItemByPID(ctx, pid)
	if err != nil || v.Kind != model.KindBook || v.Title != "The Hobbit" {
		t.Fatalf("item = %+v (err %v), want the book", v, err)
	}
	for _, c := range []struct {
		table, col string
		want       int
	}{
		{"track", "item_id", 0}, {"book", "item_id", 1}, {"item_contributor", "item_id", 1}, {"chapter", "book_item_id", 1},
		{"play_state", "item_id", 2}, {"bookmark", "item_id", 1}, {"playlist_item", "item_id", 1},
		{"play_session", "item_id", 1}, {"acquisition", "item_id", 1}, {"item_tag", "item_id", 1},
		{"art_map", "entity_id", 1}, {"search_fts", "rowid", 1}, {"lyrics", "item_id", 0},
		{"entity_enrichment", "entity_id", 0},
	} {
		if n := itemRows(t, st, c.table, c.col, pid); n != c.want {
			t.Errorf("%s rows = %d, want %d", c.table, n, c.want)
		}
	}
	rows, err := st.FieldProvenance(ctx, pid)
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	fields := map[string]bool{}
	for _, r := range rows {
		fields[r.Field] = r.Locked
	}
	for _, gone := range []string{"album", "mbid", "title", "tag.ASIN"} {
		if _, ok := fields[gone]; ok {
			t.Errorf("provenance %q kept, want it dropped with the track", gone)
		}
	}
	if !fields["genre"] {
		t.Errorf("genre lock = %v, want it kept", fields["genre"])
	}
	owed := map[string]bool{}
	rowsOwed, err := st.read.QueryContext(ctx, `SELECT tag_key FROM file_diagnostic WHERE code = 'tag_write_owed'`)
	if err != nil {
		t.Fatalf("owed rows: %v", err)
	}
	for rowsOwed.Next() {
		var k string
		if err := rowsOwed.Scan(&k); err != nil {
			t.Fatal(err)
		}
		owed[k] = true
	}
	rowsOwed.Close()
	if owed["album"] || !owed["tag.MOOD"] {
		t.Errorf("owed = %v, want the album dropped with the track and the custom tag kept", owed)
	}
	if all, updates := itemChanges(t, st, seq, pid); all != 1 || updates != 1 {
		t.Errorf("item changes = %d (%d updates), want one update", all, updates)
	}
	assertVerifyClean(t, st)
}

// TestRekindBookToTrackDropsChapters: a single-file book whose file is put again as a track
// stays the same item, and its chapters and chapters lock go with the book.
func TestRekindBookToTrackDropsChapters(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const path = "/lib/Tolkien/hobbit.mp3"
	pid := putBook(t, st, lib.ID, bookSpec{path: path, essence: "rk2", content: "rkc2", title: "The Hobbit",
		author: "Tolkien", narrators: []string{"Inglis"}, durationMS: 2000,
		chapters: []model.Chapter{{Position: 0, Title: "One"}, {Position: 1, Title: "Two", FileStartMS: 1000}}}).ItemPID
	if err := st.LockField(ctx, pid, "chapters"); err != nil {
		t.Fatalf("lock chapters: %v", err)
	}
	res := putTrack(t, st, lib.ID, trackSpec{path: path, essence: "rk2", content: "rkc2", title: "The Hobbit",
		artist: "Tolkien", album: "Audio"})
	if res.ItemPID != pid || res.ItemCreated {
		t.Fatalf("put as a track: item %s created %v, want the book's item %s kept", res.ItemPID, res.ItemCreated, pid)
	}
	if v, err := st.ItemByPID(ctx, pid); err != nil || v.Kind != model.KindTrack {
		t.Fatalf("item = %+v (err %v), want a track", v, err)
	}
	for _, c := range []struct {
		table, col string
		want       int
	}{{"chapter", "book_item_id", 0}, {"book", "item_id", 0}, {"track", "item_id", 1}} {
		if n := itemRows(t, st, c.table, c.col, pid); n != c.want {
			t.Errorf("%s rows = %d, want %d", c.table, n, c.want)
		}
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_contributor ic JOIN playable_item pi ON pi.id = ic.item_id
		WHERE pi.pid = ? AND ic.role = 'narrator'`, string(pid)); n != 0 {
		t.Errorf("narrator credits = %d, want none on a track", n)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM field_provenance fp JOIN playable_item pi ON pi.id = fp.item_id
		WHERE pi.pid = ? AND fp.field = 'chapters'`, string(pid)); n != 0 {
		t.Errorf("chapters lock rows = %d, want it gone with the chapters", n)
	}
	assertVerifyClean(t, st)
}

// TestRekindLeavesAMultiPartBook: the primary of a book with other parts, put again as a
// track, leaves the book rather than turning it into a track, and the book keeps its pid
// on its remaining part.
func TestRekindLeavesAMultiPartBook(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/b/p1.mp3", essence: "mp1", content: "mpc1", title: "Tome",
		author: "Auth", position: 1, durationMS: 1000}).ItemPID
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/p2.mp3", essence: "mp2", content: "mpc2", title: "Tome",
		author: "Auth", position: 2, durationMS: 1000})
	tr := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/p1.mp3", essence: "mp1", content: "mpc1", title: "Song", artist: "Band"})
	if tr.ItemPID == book || !tr.ItemCreated {
		t.Fatalf("p1 as a track: item %s created %v, want a new track", tr.ItemPID, tr.ItemCreated)
	}
	if v, err := st.ItemByPID(ctx, book); err != nil || v.Kind != model.KindBook {
		t.Fatalf("book after losing p1 = %+v (err %v), want it still a book", v, err)
	}
	assertVerifyClean(t, st)
}

// TestPreserveIdentityKeepsAnotherKind: the in-place re-key for a new essence algorithm
// re-keys only an item of the put's kind, so a book part read as a track under a new
// essence leaves the book's key alone.
func TestPreserveIdentityKeepsAnotherKind(t *testing.T) {
	st, lib := entityFixture(t)
	book := putBook(t, st, lib.ID, bookSpec{path: "/lib/b/p1.mp3", essence: "pk1", content: "pkc1", title: "Tome",
		author: "Auth", position: 1})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/p2.mp3", essence: "pk2", content: "pkc2", title: "Tome",
		author: "Auth", position: 2})
	key := scalarStr(t, st, "SELECT identity_key FROM playable_item WHERE pid = ?", string(book.ItemPID))
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/p1.mp3", essence: "pk1-v2", content: "pkc1", title: "Song", artist: "Band"})
	if got := scalarStr(t, st, "SELECT identity_key FROM playable_item WHERE pid = ?", string(book.ItemPID)); got != key {
		t.Errorf("book key = %q, want %q kept", got, key)
	}
}

// TestRekindHoldsBackForAKindLock: a put that did not force its kind leaves an item a kind
// lock pins alone, as with a lock set between a scan's read and its write, and the scan
// reads the file again; a forced put re-kinds it.
func TestRekindHoldsBackForAKindLock(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	const path = "/lib/Tolkien/hobbit.mp3"
	pid := putTrack(t, st, lib.ID, trackSpec{path: path, essence: "lk1", content: "lkc1", title: "Chapter One",
		artist: "Tolkien", album: "The Hobbit"}).ItemPID
	if err := st.LockField(ctx, pid, model.KindLockField); err != nil {
		t.Fatalf("lock kind: %v", err)
	}
	in := bookSpecInput(lib.ID, bookSpec{path: path, essence: "lk1", content: "lkc1", title: "The Hobbit",
		author: "Tolkien", preserveLocks: true})
	if _, err := st.PutScannedBook(ctx, in); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("unforced put over a kind lock = %v, want a conflict", err)
	}
	if v, err := st.ItemByPID(ctx, pid); err != nil || v.Kind != model.KindTrack {
		t.Fatalf("item = %+v (err %v), want the locked track", v, err)
	}
	in.KindForced = true
	if res, err := st.PutScannedBook(ctx, in); err != nil || res.ItemPID != pid {
		t.Fatalf("forced put = %+v (err %v), want the item re-kinded", res, err)
	}
	if v, err := st.ItemByPID(ctx, pid); err != nil || v.Kind != model.KindBook {
		t.Errorf("item = %+v (err %v), want a book", v, err)
	}
}

// TestRekindPutsAlternatesAtThePart: a track turned into a book keeps its other encoding
// as the alternate of its one part, at the part's position, and that encoding read again
// as the book stays the alternate rather than becoming a second part.
func TestRekindPutsAlternatesAtThePart(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	const mbid = "4e2b1b2a-0000-4000-8000-0000000000aa"
	flacPath, mp3Path := file("Dune/03.flac", "f"), file("Dune/03.mp3", "m")
	enc := func(path, essence, codec string, depth, bitrate int) model.PutScannedTrackInput {
		in := trackSpecInput(lib.ID, trackSpec{path: path, essence: essence, content: essence + "-bytes", title: "Dune",
			artist: "Frank Herbert", album: "Dune", mbRecording: mbid, durationMS: 1000})
		in.File.Codec, in.File.SampleRate, in.File.BitDepth, in.File.Bitrate = codec, 44100, depth, bitrate
		return in
	}
	tr := putTrackInput(t, st, enc(flacPath, "rp-flac", "flac", 16, 900))
	if out := putTrackInput(t, st, enc(mp3Path, "rp-mp3", "mp3", 0, 320)); !out.AttachedAsCopy {
		t.Fatalf("mp3 = %+v, want the FLAC's alternate", out)
	}
	put := encodedPart(lib.ID, flacPath, "rp-flac", "flac", 3, 1000)
	if res, err := st.PutScannedBook(ctx, put); err != nil || res.ItemPID != tr.ItemPID {
		t.Fatalf("flac as a book = %+v (err %v), want the track's item", res, err)
	}
	if res, err := st.PutScannedBook(ctx, encodedPart(lib.ID, mp3Path, "rp-mp3", "mp3", 3, 1000)); err != nil || !res.AttachedAsCopy {
		t.Fatalf("mp3 as a book = %+v (err %v), want it kept the alternate", res, err)
	}
	refs, err := st.ItemFiles(ctx, tr.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		want := "primary"
		if r.DisplayPath == mp3Path {
			want = "alternate"
		}
		if r.Role != want || r.Position != 3 {
			t.Errorf("%s = %s at %d, want %s at 3", r.DisplayPath, r.Role, r.Position, want)
		}
	}
	if d, err := st.BookByPID(ctx, tr.ItemPID); err != nil || len(d.Files) != 1 || d.TotalDurationMS != 1000 {
		t.Errorf("book = %+v (err %v), want one part over 1000 ms", d, err)
	}
}

func putTrackInput(t *testing.T, st *Store, in model.PutScannedTrackInput) *model.ScanItemResult {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s: %v", in.File.DisplayPath, err)
	}
	return res
}
