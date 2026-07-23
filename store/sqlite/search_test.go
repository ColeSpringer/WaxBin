package sqlite

import (
	"context"
	"strconv"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

func TestSearchGroupsAndMatches(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "Paranoid Android", artist: "Radiohead", album: "OK Computer", albumArt: "Radiohead"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "Karma Police", artist: "Radiohead", album: "OK Computer", albumArt: "Radiohead"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3", title: "Bohemian Rhapsody", artist: "Queen", album: "A Night at the Opera", albumArt: "Queen"})

	res, err := st.Search(ctx, "radiohead", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// Two tracks, one artist, one album for the Radiohead query.
	if len(res.Tracks) != 2 {
		t.Errorf("tracks = %d, want 2", len(res.Tracks))
	}
	if len(res.Artists) != 1 || res.Artists[0].Title != "Radiohead" {
		t.Errorf("artists = %+v, want [Radiohead]", res.Artists)
	}
	if len(res.Albums) != 1 || res.Albums[0].Title != "OK Computer" {
		t.Errorf("albums = %+v, want [OK Computer]", res.Albums)
	}
	if res.Albums[0].PID == "" || res.Artists[0].PID == "" {
		t.Error("artist/album hits must carry their entity pid for drilldown")
	}
}

// TestSearchTitleOutranksArtist verifies BM25 field weighting: a track whose
// title contains the term ranks above one that only matches via an artist/album
// column.
func TestSearchTitleOutranksArtist(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// One fixture has "Mercury" as the title.
	titleHit := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "Mercury", artist: "The Planets", album: "Holst"})
	// The other has "Mercury" as the artist.
	artistHit := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "Killer Queen", artist: "Mercury", album: "Sheer Heart"})

	res, err := st.Search(ctx, "mercury", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Tracks) < 2 {
		t.Fatalf("tracks = %d, want >= 2", len(res.Tracks))
	}
	if res.Tracks[0].PID != model.PID(titleHit.ItemPID) {
		t.Errorf("top track = %s (%q), want the title match %s",
			res.Tracks[0].PID, res.Tracks[0].Title, titleHit.ItemPID)
	}
	if res.Tracks[0].Score >= res.Tracks[1].Score {
		t.Errorf("title hit score %v should be lower (better) than artist hit %v",
			res.Tracks[0].Score, res.Tracks[1].Score)
	}
	_ = artistHit
}

func TestSearchEmptyAndPunctuationQuery(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "Hello", artist: "X", album: "Al"})

	// A query that tokenizes to nothing returns an empty (non-error) result.
	res, err := st.Search(ctx, "   !!! ", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search punctuation: %v", err)
	}
	if !res.Empty() {
		t.Errorf("punctuation-only query should be empty, got %+v", res)
	}

	// FTS operator words are neutralized by lowercasing, so "OR" is a plain term,
	// not a syntax error.
	if _, err := st.Search(ctx, "OR AND NOT", read.SearchOptions{}); err != nil {
		t.Errorf("operator-word query should not error: %v", err)
	}
}

