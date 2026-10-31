package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// episodeFixture opens a seeded catalog holding a podcast library and one remote episode
// of a manual show, the episode declaring durationMS.
func episodeFixture(t *testing.T, durationMS int64) (*Store, int64, model.PID) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, OpenOptions{Path: seedCatalog(t, filepath.Join(t.TempDir(), "c.db")), Owner: "test"})
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
	ep, err := st.UpsertEpisode(ctx, model.UpsertEpisodeInput{PodcastPID: showPID, Episode: model.FeedEpisode{
		Title: "Ep", GUID: "e1", EnclosureURL: "http://x/e1.mp3", EnclosureType: "audio/mpeg", DurationMS: durationMS,
	}})
	if err != nil {
		t.Fatalf("episode: %v", err)
	}
	return st, libID.ID, ep.EpisodePID
}

// attachEpisode downloads ep as the file at path running durationMS, returning its pid.
func attachEpisode(t *testing.T, st *Store, libID int64, ep model.PID, path string, durationMS int64) model.PID {
	t.Helper()
	pid, err := st.AttachEpisodeFile(context.Background(), model.AttachEpisodeFileInput{
		EpisodePID: ep, LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, ContentHash: path, DurationMS: durationMS, ScanState: model.ScanIndexed,
		},
	})
	if err != nil {
		t.Fatalf("attach %s: %v", path, err)
	}
	return pid
}

var feedChapters = []model.Chapter{{Position: 0, Title: "Intro", FileStartMS: 0}, {Position: 1, Title: "Topic", FileStartMS: 120000}}

// wantChapters asserts an episode reads back the two feed chapters, the second ending at
// end, both naming file (empty for none).
func wantChapters(t *testing.T, step string, got []model.Chapter, file model.PID, end int64) {
	t.Helper()
	if len(got) != 2 || got[0].Title != "Intro" || got[0].StartMS != 0 || got[0].EndMS != 120000 ||
		got[1].Title != "Topic" || got[1].StartMS != 120000 || got[1].EndMS != end ||
		got[0].FilePID != file || got[1].FilePID != file {
		t.Fatalf("%s: chapters = %+v, want Intro [0,120000) and Topic [120000,%d) on file %q", step, got, end, file)
	}
}

// TestEpisodeChaptersStoreWithoutAFile: an episode's feed chapters store before it is
// downloaded and read back over its declared duration, with no file behind them.
func TestEpisodeChaptersStoreWithoutAFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, ep := episodeFixture(t, 600000)
	if err := st.PutEpisodeChapters(ctx, ep, feedChapters); err != nil {
		t.Fatalf("put chapters: %v", err)
	}
	got, err := st.EpisodeChapters(ctx, ep)
	if err != nil {
		t.Fatalf("read chapters: %v", err)
	}
	wantChapters(t, "episode chapters", got, "", 600000)
	d, err := st.EpisodeByPID(ctx, ep)
	if err != nil {
		t.Fatalf("episode detail: %v", err)
	}
	wantChapters(t, "episode detail", d.Chapters, "", 600000)

	before, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutEpisodeChapters(ctx, ep, feedChapters); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	if after, err := st.LatestChangeSeq(ctx); err != nil || after != before {
		t.Fatalf("an unchanged re-put moved the change feed from %d to %d (err %v)", before, after, err)
	}
}

// TestEpisodeChaptersSurviveUnfetch: a download, an unfetch and a re-download keep the
// feed's chapters, each read naming the file behind them while there is one.
func TestEpisodeChaptersSurviveUnfetch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, libID, ep := episodeFixture(t, 0)
	file := attachEpisode(t, st, libID, ep, "/podcasts/e1.mp3", 600000)
	if err := st.PutEpisodeChapters(ctx, ep, feedChapters); err != nil {
		t.Fatalf("put chapters: %v", err)
	}
	read := func() []model.Chapter {
		t.Helper()
		got, err := st.EpisodeChapters(ctx, ep)
		if err != nil {
			t.Fatalf("read chapters: %v", err)
		}
		return got
	}
	wantChapters(t, "downloaded", read(), file, 600000)
	if err := st.DropEpisodeFile(ctx, ep); err != nil {
		t.Fatalf("unfetch: %v", err)
	}
	// With no file and no declared duration the last chapter's end is unknown: open.
	wantChapters(t, "unfetched", read(), "", 0)
	again := attachEpisode(t, st, libID, ep, "/podcasts/e1-again.mp3", 700000)
	wantChapters(t, "downloaded again", read(), again, 700000)
}

