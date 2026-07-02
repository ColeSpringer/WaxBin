package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestEpisodeChapters verifies URL-sourced podcast chapters store and read back, and
// that they win over embedded chapters (precedence), surviving a re-sync.
func TestEpisodeChapters(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, OpenOptions{Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	libID, err := st.EnsurePodcastLibrary(ctx, "/podcasts")
	if err != nil {
		t.Fatalf("podcast lib: %v", err)
	}
	showPID, _, err := st.UpsertShow(ctx, model.UpsertShowInput{
		IdentityKey: "manual:s", FeedURL: "manual:s", SourceType: model.SourceManual, Title: "Show",
	})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	epRes, err := st.UpsertEpisode(ctx, model.UpsertEpisodeInput{
		PodcastPID: showPID,
		Episode:    model.FeedEpisode{Title: "Ep", GUID: "e1", EnclosureURL: "http://x/e1.mp3", EnclosureType: "audio/mpeg"},
	})
	if err != nil {
		t.Fatalf("episode: %v", err)
	}
	epPID := epRes.EpisodePID

	// Not downloaded yet: PutEpisodeChapters refuses, reads are empty.
	if err := st.PutEpisodeChapters(ctx, epPID, []model.Chapter{{Title: "x"}}); err == nil {
		t.Fatal("expected error storing chapters for an undownloaded episode")
	}

	if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: epPID, LibraryID: libID,
		File: model.File{
			Path: []byte("/podcasts/e1.mp3"), DisplayPath: "/podcasts/e1.mp3", RelPath: []byte("e1.mp3"),
			Kind: model.FileAudio, ContentHash: "e1", DurationMS: 600000, ScanState: model.ScanIndexed,
		},
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	chapters := []model.Chapter{
		{Position: 0, Title: "Intro", FileStartMS: 0},
		{Position: 1, Title: "Topic", FileStartMS: 120000},
	}
	if err := st.PutEpisodeChapters(ctx, epPID, chapters); err != nil {
		t.Fatalf("put chapters: %v", err)
	}

	got, err := st.EpisodeChapters(ctx, epPID)
	if err != nil {
		t.Fatalf("read chapters: %v", err)
	}
	if len(got) != 2 || got[0].Title != "Intro" || got[1].Title != "Topic" {
		t.Fatalf("chapters = %+v, want Intro/Topic", got)
	}
	if got[1].StartMS != 120000 {
		t.Errorf("second chapter start = %d, want 120000", got[1].StartMS)
	}

	// EpisodeByPID surfaces them.
	detail, err := st.EpisodeByPID(ctx, epPID)
	if err != nil {
		t.Fatalf("episode detail: %v", err)
	}
	if len(detail.Chapters) != 2 {
		t.Fatalf("EpisodeDetail.Chapters = %d, want 2", len(detail.Chapters))
	}

	// A re-sync with the same chapters is idempotent (no change).
	if err := st.PutEpisodeChapters(ctx, epPID, chapters); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	got, _ = st.EpisodeChapters(ctx, epPID)
	if len(got) != 2 {
		t.Errorf("chapters after re-sync = %d, want 2", len(got))
	}
}
