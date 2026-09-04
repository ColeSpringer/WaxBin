package enrich_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/store/sqlite"
)

// The item-rung fields walks: the phases that let an injected provider fill a scalar
// field a track or a book left empty. Before them Candidate.Fields was a slot nothing in
// the engine read, and CapBookMeta was dead: enrichBook consulted only the MusicBrainz
// spine, so a registered book provider was never asked.

// fieldsService builds a service whose only route to work is the injected providers, so a
// test's expectations are not clouded by an identity phase running beside them.
func fieldsService(t *testing.T, st enrich.Store, providers ...enrich.Provider) *enrich.Service {
	t.Helper()
	return enrich.New(st, enrich.Config{
		MinRequestInterval: time.Millisecond, Providers: providers,
	}, nil)
}

// seedBook persists one single-file audiobook with the given author, leaving every
// fillable field empty.
func seedBook(t *testing.T, st *sqlite.Store, libID int64, path, essence, title, author string) model.PID {
	t.Helper()
	res, err := st.PutScannedBook(context.Background(), model.PutScannedBookInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1, DurationMS: 3600000,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "book:" + essence,
		},
		Book: model.Book{Author: author, Authors: []string{author}},
	})
	if err != nil {
		t.Fatalf("PutScannedBook: %v", err)
	}
	return res.ItemPID
}

// assertFieldsVerifyClean runs the derived-data check the way `db verify` does, so a
// fields apply that left a rollup or a sort key behind fails here rather than later.
func assertFieldsVerifyClean(t *testing.T, st *sqlite.Store) {
	t.Helper()
	rep, err := st.VerifyDerived(context.Background())
	if err != nil || !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v (err %v)", rep, err)
	}
}

// provenanceRow reads one item field's provenance: source, provider, locked, value.
func provenanceRow(t *testing.T, db *sql.DB, pid model.PID, field string) (string, string, int, string) {
	t.Helper()
	var source, provider, value string
	var locked int
	err := db.QueryRow(`SELECT fp.source, fp.provider, fp.locked, COALESCE(fp.value,'')
		FROM field_provenance fp JOIN playable_item pi ON pi.id = fp.item_id
		WHERE pi.pid = ? AND fp.field = ?`, string(pid), field).Scan(&source, &provider, &locked, &value)
	if err != nil {
		t.Fatalf("provenance for %s: %v", field, err)
	}
	return source, provider, locked, value
}

// TestTrackFieldsFillsEmptyScalars is the ask: a CapFields provider's answer for a
// recording lands on that one track, fill-when-empty, stamped with its name. The keys
// outside the track fill set are dropped rather than applied, so a provider returning
// everything it knows cannot fork the album off its year or write a genre the genre pass
// owns.
func TestTrackFieldsFillsEmptyScalars(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	pid := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	// The album rung of the same walk asks this provider too, so only the recording
	// requests are collected here; the album's own answer is TestAlbumFields' subject.
	var reqs []enrich.Request
	fields := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRecording {
				return nil, nil
			}
			reqs = append(reqs, req)
			return &enrich.Candidate{Fields: map[string]string{
				"bpm": "128", "isrc": "gbaya7500098",
				// Ignored: year would fork the album, artist is a chain key, genre is
				// CapGenres' to decide.
				"year": "1975", "artist": "Somebody Else", "genre": "Prog",
			}}, nil
		}}
	res, err := fieldsService(t, st, fields).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TrackFieldsEnriched != 1 || res.TrackFieldsMatched != 1 {
		t.Fatalf("track fields = %d walked / %d matched, want 1 and 1",
			res.TrackFieldsEnriched, res.TrackFieldsMatched)
	}
	if len(reqs) != 1 {
		t.Fatalf("provider asked %d times, want once", len(reqs))
	}
	if reqs[0].Type != enrich.TargetRecording || reqs[0].Want != enrich.CapFields {
		t.Errorf("request = %s / want %v, want a recording target under CapFields", reqs[0].Type, reqs[0].Want)
	}
	if reqs[0].Title != "Shine On" || reqs[0].Artist != "Pink Floyd" || reqs[0].Album != "Wish You Were Here" {
		t.Errorf("request hints = %+v, want the track's title, artist and album", reqs[0])
	}

	db := roDB(t, dbPath)
	if got := scalarStr(t, db, "SELECT CAST(bpm AS TEXT) FROM track"); got != "128" {
		t.Errorf("bpm = %q, want 128", got)
	}
	// The identifier is folded to its canonical stored form on the way in.
	if got := scalarStr(t, db, "SELECT isrc FROM track"); got != "GBAYA7500098" {
		t.Errorf("isrc = %q, want the normalized identifier", got)
	}
	source, provider, locked, value := provenanceRow(t, db, pid, "bpm")
	if source != "enrichment" || provider != "deezer" || locked != 0 || value != "128" {
		t.Errorf("bpm provenance = %s/%s locked=%d value=%q, want enrichment/deezer unlocked 128",
			source, provider, locked, value)
	}
	// The ignored keys landed nowhere: the year is still the scan's and the album is
	// still one row.
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("albums = %d, want the one the scan made", n)
	}
	if a := scalarStr(t, db, "SELECT artist FROM track"); a != "Pink Floyd" {
		t.Errorf("artist = %q, want the scan's; a chain key is not fillable", a)
	}
	if g := scalarStr(t, db, "SELECT genre FROM track"); g != "" {
		t.Errorf("genre = %q, want it left to the genre pass", g)
	}

	assertFieldsVerifyClean(t, st)

	// The marker stops a repeat.
	again, err := fieldsService(t, st, fields).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again.TrackFieldsEnriched != 0 {
		t.Errorf("second run walked %d tracks; the marker should have held", again.TrackFieldsEnriched)
	}
}

