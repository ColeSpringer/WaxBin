package sqlite_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

func TestAcquisitionRoundTripAndSourceSurfacing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	// A locally-scanned track has no acquisition row: it reads back source "local".
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Local Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if _, err := st.AcquisitionByItem(ctx, res.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("AcquisitionByItem on a scanned item = %v, want CodeNotFound", err)
	}
	v, err := st.ItemByPID(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("ItemByPID: %v", err)
	}
	if v.Source != model.SourceLocal {
		t.Fatalf("scanned item source = %q, want local", v.Source)
	}

	// Stamp acquisition by the file's path (the import path) and read it back.
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/a.mp3"), model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://y/watch?v=1", SourceID: "1", Provider: "waxtap",
	}, res.Made); err != nil {
		t.Fatalf("PutAcquisitionForFile: %v", err)
	}
	acq, err := st.AcquisitionByItem(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("AcquisitionByItem: %v", err)
	}
	if acq.SourceType != model.SourceYouTube || acq.SourceURL != "https://y/watch?v=1" || acq.Provider != "waxtap" {
		t.Fatalf("acquisition = %+v", acq)
	}
	if acq.AcquiredAt == 0 {
		t.Error("acquired_at was not stamped")
	}

	// The item view now surfaces the acquired source, and a source filter finds it.
	v, _ = st.ItemByPID(ctx, res.ItemPID)
	if v.Source != model.SourceYouTube {
		t.Fatalf("acquired item source = %q, want youtube", v.Source)
	}
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("source", query.OpIs, string(model.SourceYouTube)).Build(), "")
	if err != nil {
		t.Fatalf("query by source: %v", err)
	}
	if len(items) != 1 || items[0].PID != res.ItemPID {
		t.Fatalf("source filter returned %d items, want the acquired one", len(items))
	}
	// The local-source filter excludes it.
	locals, _ := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("source", query.OpIs, string(model.SourceLocal)).Build(), "")
	for _, it := range locals {
		if it.PID == res.ItemPID {
			t.Fatal("acquired item leaked into the local-source filter")
		}
	}
}

func TestLibraryMediaPersistence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := openTestStore(t)
	if _, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/books"), DisplayRoot: "/books", Mode: model.ModeManaged,
		Media: model.MediaAudiobook, Profile: "waxbin-native",
	}); err != nil {
		t.Fatalf("EnsureLibrary: %v", err)
	}
	libs, err := st.Libraries(ctx)
	if err != nil {
		t.Fatalf("Libraries: %v", err)
	}
	var books *model.Library
	for _, l := range libs {
		if l.DisplayRoot == "/books" {
			books = l
		}
	}
	if books == nil || books.Media != model.MediaAudiobook {
		t.Fatalf("audiobook library media = %v", books)
	}
	// The default-seeded /lib library (created with no Media) reads back mixed.
	for _, l := range libs {
		if l.DisplayRoot == "/lib" && l.MediaType() != model.MediaMixed {
			t.Fatalf("default library media = %q, want mixed", l.MediaType())
		}
	}
}

// TestAcquisitionFromTagsAttributesScannedItem covers D's headline: a scanned file
// carrying SOURCE_URL/SOURCE_ID/ACQUISITION_DATE is evidence of external origin, so
// it gets an acquisition row and stops reading as source:local.
func TestAcquisitionFromTagsAttributesScannedItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	// 2019-05-03T00:00:00Z in unix nanoseconds.
	const acquired2019 = int64(1556841600) * int64(1e9)

	in := input(lib.ID, "/lib/dl.mp3", "ess-dl", "c-dl", "Downloaded Song")
	in.Acquisition = model.TagAcquisition{
		SourceURL:  "https://example.test/track/9",
		SourceID:   "9",
		AcquiredAt: acquired2019,
	}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}

	acq, err := st.AcquisitionByItem(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("AcquisitionByItem: %v", err)
	}
	// manual, not a newly invented 'tagged' type: the tags evidence external origin
	// but say nothing about the mechanism.
	if acq.SourceType != model.SourceManual {
		t.Errorf("source type = %q, want manual", acq.SourceType)
	}
	if acq.SourceURL != "https://example.test/track/9" || acq.SourceID != "9" {
		t.Errorf("acquisition = %+v", acq)
	}
	if acq.Provider != "" {
		t.Errorf("provider = %q, want empty: tags say nothing about the mechanism", acq.Provider)
	}
	if acq.AcquiredAt != acquired2019 {
		t.Errorf("acquiredAt = %d, want the tag's 2019 date %d, not scan time", acq.AcquiredAt, acquired2019)
	}
	v, err := st.ItemByPID(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("ItemByPID: %v", err)
	}
	if v.Source != model.SourceManual {
		t.Errorf("item source = %q, want manual: it is no longer local", v.Source)
	}
}

// TestAcquisitionFromTagsRequiresURLOrID pins Present(): a bare ACQUISITION_DATE is
// not a claim of external origin (a local rip can carry one), so it alone must not
// flip an item off source:local.
func TestAcquisitionFromTagsRequiresURLOrID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	in := input(lib.ID, "/lib/rip.mp3", "ess-rip", "c-rip", "Local Rip")
	in.Acquisition = model.TagAcquisition{AcquiredAt: int64(1556841600) * int64(1e9)}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if _, err := st.AcquisitionByItem(ctx, res.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("AcquisitionByItem = %v, want CodeNotFound: a bare date is not external origin", err)
	}
	v, err := st.ItemByPID(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("ItemByPID: %v", err)
	}
	if v.Source != model.SourceLocal {
		t.Errorf("item source = %q, want local", v.Source)
	}
}