// TestFeedChaptersOutrankEmbeddedAfterADownload: the feed's chapters are what an episode
// reads over chapters its downloaded file carries, which it reads when the feed gave
// none.
func TestFeedChaptersOutrankEmbeddedAfterADownload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, libID, ep := episodeFixture(t, 0)
	file := attachEpisode(t, st, libID, ep, "/podcasts/e1.mp3", 600000)
	// Nothing writes an episode file's own chapters today, so a row stands in for them.
	if _, err := st.wdb().ExecContext(ctx, `INSERT INTO chapter(book_item_id, file_id, position, title, start_ms, end_ms, source)
		SELECT pi.id, f.id, 0, 'Embedded', 0, 0, 'embedded' FROM playable_item pi
		JOIN item_file itf ON itf.item_id = pi.id AND itf.role = 'primary' JOIN file f ON f.id = itf.file_id
		WHERE pi.pid = ?`, string(ep)); err != nil {
		t.Fatalf("embedded row: %v", err)
	}
	got, err := st.EpisodeChapters(ctx, ep)
	if err != nil || len(got) != 1 || got[0].Title != "Embedded" || got[0].FilePID != file || got[0].EndMS != 600000 {
		t.Fatalf("chapters before the feed's = %+v (err %v), want the file's own one", got, err)
	}
	if err := st.PutEpisodeChapters(ctx, ep, feedChapters); err != nil {
		t.Fatalf("put chapters: %v", err)
	}
	got, err = st.EpisodeChapters(ctx, ep)
	if err != nil {
		t.Fatalf("read chapters: %v", err)
	}
	wantChapters(t, "with the feed's", got, file, 600000)
}

// scansChapters finds a plan line that walks a chapter table rather than seeking it.
var scansChapters = regexp.MustCompile(`SCAN (chapter|c|d)\b`)

// TestItemKeptChapterRowsSeekTheirItem: the statements that read, replace and measure an
// item's own chapter rows, or its rows of one file, seek exactly those rows rather than
// walking every item-kept row in the catalog.
func TestItemKeptChapterRowsSeekTheirItem(t *testing.T) {
	t.Parallel()
	st, _, _ := episodeFixture(t, 0)
	type stmt struct {
		q    string
		args []any
	}
	stmts := []stmt{{itemChaptersQ, []any{1, "podcast_url"}}, {fileExtentQ, []any{1, 1}}}
	for _, prefix := range []string{chapterRowsSelect, chapterRowsDelete} {
		for _, file := range []sql.NullInt64{{}, fileKey(1)} {
			q, args := chapterRowsOf(prefix, 1, file)
			stmts = append(stmts, stmt{q, args})
		}
	}
	for _, s := range stmts {
		plan := queryPlan(t, st, s.q, s.args...)
		if !strings.Contains(plan, "chapter_book_file (book_item_id=? AND file_id=?)") || scansChapters.MatchString(plan) {
			t.Errorf("%s\nplan:\n%s\nwant a seek on the item and the file together", s.q, plan)
		}
	}
	// A book's extents read its own rows, each file's through the same seek.
	if plan := queryPlan(t, st, partExtentsQ, 1); !strings.Contains(plan, "chapter_book_file (book_item_id=?") ||
		scansChapters.MatchString(plan) {
		t.Errorf("partExtentsQ plan:\n%s\nwant every chapter read to seek its book", plan)
	}
}

// TestEpisodeChaptersFallBackToTheDeclaredDuration: a downloaded file whose duration was
// not read leaves the last feed chapter running to the feed's declared duration.
func TestEpisodeChaptersFallBackToTheDeclaredDuration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, libID, ep := episodeFixture(t, 600000)
	file := attachEpisode(t, st, libID, ep, "/podcasts/e1.mp3", 0)
	if err := st.PutEpisodeChapters(ctx, ep, feedChapters); err != nil {
		t.Fatalf("put chapters: %v", err)
	}
	got, err := st.EpisodeChapters(ctx, ep)
	if err != nil {
		t.Fatalf("read chapters: %v", err)
	}
	wantChapters(t, "a file of unknown length", got, file, 600000)
}

// TestALastChapterPastTheKnownLengthIsOpen: a feed chapter that starts at or past every
// length the episode knows reads as open-ended rather than as a span with no length.
func TestALastChapterPastTheKnownLengthIsOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, libID, ep := episodeFixture(t, 0)
	file := attachEpisode(t, st, libID, ep, "/podcasts/e1.mp3", 100000)
	if err := st.PutEpisodeChapters(ctx, ep, feedChapters); err != nil {
		t.Fatalf("put chapters: %v", err)
	}
	got, err := st.EpisodeChapters(ctx, ep)
	if err != nil {
		t.Fatalf("read chapters: %v", err)
	}
	wantChapters(t, "a chapter past the file's length", got, file, 0)
}
