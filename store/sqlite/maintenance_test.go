package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func itemID(t *testing.T, st *Store, title string) int64 {
	t.Helper()
	var id int64
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT id FROM playable_item WHERE title = ?", title).Scan(&id); err != nil {
		t.Fatalf("item %q: %v", title, err)
	}
	return id
}

func defaultUserID(t *testing.T, st *Store) int64 {
	t.Helper()
	var id int64
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT id FROM user ORDER BY id LIMIT 1").Scan(&id); err != nil {
		t.Fatalf("default user: %v", err)
	}
	return id
}

func TestYearInReview(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Anthem",
		artist: "Muse", album: "A", genre: "Rock", durationMS: 240000,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/2.flac", essence: "e2", content: "c2", title: "Ballad",
		artist: "Muse", album: "A", genre: "Rock", durationMS: 180000,
	})
	uid := defaultUserID(t, st)
	anthem := itemID(t, st, "Anthem")
	ballad := itemID(t, st, "Ballad")

	in2025 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC).UnixNano()
	in2024 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC).UnixNano()
	seed := func(item, started, ms int64) {
		if _, err := st.write.ExecContext(ctx,
			"INSERT INTO play_session(pid, user_id, item_id, started_at, ended_at, ms_played, client) VALUES (?,?,?,?,?,?,'test')",
			string(model.NewPID()), uid, item, started, started+ms, ms); err != nil {
			t.Fatal(err)
		}
	}
	seed(anthem, in2025, 240000) // 4 min
	seed(anthem, in2025+1, 240000)
	seed(ballad, in2025+2, 60000) // 1 min
	seed(anthem, in2024, 240000)  // prior year, excluded

	// A podcast-episode session in-year must NOT count toward the music/book recap
	// (episodes lack the artist/genre entities the top lists key on).
	res, err := st.write.ExecContext(ctx,
		`INSERT INTO playable_item(pid,kind,state,title,sort_key,identity_key,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		string(model.NewPID()), "episode", "present", "Some Episode", "some episode", "ep:x", in2025, in2025)
	if err != nil {
		t.Fatal(err)
	}
	epItem, _ := res.LastInsertId()
	seed(epItem, in2025+3, 600000) // 10 min episode, excluded from the recap

	yr, err := st.YearInReview(ctx, "", 2025, 10)
	if err != nil {
		t.Fatal(err)
	}
	if yr.Sessions != 3 {
		t.Errorf("sessions = %d, want 3 (2024 session excluded)", yr.Sessions)
	}
	if yr.MinutesPlayed != 9 { // 4+4+1
		t.Errorf("minutes = %d, want 9", yr.MinutesPlayed)
	}
	if yr.TracksPlayed != 2 {
		t.Errorf("tracks played = %d, want 2", yr.TracksPlayed)
	}
	if len(yr.TopTracks) == 0 || yr.TopTracks[0].Title != "Anthem" || yr.TopTracks[0].PlayCount != 2 {
		t.Errorf("top track = %+v, want Anthem x2 first", yr.TopTracks)
	}
	if len(yr.TopArtists) == 0 || yr.TopArtists[0].Display != "Muse" || yr.TopArtists[0].Count != 3 {
		t.Errorf("top artist = %+v, want Muse x3", yr.TopArtists)
	}
	if len(yr.TopGenres) == 0 || yr.TopGenres[0].Display != "Rock" {
		t.Errorf("top genre = %+v, want Rock", yr.TopGenres)
	}
}

func TestYearInReviewRejectsOutOfRange(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	// A year outside the unix-nanosecond range would produce wrapped bounds; reject it.
	if _, err := st.YearInReview(ctx, "", 1000000, 10); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("year=1000000: got %v, want CodeInvalid", err)
	}
	if _, err := st.YearInReview(ctx, "", 0, 10); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("year=0: got %v, want CodeInvalid", err)
	}
}

func TestVacuumAndIntegrity(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "A",
	})
	if err := st.Vacuum(ctx); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	// The catalog is still queryable after a vacuum.
	if got := rollupTrackCount(t, st, "artist_rollup", "artist", "name", "X"); got != 1 {
		t.Errorf("rollup after vacuum = %d, want 1", got)
	}
	problems, err := st.IntegrityCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || problems[0] != "ok" {
		t.Errorf("integrity check = %v, want [ok]", problems)
	}
}

func TestPruneChangeLog(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, spec := range []trackSpec{
		{path: "/lib/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", album: "X"},
		{path: "/lib/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", album: "Y"},
		{path: "/lib/3.flac", essence: "e3", content: "c3", title: "Three", artist: "C", album: "Z"},
	} {
		_ = i
		putTrack(t, st, lib.ID, spec)
	}
	before, err := st.LatestChangeSeq(ctx)
	if err != nil || before < 3 {
		t.Fatalf("expected several change_log rows, got seq=%d err=%v", before, err)
	}
	deleted, err := st.PruneChangeLog(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted == 0 {
		t.Error("prune deleted nothing despite many rows")
	}
	var remaining int
	if err := st.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM change_log").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Errorf("remaining change_log rows = %d, want 2", remaining)
	}
	if _, err := st.PruneChangeLog(ctx, 0); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("prune keep=0: got %v, want CodeInvalid", err)
	}
}