// TestAcquisitionFromTagsNeverClobbersEvent is the test that justifies DO NOTHING.
// A tag is copyable and is re-derived on every full scan, so without it one rescan of
// a downloaded episode would overwrite its real source_type='rss' and provider with a
// bare 'manual', destroying the authoritative record of how the item arrived.
func TestAcquisitionFromTagsNeverClobbersEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/ep.mp3", "ess-ep", "c-ep", "Episode"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	// The authoritative record: an acquisition WaxBin actually performed.
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{
		SourceType: model.SourceRSS, SourceURL: "https://feed.test/ep9.mp3",
		SourceID: "guid-9", Provider: "rss", ProviderVersion: "1",
	}); err != nil {
		t.Fatalf("PutAcquisition: %v", err)
	}
	before, err := st.AcquisitionByItem(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("AcquisitionByItem: %v", err)
	}

	// Now a full rescan of the same file, whose tags evidence a plain external origin.
	in := input(lib.ID, "/lib/ep.mp3", "ess-ep", "c-ep", "Episode")
	in.Acquisition = model.TagAcquisition{SourceURL: "https://elsewhere.test/x", SourceID: "x"}
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	after, err := st.AcquisitionByItem(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("AcquisitionByItem after rescan: %v", err)
	}
	if after.SourceType != model.SourceRSS {
		t.Errorf("source type = %q after rescan, want rss preserved: an event outranks a tag", after.SourceType)
	}
	if after.Provider != "rss" || after.SourceID != "guid-9" || after.SourceURL != before.SourceURL {
		t.Errorf("tag-derived rescan clobbered the event record: %+v", after)
	}
	if after.AcquiredAt != before.AcquiredAt {
		t.Errorf("acquiredAt moved: %d -> %d", before.AcquiredAt, after.AcquiredAt)
	}
}

// TestAcquisitionReRecordIsMergeWise covers the ON CONFLICT branch, which stood
// untested while it clobbered every column unconditionally. A field an event does not
// name keeps what stands, so a bare event can neither erase what a tag established nor
// downgrade a real acquisition to manual.
func TestAcquisitionReRecordIsMergeWise(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	// A tag-derived row: url and id, mechanism unstated, so type is manual.
	in := input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Tagged Song")
	in.Acquisition = model.TagAcquisition{SourceURL: "https://tagged.test/x", SourceID: "tag-1"}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	first, err := st.AcquisitionByItem(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("AcquisitionByItem: %v", err)
	}
	if first.SourceURL != "https://tagged.test/x" || first.SourceType != model.SourceManual {
		t.Fatalf("tag-derived row = %+v", first)
	}

	// A bare event: it names no url, no id and no mechanism. Everything survives, and
	// the original acquisition time with it.
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{}); err != nil {
		t.Fatalf("bare PutAcquisition: %v", err)
	}
	after, err := st.AcquisitionByItem(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("AcquisitionByItem: %v", err)
	}
	if after.SourceURL != "https://tagged.test/x" || after.SourceID != "tag-1" {
		t.Errorf("bare event erased tag-derived evidence: %+v", after)
	}
	if after.AcquiredAt != first.AcquiredAt {
		t.Errorf("acquired_at = %d, want the first stamp %d", after.AcquiredAt, first.AcquiredAt)
	}

	// A real acquisition replaces field for field.
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{
		SourceType: model.SourceRSS, SourceURL: "https://feed.test/ep", SourceID: "ep-9",
		Provider: "rss", ProviderVersion: "1.2", OptionsJSON: `{"q":"best"}`,
	}); err != nil {
		t.Fatalf("full PutAcquisition: %v", err)
	}
	full, _ := st.AcquisitionByItem(ctx, res.ItemPID)
	if full.SourceType != model.SourceRSS || full.SourceURL != "https://feed.test/ep" ||
		full.SourceID != "ep-9" || full.Provider != "rss" || full.ProviderVersion != "1.2" ||
		full.OptionsJSON != `{"q":"best"}` {
		t.Fatalf("full event did not replace field for field: %+v", full)
	}

	// A bare event over the rss row does not downgrade it.
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{}); err != nil {
		t.Fatalf("bare PutAcquisition over rss: %v", err)
	}
	kept, _ := st.AcquisitionByItem(ctx, res.ItemPID)
	if kept.SourceType != model.SourceRSS || kept.Provider != "rss" {
		t.Errorf("bare event downgraded a real acquisition: %+v", kept)
	}

	// An explicitly passed manual is a claim about the mechanism, so it does replace.
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{
		SourceType: model.SourceManual,
	}); err != nil {
		t.Fatalf("manual PutAcquisition: %v", err)
	}
	manual, _ := st.AcquisitionByItem(ctx, res.ItemPID)
	if manual.SourceType != model.SourceManual {
		t.Errorf("source type = %q, want an explicit manual to win", manual.SourceType)
	}
	if manual.SourceURL != "https://feed.test/ep" {
		t.Errorf("an explicit type also cleared the url: %+v", manual)
	}
}

// TestClearAcquisitionReturnsItemToLocal pins the inverse the merge-wise upsert needs:
// nothing else can lower a field, so removing the row is the only correction downward.
func TestClearAcquisitionReturnsItemToLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://y/watch?v=1", Provider: "waxtap",
	}); err != nil {
		t.Fatalf("PutAcquisition: %v", err)
	}
	// LockOff keeps this test about the delete: the default lock has its own test.
	if err := st.ClearAcquisition(ctx, res.ItemPID, model.LockOff, false); err != nil {
		t.Fatalf("ClearAcquisition: %v", err)
	}
	if _, err := st.AcquisitionByItem(ctx, res.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("AcquisitionByItem after clear = %v, want CodeNotFound", err)
	}
	v, err := st.ItemByPID(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("ItemByPID: %v", err)
	}
	if v.Source != model.SourceLocal {
		t.Errorf("source after clear = %q, want local", v.Source)
	}
	// Clearing again is a no-op rather than an error, so a batch correction need not
	// check first.
	if err := st.ClearAcquisition(ctx, res.ItemPID, model.LockOff, false); err != nil {
		t.Errorf("second ClearAcquisition: %v", err)
	}
}

