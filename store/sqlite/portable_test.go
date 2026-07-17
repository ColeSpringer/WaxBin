package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// trackIn builds a track scan input with explicit descriptive + strong-id fields, on top
// of the shared input() helper (which keys identity on essence).
func trackIn(libID int64, path, essence, title, artist, album, mbid string) model.PutScannedTrackInput {
	in := input(libID, path, essence, "c-"+essence, title)
	in.Track.Artist = artist
	in.Track.AlbumArtist = artist
	in.Track.Album = album
	in.Track.MBID = mbid
	return in
}

// bookIn builds a single-file audiobook scan input with strong-id + descriptive fields.
func bookIn(libID int64, path, essence, title, author, series, asin, isbn string) model.PutScannedBookInput {
	key := identity.BookKey(asin, isbn, author, title, "")
	if key == "" {
		key = "essence:" + essence
	}
	return model.PutScannedBookInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 10, MTimeNS: 1,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: key,
		},
		Book: model.Book{
			Author: author, AuthorSort: model.SortKey(author), Authors: []string{author},
			Series: series, ASIN: asin, ISBN: isbn,
		},
	}
}

// TestItemIdentityBookDurationSumsParts verifies a multi-file audiobook exports its
// TOTAL running time (the sum of its parts), matching the item view it is later compared
// against, rather than only the primary part's duration.
func TestItemIdentityBookDurationSumsParts(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	// Two parts of one book (shared BookKey via author+title), with distinct durations.
	part := func(path, essence string, pos int, dur int64) model.PutScannedBookInput {
		in := bookIn(lib.ID, path, essence, "Long Book", "Author X", "", "", "")
		in.File.DurationMS = dur
		in.Position = pos
		return in
	}
	r1, err := st.PutScannedBook(ctx, part("/lib/b1.m4b", "E-P1", 1, 100000))
	if err != nil {
		t.Fatalf("put part 1: %v", err)
	}
	r2, err := st.PutScannedBook(ctx, part("/lib/b2.m4b", "E-P2", 2, 200000))
	if err != nil {
		t.Fatalf("put part 2: %v", err)
	}
	if r2.ItemPID != r1.ItemPID {
		t.Fatalf("both parts should join one book item: %s vs %s", r1.ItemPID, r2.ItemPID)
	}

	refs, err := st.ItemIdentitiesByPIDs(ctx, []model.PID{r1.ItemPID})
	if err != nil || len(refs) != 1 {
		t.Fatalf("identities = %d refs (err %v), want 1", len(refs), err)
	}
	if refs[0].DurationMS != 300000 {
		t.Fatalf("book ref duration = %d, want 300000 (sum of both parts, not just the primary)", refs[0].DurationMS)
	}
}

func TestItemsByEssence(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	tr, err := st.PutScannedTrack(ctx, trackIn(lib.ID, "/lib/a.mp3", "E-A", "Song A", "Alpha", "Album A", ""))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}

	got, err := st.ItemsByEssence(ctx, "E-A")
	if err != nil {
		t.Fatalf("ItemsByEssence: %v", err)
	}
	if len(got) != 1 || got[0].PID != tr.ItemPID {
		t.Fatalf("essence lookup = %d items, want the one track %s", len(got), tr.ItemPID)
	}

	if miss, err := st.ItemsByEssence(ctx, "E-NONE"); err != nil || len(miss) != 0 {
		t.Fatalf("miss lookup = %d items (err %v), want 0 and no error", len(miss), err)
	}
	if empty, err := st.ItemsByEssence(ctx, ""); err != nil || empty != nil {
		t.Fatalf("empty essence should short-circuit to nil, got %v (err %v)", empty, err)
	}

	// A single-file CUE album: two virtual tracks share one file (one essence), so the
	// lookup is plural.
	if _, err := st.PutScannedVirtualTracks(ctx,
		vtrackInput(lib.ID, "/lib/cue.mp3", "E-CUE", "cue-content", 300, [][2]int64{{0, 100}, {100, 300}})); err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}
	vt, err := st.ItemsByEssence(ctx, "E-CUE")
	if err != nil {
		t.Fatalf("ItemsByEssence(cue): %v", err)
	}
	if len(vt) != 2 {
		t.Fatalf("cue essence lookup = %d items, want 2 (both virtual tracks)", len(vt))
	}
}