func TestFTSMatchQuery(t *testing.T) {
	cases := map[string]string{
		"Beatles":     "beatles*",
		"AC/DC":       "ac* dc*",
		"  hello  ":   "hello*",
		"!!!":         "",
		"Sgt. Pepper": "sgt* pepper*",
		"OR":          "or*", // lowercased: a plain term, not the FTS operator
	}
	for in, want := range cases {
		if got := ftsMatchQuery(in); got != want {
			t.Errorf("ftsMatchQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSearchStmtZeroPathGolden pins the option-free statement to the exact text
// the search ran before the candidate-cap/scope options existed, so the default
// path stays byte-identical (same plan, same behavior) as the builder evolves.
func TestSearchStmtZeroPathGolden(t *testing.T) {
	want := `SELECT pi.pid, pi.kind, pi.title,
		COALESCE(NULLIF(t.artist,''), bk.author, pod.title, ''), COALESCE(t.album_artist,''),
		COALESCE(t.album,''), COALESCE(art.pid,''), COALESCE(al.pid,''), ` + searchBM25 + ` AS score
		FROM search_fts
		JOIN playable_item pi ON pi.id = search_fts.rowid
		LEFT JOIN track t ON t.item_id = pi.id
		LEFT JOIN book bk ON bk.item_id = pi.id
		LEFT JOIN episode ep ON ep.item_id = pi.id
		LEFT JOIN podcast pod ON pod.id = ep.podcast_id
		LEFT JOIN artist art ON art.id = t.artist_id
		LEFT JOIN album al ON al.id = t.album_id
		WHERE search_fts MATCH ?
		ORDER BY score, pi.pid
		LIMIT ?`
	stmt, args, cap := searchStmt("beatles*", 20, 0, nil)
	if stmt != want {
		t.Errorf("zero-option statement drifted:\ngot:\n%s\nwant:\n%s", stmt, want)
	}
	if cap != searchFetchCap(20) {
		t.Errorf("scan cap = %d, want %d", cap, searchFetchCap(20))
	}
	if len(args) != 2 || args[0] != "beatles*" || args[1] != cap+1 {
		t.Errorf("args = %v, want [beatles* %d]", args, cap+1)
	}
}

// TestSearchCandidateCapPrunesOldest verifies the cap actually prunes and prunes
// the old end: the best-ranked match (a title hit, inserted first) disappears
// under a cap smaller than the match count, because the pool keeps the newest
// rows, and Truncated reports the pruning.
func TestSearchCandidateCapPrunesOldest(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Oldest row: the only TITLE match for "nebula" (would rank first).
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/t/0.flac", essence: "e0", content: "c0",
		title: "Nebula", artist: "Someone", album: "Alpha"})
	// Then five newer rows matching only via the artist column (weaker rank).
	for i := 1; i <= 5; i++ {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/t/" + strconv.Itoa(i) + ".flac", essence: "e" + strconv.Itoa(i), content: "c" + strconv.Itoa(i),
			title: "Song " + strconv.Itoa(i), artist: "Nebula Drive", album: "Alb" + strconv.Itoa(i)})
	}

	full, err := st.Search(ctx, "nebula", read.SearchOptions{})
	if err != nil {
		t.Fatalf("uncapped search: %v", err)
	}
	if full.Truncated || len(full.Tracks) != 6 || full.Tracks[0].Title != "Nebula" {
		t.Fatalf("uncapped = truncated=%v tracks=%d top=%q, want 6 tracks led by the title hit",
			full.Truncated, len(full.Tracks), firstTitle(full.Tracks))
	}

	capped, err := st.Search(ctx, "nebula", read.SearchOptions{MaxCandidates: 3})
	if err != nil {
		t.Fatalf("capped search: %v", err)
	}
	if !capped.Truncated {
		t.Error("a spent candidate pool must set Truncated")
	}
	if len(capped.Tracks) != 3 {
		t.Fatalf("capped tracks = %d, want 3 (the pool)", len(capped.Tracks))
	}
	for _, h := range capped.Tracks {
		if h.Title == "Nebula" {
			t.Error("the oldest (best-ranked) match survived a cap that must keep only the newest rows")
		}
	}
}

// TestSearchCapAboveMatchCountIsExact verifies a cap at or above the match count
// changes nothing: same groups as uncapped, no truncation.
func TestSearchCapAboveMatchCountIsExact(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Paranoid Android", artist: "Radiohead", album: "OK Computer"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "Karma Police", artist: "Radiohead", album: "OK Computer"})

	full, err := st.Search(ctx, "radiohead", read.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	capped, err := st.Search(ctx, "radiohead", read.SearchOptions{MaxCandidates: 50})
	if err != nil {
		t.Fatal(err)
	}
	if capped.Truncated {
		t.Error("cap above the match count must not report truncation")
	}
	if len(capped.Tracks) != len(full.Tracks) || len(capped.Artists) != len(full.Artists) ||
		len(capped.Albums) != len(full.Albums) {
		t.Errorf("capped groups %d/%d/%d differ from uncapped %d/%d/%d",
			len(capped.Tracks), len(capped.Artists), len(capped.Albums),
			len(full.Tracks), len(full.Artists), len(full.Albums))
	}
	for i := range full.Tracks {
		if capped.Tracks[i].PID != full.Tracks[i].PID {
			t.Errorf("track %d = %s, want %s (order must match the uncapped ranking)",
				i, capped.Tracks[i].PID, full.Tracks[i].PID)
		}
	}
}

// TestSearchLibraryScope verifies a scoped search returns only items playable
// from the given libraries and that an unknown library pid errors instead of
// silently narrowing.
func TestSearchLibraryScope(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/other"), DisplayRoot: "/other", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("second library: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1",
		title: "Harbor Lights", artist: "A", album: "Alp"})
	putTrack(t, st, lib2.ID, trackSpec{path: "/other/b.flac", essence: "e2", content: "c2",
		title: "Harbor Nights", artist: "B", album: "Bet"})

	scoped, err := st.Search(ctx, "harbor", read.SearchOptions{Libraries: []model.PID{lib.PID}})
	if err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	if len(scoped.Tracks) != 1 || scoped.Tracks[0].Title != "Harbor Lights" {
		t.Errorf("scoped tracks = %+v, want only the /lib item", scoped.Tracks)
	}

	both, err := st.Search(ctx, "harbor", read.SearchOptions{Libraries: []model.PID{lib.PID, lib2.PID}})
	if err != nil {
		t.Fatalf("two-library search: %v", err)
	}
	if len(both.Tracks) != 2 {
		t.Errorf("two-library tracks = %d, want 2", len(both.Tracks))
	}

	if _, err := st.Search(ctx, "harbor", read.SearchOptions{Libraries: []model.PID{"nope"}}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown library = %v, want CodeNotFound", err)
	}
}

