package sqlite

import (
	"context"
	"reflect"
	"strconv"
	"strings"
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
	stmt, args, cap := searchStmt("beatles*", 20, 0, nil, nil)
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

// transcriptFixture builds a show with one downloaded and one undownloaded episode,
// each carrying a transcript that mentions "zanzibar", and returns their item pids.
// The downloaded one is present and in lib; the remote one has no file at all.
func transcriptFixture(t *testing.T) (*Store, *model.Library, model.PID, model.PID) {
	t.Helper()
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
	return st, lib, downloaded, remote
}

// TestSearchScopeCoversTranscripts verifies the library scope reaches the
// transcript rung: a transcript hit for an undownloaded episode (no file, so no
// library) drops out of a scoped search but still surfaces unscoped.
func TestSearchScopeCoversTranscripts(t *testing.T) {
	st, lib, downloaded, _ := transcriptFixture(t)
	ctx := context.Background()

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

// TestSearchStateNarrowing mirrors TestSearchLibraryScope for the state allow-list:
// an archived item stays searchable unnarrowed (it keeps its FTS row, which is what
// makes States: [archived] work at all), each single state selects its own item, two
// states select both, and an unknown state errors instead of narrowing to nothing.
func TestSearchStateNarrowing(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	live := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1",
		title: "Harbor Lights", artist: "A", album: "Alp"})
	gone := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "e2", content: "c2",
		title: "Harbor Nights", artist: "B", album: "Bet"})
	// After the last putTrack for the item: upsertItem rewrites state on every rescan.
	if err := st.DetachFile(ctx, gone.FilePID); err != nil {
		t.Fatalf("detach: %v", err)
	}

	full, err := st.Search(ctx, "harbor", read.SearchOptions{})
	if err != nil {
		t.Fatalf("unnarrowed: %v", err)
	}
	if len(full.Tracks) != 2 {
		t.Fatalf("unnarrowed tracks = %d, want 2 (an archived item keeps its FTS row)", len(full.Tracks))
	}

	present, err := st.Search(ctx, "harbor", read.SearchOptions{States: []model.ItemState{model.StatePresent}})
	if err != nil {
		t.Fatalf("present: %v", err)
	}
	if len(present.Tracks) != 1 || present.Tracks[0].PID != model.PID(live.ItemPID) {
		t.Errorf("present tracks = %+v, want only the live item", present.Tracks)
	}

	archived, err := st.Search(ctx, "harbor", read.SearchOptions{States: []model.ItemState{model.StateArchived}})
	if err != nil {
		t.Fatalf("archived: %v", err)
	}
	if len(archived.Tracks) != 1 || archived.Tracks[0].PID != model.PID(gone.ItemPID) {
		t.Errorf("archived tracks = %+v, want only the archived item", archived.Tracks)
	}

	both, err := st.Search(ctx, "harbor", read.SearchOptions{
		States: []model.ItemState{model.StatePresent, model.StateArchived},
	})
	if err != nil {
		t.Fatalf("two states: %v", err)
	}
	if len(both.Tracks) != 2 {
		t.Errorf("two-state tracks = %d, want 2", len(both.Tracks))
	}

	if _, err := st.Search(ctx, "harbor", read.SearchOptions{
		States: []model.ItemState{"nope"},
	}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown state = %v, want CodeInvalid", err)
	}
}

// TestSearchStateAndScopeCompose pins the orthogonality the option doc claims: a
// missing item keeps its file row and its item_file edges, so a library scope still
// admits it and only States can remove it.
func TestSearchStateAndScopeCompose(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	live := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1",
		title: "Harbor Lights", artist: "A", album: "Alp"})
	absent := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "e2", content: "c2",
		title: "Harbor Nights", artist: "B", album: "Bet"})
	if n, err := st.MarkFilesMissing(ctx, []model.PID{absent.FilePID}); err != nil || n != 1 {
		t.Fatalf("MarkFilesMissing = %d, %v", n, err)
	}

	scoped, err := st.Search(ctx, "harbor", read.SearchOptions{Libraries: []model.PID{lib.PID}})
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(scoped.Tracks) != 2 {
		t.Fatalf("scoped tracks = %d, want 2 (a missing item keeps its file row, so the scope admits it)",
			len(scoped.Tracks))
	}

	// Fully loaded: the scope, the narrowing, and the cap together, so the whole bind
	// order actually executes.
	both, err := st.Search(ctx, "harbor", read.SearchOptions{
		Libraries:     []model.PID{lib.PID},
		States:        []model.ItemState{model.StatePresent},
		MaxCandidates: 10,
	})
	if err != nil {
		t.Fatalf("scope+states+cap: %v", err)
	}
	if len(both.Tracks) != 1 || both.Tracks[0].PID != model.PID(live.ItemPID) {
		t.Errorf("scope+states tracks = %+v, want only the present item", both.Tracks)
	}
}