func TestItemByRecordingMBID(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	tr, err := st.PutScannedTrack(ctx, trackIn(lib.ID, "/lib/a.mp3", "E-A", "Song A", "Alpha", "Album A", "rec-abc"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}

	// Exact match, and case-insensitive match (MBID case is the only cross-catalog variance).
	for _, q := range []string{"rec-abc", "REC-ABC"} {
		got, err := st.ItemByRecordingMBID(ctx, q)
		if err != nil {
			t.Fatalf("ItemByRecordingMBID(%q): %v", q, err)
		}
		if got.PID != tr.ItemPID {
			t.Fatalf("ItemByRecordingMBID(%q) = %s, want %s", q, got.PID, tr.ItemPID)
		}
	}

	// A miss and an empty id both return CodeNotFound (no error to the caller's ladder).
	for _, q := range []string{"rec-none", ""} {
		if _, err := st.ItemByRecordingMBID(ctx, q); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Fatalf("ItemByRecordingMBID(%q) err = %v, want CodeNotFound", q, err)
		}
	}

	// The same recording MBID on two distinct items (a single + a compilation) is
	// ambiguous and declines, rather than resolving to an arbitrary one.
	if _, err := st.PutScannedTrack(ctx, trackIn(lib.ID, "/lib/b.mp3", "E-B", "Song A", "Alpha", "Best Of", "dup-mbid")); err != nil {
		t.Fatalf("put track b: %v", err)
	}
	if _, err := st.PutScannedTrack(ctx, trackIn(lib.ID, "/lib/c.mp3", "E-C", "Song A", "Alpha", "Album A", "dup-mbid")); err != nil {
		t.Fatalf("put track c: %v", err)
	}
	if _, err := st.ItemByRecordingMBID(ctx, "dup-mbid"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("ambiguous recording mbid err = %v, want CodeNotFound", err)
	}
}

func TestItemsByArtistKey(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	tr, err := st.PutScannedTrack(ctx, trackIn(lib.ID, "/lib/a.mp3", "E-A", "Halo", "Beyoncé", "Album A", ""))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}

	// The artist entity match key folds diacritics, so "Beyoncé" seeds on "beyonce".
	got, err := st.ItemsByArtistKey(ctx, identity.MatchKey("Beyonce"))
	if err != nil {
		t.Fatalf("ItemsByArtistKey: %v", err)
	}
	if len(got) != 1 || got[0].PID != tr.ItemPID {
		t.Fatalf("artist-key seed = %d items, want the one track", len(got))
	}
	if miss, err := st.ItemsByArtistKey(ctx, identity.MatchKey("Nobody")); err != nil || len(miss) != 0 {
		t.Fatalf("unknown artist key = %d items (err %v), want 0", len(miss), err)
	}
	if empty, err := st.ItemsByArtistKey(ctx, ""); err != nil || empty != nil {
		t.Fatalf("empty artist key should short-circuit to nil, got %v (err %v)", empty, err)
	}
}

func TestItemByBookIdent(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	bk, err := st.PutScannedBook(ctx, bookIn(lib.ID, "/lib/hobbit.m4b", "E-BK", "The Hobbit", "Tolkien", "Middle Earth", "B01ASIN", "978-0-261"))
	if err != nil {
		t.Fatalf("put book: %v", err)
	}

	// ASIN and ISBN each resolve (case-insensitively for ASIN); the raw ISBN matches its
	// stored form.
	for _, tc := range []struct{ mbid, asin, isbn string }{
		{"", "B01ASIN", ""},
		{"", "b01asin", ""}, // case-insensitive
		{"", "", "978-0-261"},
	} {
		got, err := st.ItemByBookIdent(ctx, tc.mbid, tc.asin, tc.isbn)
		if err != nil {
			t.Fatalf("ItemByBookIdent(%+v): %v", tc, err)
		}
		if got.PID != bk.ItemPID {
			t.Fatalf("ItemByBookIdent(%+v) = %s, want %s", tc, got.PID, bk.ItemPID)
		}
	}

	// All-empty and a pure miss return CodeNotFound. (The ambiguous >1 decline shares the
	// singleItem LIMIT-2 path proven by TestItemByRecordingMBID; a strong id cannot back
	// two distinct book items via scan, since ASIN/ISBN drive BookKey identity.)
	if _, err := st.ItemByBookIdent(ctx, "", "", ""); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("all-empty book ident err = %v, want CodeNotFound", err)
	}
	if _, err := st.ItemByBookIdent(ctx, "", "NOPE", ""); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("missing book ident err = %v, want CodeNotFound", err)
	}
}