// TestAcquisitionSourceIDFollowsProvider: source_id is a provider-native id, so it means
// nothing under a different provider's name. An event that switches the provider without
// naming an id of its own drops the standing one rather than mislabelling it.
func TestAcquisitionSourceIDFollowsProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{
		SourceType: model.SourceRSS, SourceURL: "https://feed.test/ep", SourceID: "ep-9", Provider: "rss",
	}); err != nil {
		t.Fatalf("PutAcquisition: %v", err)
	}
	// A different provider, no id: the rss id would be a lie under it.
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{Provider: "waxtap"}); err != nil {
		t.Fatalf("provider switch: %v", err)
	}
	a, _ := st.AcquisitionByItem(ctx, res.ItemPID)
	if a.Provider != "waxtap" || a.SourceID != "" {
		t.Errorf("acquisition = %+v, want provider waxtap with the rss id dropped", a)
	}
	if a.SourceURL != "https://feed.test/ep" || a.SourceType != model.SourceRSS {
		t.Errorf("acquisition = %+v, want the url and type still standing", a)
	}
	// The same provider re-recording keeps its own id.
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{
		SourceID: "tap-1", Provider: "waxtap",
	}); err != nil {
		t.Fatalf("same-provider re-record: %v", err)
	}
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{Provider: "waxtap"}); err != nil {
		t.Fatalf("bare same-provider event: %v", err)
	}
	if a, _ := st.AcquisitionByItem(ctx, res.ItemPID); a.SourceID != "tap-1" {
		t.Errorf("source id = %q, want the same provider's id kept", a.SourceID)
	}
}

// TestTagDerivedSourceIDSurvivesAProvider: a tag-derived row names no provider, so a later
// event that names one is not contradicting anything and the tag's id stands.
func TestTagDerivedSourceIDSurvivesAProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	in := input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Tagged")
	in.Acquisition = model.TagAcquisition{SourceURL: "https://tagged.test/x", SourceID: "tag-1"}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{Provider: "waxtap"}); err != nil {
		t.Fatalf("PutAcquisition: %v", err)
	}
	if a, _ := st.AcquisitionByItem(ctx, res.ItemPID); a.SourceID != "tag-1" {
		t.Errorf("source id = %q, want the tag-derived id kept", a.SourceID)
	}
}

// TestAcquisitionLockAppliesToEveryKind pins acquisition's place in the lock
// vocabulary. It is the first static curation field that covers episodes, so all three
// kinds are checked, and a lock the plain command set carries no attribution, so an
// unlock drops it through setLock's ordinary tag-sourced sparse delete.
func TestAcquisitionLockAppliesToEveryKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	track, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	book, err := st.PutScannedBook(ctx, bookIn(lib.ID, "/lib/b.m4b", "ess-b", "Book", "Author X", "", "", ""))
	if err != nil {
		t.Fatalf("PutScannedBook: %v", err)
	}
	if _, err := st.UpsertFeed(ctx, oneEpisodeFeed("https://feed.test/f", "g-1", "Ep", "d", "l")); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	ep := episodePID(t, st)

	for name, pid := range map[string]model.PID{"track": track.ItemPID, "book": book.ItemPID, "episode": ep} {
		if err := st.LockField(ctx, pid, "acquisition"); err != nil {
			t.Fatalf("LockField on a %s: %v", name, err)
		}
		if locked, err := st.IsFieldLocked(ctx, pid, "acquisition"); err != nil || !locked {
			t.Fatalf("%s acquisition locked = %v (err %v), want true", name, locked, err)
		}
		if err := st.UnlockField(ctx, pid, "acquisition"); err != nil {
			t.Fatalf("UnlockField on a %s: %v", name, err)
		}
		fields, err := st.FieldProvenance(ctx, pid)
		if err != nil {
			t.Fatalf("FieldProvenance for a %s: %v", name, err)
		}
		for _, f := range fields {
			if f.Field == "acquisition" {
				t.Errorf("%s kept an inert acquisition row after unlock: %+v", name, f)
			}
		}
	}
}

// TestAcquisitionLockSurvivesRescan is the durability case the whole lock exists for.
// The file still carries SOURCE_URL, and insertAcquisitionIfAbsentTx only ever protected
// a row that exists, so without the lock a cleared origin comes straight back.
func TestAcquisitionLockSurvivesRescan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	in := input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Tagged")
	in.Acquisition = model.TagAcquisition{SourceURL: "https://wrong.test/x", SourceID: "x"}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if _, err := st.AcquisitionByItem(ctx, res.ItemPID); err != nil {
		t.Fatalf("the tag-derived row should exist first: %v", err)
	}
	// Locked through the plain lock command rather than the clear's default, so this
	// pins the scan gate rather than the clear's lock rule.
	if err := st.ClearAcquisition(ctx, res.ItemPID, model.LockOff, false); err != nil {
		t.Fatalf("ClearAcquisition: %v", err)
	}
	if err := st.LockField(ctx, res.ItemPID, "acquisition"); err != nil {
		t.Fatalf("LockField: %v", err)
	}

	rescan := input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Tagged")
	rescan.Acquisition = model.TagAcquisition{SourceURL: "https://wrong.test/x", SourceID: "x"}
	rescan.PreserveLocks = true
	if _, err := st.PutScannedTrack(ctx, rescan); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if _, err := st.AcquisitionByItem(ctx, res.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		a, _ := st.AcquisitionByItem(ctx, res.ItemPID)
		t.Fatalf("rescan re-derived a locked-away origin: %+v (err %v)", a, err)
	}

	// scan --force --ignore-locks drops PreserveLocks, and the acquisition lock has to
	// give way with every other one rather than outliving them.
	ignore := input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Tagged")
	ignore.Acquisition = model.TagAcquisition{SourceURL: "https://wrong.test/x", SourceID: "x"}
	if _, err := st.PutScannedTrack(ctx, ignore); err != nil {
		t.Fatalf("ignore-locks rescan: %v", err)
	}
	if _, err := st.AcquisitionByItem(ctx, res.ItemPID); err != nil {
		t.Fatalf("ignore-locks rescan honoured the lock: %v", err)
	}
}