// TestSearchCapAndStatesCombined mirrors TestSearchCapAndScopeCombined and is the
// discriminating test for the capped splice: the narrowing must apply inside the
// candidate pool, so newer excluded matches cannot consume the cap and starve an
// older included one. A predicate applied outside the pool returns nothing here.
func TestSearchCapAndStatesCombined(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Oldest match is the only one left present; four newer matches get archived.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/only.flac", essence: "e0", content: "c0",
		title: "Meridian Home", artist: "A", album: "Alp"})
	for i := 1; i <= 4; i++ {
		r := putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + strconv.Itoa(i) + ".flac", essence: "e" + strconv.Itoa(i), content: "c" + strconv.Itoa(i),
			title: "Meridian " + strconv.Itoa(i), artist: "B", album: "Bet"})
		if err := st.DetachFile(ctx, r.FilePID); err != nil {
			t.Fatalf("detach %d: %v", i, err)
		}
	}

	got, err := st.Search(ctx, "meridian", read.SearchOptions{
		States: []model.ItemState{model.StatePresent}, MaxCandidates: 2,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got.Tracks) != 1 || got.Tracks[0].Title != "Meridian Home" {
		t.Errorf("tracks = %+v, want the lone present match (states must sit inside the pool)", got.Tracks)
	}
	if got.Truncated {
		t.Error("one included match under a cap of two is not a truncation")
	}
}

// TestSearchStatesCoverTranscripts mirrors TestSearchScopeCoversTranscripts for the
// state narrowing, including the case no library scope can express: States [remote]
// searches the unfetched backlog.
func TestSearchStatesCoverTranscripts(t *testing.T) {
	st, _, downloaded, remote := transcriptFixture(t)
	ctx := context.Background()

	present, err := st.Search(ctx, "zanzibar", read.SearchOptions{
		States: []model.ItemState{model.StatePresent},
	})
	if err != nil {
		t.Fatalf("present: %v", err)
	}
	if len(present.Episodes) != 1 || present.Episodes[0].PID != downloaded {
		t.Errorf("present transcript hits = %+v, want only the downloaded episode", present.Episodes)
	}

	backlog, err := st.Search(ctx, "zanzibar", read.SearchOptions{
		States: []model.ItemState{model.StateRemote},
	})
	if err != nil {
		t.Fatalf("remote: %v", err)
	}
	if len(backlog.Episodes) != 1 || backlog.Episodes[0].PID != remote {
		t.Errorf("remote transcript hits = %+v, want only the unfetched episode", backlog.Episodes)
	}

	// The candidate cap composes with the narrowed transcript rung too.
	capped, err := st.Search(ctx, "zanzibar", read.SearchOptions{
		States: []model.ItemState{model.StateRemote}, MaxCandidates: 1,
	})
	if err != nil {
		t.Fatalf("remote+capped: %v", err)
	}
	if len(capped.Episodes) != 1 || capped.Episodes[0].PID != remote {
		t.Errorf("remote+capped transcript hits = %+v, want the unfetched episode", capped.Episodes)
	}
}