func TestItemsByAuthorKey(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	bk, err := st.PutScannedBook(ctx, bookIn(lib.ID, "/lib/hobbit.m4b", "E-BK", "The Hobbit", "J.R.R. Tolkien", "Middle Earth", "B01", ""))
	if err != nil {
		t.Fatalf("put book: %v", err)
	}
	got, err := st.ItemsByAuthorKey(ctx, identity.MatchKey("J.R.R. Tolkien"))
	if err != nil {
		t.Fatalf("ItemsByAuthorKey: %v", err)
	}
	if len(got) != 1 || got[0].PID != bk.ItemPID {
		t.Fatalf("author-key seed = %d items, want the one book", len(got))
	}
}

func TestItemIdentitiesByPIDs(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	tr, err := st.PutScannedTrack(ctx, trackIn(lib.ID, "/lib/a.mp3", "E-A", "Song A", "Alpha", "Album A", "rec-1"))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	bk, err := st.PutScannedBook(ctx, bookIn(lib.ID, "/lib/hobbit.m4b", "E-BK", "The Hobbit", "Tolkien", "Middle Earth", "B01", "978-1"))
	if err != nil {
		t.Fatalf("put book: %v", err)
	}
	vt, err := st.PutScannedVirtualTracks(ctx,
		vtrackInput(lib.ID, "/lib/cue.mp3", "E-CUE", "cue-content", 300, [][2]int64{{0, 100}, {100, 300}}))
	if err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}

	// Give BOTH the whole-file track and the shared CUE file a fingerprint, so the
	// virtual-track omission is proven by the join gate, not by an absent fingerprint.
	fp := func(filePID model.PID, essence string) {
		if err := st.PutAnalysis(ctx, model.AnalysisInput{AnalysisVersion: 1, Fingerprint: model.FingerprintInput{
			FilePID: filePID, EssenceHash: essence, AlgoVersion: 1, DurationBucket: 1,
			FP: []byte{1, 2, 3, 4}, Terms: []int64{7, 8, 9},
		}}); err != nil {
			t.Fatalf("put analysis: %v", err)
		}
	}
	fp(tr.FilePID, "E-A")
	fp(vt.FilePID, "E-CUE")

	// Input order preserved, a repeated pid collapsed to its first position, a missing
	// pid skipped.
	refs, err := st.ItemIdentitiesByPIDs(ctx, []model.PID{bk.ItemPID, tr.ItemPID, tr.ItemPID, "nope"})
	if err != nil {
		t.Fatalf("ItemIdentitiesByPIDs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2 (book, track; dup collapsed, missing skipped)", len(refs))
	}

	book, track := refs[0], refs[1]
	if book.Kind != model.KindBook || book.Essence != "E-BK" || book.ASIN != "B01" ||
		book.Artist != "Tolkien" || book.Title != "The Hobbit" || book.Album != "Middle Earth" {
		t.Fatalf("book ref wrong: %+v", book)
	}
	if book.MBID != "" {
		t.Fatalf("book ref MBID should be empty (scan does not set book.mbid), got %q", book.MBID)
	}
	if track.Kind != model.KindTrack || track.Essence != "E-A" || track.MBID != "rec-1" ||
		track.Artist != "Alpha" || track.Title != "Song A" || track.Album != "Album A" {
		t.Fatalf("track ref wrong: %+v", track)
	}
	if len(track.Fingerprint) == 0 || track.FingerprintAlgo != 1 {
		t.Fatalf("whole-file track ref should carry its fingerprint, got %d bytes algo %d", len(track.Fingerprint), track.FingerprintAlgo)
	}

	// A virtual track exports NO fingerprint even though its backing file has one.
	// (PutScannedVirtualTracks creates N items and leaves ScanItemResult.ItemPID empty,
	// so pick a real virtual-track pid via its shared essence.)
	vtItems, err := st.ItemsByEssence(ctx, "E-CUE")
	if err != nil || len(vtItems) == 0 {
		t.Fatalf("resolve virtual-track items: %d (err %v)", len(vtItems), err)
	}
	vtRefs, err := st.ItemIdentitiesByPIDs(ctx, []model.PID{vtItems[0].PID})
	if err != nil {
		t.Fatalf("ItemIdentitiesByPIDs(vt): %v", err)
	}
	if len(vtRefs) != 1 {
		t.Fatalf("got %d vt refs, want 1", len(vtRefs))
	}
	if len(vtRefs[0].Fingerprint) != 0 {
		t.Fatalf("virtual-track ref must omit the shared fingerprint, got %d bytes", len(vtRefs[0].Fingerprint))
	}
	if vtRefs[0].Essence != "E-CUE" {
		t.Fatalf("virtual-track ref essence = %q, want E-CUE", vtRefs[0].Essence)
	}

	if none, err := st.ItemIdentitiesByPIDs(ctx, nil); err != nil || none != nil {
		t.Fatalf("empty pids should return nil, got %v (err %v)", none, err)
	}
}