// TestPutAcquisitionSkipsLockedItem: the automatic writers skip in silence, the way
// attachArtRespectingLockTx does, rather than refusing an import outright.
func TestPutAcquisitionSkipsLockedItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{
		SourceType: model.SourceManual, SourceURL: "https://right.test/x",
	}); err != nil {
		t.Fatalf("PutAcquisition: %v", err)
	}
	if err := st.LockField(ctx, res.ItemPID, "acquisition"); err != nil {
		t.Fatalf("LockField: %v", err)
	}
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://wrong.test/y", Provider: "waxtap",
	}); err != nil {
		t.Fatalf("PutAcquisition over a lock should be a silent no-op: %v", err)
	}
	a, err := st.AcquisitionByItem(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("AcquisitionByItem: %v", err)
	}
	if a.SourceType != model.SourceManual || a.SourceURL != "https://right.test/x" || a.Provider != "" {
		t.Errorf("an automatic event wrote over a locked row: %+v", a)
	}
}

// TestSetAcquisitionIsAuthoritative pins the difference from the merge-wise recording
// path: every column is written as given, so a correction can empty a field that a bare
// event could never lower.
func TestSetAcquisitionIsAuthoritative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.PutAcquisition(ctx, res.ItemPID, model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://y/watch?v=1", SourceID: "1",
		Provider: "waxtap", ProviderVersion: "3", OptionsJSON: `{"q":"best"}`,
	}); err != nil {
		t.Fatalf("PutAcquisition: %v", err)
	}
	before, err := st.AcquisitionByItem(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("AcquisitionByItem: %v", err)
	}

	// The correction: a mis-tagged rip is manual with nothing else known.
	if err := st.SetAcquisition(ctx, res.ItemPID,
		model.AcquisitionInput{SourceType: model.SourceManual}, model.LockUnchanged, false); err != nil {
		t.Fatalf("SetAcquisition: %v", err)
	}
	a, err := st.AcquisitionByItem(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("AcquisitionByItem after set: %v", err)
	}
	if a.SourceType != model.SourceManual {
		t.Errorf("source type = %q, want manual", a.SourceType)
	}
	if a.SourceURL != "" || a.SourceID != "" || a.Provider != "" || a.ProviderVersion != "" || a.OptionsJSON != "" {
		t.Errorf("an authoritative set left fields standing: %+v", a)
	}
	if a.AcquiredAt != before.AcquiredAt {
		t.Errorf("acquired at = %d, want the stamp preserved by a zero AcquiredAt", a.AcquiredAt)
	}

	// A non-zero stamp moves it, which putAcquisitionTx can never do.
	if err := st.SetAcquisition(ctx, res.ItemPID,
		model.AcquisitionInput{SourceType: model.SourceManual, AcquiredAt: 1_700_000_000_000_000_000},
		model.LockUnchanged, false); err != nil {
		t.Fatalf("SetAcquisition with a stamp: %v", err)
	}
	if a, _ := st.AcquisitionByItem(ctx, res.ItemPID); a.AcquiredAt != 1_700_000_000_000_000_000 {
		t.Errorf("acquired at = %d, want the given stamp", a.AcquiredAt)
	}

	// SetAcquisition leaves the lock alone by default, the way every other curation verb
	// treats LockUnchanged.
	if locked, _ := st.IsFieldLocked(ctx, res.ItemPID, "acquisition"); locked {
		t.Error("a bare set locked the field; only a clear does that")
	}
}

// TestSetAcquisitionRefusesLocalAndUnknown: local is the absence of a row, so asking
// for it is asking for a clear.
func TestSetAcquisitionRefusesLocalAndUnknown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	for _, st2 := range []model.SourceType{model.SourceLocal, "", "bandcamp"} {
		err := st.SetAcquisition(ctx, res.ItemPID,
			model.AcquisitionInput{SourceType: st2}, model.LockUnchanged, false)
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("SetAcquisition with type %q = %v, want CodeInvalid", st2, err)
		}
	}
}

// TestAcquisitionCurationRefusesALock: the curation pair refuses out loud where the
// automatic writers skip in silence, and force is the way through.
func TestAcquisitionCurationRefusesALock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.SetAcquisition(ctx, res.ItemPID,
		model.AcquisitionInput{SourceType: model.SourceManual, SourceURL: "https://one.test"},
		model.LockOn, false); err != nil {
		t.Fatalf("SetAcquisition: %v", err)
	}
	if err := st.SetAcquisition(ctx, res.ItemPID,
		model.AcquisitionInput{SourceType: model.SourceRSS}, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("locked set = %v, want CodeLocked", err)
	}
	if err := st.ClearAcquisition(ctx, res.ItemPID, model.LockUnchanged, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("locked clear = %v, want CodeLocked", err)
	}
	if err := st.SetAcquisition(ctx, res.ItemPID,
		model.AcquisitionInput{SourceType: model.SourceRSS}, model.LockUnchanged, true); err != nil {
		t.Fatalf("forced set: %v", err)
	}
	if a, _ := st.AcquisitionByItem(ctx, res.ItemPID); a.SourceType != model.SourceRSS {
		t.Errorf("forced set did not apply: %+v", a)
	}
	// The force overrode the lock without clearing it.
	if locked, _ := st.IsFieldLocked(ctx, res.ItemPID, "acquisition"); !locked {
		t.Error("a forced set dropped the lock; LockUnchanged leaves it standing")
	}
}

