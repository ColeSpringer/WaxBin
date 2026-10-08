package sqlite

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
	"golang.org/x/text/unicode/norm"
)

func TestSearchGroupsAndMatches(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	cases := map[string]string{
		"Beatles":                  `"beatles"*`,
		"AC/DC":                    `("ac dc"* OR "acdc"* OR ("ac"* AND "dc"*))`,
		"  hello  ":                `"hello"*`,
		"!!!":                      "",
		"$":                        "",
		"Sgt. Pepper":              `"sgt"* AND "pepper"*`,
		"OR":                       `"or"*`, // inside quotes: a plain term, not the FTS operator
		`say "hi"`:                 `"say"* AND "hi"*`,
		"Pi'erre":                  `("pi erre"* OR "pierre"* OR ("pi"* AND "erre"*))`,
		"ガンダム・シード":                 `("ガンダム シード"* OR "がんだむ しーど"* OR "ガンダムシード"* OR "がんだむしーど"* OR ("がんだむ"* AND "しーど"*))`,
		"Ty Dolla $ign":            `"ty"* AND "dolla"* AND ("ign"* OR "sign"*)`,
		"Ty & Dolla":               `"ty"* AND "dolla"*`,
		"スパイス":                     `("スパイス"* OR "すぱいす"* OR "すぱ ぱい いす")`,
		"東京":                       `"東京"*`,
		"がんだむ":                     `("がんだむ"* OR "がん んだ だむ")`,
		"Ｆｕｌｌ":                     `("ｆｕｌｌ"* OR "full"*)`,
		"हिन्दी":                   `"हिन्दी"*`,
		"ครับ":                     `("ครับ"* OR "ครั รับ")`,
		norm.NFD.String("Pokémon"): `"pokémon"*`,
		// A mark alone folds to nothing in the tokenizer, an empty prefix matching
		// every row, so a word with no letter or digit is dropped.
		"\u0301":       "",
		"halo \u0301":  `"halo"*`,
		"\u0301\u0302": "",
	}
	for in, want := range cases {
		if got := ftsMatchQuery(in); got != want {
			t.Errorf("ftsMatchQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSearchFindsWhatPeopleType: a word typed without its inner punctuation, a dollar
// sign read as an s, a symbol-only title, CJK inside a run in either kana, a Thai word
// inside a title, decomposed input, and a whole Indic word.
func TestSearchFindsWhatPeopleType(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, tr := range []struct{ title, artist string }{
		{"Cocaine 80's", "Joey Bada$$"},
		{"That’s What I Like", "Bruno Mars"},
		{"You're the One", "Kaytranada"},
		{"Mask Off", "Pi'erre Bourne"},
		{"Mount Olympus", "Big K.R.I.T."},
		{"Healing", "A.CHAL"},
		{"Bottom of the Bottle", "Curren$y"},
		{"Or Nah", "Ty Dolla $ign"},
		{"$", "Symbols"},
		{"東京スパイス", "Spice"},
		{"ガンダム", "Sunrise"},
		{"マシーンLOVE", "Machine"},
		{"純愛", "Junai"},
		{"Pokémon Theme", "Jason Paige"},
		{"हिन्दी", "Hindi"},
		// Splitting at the marks would cut both into the same three letters.
		{"हानोदे", "Other"},
		{"สวัสดีครับ", "Thai"},
		{"한국 노래", "Korean"},
		{"Thats Life", "Sinatra"},
		{"Tik Tok", "Kesha"},
		{"Rock and Roll", "Led Zeppelin"},
		{"Hello", "Adele"},
	} {
		n := strconv.Itoa(i)
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: tr.title, artist: tr.artist, album: "Album " + n})
	}
	for _, c := range []struct{ q, want string }{
		{"Cocaine 80s", "Cocaine 80's"},
		{"Thats What I Like", "That’s What I Like"},
		{"Youre the one", "You're the One"},
		{"Pierre", "Mask Off"},
		{"krit", "Mount Olympus"},
		{"achal", "Healing"},
		{"Curren$y", "Bottom of the Bottle"},
		{"currensy", "Bottom of the Bottle"},
		{"Ty Dolla Sign", "Or Nah"},
		{"Ty Dolla $ign", "Or Nah"},
		{"$", "$"},
		{"スパイス", "東京スパイス"},
		{"すぱいす", "東京スパイス"},
		{"東京", "東京スパイス"},
		{"がんだむ", "ガンダム"},
		{norm.NFD.String("Pokémon"), "Pokémon Theme"},
		{"हिन्दी", "हिन्दी"},
		{"ครับ", "สวัสดีครับ"},
		{norm.NFD.String("한국"), "한국 노래"},
		{"スパ", "東京スパイス"},
		{"愛", "純愛"},
		{"love", "マシーンLOVE"},
		{"That's Life", "Thats Life"},
		{"Ke$ha", "Tik Tok"},
		{"Joey Badass", "Cocaine 80's"},
		// The words either side of inner punctuation can sit apart, or in other columns.
		{"rock&roll", "Rock and Roll"},
		{"Adele-Hello", "Hello"},
		{"Adele_Hello", "Hello"},
	} {
		res, err := st.Search(ctx, c.q, read.SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", c.q, err)
		}
		if len(res.Tracks) != 1 || res.Tracks[0].Title != c.want {
			t.Errorf("search %q = %+v, want only %q", c.q, res.Tracks, c.want)
		}
	}
	if res, err := st.Search(ctx, "\u0301", read.SearchOptions{}); err != nil || !res.Empty() {
		t.Errorf("search of a lone combining mark = %+v (err %v), want nothing", res, err)
	}
}

// TestSearchSymbolOnlyQueryFindsBySortKey: a query with no word in it looks the text up
// by sort key among item titles, album titles and track artists, under the same
// narrowing. An album gets such a title only when an MBID keys it (a title with no
// letter keys no album otherwise), and no artist entity ever gets such a name, so a
// band named "!!!" is found through its tracks.
func TestSearchSymbolOnlyQueryFindsBySortKey(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Perfect", artist: "Ed Sheeran", album: "÷", albumArt: "Ed Sheeran",
		mbReleaseGroup: "e0000000-0000-4000-8000-000000000001", mbRelease: "e0000000-0000-4000-8000-000000000002"})
	dollar := putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "$", artist: "Someone", album: "Money"})
	band := putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3",
		title: "Myth Takes", artist: "!!!", album: "Myth Takes"})
	// The band is found by the name it is credited under, whatever its sort tag says and
	// wherever it sits in a list of artists.
	sorted := trackSpecInput(lib.ID, trackSpec{path: "/lib/4.flac", essence: "e4", content: "c4",
		title: "Shake the Shudder", artist: "!!!", album: "Shake the Shudder"})
	sorted.Track.ArtistSort = model.SortKey("Chk Chk Chk")
	if _, err := st.PutScannedTrack(ctx, sorted); err != nil {
		t.Fatalf("put: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/5.flac", essence: "e5", content: "c5",
		title: "First Credit", artist: "!!!, Someone", artists: []string{"!!!", "Someone"}, album: "Collabs"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/6.flac", essence: "e6", content: "c6",
		title: "Second Credit", artist: "Someone, !!!", artists: []string{"Someone", "!!!"}, album: "Collabs"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/7.flac", essence: "e7", content: "c7",
		title: "Not The Band", artist: "Wow!!!", album: "Other"})
	found, err := st.Search(ctx, "!!!", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var titles []string
	for _, h := range found.Tracks {
		titles = append(titles, h.Title)
	}
	slices.Sort(titles)
	if want := []string{"First Credit", "Myth Takes", "Second Credit", "Shake the Shudder"}; !slices.Equal(titles, want) {
		t.Errorf("search !!! = %v, want %v", titles, want)
	}
	if !slices.ContainsFunc(found.Tracks, func(h read.SearchHit) bool { return h.PID == band.ItemPID }) {
		t.Errorf("search !!! = %+v, want the band's track", found.Tracks)
	}

	found, err = st.Search(ctx, "÷", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found.Albums) != 1 || found.Albums[0].Title != "÷" || found.Albums[0].Subtitle != "Ed Sheeran" || !found.Albums[0].Exact {
		t.Errorf("albums = %+v, want ÷ by Ed Sheeran, exact", found.Albums)
	}
	if len(found.Tracks) != 0 {
		t.Errorf("tracks = %+v, want none (no title is ÷)", found.Tracks)
	}
	found, err = st.Search(ctx, "$", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found.Tracks) != 1 || found.Tracks[0].PID != dollar.ItemPID || len(found.Albums) != 1 || found.Albums[0].Title != "Money" {
		t.Errorf("search $ = %+v, want the track titled $ and its album", found)
	}
	// The narrowing applies: a state nothing is in hides all of it.
	for _, q := range []string{"÷", "$", "!!!"} {
		found, err = st.Search(ctx, q, read.SearchOptions{States: []model.ItemState{model.StateArchived}})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if !found.Empty() {
			t.Errorf("archived-only search %q = %+v, want nothing", q, found)
		}
	}
}