// TestTrackFieldsRespectsLocksAndFilledValues: a locked field and an already-filled one
// are both left alone, which is the fill-when-empty, lock-respecting rule the rest of
// enrichment follows. The marker still lands, so an item nothing could fill costs one
// pass rather than a request every run.
func TestTrackFieldsRespectsLocksAndFilledValues(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	pid := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	// A curated isrc, locked, plus a composer the scan would have written.
	if err := st.EditItemFields(ctx, pid, map[string]string{"isrc": "USRC17607839"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("lock the isrc: %v", err)
	}
	if err := st.EditItemFields(ctx, pid, map[string]string{"composer": "Gilmour"},
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("set the composer: %v", err)
	}

	fields := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRecording {
				return nil, nil
			}
			return &enrich.Candidate{Fields: map[string]string{
				"isrc": "GBAYA7500098", "composer": "Someone Else", "bpm": "128",
			}}, nil
		}}
	if _, err := fieldsService(t, st, fields).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	db := roDB(t, dbPath)
	if got := scalarStr(t, db, "SELECT isrc FROM track"); got != "USRC17607839" {
		t.Errorf("isrc = %q, want the locked value untouched", got)
	}
	if got := scalarStr(t, db, "SELECT composer FROM track"); got != "Gilmour" {
		t.Errorf("composer = %q, want the filled value untouched", got)
	}
	if got := scalarStr(t, db, "SELECT CAST(bpm AS TEXT) FROM track"); got != "128" {
		t.Errorf("bpm = %q, want the one empty field filled", got)
	}
}

// TestTrackFieldsSkipsAMalformedValue: a provider's bad value costs that key alone. The
// pass answers for many items and one unparseable bpm must not abort the rest, and the
// marker still lands so the item is not re-asked every run.
func TestTrackFieldsSkipsAMalformedValue(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	fields := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRecording {
				return nil, nil
			}
			return &enrich.Candidate{Fields: map[string]string{
				"bpm": "one hundred", "composer": "Roger Waters",
			}}, nil
		}}
	res, err := fieldsService(t, st, fields).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TrackFieldsMatched != 1 {
		t.Errorf("matched = %d, want 1: one key survived", res.TrackFieldsMatched)
	}
	db := roDB(t, dbPath)
	if got := scalarStr(t, db, "SELECT composer FROM track"); got != "Roger Waters" {
		t.Errorf("composer = %q, want the good key applied", got)
	}
	if got := scalarStr(t, db, "SELECT COALESCE(CAST(bpm AS TEXT),'') FROM track"); got != "" {
		t.Errorf("bpm = %q, want the malformed value skipped", got)
	}
	if p := scalarStr(t, db,
		"SELECT COALESCE((SELECT provider FROM entity_enrichment WHERE entity_type='fields'),'')"); p != "deezer" {
		t.Errorf("fields marker provider = %q, want the marker written anyway", p)
	}
}