// TestClearAcquisitionLocksByDefault is the asymmetry that makes a clear stick: the row
// it removes is one the next scan would re-derive from the file's own tags.
func TestClearAcquisitionLocksByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	tagged := func(path, essence string) model.PutScannedTrackInput {
		in := input(lib.ID, path, essence, "c-"+essence, "Tagged")
		in.Acquisition = model.TagAcquisition{SourceURL: "https://wrong.test/x", SourceID: "x"}
		in.PreserveLocks = true
		return in
	}
	res, err := st.PutScannedTrack(ctx, tagged("/lib/a.mp3", "ess-a"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.ClearAcquisition(ctx, res.ItemPID, model.LockUnchanged, false); err != nil {
		t.Fatalf("ClearAcquisition: %v", err)
	}
	if locked, _ := st.IsFieldLocked(ctx, res.ItemPID, "acquisition"); !locked {
		t.Fatal("a bare clear did not lock; without the lock it does not survive a rescan")
	}
	if _, err := st.PutScannedTrack(ctx, tagged("/lib/a.mp3", "ess-a")); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if _, err := st.AcquisitionByItem(ctx, res.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("the cleared origin came back across a rescan: %v", err)
	}

	// The explicit opt-out leaves the file's evidence free to re-derive.
	other, err := st.PutScannedTrack(ctx, tagged("/lib/b.mp3", "ess-b"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.ClearAcquisition(ctx, other.ItemPID, model.LockOff, false); err != nil {
		t.Fatalf("unlocking clear: %v", err)
	}
	if locked, _ := st.IsFieldLocked(ctx, other.ItemPID, "acquisition"); locked {
		t.Error("an explicit LockOff clear locked anyway")
	}
	if _, err := st.PutScannedTrack(ctx, tagged("/lib/b.mp3", "ess-b")); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if _, err := st.AcquisitionByItem(ctx, other.ItemPID); err != nil {
		t.Errorf("an unlocked clear should let the tags re-derive: %v", err)
	}
}

// TestClearAcquisitionOfNothingEmitsNoDelta: a clear that removed no row and left the
// lock where it stood did nothing, so it must not publish a change.
func TestClearAcquisitionOfNothingEmitsNoDelta(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	before, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("LatestChangeSeq: %v", err)
	}
	if err := st.ClearAcquisition(ctx, res.ItemPID, model.LockOff, false); err != nil {
		t.Fatalf("ClearAcquisition: %v", err)
	}
	after, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("LatestChangeSeq: %v", err)
	}
	if after != before {
		t.Errorf("change seq moved from %d to %d for a clear that did nothing", before, after)
	}
	// A clear that only sets the lock still publishes: the item's origin now reads
	// differently to a delta-sync consumer, because it will not come back.
	if err := st.ClearAcquisition(ctx, res.ItemPID, model.LockUnchanged, false); err != nil {
		t.Fatalf("locking clear: %v", err)
	}
	if seq, _ := st.LatestChangeSeq(ctx); seq == after {
		t.Error("a clear that set the lock published no delta")
	}
}

// TestSetAcquisitionOverridesAnEpisodeShowSource: an episode already reads its show's
// type through the source COALESCE, so its own acquisition row is the only thing that
// overrides it, and the lock is what keeps that override across a re-sync.
func TestSetAcquisitionOverridesAnEpisodeShowSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := openTestStore(t)
	if _, err := st.UpsertFeed(ctx, oneEpisodeFeed("https://feed.test/f", "g-1", "Ep", "d", "l")); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	ep := episodePID(t, st)
	v, err := st.ItemByPID(ctx, ep)
	if err != nil {
		t.Fatalf("ItemByPID: %v", err)
	}
	if v.Source != model.SourceRSS {
		t.Fatalf("episode source = %q with no row of its own, want the show's rss", v.Source)
	}

	if err := st.SetAcquisition(ctx, ep,
		model.AcquisitionInput{SourceType: model.SourceManual}, model.LockOn, false); err != nil {
		t.Fatalf("SetAcquisition on an episode: %v", err)
	}
	if v, _ := st.ItemByPID(ctx, ep); v.Source != model.SourceManual {
		t.Errorf("episode source = %q after a curated set, want manual", v.Source)
	}

	// Clearing returns it to the show's type, not to local.
	if err := st.ClearAcquisition(ctx, ep, model.LockUnchanged, true); err != nil {
		t.Fatalf("ClearAcquisition on an episode: %v", err)
	}
	if v, _ := st.ItemByPID(ctx, ep); v.Source != model.SourceRSS {
		t.Errorf("episode source = %q after a clear, want the show's rss rather than local", v.Source)
	}
}

// TestUnlockKeepsCuratedAcquisitionProvenance: acquisition is not in setLock's
// sparse-delete exception, and must not be. The exception exists for art, whose real
// attribution is re-reported from art_map, so dropping the row loses nothing. An
// acquisition row's "this origin was hand-set" has no such overlay, and the origin it
// describes is still standing, so an unlock must leave the attribution behind.
func TestUnlockKeepsCuratedAcquisitionProvenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	res, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Song"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	if err := st.SetAcquisition(ctx, res.ItemPID,
		model.AcquisitionInput{SourceType: model.SourceManual, SourceURL: "https://right.test/y"},
		model.LockOn, false); err != nil {
		t.Fatalf("SetAcquisition: %v", err)
	}
	if err := st.UnlockField(ctx, res.ItemPID, "acquisition"); err != nil {
		t.Fatalf("UnlockField: %v", err)
	}
	rows, err := st.FieldProvenance(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("FieldProvenance: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Field == "acquisition" {
			found = true
			if r.Locked || r.Source != model.SourceUser {
				t.Errorf("acquisition provenance = %+v, want an unlocked user row", r)
			}
		}
	}
	if !found {
		t.Error("unlocking dropped the record that the origin was hand-set, which nothing else reports")
	}
	// The row it describes is still there, which is why the attribution is not inert.
	if _, err := st.AcquisitionByItem(ctx, res.ItemPID); err != nil {
		t.Errorf("the curated acquisition row went with the lock: %v", err)
	}
}