// TestSearchSymbolOnlyQueryHonorsTheCap: the sort-key lookup reads one row past its
// cap like the ranked search, so a full result reports Truncated, and a candidate cap
// keeps the newest matches.
func TestSearchSymbolOnlyQueryHonorsTheCap(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	var pids []model.PID
	for i := range 3 {
		n := strconv.Itoa(i)
		pids = append(pids, putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "$", artist: "Artist " + n, album: "Album " + n}).ItemPID)
	}
	res, err := st.Search(ctx, "$", read.SearchOptions{MaxCandidates: 2})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !res.Truncated || len(res.Tracks) != 2 || res.Tracks[0].PID != pids[2] || res.Tracks[1].PID != pids[1] {
		t.Errorf("capped search $ = truncated %t, tracks %+v; want the newest two and truncated", res.Truncated, res.Tracks)
	}
	if res, err := st.Search(ctx, "$", read.SearchOptions{}); err != nil || res.Truncated || len(res.Tracks) != 3 {
		t.Errorf("uncapped search $ = %+v (err %v), want all three, not truncated", res, err)
	}
}

// TestSearchValidatesOptionsBeforeTheQuery: an unknown library or state is refused
// whatever the query holds, a symbol-only or empty one included.
func TestSearchValidatesOptionsBeforeTheQuery(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	for _, q := range []string{"abc", "!!!", ""} {
		if _, err := st.Search(ctx, q, read.SearchOptions{Libraries: []model.PID{"nope"}}); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Errorf("search %q in an unknown library: err = %v, want CodeNotFound", q, err)
		}
		if _, err := st.Search(ctx, q, read.SearchOptions{States: []model.ItemState{"bogus"}}); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("search %q in an unknown state: err = %v, want CodeInvalid", q, err)
		}
	}
}