func TestFingerprintCandidatesByProbe(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	tr, err := st.PutScannedTrack(ctx, trackIn(lib.ID, "/lib/a.mp3", "E-A", "Song A", "Alpha", "Album A", ""))
	if err != nil {
		t.Fatalf("put track: %v", err)
	}
	if err := st.PutAnalysis(ctx, model.AnalysisInput{AnalysisVersion: 1, Fingerprint: model.FingerprintInput{
		FilePID: tr.FilePID, EssenceHash: "E-A", AlgoVersion: 1, DurationBucket: 5,
		FP: []byte{9, 9, 9}, Terms: []int64{10, 20, 30},
	}}); err != nil {
		t.Fatalf("put analysis: %v", err)
	}

	// Sharing two of the file's terms within the bucket window and same algorithm yields
	// the candidate, carrying the item pid and the stored vector. The shared count is
	// exactly the number of matching terms (no primary-edge fan-out for a whole-file item).
	cands, err := st.FingerprintCandidatesByProbe(ctx, "", 1, 4, 6, []int64{10, 20}, 2)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("probe candidates = %d, want 1", len(cands))
	}
	if cands[0].ItemPID != tr.ItemPID || cands[0].AlgoVersion != 1 || len(cands[0].FP) != 3 || cands[0].SharedTerms != 2 {
		t.Fatalf("candidate wrong: %+v", cands[0])
	}

	// The kind filter excludes a wrong-kind item (a book ref must not resolve to a track).
	if got, err := st.FingerprintCandidatesByProbe(ctx, model.KindBook, 1, 4, 6, []int64{10, 20}, 2); err != nil || len(got) != 0 {
		t.Fatalf("kind filter should exclude the track, got %d (err %v)", len(got), err)
	}
	// Filtering to the item's own kind keeps it.
	if got, err := st.FingerprintCandidatesByProbe(ctx, model.KindTrack, 1, 4, 6, []int64{10, 20}, 2); err != nil || len(got) != 1 {
		t.Fatalf("kind filter (track) should keep the track, got %d (err %v)", len(got), err)
	}

	// An empty term set is a clean nil (a short/corrupt fingerprint), never a SQL error.
	if got, err := st.FingerprintCandidatesByProbe(ctx, "", 1, 4, 6, nil, 2); err != nil || got != nil {
		t.Fatalf("empty terms should return nil, got %v (err %v)", got, err)
	}
	// A different algorithm never scores against this vector.
	if got, err := st.FingerprintCandidatesByProbe(ctx, "", 100, 4, 6, []int64{10, 20}, 2); err != nil || len(got) != 0 {
		t.Fatalf("algo mismatch should find nothing, got %d (err %v)", len(got), err)
	}
	// Outside the duration-bucket window, nothing matches.
	if got, err := st.FingerprintCandidatesByProbe(ctx, "", 1, 20, 30, []int64{10, 20}, 2); err != nil || len(got) != 0 {
		t.Fatalf("out-of-window probe should find nothing, got %d (err %v)", len(got), err)
	}
}

// TestProbeCUEAlbumNoFanout verifies the whole-file gate. A single-file CUE album's shared
// file is backed by N virtual-track primary edges, and the gate keeps that from fanning
// the shared count out N-fold or resolving to an arbitrary sibling. The candidate carries
// an empty item pid instead, which the facade skips.
func TestProbeCUEAlbumNoFanout(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	vt, err := st.PutScannedVirtualTracks(ctx,
		vtrackInput(lib.ID, "/lib/cue.mp3", "E-CUE", "cue-content", 300, [][2]int64{{0, 100}, {100, 300}}))
	if err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}
	if err := st.PutAnalysis(ctx, model.AnalysisInput{AnalysisVersion: 1, Fingerprint: model.FingerprintInput{
		FilePID: vt.FilePID, EssenceHash: "E-CUE", AlgoVersion: 1, DurationBucket: 5,
		FP: []byte{1, 2, 3}, Terms: []int64{10, 20, 30},
	}}); err != nil {
		t.Fatalf("put analysis: %v", err)
	}

	cands, err := st.FingerprintCandidatesByProbe(ctx, "", 1, 4, 6, []int64{10, 20}, 2)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("probe candidates = %d, want 1 (the shared file, once)", len(cands))
	}
	if cands[0].SharedTerms != 2 {
		t.Fatalf("shared terms = %d, want 2 (not N-fold inflated by the virtual-track edges)", cands[0].SharedTerms)
	}
	if cands[0].ItemPID != "" {
		t.Fatalf("virtual-track-backed file should carry an empty item pid, got %q", cands[0].ItemPID)
	}
}