// TestBookFieldsDispatchesCapBookMeta is the second dead slot closed: a CapBookMeta
// provider is asked by the engine now. The dedicated Publisher slot and the generic
// Fields map both land, the identifier is normalized, and a full release date is folded
// to its year rather than failing the parse and burning the marker.
func TestBookFieldsDispatchesCapBookMeta(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	pid := seedBook(t, st, lib.ID, "/lib/b.m4b", "ess-b", "Neuromancer", "William Gibson")

	var reqs []enrich.Request
	books := &enrich.Mock{ProviderName: "audnexus", Caps: enrich.CapBookMeta,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			reqs = append(reqs, req)
			return &enrich.Candidate{
				Publisher: "Ace Books",
				ISBN:      "978-0-441-56956-4",
				Fields: map[string]string{
					"year": "1984-07-01", "narrator": "Robertson Dean",
					"description": "Case was the sharpest data-thief in the Matrix.",
					// Ignored: the author is a book key field.
					"author": "Somebody Else",
				},
			}, nil
		}}
	res, err := fieldsService(t, st, books).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.BookFieldsEnriched != 1 || res.BookFieldsMatched != 1 {
		t.Fatalf("book fields = %d walked / %d matched, want 1 and 1",
			res.BookFieldsEnriched, res.BookFieldsMatched)
	}
	if len(reqs) != 1 || reqs[0].Type != enrich.TargetBook || reqs[0].Want != enrich.CapBookMeta {
		t.Fatalf("provider asked %+v, want one book target under CapBookMeta", reqs)
	}

	db := roDB(t, dbPath)
	if got := scalarStr(t, db, "SELECT publisher FROM book"); got != "Ace Books" {
		t.Errorf("publisher = %q, want the dedicated slot's value", got)
	}
	if got := scalarStr(t, db, "SELECT CAST(year AS TEXT) FROM book"); got != "1984" {
		t.Errorf("year = %q, want a full date folded to its year", got)
	}
	if got := scalarStr(t, db, "SELECT narrator FROM book"); got != "Robertson Dean" {
		t.Errorf("narrator = %q, want the Fields value", got)
	}
	if got := scalarStr(t, db, "SELECT description FROM book"); got == "" {
		t.Error("description = empty, want the Fields value")
	}
	if got := scalarStr(t, db, "SELECT isbn FROM book"); got != "9780441569564" {
		t.Errorf("isbn = %q, want the normalized identifier", got)
	}
	if got := scalarStr(t, db, "SELECT author FROM book"); got != "William Gibson" {
		t.Errorf("author = %q, want the scan's; a book key field is not fillable", got)
	}
	source, provider, _, _ := provenanceRow(t, db, pid, "narrator")
	if source != "enrichment" || provider != "audnexus" {
		t.Errorf("narrator provenance = %s/%s, want enrichment/audnexus", source, provider)
	}
	// The narrator re-resolves a contributor entity, so the rollups have to stay clean.
	assertFieldsVerifyClean(t, st)
}

// TestFieldsPhasesSkippedWithoutACapableProvider: a provider advertising something else
// is never asked under CapFields, and neither phase walks, so a stock install spends
// nothing and writes no markers.
func TestFieldsPhasesSkippedWithoutACapableProvider(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")

	var asked bool
	genres := &enrich.Mock{ProviderName: "lastfm", Caps: enrich.CapGenres,
		EnrichFunc: func(_ context.Context, _ enrich.Request) (*enrich.Candidate, error) {
			asked = true
			return &enrich.Candidate{Fields: map[string]string{"bpm": "128"}}, nil
		}}
	// A CapGenres provider gates no phase of its own, so with no contact beside it the
	// pass has nothing to run at all.
	svc := fieldsService(t, st, genres)
	if svc.Enabled() {
		t.Fatal("a CapGenres-only provider should not enable a contact-less pass")
	}
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err == nil {
		t.Fatal("run with nothing to do should refuse")
	}
	if asked {
		t.Error("the genres provider was asked; it advertises neither fields capability")
	}
	if n := scalarInt(t, roDB(t, dbPath),
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='fields'"); n != 0 {
		t.Errorf("fields markers = %d, want none written", n)
	}
}