// TestSearchStmtZeroPathGolden pins the option-free statement to its exact text, the
// flat FTS query with no candidate wrap and no narrowing, so the default path changes
// only when this golden does.
func TestSearchStmtZeroPathGolden(t *testing.T) {
	t.Parallel()
	want := `SELECT pi.pid, pi.kind, pi.title,
		COALESCE(NULLIF(t.artist,''), bk.author, pod.title, ''), COALESCE(t.album_artist,''),
		COALESCE(art.pid,''), COALESCE(art.name,''), COALESCE(al.pid,''), COALESCE(al.title,''), ` + searchBM25 + ` AS score
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestTranscriptSearchFoldsScripts: a transcript body is indexed with the script folds
// the metadata columns get (pairs over unspaced runs, either kana, compatibility forms,
// marks inside words), so a word inside a Japanese or Thai sentence is found. Its
// punctuation-joined forms are left out: a body is often raw subtitle markup, where
// they would index a junk word for every timestamp.
func TestTranscriptSearchFoldsScripts(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	res, err := st.UpsertFeed(ctx, model.UpsertFeedInput{FeedURL: "http://feed.example/t", IdentityKey: "podcast:feed.example/t",
		Feed: model.Feed{Title: "Show", Episodes: []model.FeedEpisode{
			{GUID: "ja", Title: "Episode One", EnclosureURL: "http://feed.example/ja.mp3"},
			{GUID: "th", Title: "Episode Two", EnclosureURL: "http://feed.example/th.mp3"},
		}}, FetchedAtNS: 1})
	if err != nil {
		t.Fatalf("upsert feed: %v", err)
	}
	eps, err := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v)", len(eps), err)
	}
	// Split at their marks, the two Hindi words would cut into the same three letters.
	bodies := map[string]string{"Episode One": "今日は東京スパイスの話をします हानोदे", "Episode Two": "สวัสดีครับ ทุกคน हिन्दी"}
	for _, ep := range eps {
		if err := st.PutTranscript(ctx, model.PutTranscriptInput{EpisodePID: ep.PID, Format: "text", Body: bodies[ep.Title]}); err != nil {
			t.Fatalf("transcript: %v", err)
		}
	}
	for q, want := range map[string]string{"スパイス": "Episode One", "すぱいす": "Episode One", "ครับ": "Episode Two", "हिन्दी": "Episode Two"} {
		found, err := st.Search(ctx, q, read.SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(found.Episodes) != 1 || found.Episodes[0].Title != want {
			t.Errorf("search %q episodes = %+v, want %s through its transcript", q, found.Episodes, want)
		}
	}
}

// TestProseJoinsWordsNotNumbers: show notes and transcripts hold a word run together
// across its inner punctuation as a title does, so "kesha" finds Ke$ha in either, but not
// a run of digits, which in prose is a number or a time: a chapter mark at 19:45 is no
// 1945. A title keeps those, so "444" finds 4:44.
func TestProseJoinsWordsNotNumbers(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res, err := st.UpsertFeed(ctx, model.UpsertFeedInput{FeedURL: "http://feed.example/p", IdentityKey: "podcast:feed.example/p",
		Feed: model.Feed{Title: "Show", Episodes: []model.FeedEpisode{
			{GUID: "n", Title: "Notes", EnclosureURL: "http://feed.example/n.mp3", Description: "<p>19:45 Pi'erre on the beat</p>"},
			{GUID: "s", Title: "Spoken", EnclosureURL: "http://feed.example/s.mp3"},
		}}, FetchedAtNS: 1})
	if err != nil {
		t.Fatalf("upsert feed: %v", err)
	}
	eps, err := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v)", len(eps), err)
	}
	for _, ep := range eps {
		if ep.Title == "Spoken" {
			if err := st.PutTranscript(ctx, model.PutTranscriptInput{EpisodePID: ep.PID, Format: "text",
				Body: "we played Ke$ha at 20:01, don't miss it"}); err != nil {
				t.Fatalf("transcript: %v", err)
			}
		}
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "4:44", artist: "JAY-Z", album: "4:44"})
	for _, c := range []struct{ q, episode, track string }{
		{"pierre", "Notes", ""}, {"kesha", "Spoken", ""}, {"dont", "Spoken", ""},
		{"1945", "", ""}, {"2001", "", ""}, {"444", "", "4:44"},
	} {
		found, err := st.Search(ctx, c.q, read.SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", c.q, err)
		}
		if got := firstTitle(found.Episodes); len(found.Episodes) > 1 || got != c.episode {
			t.Errorf("search %q episodes = %+v, want %q", c.q, found.Episodes, c.episode)
		}
		if got := firstTitle(found.Tracks); len(found.Tracks) > 1 || got != c.track {
			t.Errorf("search %q tracks = %+v, want %q", c.q, found.Tracks, c.track)
		}
	}
	if got := proseIndexText("1\n00:00:01,000 --> 00:00:04,000\nDon't stop"); !strings.Contains(got, "dont") || strings.Contains(got, "000001000") {
		t.Errorf("prose index text = %q, want the word joined and not the timestamp", got)
	}
}

// TestSearchStateNarrowing mirrors TestSearchLibraryScope for the state allow-list:
// an archived item stays searchable unnarrowed (it keeps its FTS row, which is what
// makes States: [archived] work at all), each single state selects its own item, two
// states select both, and an unknown state errors instead of narrowing to nothing.
func TestSearchStateNarrowing(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	live := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1",
		title: "Harbor Lights", artist: "A", album: "Alp"})
	gone := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "e2", content: "c2",
		title: "Harbor Nights", artist: "B", album: "Bet"})
	// After the last putTrack for the item: upsertItem rewrites state on every rescan.
	if _, err := st.DetachFile(ctx, gone.FilePID); err != nil {
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
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	live := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1",
		title: "Harbor Lights", artist: "A", album: "Alp"})
	absent := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "e2", content: "c2",
		title: "Harbor Nights", artist: "B", album: "Bet"})
	if r, err := st.MarkFilesMissing(ctx, []model.PID{absent.FilePID}); err != nil || r.Marked != 1 {
		t.Fatalf("MarkFilesMissing = %+v, %v", r, err)
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
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Oldest match is the only one left present; four newer matches get archived.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/only.flac", essence: "e0", content: "c0",
		title: "Meridian Home", artist: "A", album: "Alp"})
	for i := 1; i <= 4; i++ {
		r := putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + strconv.Itoa(i) + ".flac", essence: "e" + strconv.Itoa(i), content: "c" + strconv.Itoa(i),
			title: "Meridian " + strconv.Itoa(i), artist: "B", album: "Bet"})
		if _, err := st.DetachFile(ctx, r.FilePID); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestSearchByNamePlans pins that the direct name lookups seek their indexes rather than
// scan their tables on every search, the space-dropped artist key included
// (artist_joined_key), with a narrowing as with none, and that the sort-key item lookup a
// query of symbols alone runs seeks its keys and reads only the track table whole, for
// the artist display.
func TestSearchByNamePlans(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	for _, states := range [][]model.ItemState{nil, {model.StatePresent}} {
		narrow, args := searchNarrow([]int64{1}, states)
		artists := strings.Join(explainPlanLines(t, st, artistByNameQ(narrow, 2),
			append(append(append([]any{"a", "b", "a"}, args...), args...), 5)...), "\n")
		for _, want := range []string{"MULTI-INDEX OR", "artist_joined_key", "artist_sort"} {
			if !strings.Contains(artists, want) {
				t.Errorf("artist lookup plan lacks %q:\n%s", want, artists)
			}
		}
		if strings.Contains(artists, "SCAN art") {
			t.Errorf("artist lookup scans the table:\n%s", artists)
		}
		albums := strings.Join(explainPlanLines(t, st, albumByNameQ(narrow), append(append([]any{"a"}, args...), 5)...), "\n")
		if !strings.Contains(albums, "SEARCH al USING INDEX album_sort") {
			t.Errorf("album lookup does not seek album_sort:\n%s", albums)
		}
		items := strings.Join(explainPlanLines(t, st, itemsBySortKeyQ(narrow), append(append([]any{"a"}, args...), 5)...), "\n")
		for _, want := range []string{"item_sort", "track_artist", "SCAN track", "SCAN m", "SEARCH pi USING INTEGER PRIMARY KEY"} {
			if !strings.Contains(items, want) {
				t.Errorf("sort-key item lookup plan lacks %q:\n%s", want, items)
			}
		}
		if strings.Contains(items, "SCAN pi") {
			t.Errorf("sort-key item lookup scans playable_item:\n%s", items)
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

// searchRowCol reads one column of an item's search row.
func searchRowCol(t *testing.T, st *Store, pid model.PID, col string) string {
	t.Helper()
	return scalarStr(t, st, "SELECT "+col+" FROM search_fts WHERE rowid = (SELECT id FROM playable_item WHERE pid = ?)", string(pid))
}

// TestSearchRowCarriesCredits: the credits column holds the composer and every credited
// contributor the artist column does not already name, so a producer or a lyricist is
// searchable and a performer named in the artist column is not counted twice.
func TestSearchRowCarriesCredits(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Apeshit", artist: "The Carters", artists: []string{"Beyoncé", "JAY-Z"},
		album: "Everything Is Love", composer: "Pharrell Williams"})
	for _, c := range []struct {
		role  model.ContributorRole
		names []string
	}{{model.RoleProducer, []string{"Pharrell Williams"}}, {model.RoleLyricist, []string{"Quavo"}}} {
		if _, _, err := st.SetItemCredits(ctx, res.ItemPID, c.role, c.names,
			model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
			t.Fatalf("set %s credit: %v", c.role, err)
		}
	}
	credits := searchRowCol(t, st, res.ItemPID, "credits")
	for _, want := range []string{"Pharrell Williams", "Quavo", "Beyoncé", "JAY-Z"} {
		if !strings.Contains(credits, want) {
			t.Errorf("credits = %q, want it to carry %q", credits, want)
		}
	}
	if n := strings.Count(credits, "Pharrell"); n != 1 {
		t.Errorf("credits = %q names the composer-producer %d times, want once", credits, n)
	}
	if strings.Contains(credits, "Carters") {
		t.Errorf("credits = %q repeats the artist column", credits)
	}
	found, err := st.Search(ctx, "quavo", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found.Tracks) != 1 || found.Tracks[0].PID != res.ItemPID {
		t.Errorf("search quavo = %+v, want the credited track", found.Tracks)
	}
}

// TestSearchColumnsKeepTheRawTextFirst: each column holds its raw text, then the
// alternate forms of its words, so nothing that read the raw text loses it.
func TestSearchColumnsKeepTheRawTextFirst(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "That’s What I Like", artist: "Bruno Mars", album: "24K Magic"})
	title := searchRowCol(t, st, res.ItemPID, "title")
	if !strings.HasPrefix(title, "That’s What I Like") || !strings.HasSuffix(title, "thats") {
		t.Errorf("title column = %q, want the raw title followed by its alternate forms", title)
	}
	if got := searchRowCol(t, st, res.ItemPID, "artist"); got != "Bruno Mars" {
		t.Errorf("artist column = %q, want the plain artist with nothing appended", got)
	}
	// The performer credit is named by the artist column already.
	if got := searchRowCol(t, st, res.ItemPID, "credits"); got != "" {
		t.Errorf("credits = %q, want nothing beside the artist column", got)
	}
}

// TestSearchKindIsNotIndexed: the kind column used to be indexed, so "t" prefix-matched
// the word "track" on every track row.
func TestSearchKindIsNotIndexed(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Hello", artist: "Xavier", album: "Al"})
	res, err := st.Search(context.Background(), "t", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Tracks) != 0 {
		t.Errorf("search t = %+v, want no track (nothing it holds starts with t)", res.Tracks)
	}
}

// TestComposerEditRebuildsTheSearchRow: a composer edit routes through neither the
// entity re-resolve nor the title branch, so it used to leave the row stale.
func TestComposerEditRebuildsTheSearchRow(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "The Ecstasy of Gold", artist: "Orchestra", album: "Al", composer: "Nobody"})
	if err := st.EditItemField(ctx, res.ItemPID, "composer", "Ennio Morricone",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit composer: %v", err)
	}
	found, err := st.Search(ctx, "morricone", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found.Tracks) != 1 {
		t.Errorf("search morricone = %+v, want the edited track", found.Tracks)
	}
	if strings.Contains(searchRowCol(t, st, res.ItemPID, "credits"), "Nobody") {
		t.Error("the old composer is still in the search row")
	}
}

// TestComposerEditOverACreditLeavesItUnsearchable: a plain composer edit that moves the
// composer away from an earlier composer credit retires that credit (supersedeTwinTx),
// so its names leave the search row with it.
func TestComposerEditOverACreditLeavesItUnsearchable(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Main Theme", artist: "Orchestra", album: "Al"})
	if _, _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleComposer, []string{"Hans Zimmer"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	if err := st.EditItemField(ctx, res.ItemPID, "composer", "John Williams",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("edit composer: %v", err)
	}
	for q, want := range map[string]int{"zimmer": 0, "williams": 1} {
		found, err := st.Search(ctx, q, read.SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(found.Tracks) != want {
			t.Errorf("search %q = %+v, want %d tracks", q, found.Tracks, want)
		}
	}
}

// TestArtistMergeRebuildsCreditedRows: a merge re-points the loser's credits onto the
// survivor, whose name the credits column then carries, so the survivor's name finds a
// track still tagged with the loser's.
func TestArtistMergeRebuildsCreditedRows(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Purple Rain", artist: "Prince", album: "Purple Rain"})
	moved := putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "Gold", artist: "TAFKAP", album: "The Gold Experience"})
	survivor := model.PID(scalarStr(t, st, "SELECT pid FROM artist WHERE name = 'Prince'"))
	loser := model.PID(scalarStr(t, st, "SELECT pid FROM artist WHERE name = 'TAFKAP'"))
	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	found, err := st.Search(ctx, "prince", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var hit bool
	for _, h := range found.Tracks {
		hit = hit || h.PID == moved.ItemPID
	}
	if !hit {
		t.Errorf("search prince = %+v, want the merged artist's track too", found.Tracks)
	}
	assertVerifyClean(t, st)
}

// TestEnrichmentGenreFillRebuildsTheSearchRow: the release-group genre fill writes
// track.genre, which the search row's extra column carries, so it rebuilds the row.
func TestEnrichmentGenreFillRebuildsTheSearchRow(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Alison", artist: "Slowdive", album: "Souvlaki", albumArt: "Slowdive"})
	var rgID int64
	var rgPID string
	if err := st.rdb().QueryRowContext(ctx, `SELECT rg.id, rg.pid FROM release_group rg
		JOIN album al ON al.release_group_id = rg.id JOIN track t ON t.album_id = al.id`).Scan(&rgID, &rgPID); err != nil {
		t.Fatalf("read release group: %v", err)
	}
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{ReleaseGroupID: rgID,
		PID: model.PID(rgPID), Matched: true, MBID: "e0000000-0000-4000-8000-000000000001",
		Genres: []string{"Shoegaze"}, GenreProvider: "musicbrainz"}); err != nil {
		t.Fatalf("apply enrichment: %v", err)
	}
	found, err := st.Search(ctx, "shoegaze", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found.Tracks) != 1 || found.Tracks[0].PID != res.ItemPID {
		t.Errorf("search shoegaze = %+v, want the track the fill reached", found.Tracks)
	}
}

// TestBookSearchRowCredits: a book's credits carry its narrators and every other credit,
// a translator set by a credit edit included, and its extra column no longer repeats
// the narrator.
func TestBookSearchRowCredits(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.m4b", essence: "b1", content: "c1",
		title: "Gone Girl", author: "Gillian Flynn", narrators: []string{"Julia Whelan"}})
	if _, _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleTranslator, []string{"Edith Grossman"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set translator: %v", err)
	}
	credits := searchRowCol(t, st, res.ItemPID, "credits")
	for _, want := range []string{"Julia Whelan", "Edith Grossman"} {
		if !strings.Contains(credits, want) {
			t.Errorf("credits = %q, want it to carry %q", credits, want)
		}
	}
	if strings.Contains(credits, "Flynn") {
		t.Errorf("credits = %q repeats the author column", credits)
	}
	if extra := searchRowCol(t, st, res.ItemPID, "extra"); strings.Contains(extra, "Whelan") {
		t.Errorf("extra = %q still carries the narrator", extra)
	}
	found, err := st.Search(ctx, "grossman", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found.Books) != 1 || found.Books[0].PID != res.ItemPID {
		t.Errorf("search grossman = %+v, want the translated book", found.Books)
	}
}

// TestSearchArtistsRankOnTheirOwnNames (LIB-07): a track whose title names an artist
// outranks that artist's own tracks, and used to put its own artist first in Artists.
// The group now ranks each artist by how its name meets the query, and a hit carries
// the artist's name rather than a track's credit string.
func TestSearchArtistsRankOnTheirOwnNames(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, tr := range []struct {
		title, artist, album string
		artists              []string
	}{
		{"Peaches (feat. Giveon)", "Justin Bieber", "", nil},
		// The credit string names two artists; the hit is the entity, by its own name.
		{"Heartbreak Anniversary", "GIVĒON & Friend", "", []string{"GIVĒON", "Friend"}},
		{"Glowed Up (feat. Anderson .Paak)", "KAYTRANADA", "", nil},
		{"Come Down", "Anderson .Paak", "", nil},
		// Matched through its album, after the title match above: its name holds the
		// query inside a word, which ranks it below a name that begins with the query
		// and above one that does not hold it at all.
		{"Other Song", "Kopaak", "Paak Album", nil},
		{"B.I.G. Interlude", "Diddy", "", nil},
		{"Big Poppa", "The Notorious B.I.G.", "", nil},
	} {
		n := strconv.Itoa(i)
		album := tr.album
		if album == "" {
			album = "Album " + n
		}
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: tr.title, artist: tr.artist, artists: tr.artists, album: album})
	}
	for _, c := range []struct {
		q, first string
		exact    bool
	}{
		{"Giveon", "GIVĒON", true},
		{"paak", "Anderson .Paak", false},
		{"B.I.G", "The Notorious B.I.G.", false},
	} {
		res, err := st.Search(ctx, c.q, read.SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", c.q, err)
		}
		if len(res.Artists) < 2 || res.Artists[0].Title != c.first || res.Artists[0].Exact != c.exact {
			t.Errorf("search %q artists = %+v, want %q first (exact %t) ahead of the title match's artist", c.q, res.Artists, c.first, c.exact)
		}
		for _, h := range res.Artists[1:] {
			if h.Exact {
				t.Errorf("search %q: %+v is marked exact", c.q, h)
			}
		}
	}
	res, err := st.Search(ctx, "paak", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Artists) != 3 || res.Artists[1].Title != "Kopaak" || res.Artists[2].Title != "KAYTRANADA" {
		t.Errorf("search paak artists = %+v, want Anderson .Paak, Kopaak, KAYTRANADA", res.Artists)
	}
}

// TestSearchFindsAnArtistByNamePastTheCap: an artist whose tracks fall outside the
// ranked pool is still listed when its name is the query, and a narrowing that leaves
// none of its tracks hides it.
func TestSearchFindsAnArtistByNamePastTheCap(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	for i, a := range []string{"Love", "JAY-Z", "The Beatles", "モーニング娘。"} {
		n := strconv.Itoa(i)
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/a" + n + ".flac", essence: "a" + n, content: "a" + n,
			title: "Own Song " + n, artist: a, album: "Own Album " + n})
	}
	for i := 1; i <= 3; i++ {
		n := strconv.Itoa(i)
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "Love Song " + n + " for jayz and beatles もーにんぐ娘", artist: "Singer " + n, album: "Songs " + n})
	}
	// The pool keeps the newest two matches, so each artist's own track is outside it:
	// the name is found by its match key, by that key without its spaces, and by sort key,
	// read past fullwidth forms and either kana as the tiers read them.
	for _, c := range []struct {
		q, want string
		exact   bool
	}{
		{"love", "Love", true}, {"jayz", "JAY-Z", true}, {"beatles", "The Beatles", false},
		{"ｊａｙｚ", "JAY-Z", true}, {"もーにんぐ娘", "モーニング娘。", true},
	} {
		res, err := st.Search(ctx, c.q, read.SearchOptions{MaxCandidates: 2})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(res.Artists) == 0 || res.Artists[0].Title != c.want || res.Artists[0].Exact != c.exact {
			t.Errorf("search %q artists = %+v, want %s first (exact %t)", c.q, res.Artists, c.want, c.exact)
			continue
		}
		// Found by its name alone, it scores as well as anything the search read, so a
		// consumer still ordering by score does not sink it below the item-borne artists.
		for _, h := range res.Artists[1:] {
			if res.Artists[0].Score > h.Score {
				t.Errorf("search %q: %s scores %v, worse than %s at %v", c.q, c.want, res.Artists[0].Score, h.Title, h.Score)
			}
		}
	}
	res, err := st.Search(ctx, "love", read.SearchOptions{States: []model.ItemState{model.StateRemote}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Artists) != 0 {
		t.Errorf("remote-only artists = %+v, want none", res.Artists)
	}
}

// TestSearchAlbumsRankOnTheirOwnTitles: albums tier the same way, and one titled as
// the query is found by its title past the pool too.
func TestSearchAlbumsRankOnTheirOwnTitles(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/0.flac", essence: "e0", content: "c0",
		title: "Because", artist: "The Beatles", album: "Love", albumArt: "The Beatles"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Crazy in Love", artist: "Beyoncé", album: "Dangerously in Love", albumArt: "Beyoncé"})
	res, err := st.Search(ctx, "love", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Albums) != 2 || res.Albums[0].Title != "Love" || !res.Albums[0].Exact ||
		res.Albums[1].Title != "Dangerously in Love" || res.Albums[1].Exact {
		t.Errorf("albums = %+v, want Love (exact) then Dangerously in Love", res.Albums)
	}
	res, err = st.Search(ctx, "love", read.SearchOptions{MaxCandidates: 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Albums) == 0 || res.Albums[0].Title != "Love" || res.Albums[0].Subtitle != "The Beatles" {
		t.Errorf("capped albums = %+v, want Love by The Beatles first", res.Albums)
	}
}