// TestClearAcquisitionIsIdempotent: the default lock must not make the verb refuse its
// own second run, which is what a batch correction over a mixed selection does.
func TestClearAcquisitionIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)

	in := input(lib.ID, "/lib/a.mp3", "ess-a", "c-a", "Tagged")
	in.Acquisition = model.TagAcquisition{SourceURL: "https://wrong.test/x", SourceID: "x"}
	res, err := st.PutScannedTrack(ctx, in)
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := st.ClearAcquisition(ctx, res.ItemPID, model.LockUnchanged, false); err != nil {
			t.Fatalf("clear %d: %v", i, err)
		}
	}
	if locked, _ := st.IsFieldLocked(ctx, res.ItemPID, "acquisition"); !locked {
		t.Error("the repeated clear lost the lock")
	}

	// An item that never had a row and is not locked is left entirely alone: no delta,
	// and no lock installed by a clear that had nothing to clear.
	other, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/b.mp3", "ess-b", "c-b", "Plain"))
	if err != nil {
		t.Fatalf("PutScannedTrack: %v", err)
	}
	before, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("LatestChangeSeq: %v", err)
	}
	if err := st.ClearAcquisition(ctx, other.ItemPID, model.LockOff, false); err != nil {
		t.Fatalf("clear of a row-less item: %v", err)
	}
	if seq, _ := st.LatestChangeSeq(ctx); seq != before {
		t.Errorf("change seq moved from %d to %d for a clear that did nothing", before, seq)
	}
	if locked, _ := st.IsFieldLocked(ctx, other.ItemPID, "acquisition"); locked {
		t.Error("an explicit LockOff clear installed a lock")
	}
}

// TestPutAcquisitionForFileFillsAStandingItem: the import stamp of a file that joined an
// item standing before it (fill) keeps the item's origin and fills only what its record
// lacks, from an event of the record's own source type alone, a provider's id and version
// only within the provider the record names; a book's later part reaches its book; a stamp
// on an item with no record yet records the event.
func TestPutAcquisitionForFileFillsAStandingItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	part := func(path, essence string, position int) *model.ScanItemResult {
		t.Helper()
		in := bookIn(lib.ID, path, essence, "Dune", "Frank Herbert", "", "", "")
		in.Position = position
		res, err := st.PutScannedBook(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := part("/lib/dune/1.mp3", "ED1", 1)
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/dune/1.mp3"), model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://first.example/1",
	}, first.Made); err != nil {
		t.Fatal(err)
	}
	second := part("/lib/dune/2.mp3", "ED2", 2)
	if second.ItemPID != first.ItemPID {
		t.Fatalf("part 2 landed on %s, want the book %s", second.ItemPID, first.ItemPID)
	}
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/dune/2.mp3"), model.AcquisitionInput{
		SourceType: model.SourceManual, SourceURL: "https://second.example/2", Provider: "uploader",
	}, second.Made); err != nil {
		t.Fatalf("stamp on the joined part: %v", err)
	}
	if acq, err := st.AcquisitionByItem(ctx, first.ItemPID); err != nil || acq.Provider != "" || acq.SourceType != model.SourceYouTube {
		t.Errorf("acquisition after another kind of event = %+v (err %v), want it untouched", acq, err)
	}
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/dune/2.mp3"), model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://first.example/1", SourceID: "v2",
		Provider: "uploader", ProviderVersion: "3", OptionsJSON: `{"q":1}`,
	}, second.Made); err != nil {
		t.Fatalf("stamp on the joined part: %v", err)
	}
	acq, err := st.AcquisitionByItem(ctx, first.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	if acq.SourceType != model.SourceYouTube || acq.SourceURL != "https://first.example/1" ||
		acq.Provider != "uploader" || acq.SourceID != "v2" || acq.ProviderVersion != "3" || acq.OptionsJSON != `{"q":1}` {
		t.Errorf("acquisition = %+v, want the book's own origin kept and the rest filled", acq)
	}

	// A record naming another provider takes nothing from the event.
	track, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/t.mp3", "ET", "CT", "Song"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/t.mp3"), model.AcquisitionInput{
		SourceType: model.SourceRSS, Provider: "feeds",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if acq, err := st.AcquisitionByItem(ctx, track.ItemPID); err != nil || acq.SourceType != model.SourceRSS || acq.Provider != "feeds" {
		t.Fatalf("first stamp on a standing item with no record = %+v (err %v), want the event recorded", acq, err)
	}
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/t.mp3"), model.AcquisitionInput{
		SourceType: model.SourceRSS, SourceURL: "https://other.example", SourceID: "x", Provider: "tube", ProviderVersion: "9",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if acq, err := st.AcquisitionByItem(ctx, track.ItemPID); err != nil || acq.SourceType != model.SourceRSS ||
		acq.Provider != "feeds" || acq.SourceID != "" || acq.ProviderVersion != "" || acq.SourceURL != "" {
		t.Errorf("fill across providers = %+v (err %v), want nothing of another provider's acquisition", acq, err)
	}
}