// TestSearchStmtNarrowArgOrder walks all eight statement shapes (flat and capped, by
// nothing / scope / states / both) and pins that every placeholder has an arg and
// that the args arrive in clause order. The statement text and its binds are built
// in two places, so a transposition is cheap to introduce and only shows up on one
// combination behaviourally. limit and maxCandidates are chosen so the inner (601)
// and outer (501) limits differ and a swap is visible.
func TestSearchStmtNarrowArgOrder(t *testing.T) {
	const (
		limit = 20
		maxC  = 600
		outer = 501 // searchFetchCap(20) + 1
		inner = 601 // maxCandidates + 1
	)
	libs := []int64{7, 9}
	states := []model.ItemState{model.StatePresent, model.StateRemote}
	cases := []struct {
		name   string
		maxC   int
		libIDs []int64
		states []model.ItemState
		want   []any
	}{
		{"flat/none", 0, nil, nil, []any{"x*", outer}},
		{"flat/scope", 0, libs, nil, []any{int64(7), int64(9), "x*", outer}},
		{"flat/states", 0, nil, states, []any{"present", "remote", "x*", outer}},
		{"flat/both", 0, libs, states, []any{"present", "remote", int64(7), int64(9), "x*", outer}},
		{"capped/none", maxC, nil, nil, []any{"x*", inner, outer}},
		{"capped/scope", maxC, libs, nil, []any{int64(7), int64(9), "x*", inner, outer}},
		{"capped/states", maxC, nil, states, []any{"present", "remote", "x*", inner, outer}},
		{"capped/both", maxC, libs, states, []any{"present", "remote", int64(7), int64(9), "x*", inner, outer}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmt, args, _ := searchStmt("x*", limit, tc.maxC, tc.libIDs, tc.states)
			if n := strings.Count(stmt, "?"); n != len(args) {
				t.Errorf("%d placeholders but %d args:\n%s", n, len(args), stmt)
			}
			if !reflect.DeepEqual(args, tc.want) {
				t.Errorf("args = %#v, want %#v", args, tc.want)
			}
		})
	}
}

// TestSearchNarrowPlan pins that the narrowing stays a residual test on a row already
// in hand rather than turning the search into a scan-and-sort. playable_item.state
// has no index and needs none: MATCH forces the FTS table to be the outer loop, so pi
// is always reached by an integer primary key seek.
//
// The capped statement's candidate pool is the `c` co-routine, and a sort inside it
// would mean the documented rowid-DESC recency bias had quietly become
// materialize-and-sort-everything. The outer query sorts by bm25 score and so has a
// temp b-tree of its own, in every shape including the pre-option one, which is why
// the assertion is scoped to the co-routine rather than the whole plan.
func TestSearchNarrowPlan(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1",
		title: "Harbor Lights", artist: "A", album: "Alp"})

	flat, flatArgs, _ := searchStmt("harbor*", 20, 0, nil, []model.ItemState{model.StatePresent})
	plan := explainPlan(t, st, flat, flatArgs...)
	t.Logf("flat narrowed plan:\n%s", plan)
	if !strings.Contains(plan, "SEARCH pi USING INTEGER PRIMARY KEY") {
		t.Errorf("pi is no longer reached by rowid seek, so the state test is not a residual:\n%s", plan)
	}

	capped, cappedArgs, _ := searchStmt("harbor*", 20, 50, nil, []model.ItemState{model.StatePresent})
	lines := explainPlanLines(t, st, capped, cappedArgs...)
	cplan := strings.Join(lines, "\n")
	t.Logf("capped narrowed plan:\n%s", cplan)
	if !strings.Contains(cplan, "SEARCH pi USING INTEGER PRIMARY KEY") {
		t.Errorf("capped inner query lost its rowid seek on pi:\n%s", cplan)
	}
	pool := lines
	for i, l := range lines {
		if strings.Contains(l, "SCAN c") {
			pool = lines[:i]
			break
		}
	}
	if len(pool) == len(lines) {
		t.Fatalf("no SCAN c in the plan, so the candidate pool is not a co-routine at all:\n%s", cplan)
	}
	for _, l := range pool {
		if strings.Contains(l, "TEMP B-TREE") {
			t.Errorf("the candidate pool is materialized and sorted, not walked in rowid order:\n%s", cplan)
		}
	}
}

// firstTitle renders a hit list's leading title for failure messages.
func firstTitle(hits []read.SearchHit) string {
	if len(hits) == 0 {
		return ""
	}
	return hits[0].Title
}
