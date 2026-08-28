package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

func TestAcquisitionRoundTripAndSourceSurfacing(t *testing.T) {
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
	if err := st.PutAcquisitionForFile(ctx, []byte("/lib/a.mp3"), model.AcquisitionInput{
		SourceType: model.SourceYouTube, SourceURL: "https://y/watch?v=1", SourceID: "1", Provider: "waxtap",
	}); err != nil {
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
	if err := st.ClearAcquisition(ctx, res.ItemPID); err != nil {
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
	if err := st.ClearAcquisition(ctx, res.ItemPID); err != nil {
		t.Errorf("second ClearAcquisition: %v", err)
	}
}

// TestAcquisitionSourceIDFollowsProvider: source_id is a provider-native id, so it means
// nothing under a different provider's name. An event that switches the provider without
// naming an id of its own drops the standing one rather than mislabelling it.
func TestAcquisitionSourceIDFollowsProvider(t *testing.T) {
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