// TestFieldsHeartbeatDenominator: the count mirrors the phases that will run, so the
// ratio reaches one. A denominator counting work the run skips never does.
func TestFieldsHeartbeatDenominator(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Band", "Album")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Band", "Album")
	seedBook(t, st, lib.ID, "/lib/b.m4b", "ess-c", "A Book", "An Author")

	fields := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapFields | enrich.CapBookMeta,
		EnrichFunc: func(_ context.Context, _ enrich.Request) (*enrich.Candidate, error) { return nil, nil }}
	var last float64
	res, err := fieldsService(t, st, fields).Run(ctx, enrich.RunOptions{},
		func(p float64, _ string) error { last = p; return nil })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TrackFieldsEnriched != 2 || res.BookFieldsEnriched != 1 {
		t.Fatalf("walked %d tracks / %d books, want 2 and 1",
			res.TrackFieldsEnriched, res.BookFieldsEnriched)
	}
	if last != 1 {
		t.Errorf("final progress = %v, want 1: the denominator counted exactly the work that ran", last)
	}
}

// TestFieldsScopedToOneItem: --item reaches the fields phase, so a user pointing at one
// track re-asks about it rather than waiting for the next full pass.
func TestFieldsScopedToOneItem(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	pid := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Band", "Album")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Band", "Album")

	fields := &enrich.Mock{ProviderName: "deezer", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRecording {
				return nil, nil
			}
			return &enrich.Candidate{Fields: map[string]string{"bpm": "99"}}, nil
		}}
	scope, err := st.EnrichScopeForItem(ctx, pid)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	res, err := fieldsService(t, st, fields).Run(ctx, enrich.RunOptions{Scope: scope}, nil)
	if err != nil {
		t.Fatalf("scoped run: %v", err)
	}
	if res.TrackFieldsEnriched != 1 {
		t.Fatalf("scoped run walked %d tracks, want the one scoped", res.TrackFieldsEnriched)
	}
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM track WHERE bpm = 99"); n != 1 {
		t.Errorf("tracks with the filled bpm = %d, want only the scoped one", n)
	}
}

// The album rung of the fields walk. label is an album column; year participates in the
// album identity key, so it goes through the uniform whole-album edit and is vetoed
// unless every member agrees.

// albumFieldsMock answers only release targets, so a test can run the album walk beside
// the track one without the two mixing.
func albumFieldsMock(t *testing.T, fields map[string]string, seen *[]enrich.Request) *enrich.Mock {
	t.Helper()
	return &enrich.Mock{ProviderName: "discogs", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRelease {
				return nil, nil
			}
			if seen != nil {
				*seen = append(*seen, req)
			}
			return &enrich.Candidate{Fields: fields}, nil
		}}
}