// TestPutAcquisitionForFileStampsEveryTrackOfARip: the import stamp of a cue rip's file
// records the acquisition on every track the file plays, each having come with it.
func TestPutAcquisitionForFileStampsEveryTrackOfARip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	rip, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, "/lib/rip.flac", "ER", "CR", 600000,
		[][2]int64{{0, 7500}, {7500, 15000}, {15000, 0}}))
	if err != nil || len(rip.Made) != 3 {
		t.Fatalf("rip = %+v (err %v), want three tracks made", rip, err)
	}
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/rip.flac"), model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://rip.example",
	}, rip.Made); err != nil {
		t.Fatal(err)
	}
	tracks, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(tracks) != 3 {
		t.Fatalf("tracks = %d (err %v), want the rip's three", len(tracks), err)
	}
	for _, tr := range tracks {
		if acq, err := st.AcquisitionByItem(ctx, tr.PID); err != nil || acq.SourceURL != "https://rip.example" {
			t.Errorf("%s acquisition = %+v (err %v), want the rip's", tr.Title, acq, err)
		}
	}
}

// TestPutAcquisitionForFileFillsTheRipTracksThatStood: a rip's file read again with a new
// track names the track the read made, and the stamp of the file records the event whole
// on that track while only filling the record of the track that stood before it.
func TestPutAcquisitionForFileFillsTheRipTracksThatStood(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	first, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, "/lib/rip.flac", "ER", "CR", 600000, [][2]int64{{0, 0}}))
	if err != nil || len(first.Made) != 1 {
		t.Fatalf("first read = %+v (err %v), want one track made", first, err)
	}
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/rip.flac"), model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://first.example"}, first.Made); err != nil {
		t.Fatal(err)
	}
	second, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, "/lib/rip.flac", "ER", "CR", 600000, [][2]int64{{0, 7500}, {7500, 0}}))
	if err != nil || len(second.Made) != 1 || second.Made[0] == first.Made[0] {
		t.Fatalf("second read = %+v (err %v), want the new track alone made", second, err)
	}
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/rip.flac"), model.AcquisitionInput{
		SourceType: model.SourceManual, SourceURL: "https://second.example", Provider: "uploader"}, second.Made); err != nil {
		t.Fatal(err)
	}
	if acq, err := st.AcquisitionByItem(ctx, first.Made[0]); err != nil || acq.SourceType != model.SourceYouTube || acq.SourceURL != "https://first.example" {
		t.Errorf("the standing track's acquisition = %+v (err %v), want its own origin kept", acq, err)
	}
	if acq, err := st.AcquisitionByItem(ctx, second.Made[0]); err != nil || acq.SourceType != model.SourceManual ||
		acq.SourceURL != "https://second.example" || acq.Provider != "uploader" {
		t.Errorf("the new track's acquisition = %+v (err %v), want the event whole", acq, err)
	}
}

