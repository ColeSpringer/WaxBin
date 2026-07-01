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
		Where("source", query.OpIs, string(model.SourceYouTube)).Build())
	if err != nil {
		t.Fatalf("query by source: %v", err)
	}
	if len(items) != 1 || items[0].PID != res.ItemPID {
		t.Fatalf("source filter returned %d items, want the acquired one", len(items))
	}
	// The local-source filter excludes it.
	locals, _ := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("source", query.OpIs, string(model.SourceLocal)).Build())
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