// TestSearchScopeCoversTranscripts verifies the library scope reaches the
// transcript rung: a transcript hit for an undownloaded episode (no file, so no
// library) drops out of a scoped search but still surfaces unscoped.
func TestSearchScopeCoversTranscripts(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	res, err := st.UpsertFeed(ctx, model.UpsertFeedInput{
		FeedURL:     "http://feed.example/x",
		IdentityKey: "podcast:feed.example/x",
		Feed: model.Feed{Title: "My Show", Author: "Host", Episodes: []model.FeedEpisode{
			{GUID: "g1", Title: "Downloaded One", EnclosureURL: "http://feed.example/1.mp3", EnclosureType: "audio/mpeg"},
			{GUID: "g2", Title: "Remote Two", EnclosureURL: "http://feed.example/2.mp3", EnclosureType: "audio/mpeg"},
		}},
		FetchedAtNS: 1,
	})
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	eps, err := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v), want 2", len(eps), err)
	}
	var downloaded, remote model.PID
	for _, ep := range eps {
		if ep.Title == "Downloaded One" {
			downloaded = ep.PID
		} else {
			remote = ep.PID
		}
	}
	if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: downloaded, LibraryID: lib.ID,
		File: model.File{Path: []byte("/lib/pod/1.mp3"), DisplayPath: "/lib/pod/1.mp3",
			RelPath: []byte("pod/1.mp3"), Kind: model.FileAudio, Size: 3, MTimeNS: 1, ContentHash: "pc1"},
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	for _, pid := range []model.PID{downloaded, remote} {
		if err := st.PutTranscript(ctx, model.PutTranscriptInput{
			EpisodePID: pid, Format: "text", Body: "they discuss the zanzibar expedition at length",
		}); err != nil {
			t.Fatalf("transcript %s: %v", pid, err)
		}
	}

	full, err := st.Search(ctx, "zanzibar", read.SearchOptions{})
	if err != nil {
		t.Fatalf("unscoped: %v", err)
	}
	if len(full.Episodes) != 2 {
		t.Fatalf("unscoped transcript hits = %d, want 2", len(full.Episodes))
	}

	scoped, err := st.Search(ctx, "zanzibar", read.SearchOptions{Libraries: []model.PID{lib.PID}})
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(scoped.Episodes) != 1 || scoped.Episodes[0].PID != downloaded {
		t.Errorf("scoped transcript hits = %+v, want only the downloaded episode", scoped.Episodes)
	}

	// The candidate cap composes with the transcript rung too.
	capped, err := st.Search(ctx, "zanzibar", read.SearchOptions{Libraries: []model.PID{lib.PID}, MaxCandidates: 1})
	if err != nil {
		t.Fatalf("scoped+capped: %v", err)
	}
	if len(capped.Episodes) != 1 || capped.Episodes[0].PID != downloaded {
		t.Errorf("scoped+capped transcript hits = %+v, want the downloaded episode", capped.Episodes)
	}
}

// TestSearchCapAndScopeCombined verifies the scope applies inside the candidate
// pool: newer out-of-scope matches must not consume the cap and starve an older
// in-scope match.
func TestSearchCapAndScopeCombined(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/other"), DisplayRoot: "/other", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("second library: %v", err)
	}
	// Oldest match is the only in-scope one; four newer matches live elsewhere.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/only.flac", essence: "e0", content: "c0",
		title: "Meridian Home", artist: "A", album: "Alp"})
	for i := 1; i <= 4; i++ {
		putTrack(t, st, lib2.ID, trackSpec{
			path: "/other/" + strconv.Itoa(i) + ".flac", essence: "e" + strconv.Itoa(i), content: "c" + strconv.Itoa(i),
			title: "Meridian " + strconv.Itoa(i), artist: "B", album: "Bet"})
	}

	got, err := st.Search(ctx, "meridian", read.SearchOptions{
		Libraries: []model.PID{lib.PID}, MaxCandidates: 2,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got.Tracks) != 1 || got.Tracks[0].Title != "Meridian Home" {
		t.Errorf("tracks = %+v, want the lone in-scope match (scope must sit inside the pool)", got.Tracks)
	}
	if got.Truncated {
		t.Error("one in-scope match under a cap of two is not a truncation")
	}
}

// firstTitle renders a hit list's leading title for failure messages.
func firstTitle(hits []read.SearchHit) string {
	if len(hits) == 0 {
		return ""
	}
	return hits[0].Title
}