// TestAlbumFieldsFillsLabelAndYear: the label lands on the album row with a curation row
// naming the provider, and the year lands on every member at once through the uniform
// edit, leaving one album row with the pid it had. The release identifiers a provider
// also offered are refused: they are the release matcher's evidence.
func TestAlbumFieldsFillsLabelAndYear(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Pink Floyd", "Wish You Were Here")
	db := roDB(t, dbPath)
	beforePID := scalarStr(t, db, "SELECT pid FROM album")

	var reqs []enrich.Request
	mock := albumFieldsMock(t, map[string]string{
		"label": "Harvest", "year": "1975-09-12",
		// Refused: the release matcher searches by these.
		"barcode": "0075992739429", "media": "12\" Vinyl", "country": "GB",
	}, &reqs)
	res, err := fieldsService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.AlbumFieldsEnriched != 1 || res.AlbumFieldsMatched != 1 {
		t.Fatalf("album fields = %d walked / %d matched, want 1 and 1",
			res.AlbumFieldsEnriched, res.AlbumFieldsMatched)
	}
	if len(reqs) != 1 || reqs[0].Title != "Wish You Were Here" || reqs[0].Artist != "Pink Floyd" {
		t.Fatalf("provider asked %+v, want one release request keyed on the album", reqs)
	}

	if got := scalarStr(t, db, "SELECT COALESCE(label,'') FROM album"); got != "Harvest" {
		t.Errorf("album label = %q, want Harvest", got)
	}
	var source, provider, value string
	err = db.QueryRow(`SELECT source, COALESCE(provider,''), COALESCE(value,'') FROM entity_curation
		WHERE entity_type='album' AND field='label'`).Scan(&source, &provider, &value)
	if err != nil {
		t.Fatalf("label curation row: %v", err)
	}
	if source != "enrichment" || provider != "discogs" || value != "Harvest" {
		t.Errorf("label curation = %s/%s/%q, want enrichment/discogs/Harvest", source, provider, value)
	}
	// The full release date folded to its year rather than failing the parse.
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM track WHERE year = 1975"); n != 2 {
		t.Errorf("members carrying the year = %d, want both", n)
	}
	if got := scalarStr(t, db, "SELECT COALESCE(CAST(year AS TEXT),'') FROM album"); got != "1975" {
		t.Errorf("album.year = %q, want the pre-pass to have rewritten it", got)
	}
	// One album, still the one that was asked about: the uniform edit rewrote the key in
	// place rather than forking a member onto a second album.
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("albums = %d, want the one rewritten in place", n)
	}
	if got := scalarStr(t, db, "SELECT pid FROM album"); got != beforePID {
		t.Errorf("album pid = %q, want the original %q kept", got, beforePID)
	}
	for _, f := range []string{"barcode", "media", "country"} {
		if got := scalarStr(t, db, "SELECT COALESCE("+f+",'') FROM album"); got != "" {
			t.Errorf("album %s = %q, want a provider guess refused there", f, got)
		}
	}
	assertFieldsVerifyClean(t, st)

	again, err := fieldsService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again.AlbumFieldsEnriched != 0 {
		t.Errorf("second run walked %d albums; the marker should have held", again.AlbumFieldsEnriched)
	}
}