// TestFillKeepsOneAcquisitionTogether: an event fills a standing record only when it names
// the record's own acquisition: the same url, the same id under the record's provider when
// it holds no url, or anything at all when the record names none. An event for another
// acquisition, a book's next part with a url and id of its own, fills nothing, so a record
// never holds one part's url beside another's id. The provider is named only where the
// record has none, or where the event claims the record's own id under it.
func TestFillKeepsOneAcquisitionTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	yt := func(url, id, provider, version, options string) model.AcquisitionInput {
		return model.AcquisitionInput{SourceType: model.SourceYouTube, SourceURL: url, SourceID: id,
			Provider: provider, ProviderVersion: version, OptionsJSON: options}
	}
	for i, tc := range []struct {
		name   string
		record model.AcquisitionInput
		events []model.AcquisitionInput
		want   model.AcquisitionInput
	}{
		{"another url fills nothing", yt("https://y/1", "", "waxtap", "", ""),
			[]model.AcquisitionInput{yt("https://y/2", "id2", "waxtap", "2", `{"q":1}`)},
			yt("https://y/1", "", "waxtap", "", "")},
		{"another url names no provider either", yt("https://y/1", "", "", "", ""),
			[]model.AcquisitionInput{yt("https://y/2", "id2", "waxtap", "2", "")},
			yt("https://y/1", "", "", "", "")},
		{"an id alone takes only its own url", yt("", "id1", "waxtap", "", ""),
			[]model.AcquisitionInput{yt("https://y/2", "id2", "waxtap", "", ""), yt("https://y/1", "id1", "waxtap", "3", "")},
			yt("https://y/1", "id1", "waxtap", "3", "")},
		{"the same url fills the rest", yt("https://y/1", "", "waxtap", "", ""),
			[]model.AcquisitionInput{yt("https://y/1", "id1", "waxtap", "3", `{"q":1}`)},
			yt("https://y/1", "id1", "waxtap", "3", `{"q":1}`)},
		{"a record naming no acquisition takes one", model.AcquisitionInput{SourceType: model.SourceManual, Provider: "uploader"},
			[]model.AcquisitionInput{{SourceType: model.SourceManual, SourceURL: "https://u/2", SourceID: "u2", Provider: "uploader"}},
			model.AcquisitionInput{SourceType: model.SourceManual, SourceURL: "https://u/2", SourceID: "u2", Provider: "uploader"}},
		{"a tag id the event claims takes its provider", yt("https://y/1", "id1", "", "", ""),
			[]model.AcquisitionInput{yt("https://y/1", "id1", "waxtap", "3", `{"q":1}`)},
			yt("https://y/1", "id1", "waxtap", "3", `{"q":1}`)},
		{"a tag id another id contradicts keeps no provider, version or options", yt("https://y/1", "id1", "", "", ""),
			[]model.AcquisitionInput{yt("https://y/1", "id9", "waxtap", "3", `{"q":1}`)},
			yt("https://y/1", "id1", "", "", "")},
		{"another id under the record's own provider is another acquisition", yt("https://y/1", "id1", "waxtap", "", ""),
			[]model.AcquisitionInput{yt("https://y/1", "id9", "waxtap", "3", `{"q":1}`)},
			yt("https://y/1", "id1", "waxtap", "", "")},
		{"a record naming only a provider takes nothing from another", yt("", "", "uploader", "", ""),
			[]model.AcquisitionInput{yt("https://y/2", "id2", "waxtap", "2", `{"q":1}`)},
			yt("", "", "uploader", "", "")},
		{"a record naming only a provider takes its own acquisition", yt("", "", "waxtap", "", ""),
			[]model.AcquisitionInput{yt("https://y/2", "id2", "waxtap", "2", `{"q":1}`)},
			yt("https://y/2", "id2", "waxtap", "2", `{"q":1}`)},
		{"no version without a provider", yt("https://y/1", "id1", "", "", ""),
			[]model.AcquisitionInput{yt("https://y/1", "id1", "", "3", `{"q":1}`)},
			yt("https://y/1", "id1", "", "", `{"q":1}`)},
	} {
		path := fmt.Sprintf("/lib/t%d.mp3", i)
		track, err := st.PutScannedTrack(ctx, input(lib.ID, path, fmt.Sprintf("E%d", i), fmt.Sprintf("C%d", i), "Song"))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutAcquisition(ctx, track.ItemPID, tc.record); err != nil {
			t.Fatal(err)
		}
		for _, ev := range tc.events {
			if _, err := st.PutAcquisitionForFile(ctx, []byte(path), ev, nil); err != nil {
				t.Fatal(err)
			}
		}
		acq, err := st.AcquisitionByItem(ctx, track.ItemPID)
		if err != nil {
			t.Fatal(err)
		}
		got := model.AcquisitionInput{SourceType: acq.SourceType, SourceURL: acq.SourceURL, SourceID: acq.SourceID,
			Provider: acq.Provider, ProviderVersion: acq.ProviderVersion, OptionsJSON: acq.OptionsJSON}
		if got != tc.want {
			t.Errorf("%s: record = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestCarvedTracksCarryTheWholeFilesOrigin: the tracks a cue sheet carves out of an item's
// whole file carry its origin, the opener as that item and the rest by inheriting it, and
// none is made from nothing, so an import of the file re-acquired elsewhere with its sheet
// leaves every track with the one origin rather than the opener's beside the others'.
func TestCarvedTracksCarryTheWholeFilesOrigin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	whole, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/rip.flac", "ER", "CR", "Whole"))
	if err != nil {
		t.Fatal(err)
	}
	first := model.AcquisitionInput{SourceType: model.SourceYouTube, SourceURL: "https://a.example", Provider: "waxtap"}
	if err := st.PutAcquisition(ctx, whole.ItemPID, first); err != nil {
		t.Fatal(err)
	}
	carved, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, "/lib/rip.flac", "ER", "CR", 600000, [][2]int64{{0, 7500}, {7500, 0}}))
	if err != nil || len(carved.Made) != 0 || len(carved.Minted) != 1 {
		t.Fatalf("carve = %+v (err %v), want one track minted and none made from nothing", carved, err)
	}
	origins := func(want string) {
		t.Helper()
		tracks, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
		if err != nil || len(tracks) != 2 {
			t.Fatalf("tracks = %d (err %v), want the rip's two", len(tracks), err)
		}
		for _, tr := range tracks {
			if acq, err := st.AcquisitionByItem(ctx, tr.PID); err != nil || acq.SourceURL != want || acq.Provider != "waxtap" {
				t.Errorf("%s acquisition = %+v (err %v), want %s", tr.Title, acq, err, want)
			}
		}
	}
	origins("https://a.example")
	if _, err := st.PutAcquisitionForFile(ctx, []byte("/lib/rip.flac"), model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://b.example", Provider: "waxtap"}, carved.Made); err != nil {
		t.Fatal(err)
	}
	origins("https://a.example")
}

// TestAPartLeavingItsBookCarriesItsOrigin: a part read as a track of its own inherits the
// book's origin, unless its own tags name one, and is not made from nothing.
func TestAPartLeavingItsBookCarriesItsOrigin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, lib := openTestStore(t)
	var book model.PID
	for i := 1; i <= 3; i++ {
		in := bookIn(lib.ID, fmt.Sprintf("/lib/dune/%d.mp3", i), fmt.Sprintf("ED%d", i), "Dune", "Frank Herbert", "", "", "")
		in.Position = i
		res, err := st.PutScannedBook(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		book = res.ItemPID
	}
	if err := st.PutAcquisition(ctx, book, model.AcquisitionInput{SourceType: model.SourceYouTube, SourceURL: "https://book.example", Provider: "waxtap"}); err != nil {
		t.Fatal(err)
	}
	split, err := st.PutScannedTrack(ctx, input(lib.ID, "/lib/dune/2.mp3", "ED2", "C2", "Two"))
	if err != nil || split.Minted[split.ItemPID] != book || len(split.Made) != 0 {
		t.Fatalf("split = %+v (err %v), want the track minted from the book and not made from nothing", split, err)
	}
	if acq, err := st.AcquisitionByItem(ctx, split.ItemPID); err != nil || acq.SourceURL != "https://book.example" || acq.Provider != "waxtap" {
		t.Errorf("the track's acquisition = %+v (err %v), want the book's origin inherited", acq, err)
	}
	tagged := input(lib.ID, "/lib/dune/3.mp3", "ED3", "C3", "Three")
	tagged.Acquisition = model.TagAcquisition{SourceURL: "https://tag.example"}
	own, err := st.PutScannedTrack(ctx, tagged)
	if err != nil || own.Minted[own.ItemPID] != book {
		t.Fatalf("tagged split = %+v (err %v), want the track minted from the book", own, err)
	}
	if acq, err := st.AcquisitionByItem(ctx, own.ItemPID); err != nil || acq.SourceURL != "https://tag.example" || acq.Provider != "" {
		t.Errorf("the tagged track's acquisition = %+v (err %v), want its own tags' origin", acq, err)
	}
}