// TestAlbumFieldsRespectsLocks: a locked album label is left alone, and a member's locked
// year vetoes the whole year fill (a per-member write would fork the album) while the
// label beside it still lands.
func TestAlbumFieldsRespectsLocks(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Pink Floyd", "Wish You Were Here")
	pidB := seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Pink Floyd", "Wish You Were Here")
	db := roDB(t, dbPath)

	// A locked-empty year on one member. It carries no value, so the vacancy test still
	// passes and only the lock probe can stop the fill.
	if err := st.LockField(ctx, pidB, "year"); err != nil {
		t.Fatalf("lock the year: %v", err)
	}
	mock := albumFieldsMock(t, map[string]string{"label": "Harvest", "year": "1975"}, nil)
	if _, err := fieldsService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := scalarStr(t, db, "SELECT COALESCE(label,'') FROM album"); got != "Harvest" {
		t.Errorf("album label = %q, want the label to land beside a vetoed year", got)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM track WHERE year IS NOT NULL"); n != 0 {
		t.Errorf("members carrying a year = %d, want the fill vetoed by one member's lock", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("albums = %d, want no fork", n)
	}
	assertFieldsVerifyClean(t, st)
}

// TestAlbumFieldsSkipsAlbumWithMemberYears: album.year is written at insert and never
// topped up, so an album can hold a NULL year over members that carry theirs. Filling it
// would rewrite every member's tagged year, so the walk leaves it alone.
func TestAlbumFieldsSkipsAlbumWithMemberYears(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	// An mbid-keyed album: its key ignores the year, so a member's year does not fork it
	// and album.year (written at insert, never topped up) stays NULL over members that
	// carry one. This is the state the veto exists for and the only way to reach it.
	const relMBID = "b1000000-0000-4000-8000-000000000001"
	seedTrackRelease(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Pink Floyd", "Wish You Were Here", relMBID, 0)
	seedTrackRelease(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Pink Floyd", "Wish You Were Here", relMBID, 1975)
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("albums = %d, want the one the mbid key held together", n)
	}
	if got := scalarStr(t, db, "SELECT COALESCE(CAST(year AS TEXT),'') FROM album"); got != "" {
		t.Fatalf("album.year = %q, want NULL: the fixture did not reach the state under test", got)
	}
	before := scalarStr(t, db, "SELECT COALESCE(CAST(year AS TEXT),'') FROM track WHERE year IS NOT NULL")

	mock := albumFieldsMock(t, map[string]string{"year": "1999"}, nil)
	if _, err := fieldsService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := scalarStr(t, db, "SELECT COALESCE(CAST(year AS TEXT),'') FROM track WHERE year IS NOT NULL"); got != before {
		t.Errorf("member year = %q, want the tagged %q left alone", got, before)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM track WHERE year = 1999"); n != 0 {
		t.Errorf("members took the provider year on %d tracks, want none", n)
	}
	assertFieldsVerifyClean(t, st)
}

// TestAlbumFieldsMergesOntoATakenKey: another album already holds the key this fill would
// move onto, so the rename pre-pass folds this album into the incumbent and the row is
// gone. Nothing is written on the dead rowid, and the incumbent is asked on its own.
func TestAlbumFieldsMergesOntoATakenKey(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	// Two albums with the same title and artist, separated only by their year.
	seedTrackYear(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Pink Floyd", "Animals", 1977)
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Pink Floyd", "Animals")
	db := roDB(t, dbPath)
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("albums = %d, want the two the year separated", n)
	}
	incumbent := scalarStr(t, db, "SELECT pid FROM album WHERE year = 1977")

	mock := albumFieldsMock(t, map[string]string{"year": "1977"}, nil)
	if _, err := fieldsService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("albums = %d, want the year-less one folded into the incumbent", n)
	}
	if got := scalarStr(t, db, "SELECT pid FROM album"); got != incumbent {
		t.Errorf("surviving album pid = %q, want the incumbent %q", got, incumbent)
	}
	// No marker stranded on the rowid the merge freed.
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_enrichment ee
		WHERE ee.entity_type = 'fields_album'
		  AND NOT EXISTS (SELECT 1 FROM album al WHERE al.id = ee.entity_id)`); n != 0 {
		t.Errorf("stranded fields_album markers = %d, want none on a dead rowid", n)
	}
	assertFieldsVerifyClean(t, st)
}

// TestAlbumFieldsYearRevertsOnRescan pins what the year fill does NOT survive. Only
// locked fields are overlaid by a scan, so a forced rescan without write-back reverts
// each member's year and the heuristic key with it, while the album keeps its pid through
// the reconcile path. label is the other half: the scan's top-up is fill-when-empty and
// never clears it, so that one does survive.
func TestAlbumFieldsYearRevertsOnRescan(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Pink Floyd", "Wish You Were Here")
	db := roDB(t, dbPath)

	mock := albumFieldsMock(t, map[string]string{"label": "Harvest", "year": "1975"}, nil)
	if _, err := fieldsService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	albumPID := scalarStr(t, db, "SELECT pid FROM album")
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM track WHERE year = 1975"); n != 2 {
		t.Fatalf("members carrying the year = %d, want both before the rescan", n)
	}

	// The same files scanned again, still tagged with no year: the scan overlays only
	// locked fields, so each member's year goes back to the tag's.
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Pink Floyd", "Wish You Were Here")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Pink Floyd", "Wish You Were Here")
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM track WHERE year IS NOT NULL"); n != 0 {
		t.Errorf("members still carrying a year after a rescan = %d, want 0 without write-back", n)
	}
	if got := scalarStr(t, db, "SELECT COALESCE(label,'') FROM album"); got != "Harvest" {
		t.Errorf("album label = %q, want it to survive the rescan", got)
	}
	if got := scalarStr(t, db, "SELECT pid FROM album"); got != albumPID {
		t.Errorf("album pid = %q, want %q kept through the scan reconcile", got, albumPID)
	}
	assertFieldsVerifyClean(t, st)
}

// TestAlbumFieldsScopedToOneEntity: --entity album:<pid> reaches the album fields phase.
func TestAlbumFieldsScopedToOneEntity(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Band A", "Album A")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Band B", "Album B")
	db := roDB(t, dbPath)
	target := model.PID(scalarStr(t, db, "SELECT pid FROM album WHERE title = 'Album A'"))

	scope, err := st.EnrichScopeForEntity(ctx, read.EntityAlbum, target)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	mock := albumFieldsMock(t, map[string]string{"label": "Harvest"}, nil)
	res, err := fieldsService(t, st, mock).Run(ctx, enrich.RunOptions{Scope: scope}, nil)
	if err != nil {
		t.Fatalf("scoped run: %v", err)
	}
	if res.AlbumFieldsEnriched != 1 {
		t.Fatalf("scoped run walked %d albums, want the one scoped", res.AlbumFieldsEnriched)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM album WHERE label = 'Harvest'"); n != 1 {
		t.Errorf("albums carrying the label = %d, want only the scoped one", n)
	}
}

// seedTrackYear is seedTrack with a tagged year, which is what separates two albums that
// otherwise share a title and an artist.
func seedTrackYear(t *testing.T, st *sqlite.Store, libID int64, path, essence, title, artist, album string, year int) model.PID {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1, DurationMS: 300000,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{Artist: artist, AlbumArtist: artist, Album: album, TrackNo: 1, Year: year},
	})
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	return res.ItemPID
}

// seedTrackRelease is seedTrack with an album release MBID, which makes the album key
// mbid-based and therefore blind to the year.
func seedTrackRelease(t *testing.T, st *sqlite.Store, libID int64, path, essence, title, artist, album, relMBID string, year int) model.PID {
	t.Helper()
	res, err := st.PutScannedTrack(context.Background(), model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: 100, MTimeNS: 1, DurationMS: 300000,
			ContentHash: "c-" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:" + essence,
		},
		Track: model.Track{
			Artist: artist, AlbumArtist: artist, Album: album, TrackNo: 1,
			Year: year, MBReleaseID: relMBID,
		},
	})
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	return res.ItemPID
}

// TestAlbumFieldsLabelSurvivesTheYearMerge: the year is the one fill that can move an
// album onto a key another album already holds, which merges this row away. A label
// written before that would go down with the row, since the column is on the album and
// the merge does not carry it over. So the year runs first and the label is skipped
// outright when the row is gone, leaving the survivor to be asked on its own terms rather
// than silently losing the value to a deleted row.
func TestAlbumFieldsLabelSurvivesTheYearMerge(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrackYear(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Pink Floyd", "Animals", 1977)
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Pink Floyd", "Animals")
	db := roDB(t, dbPath)
	incumbent := scalarStr(t, db, "SELECT pid FROM album WHERE year = 1977")

	mock := albumFieldsMock(t, map[string]string{"year": "1977", "label": "Harvest"}, nil)
	if _, err := fieldsService(t, st, mock).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("albums = %d, want the year-less one folded into the incumbent", n)
	}
	if got := scalarStr(t, db, "SELECT pid FROM album"); got != incumbent {
		t.Fatalf("surviving album = %q, want the incumbent %q", got, incumbent)
	}
	// No curation row stranded on the rowid the merge freed, which is what a label
	// written before the merge would have left behind.
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM entity_curation ec
		WHERE ec.entity_type = 'album'
		  AND NOT EXISTS (SELECT 1 FROM album al WHERE al.id = ec.entity_id)`); n != 0 {
		t.Errorf("stranded album curation rows = %d, want none on a dead rowid", n)
	}
	assertFieldsVerifyClean(t, st)
}

// TestAlbumFieldsStampsEachProviderSeparately: the album's label curation row names the
// provider that supplied the label, not whichever provider answered first overall.
func TestAlbumFieldsStampsEachProviderSeparately(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Pink Floyd", "Animals")

	// The first provider answers the year alone; the second supplies the label.
	years := &enrich.Mock{ProviderName: "musicbrainz-ish", Caps: enrich.CapFields,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			if req.Type != enrich.TargetRelease {
				return nil, nil
			}
			return &enrich.Candidate{Fields: map[string]string{"year": "1977"}}, nil
		}}
	labels := albumFieldsMock(t, map[string]string{"label": "Harvest"}, nil)
	if _, err := fieldsService(t, st, years, labels).Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	db := roDB(t, dbPath)
	got := scalarStr(t, db, `SELECT COALESCE(provider,'') FROM entity_curation
		WHERE entity_type='album' AND field='label'`)
	if got != "discogs" {
		t.Errorf("label curation provider = %q, want the provider that supplied the label", got)
	}
}
