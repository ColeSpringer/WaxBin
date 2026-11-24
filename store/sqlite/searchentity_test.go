package sqlite

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
)

// hitTitles renders a group as its titles, in order.
func hitTitles(hits []read.SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Title
	}
	return out
}

func wantTitles(t *testing.T, q, group string, hits []read.SearchHit, want ...string) {
	t.Helper()
	if got := hitTitles(hits); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("search %q %s = %q, want %q", q, group, got, want)
	}
}

func search(t *testing.T, st *Store, q string, opt read.SearchOptions) *read.SearchResult {
	t.Helper()
	res, err := st.Search(context.Background(), q, opt)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	return res
}

// TestSearchBooksBringTheirPeople: a matched book brings every author and narrator it
// credits, a co-author included, so a person the query names is listed whoever the
// book's first author is, and nobody credited on books alone lands in Artists.
func TestSearchBooksBringTheirPeople(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	omens := putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.m4b", essence: "b1", content: "c1",
		title: "Good Omens", authors: []string{"Terry Pratchett", "Neil Gaiman"}, narrators: []string{"Martin Jarvis"}})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/2.m4b", essence: "b2", content: "c2",
		title: "Mort", author: "Terry Pratchett", narrators: []string{"Nigel Planer"}})

	res := search(t, st, "gaim", read.SearchOptions{})
	wantTitles(t, "gaim", "authors", res.Authors, "Neil Gaiman", "Terry Pratchett")
	wantTitles(t, "gaim", "narrators", res.Narrators, "Martin Jarvis")
	if len(res.Artists) != 0 {
		t.Errorf("search gaim artists = %+v, want none: nobody here is credited on a track", res.Artists)
	}
	for _, h := range res.Authors {
		if ref, ok := h.ArtRef(); h.Kind != "author" || h.Subtitle != "" || !ok || ref.Type != model.ArtArtist || ref.PID != h.PID {
			t.Errorf("author hit = %+v (art ref %+v), want kind author asked for art as the artist it is", h, ref)
		}
	}
	if h := res.Narrators[0]; h.Kind != "narrator" || h.Exact {
		t.Errorf("narrator hit = %+v, want kind narrator, not exact", h)
	}

	// Found by title, the book lists its people in credited order.
	res = search(t, st, "good omens", read.SearchOptions{})
	wantTitles(t, "good omens", "authors", res.Authors, "Terry Pratchett", "Neil Gaiman")

	res = search(t, st, "planer", read.SearchOptions{})
	wantTitles(t, "planer", "narrators", res.Narrators, "Nigel Planer")
	wantTitles(t, "planer", "authors", res.Authors, "Terry Pratchett")

	// The pid is the one the author facet and author_pid know the person by.
	gaiman := scalarStr(t, st, "SELECT pid FROM artist WHERE name = 'Neil Gaiman'")
	res = search(t, st, "neil gaiman", read.SearchOptions{})
	if len(res.Authors) == 0 || string(res.Authors[0].PID) != gaiman || !res.Authors[0].Exact {
		t.Errorf("search neil gaiman authors = %+v, want %s first and exact", res.Authors, gaiman)
	}

	// A translator is found through the book and is a hit in no group, by part of the
	// name or by all of it.
	if _, _, err := st.SetItemCredits(context.Background(), omens.ItemPID, model.RoleTranslator, []string{"Edith Grossman"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set translator: %v", err)
	}
	for _, q := range []string{"grossman", "edith grossman"} {
		res = search(t, st, q, read.SearchOptions{})
		wantTitles(t, q, "books", res.Books, "Good Omens")
		for _, hits := range [][]read.SearchHit{res.Artists, res.Authors, res.Narrators} {
			for _, h := range hits {
				if h.Title == "Edith Grossman" {
					t.Errorf("search %q lists the translator as %s %+v", q, h.Kind, h)
				}
			}
		}
	}
}

// TestSearchBookPeopleAreCreditedOnBooks: Authors and Narrators are the people books
// credit, as the author and narrator facets count them, so a row in one of those roles on
// anything else lists nobody.
func TestSearchBookPeopleAreCreditedOnBooks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Suzanne", artist: "Leonard Cohen", album: "Songs"})
	if _, err := st.wdb().ExecContext(context.Background(), `INSERT INTO item_contributor(item_id, artist_id, role, position)
		SELECT item_id, artist_id, 'author', 0 FROM track`); err != nil {
		t.Fatalf("credit the track's artist as an author: %v", err)
	}
	res := search(t, st, "leonard cohen", read.SearchOptions{})
	wantTitles(t, "leonard cohen", "artists", res.Artists, "Leonard Cohen")
	if len(res.Authors) != 0 {
		t.Errorf("authors = %+v, want none: no book credits him", res.Authors)
	}
}

// TestSearchOnePersonInEveryRole: an artist entity is a hit in each group whose role it
// holds, under one pid.
func TestSearchOnePersonInEveryRole(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Suzanne", artist: "Leonard Cohen", album: "Songs"})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.m4b", essence: "b1", content: "c2",
		title: "Beautiful Losers", author: "Leonard Cohen", narrators: []string{"Leonard Cohen"}})
	res := search(t, st, "cohen", read.SearchOptions{})
	for name, hits := range map[string][]read.SearchHit{"artists": res.Artists, "authors": res.Authors, "narrators": res.Narrators} {
		if len(hits) != 1 || hits[0].Title != "Leonard Cohen" || hits[0].PID != res.Artists[0].PID {
			t.Errorf("search cohen %s = %+v, want the one Leonard Cohen", name, hits)
		}
	}
}

// TestSearchFindsBookPeopleByNamePastTheCap: an author or a narrator whose books fall
// outside the ranked pool is listed when the name is the query, under the role held and
// no other, and a narrowing that leaves none of the books hides the person.
func TestSearchFindsBookPeopleByNamePastTheCap(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/other"), DisplayRoot: "/other", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("second library: %v", err)
	}
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/0.m4b", essence: "b0", content: "c0",
		title: "A Wizard of Earthsea", author: "Ursula K. Le Guin", narrators: []string{"Rob Inglis"}})
	// Newer books whose titles hold both names fill a pool of two.
	for i := 1; i <= 3; i++ {
		n := strconv.Itoa(i)
		putBook(t, st, lib2.ID, bookSpec{path: "/other/b/" + n + ".m4b", essence: "b" + n, content: "c" + n,
			title: "Ursula K. Le Guin and Rob Inglis, Volume " + n, author: "Writer " + n})
	}
	capped := read.SearchOptions{MaxCandidates: 2}
	for _, c := range []struct {
		q                string
		authors, readers bool
	}{
		{"ursula k le guin", true, false}, {"ursulakleguin", true, false}, {"rob inglis", false, true},
	} {
		res := search(t, st, c.q, capped)
		if got := len(res.Authors) > 0 && res.Authors[0].Title == "Ursula K. Le Guin" && res.Authors[0].Exact; got != c.authors {
			t.Errorf("search %q authors = %+v, want Le Guin first and exact: %t", c.q, res.Authors, c.authors)
		}
		if got := len(res.Narrators) > 0 && res.Narrators[0].Title == "Rob Inglis" && res.Narrators[0].Exact; got != c.readers {
			t.Errorf("search %q narrators = %+v, want Rob Inglis first and exact: %t", c.q, res.Narrators, c.readers)
		}
		// Found by name alone, the person scores as well as the best row read.
		for _, h := range append(res.Authors, res.Narrators...) {
			if len(res.Books) > 0 && (h.Title == "Ursula K. Le Guin" || h.Title == "Rob Inglis") && h.Score > res.Books[0].Score {
				t.Errorf("search %q: %s scores %v, worse than the best row at %v", c.q, h.Title, h.Score, res.Books[0].Score)
			}
		}
	}
	// A scope that keeps the book keeps its people. Nothing else matches inside it, so
	// they are found by name alone.
	for _, opt := range []read.SearchOptions{
		{MaxCandidates: 2, Libraries: []model.PID{lib.PID}}, {MaxCandidates: 2, PrimaryLibraries: []model.PID{lib.PID}},
	} {
		if res := search(t, st, "ursulakleguin", opt); len(res.Authors) != 1 || res.Authors[0].Title != "Ursula K. Le Guin" {
			t.Errorf("search ursulakleguin under %+v authors = %+v, want Le Guin", opt, res.Authors)
		}
		if res := search(t, st, "rob inglis", opt); len(res.Narrators) != 1 || res.Narrators[0].Title != "Rob Inglis" {
			t.Errorf("search rob inglis under %+v narrators = %+v, want Rob Inglis", opt, res.Narrators)
		}
	}
	for name, opt := range map[string]read.SearchOptions{
		"another library": {MaxCandidates: 2, Libraries: []model.PID{lib2.PID}},
		"another placing": {MaxCandidates: 2, PrimaryLibraries: []model.PID{lib2.PID}},
		"another state":   {MaxCandidates: 2, States: []model.ItemState{model.StateRemote}},
	} {
		for _, q := range []string{"ursula k le guin", "rob inglis"} {
			res := search(t, st, q, opt)
			for _, h := range append(res.Authors, res.Narrators...) {
				if h.Title == "Ursula K. Le Guin" || h.Title == "Rob Inglis" {
					t.Errorf("search %q under %s lists %s, whose only book the narrowing drops", q, name, h.Title)
				}
			}
		}
	}
}

// TestSearchBooksBringTheirSeries: a matched book brings its series, subtitled with the
// book's author, and a series named as the query is found past the pool.
func TestSearchBooksBringTheirSeries(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for i, title := range []string{"Leviathan Wakes", "Caliban's War"} {
		n := strconv.Itoa(i)
		putBook(t, st, lib.ID, bookSpec{path: "/lib/b/" + n + ".m4b", essence: "b" + n, content: "c" + n,
			title: title, author: "James S. A. Corey", series: "The Expanse", seq: strconv.Itoa(i + 1)})
	}
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/5.m4b", essence: "b5", content: "c5",
		title: "Drive", author: "Guest Writer", series: "The Expanse", seq: "0.1"})
	for i, title := range []string{"The Expanse Companion", "The Expanse Notes"} {
		n := strconv.Itoa(8 + i)
		putBook(t, st, lib.ID, bookSpec{path: "/lib/b/" + n + ".m4b", essence: "b" + n, content: "c" + n,
			title: title, author: "Someone Else"})
	}

	res := search(t, st, "expanse", read.SearchOptions{})
	wantTitles(t, "expanse", "series", res.Series, "The Expanse")
	h := res.Series[0]
	if _, ok := h.ArtRef(); h.Kind != "series" || h.Subtitle != "James S. A. Corey" || h.Exact || ok || h.ArtHash != "" {
		t.Errorf("series hit = %+v, want kind series by its author, not exact, with no art", h)
	}
	if want := scalarStr(t, st, "SELECT pid FROM series WHERE name = 'The Expanse'"); string(h.PID) != want {
		t.Errorf("series pid = %s, want %s", h.PID, want)
	}

	// The series is subtitled with the author of its first book, whichever book found it.
	res = search(t, st, "drive", read.SearchOptions{})
	if len(res.Series) != 1 || res.Series[0].Subtitle != "James S. A. Corey" {
		t.Errorf("search drive series = %+v, want The Expanse by the author of its first book", res.Series)
	}

	// The pool is the two newest matches, which are in no series.
	for _, q := range []string{"the expanse", "The-Expanse", "expanse"} {
		res = search(t, st, q, read.SearchOptions{MaxCandidates: 1})
		if len(res.Series) != 1 || res.Series[0].Title != "The Expanse" || res.Series[0].Subtitle != "James S. A. Corey" ||
			res.Series[0].Exact != (q != "expanse") {
			t.Errorf("capped search %q series = %+v, want The Expanse by its author", q, res.Series)
		}
	}
	res = search(t, st, "the expanse", read.SearchOptions{MaxCandidates: 1, States: []model.ItemState{model.StateRemote}})
	if len(res.Series) != 0 {
		t.Errorf("remote-only series = %+v, want none", res.Series)
	}
	res = search(t, st, "the expanse", read.SearchOptions{MaxCandidates: 1, Libraries: []model.PID{lib.PID}})
	if len(res.Series) != 1 || !res.Series[0].Exact {
		t.Errorf("capped series in their own library = %+v, want The Expanse", res.Series)
	}
}

// TestSearchShowFollowsItsDownloadedEpisodes: a show is in a library's search through the
// episodes downloaded there, by its rows and by its name past the pool.
func TestSearchShowFollowsItsDownloadedEpisodes(t *testing.T) {
	t.Parallel()
	st, lib, _, _ := transcriptFixture(t)
	lib2, err := st.EnsureLibrary(context.Background(), &model.Library{
		Root: []byte("/other"), DisplayRoot: "/other", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("second library: %v", err)
	}
	for i := range 2 {
		n := strconv.Itoa(i)
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "My Show Tune " + n, artist: "Singer", album: "Al"})
	}
	for _, c := range []struct {
		opt  read.SearchOptions
		want int
	}{
		{read.SearchOptions{Libraries: []model.PID{lib.PID}}, 1},
		{read.SearchOptions{MaxCandidates: 1, Libraries: []model.PID{lib.PID}}, 1},
		{read.SearchOptions{MaxCandidates: 1, PrimaryLibraries: []model.PID{lib.PID}}, 1},
		{read.SearchOptions{Libraries: []model.PID{lib2.PID}}, 0},
		{read.SearchOptions{MaxCandidates: 1, PrimaryLibraries: []model.PID{lib2.PID}}, 0},
	} {
		res := search(t, st, "my show", c.opt)
		if len(res.Podcasts) != c.want || (c.want == 1 && (res.Podcasts[0].Title != "My Show" || !res.Podcasts[0].Exact)) {
			t.Errorf("search my show under %+v podcasts = %+v, want %d", c.opt, res.Podcasts, c.want)
		}
	}
}

// TestSearchEpisodesBringTheirShow: a matched episode brings its show, subtitled with the
// show's author, a show titled as the query is found past the pool, and the show list
// narrows the group as it narrows the episodes.
func TestSearchEpisodesBringTheirShow(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	feed := putFeed(t, st, "http://feed.example/a", "Harbor One", "Harbor Two")
	for i, title := range []string{"Cast Away", "Cast Iron"} {
		n := strconv.Itoa(i)
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: title, artist: "Singer", album: "Al"})
	}

	res := search(t, st, "harbor", read.SearchOptions{})
	wantTitles(t, "harbor", "podcasts", res.Podcasts, "Cast")
	h := res.Podcasts[0]
	if ref, ok := h.ArtRef(); h.Kind != "podcast" || h.PID != feed.PodcastPID || h.Subtitle != "Host" || h.Exact ||
		!ok || ref != (model.EntityRef{Type: model.ArtPodcast, PID: feed.PodcastPID}) {
		t.Errorf("podcast hit = %+v (art ref %+v), want the show by its author", h, ref)
	}

	// The tracks are the two newest matches, so no episode is in the pool.
	res = search(t, st, "cast", read.SearchOptions{MaxCandidates: 1})
	if len(res.Podcasts) != 1 || res.Podcasts[0].PID != feed.PodcastPID || !res.Podcasts[0].Exact || res.Podcasts[0].Subtitle != "Host" {
		t.Errorf("capped search cast podcasts = %+v, want the show, exact", res.Podcasts)
	}
	for name, opt := range map[string]read.SearchOptions{
		"no show":       {Podcasts: []model.PID{}},
		"a library":     {Libraries: []model.PID{lib.PID}},
		"another state": {States: []model.ItemState{model.StatePresent}},
	} {
		if res := search(t, st, "cast", opt); len(res.Podcasts) != 0 {
			t.Errorf("search cast under %s podcasts = %+v, want none: it keeps no episode of the show", name, res.Podcasts)
		}
	}
	if res := search(t, st, "cast", read.SearchOptions{Podcasts: []model.PID{feed.PodcastPID}}); len(res.Podcasts) != 1 {
		t.Errorf("search cast in the show = %+v, want it", res.Podcasts)
	}
}

// TestSearchTracksBringEveryCredit: a matched track brings each artist its artist credit
// names, so a featured artist is found by part of a name, and an artist it credits in
// another role or as its album artist is listed when the query names them, a word of the
// name beginning with each word of it, and not otherwise.
func TestSearchTracksBringEveryCredit(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Glowed Up", artist: "KAYTRANADA feat. Anderson .Paak", artists: []string{"KAYTRANADA", "Anderson .Paak"}, album: "99.9%"})
	theme := putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "Main Theme", artist: "Studio Orchestra", album: "Score"})
	if _, _, err := st.SetItemCredits(ctx, theme.ItemPID, model.RoleComposer, []string{"Hans Zimmer"},
		model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
		t.Fatalf("set composer credit: %v", err)
	}
	credit := func(item model.PID, role model.ContributorRole, name string) {
		t.Helper()
		if _, _, err := st.SetItemCredits(ctx, item, role, []string{name},
			model.Attribution{Source: model.SourceUser}, model.LockOf(false), false, false); err != nil {
			t.Fatalf("set %s credit: %v", role, err)
		}
	}
	// A performer credit is not the artist credit, so it is listed by name only.
	credit(theme.ItemPID, model.RolePerformer, "Yo-Yo Ma")
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3",
		title: "Guest Spot", artist: "Visiting Singer", albumArt: "Resident Band", album: "Residency"})
	// An arranger's role sorts ahead of the artist role, and the album artist is read with
	// the item, yet the artist credit still leads.
	trio := putTrack(t, st, lib.ID, trackSpec{path: "/lib/4.flac", essence: "e4", content: "c4",
		title: "Trio", artist: "Anna Smith & Zed Smith", artists: []string{"Anna Smith", "Zed Smith"},
		albumArt: "Smith Family", album: "Three"})
	credit(trio.ItemPID, model.RoleArranger, "Bob Smith")
	// A word of the title begins with the query, and the producer's name only holds it.
	zula := putTrack(t, st, lib.ID, trackSpec{path: "/lib/5.flac", essence: "e5", content: "c5",
		title: "Song for Zula", artist: "Phosphorescent", album: "Muchacho"})
	credit(zula.ItemPID, model.RoleProducer, "Jason Bigson")
	credit(zula.ItemPID, model.RoleEngineer, "Big K.R.I.T.")
	credit(zula.ItemPID, model.RoleMixer, "坂本龍一")

	for _, c := range []struct {
		q    string
		want []string
	}{
		{"paak", []string{"Anderson .Paak", "KAYTRANADA"}},
		{"glowed", []string{"KAYTRANADA", "Anderson .Paak"}},
		{"zimm", []string{"Hans Zimmer", "Studio Orchestra"}},
		{"main theme", []string{"Studio Orchestra"}},
		{"zimmer hans", []string{"Hans Zimmer", "Studio Orchestra"}},
		{"yo", []string{"Yo-Yo Ma", "Studio Orchestra"}},
		{"resident", []string{"Resident Band", "Visiting Singer"}},
		{"guest spot", []string{"Visiting Singer"}},
		{"smith", []string{"Anna Smith", "Zed Smith", "Bob Smith", "Smith Family"}},
		{"son", []string{"Phosphorescent"}},
		{"bigs", []string{"Jason Bigson", "Phosphorescent"}},
		{"krit", []string{"Big K.R.I.T.", "Phosphorescent"}},
		// Inside a name written without spaces, as the match finds it.
		{"本龍一", []string{"坂本龍一", "Phosphorescent"}},
	} {
		res := search(t, st, c.q, read.SearchOptions{})
		wantTitles(t, c.q, "artists", res.Artists, c.want...)
		for _, h := range res.Artists {
			if h.Kind != "artist" {
				t.Errorf("search %q: artist hit %+v has kind %q", c.q, h, h.Kind)
			}
		}
		if len(res.Authors)+len(res.Narrators) != 0 {
			t.Errorf("search %q lists book people for tracks: %+v %+v", c.q, res.Authors, res.Narrators)
		}
	}
}

// TestSearchFindsNamesTypedWithoutTheirPunctuation: an album, a series and a show whose
// items fall outside the ranked pool are found by a name typed without its apostrophe, as
// an artist is.
func TestSearchFindsNamesTypedWithoutTheirPunctuation(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/0.flac", essence: "e0", content: "c0",
		title: "Mercy Mercy Me", artist: "Marvin Gaye", album: "What's Going On", albumArt: "Marvin Gaye"})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/0.m4b", essence: "b0", content: "cb0",
		title: "Speaker for the Dead", author: "Orson Scott Card", series: "Ender's Game", seq: "2"})
	show, err := st.UpsertFeed(ctx, model.UpsertFeedInput{
		FeedURL: "http://feed.example/hh", IdentityKey: "podcast:feed.example/hh",
		Feed: model.Feed{Title: "Dan Carlin's Hardcore History", Author: "Dan Carlin", Episodes: []model.FeedEpisode{
			{GUID: "g1", Title: "Blueprint for Armageddon", EnclosureURL: "http://feed.example/1.mp3", EnclosureType: "audio/mpeg"},
		}},
		FetchedAtNS: 1,
	})
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	// Newer tracks titled with each name fill a pool of one.
	for i, title := range []string{"Whats Going On", "Enders Game", "Dan Carlins Hardcore History"} {
		for j := range 2 {
			n := strconv.Itoa(2*i + j + 1)
			putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
				title: title + " " + n, artist: "Singer", album: "Album " + n})
		}
	}
	capped := read.SearchOptions{MaxCandidates: 1}
	res := search(t, st, "whats going on", capped)
	if len(res.Albums) == 0 || res.Albums[0].Title != "What's Going On" || !res.Albums[0].Exact || res.Albums[0].Subtitle != "Marvin Gaye" {
		t.Errorf("search whats going on albums = %+v, want What's Going On by Marvin Gaye first and exact", res.Albums)
	}
	res = search(t, st, "enders game", capped)
	if len(res.Series) != 1 || res.Series[0].Title != "Ender's Game" || !res.Series[0].Exact {
		t.Errorf("search enders game series = %+v, want Ender's Game, exact", res.Series)
	}
	res = search(t, st, "dan carlins hardcore history", capped)
	if len(res.Podcasts) != 1 || res.Podcasts[0].PID != show.PodcastPID || !res.Podcasts[0].Exact {
		t.Errorf("search dan carlins hardcore history podcasts = %+v, want the show, exact", res.Podcasts)
	}
	assertVerifyClean(t, st)
}

// TestSearchSubtitlesStayInsideTheScope: a series is subtitled from the first of its
// books the narrowing keeps, and an album found by name from the first of its tracks it
// keeps, so a reader of one library is not shown an author or an artist from another.
func TestSearchSubtitlesStayInsideTheScope(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	lib2, err := st.EnsureLibrary(context.Background(), &model.Library{
		Root: []byte("/other"), DisplayRoot: "/other", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("second library: %v", err)
	}
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/1.m4b", essence: "b1", content: "c1",
		title: "First Volume", author: "First Author", series: "Shared World", seq: "1"})
	putBook(t, st, lib2.ID, bookSpec{path: "/other/b/2.m4b", essence: "b2", content: "c2",
		title: "Second Volume", author: "Second Author", series: "Shared World", seq: "2"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c3",
		title: "Opening", artist: "First Band", album: "Split Single", mbRelease: "a0000000-0000-4000-8000-000000000001"})
	putTrack(t, st, lib2.ID, trackSpec{path: "/other/2.flac", essence: "e2", content: "c4",
		title: "Closing", artist: "Second Band", album: "Split Single", mbRelease: "a0000000-0000-4000-8000-000000000001"})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("the two tracks sit in %d albums, want the one release", n)
	}
	// Newer tracks titled with both names keep the books and the album out of a pool of one.
	for i := range 2 {
		n := strconv.Itoa(i)
		putTrack(t, st, lib2.ID, trackSpec{path: "/other/d" + n + ".flac", essence: "d" + n, content: "d" + n,
			title: "Shared World Split Single " + n, artist: "Singer", album: "Decoy " + n})
	}
	for _, c := range []struct {
		name           string
		opt            read.SearchOptions
		author, artist string
	}{
		{"no scope", read.SearchOptions{}, "First Author", "First Band"},
		{"the first library", read.SearchOptions{Libraries: []model.PID{lib.PID}}, "First Author", "First Band"},
		{"the second library", read.SearchOptions{Libraries: []model.PID{lib2.PID}}, "Second Author", "Second Band"},
		{"placed in the second", read.SearchOptions{PrimaryLibraries: []model.PID{lib2.PID}}, "Second Author", "Second Band"},
	} {
		// Through its book, and by its name alone.
		for _, q := range []string{"volume", "shared world"} {
			opt := c.opt
			if q == "shared world" {
				opt.MaxCandidates = 1
			}
			res := search(t, st, q, opt)
			if len(res.Series) != 1 || res.Series[0].Subtitle != c.author {
				t.Errorf("search %q under %s series = %+v, want Shared World by %s", q, c.name, res.Series, c.author)
			}
		}
		opt := c.opt
		opt.MaxCandidates = 1
		res := search(t, st, "split single", opt)
		if len(res.Albums) == 0 || res.Albums[0].Title != "Split Single" || res.Albums[0].Subtitle != c.artist {
			t.Errorf("search split single under %s albums = %+v, want Split Single by %s first", c.name, res.Albums, c.artist)
		}
	}
}

// TestSearchFindsAShowWithNoEpisodes: a show is found by its name before it has an
// episode, and by a show list that names it, but a library or a state is an episode's to
// meet.
func TestSearchFindsAShowWithNoEpisodes(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	feed := putFeedSpec(t, st, "http://feed.example/empty", false)
	other := putFeed(t, st, "http://feed.example/full", "Episode One")
	for _, c := range []struct {
		name string
		opt  read.SearchOptions
		want bool
	}{
		{"no narrowing", read.SearchOptions{}, true},
		{"its own show list", read.SearchOptions{Podcasts: []model.PID{feed.PodcastPID}}, true},
		{"another show's list", read.SearchOptions{Podcasts: []model.PID{other.PodcastPID}}, false},
		{"an empty show list", read.SearchOptions{Podcasts: []model.PID{}}, false},
		{"a library", read.SearchOptions{Libraries: []model.PID{lib.PID}}, false},
		{"a placement", read.SearchOptions{PrimaryLibraries: []model.PID{lib.PID}}, false},
		{"a state", read.SearchOptions{States: []model.ItemState{model.StateRemote}}, false},
	} {
		res := search(t, st, "cast", c.opt)
		var found bool
		for _, h := range res.Podcasts {
			found = found || h.PID == feed.PodcastPID
		}
		if found != c.want {
			t.Errorf("search cast under %s podcasts = %+v, the show with no episodes listed: %t, want %t", c.name, res.Podcasts, found, c.want)
		}
	}
}

// TestSearchCapCountsItemsNotCredits: an item comes back once for each credit, and the
// caps count items, so a kept item keeps every credit and a full pool is not mistaken for
// a spent one.
func TestSearchCapCountsItemsNotCredits(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for i := range 3 {
		n := strconv.Itoa(i)
		names := []string{"Lead " + n, "Second " + n, "Third " + n}
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "Zebra " + n, artist: strings.Join(names, ", "), artists: names, album: "Album " + n})
	}
	for _, c := range []struct {
		maxCandidates, tracks int
		truncated             bool
	}{{0, 3, false}, {3, 3, false}, {2, 2, true}} {
		res := search(t, st, "zebra", read.SearchOptions{MaxCandidates: c.maxCandidates})
		if len(res.Tracks) != c.tracks || len(res.Artists) != 3*c.tracks || len(res.Albums) != c.tracks || res.Truncated != c.truncated {
			t.Errorf("a pool of %d: %d tracks, %d artists, %d albums, truncated %t; want %d tracks with three artists each, truncated %t",
				c.maxCandidates, len(res.Tracks), len(res.Artists), len(res.Albums), res.Truncated, c.tracks, c.truncated)
		}
	}
}

// TestSearchEntityLookupPlans pins that the item statement cuts its ranking before it
// joins anything to it and then seeks each kept item's own credits, and that the authors,
// series and shows a search looks up by name seek their keys and then walk the named
// entity's own items, under a narrowing as with none.
func TestSearchEntityLookupPlans(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	stmts := map[string]string{"by sort key": itemsBySortKeyQ("", 21)}
	stmts["flat"], _, _ = searchStmt("harbor*", 20, 0, searchScope{})
	stmts["capped"], _, _ = searchStmt("harbor*", 20, 50, searchScope{})
	for name, stmt := range stmts {
		lines := explainPlanLines(t, st, stmt, "harbor*")
		plan := strings.Join(lines, "\n")
		ranked := slices.Index(lines, "SCAN r")
		if ranked < 0 {
			t.Fatalf("%s: the ranking is not read as a whole before its joins:\n%s", name, plan)
		}
		for _, l := range lines[:ranked] {
			for _, joined := range []string{" t ", " bk ", " ep ", " pod ", " al ", " ser ", " art ", " aart ", " ic ", " ca "} {
				if strings.Contains(l, joined) {
					t.Errorf("%s: %q is joined inside the ranking, so every match pays for it:\n%s", name, l, plan)
				}
			}
		}
		// The kept rows come in the ranking's own order, so only an item's credits sort.
		after := strings.Join(lines[ranked:], "\n")
		for _, want := range []string{"SEARCH ic USING", "(item_id=?)", "SEARCH ca USING INTEGER PRIMARY KEY",
			"SEARCH art USING INTEGER PRIMARY KEY", "SEARCH aart USING INTEGER PRIMARY KEY",
			"SEARCH ser USING INTEGER PRIMARY KEY", "LAST 3 TERMS OF ORDER BY"} {
			if !strings.Contains(after, want) {
				t.Errorf("%s: the joins of the kept rows lack %q:\n%s", name, want, plan)
			}
		}
	}
	plan := func(q string, args ...[]any) string {
		t.Helper()
		return strings.Join(explainPlanLines(t, st, q, slices.Concat(args...)...), "\n")
	}
	named := []any{"a", "b", "a"}
	for _, states := range [][]model.ItemState{nil, {model.StatePresent}} {
		sc := searchScope{libIDs: []int64{1}, primaryLibIDs: []int64{1}, states: states, shows: []int64{2}}
		narrow, args := searchNarrow(sc)
		people := plan(artistByName.q("''", bookCredited(narrow), 2, 5), named, args, []any{"author"})
		for _, want := range []string{"MULTI-INDEX OR", "artist_joined_key", "artist_name_key", "SEARCH ic USING INDEX item_contributor_artist"} {
			if !strings.Contains(people, want) {
				t.Errorf("author lookup plan lacks %q:\n%s", want, people)
			}
		}
		series := plan(seriesByName.q("''", seriesHeld(narrow), 2, 5), named, args)
		for _, want := range []string{"MULTI-INDEX OR", "series_joined_key", "series_sort", "SEARCH bk USING COVERING INDEX book_series (series_id=?)"} {
			if !strings.Contains(series, want) {
				t.Errorf("series lookup plan lacks %q:\n%s", want, series)
			}
		}
		if bySort := plan(seriesByName.q("''", seriesHeld(narrow), 0, 5), named[:1], args); !strings.Contains(bySort, "SEARCH ser USING INDEX series_sort") {
			t.Errorf("series lookup by sort key alone does not seek series_sort:\n%s", bySort)
		}
		// A series' author is the first kept book in the index's own order, with no sort.
		authors := plan(seriesAuthorsQ(narrow), args, []any{`["p"]`})
		if !strings.Contains(authors, "SEARCH bk USING INDEX book_series (series_id=?)") || strings.Contains(authors, "TEMP B-TREE") ||
			strings.Contains(authors, "SCAN ser") {
			t.Errorf("series author read does not seek its series and its first book:\n%s", authors)
		}
		member, memberArgs, _ := showMember(sc)
		shows := plan(podcastByName.q("pod.author", member, 2, 5), named, memberArgs)
		for _, want := range []string{"MULTI-INDEX OR", "podcast_title_joined_key", "podcast_sort", "SEARCH ep USING COVERING INDEX episode_podcast (podcast_id=?)"} {
			if !strings.Contains(shows, want) {
				t.Errorf("show lookup plan lacks %q:\n%s", want, shows)
			}
		}
	}
	// With no library or state to meet, a show list is asked of the show itself.
	member, memberArgs, ok := showMember(searchScope{shows: []int64{2}})
	listed := plan(podcastByName.q("pod.author", member, 2, 5), named, memberArgs)
	if !ok || !strings.Contains(listed, "podcast_title_joined_key") || strings.Contains(listed, "episode") {
		t.Errorf("show lookup under a show list alone reads episodes or scans:\n%s", listed)
	}
	if _, _, ok := showMember(searchScope{shows: []int64{}}); ok {
		t.Error("an empty show list admits a show")
	}
}

// TestJoinedTitleKeysFollowTheTitle: an album's and a show's joined title key is written
// with the title by every writer of one, so a renamed album and a retitled show are found
// by the new name typed without its punctuation, and a key left stale is counted by the
// verify and rewritten by its repair.
func TestJoinedTitleKeysFollowTheTitle(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Opening", artist: "Band", album: "First Draft", albumArt: "Band"})
	keys := func(want ...string) {
		t.Helper()
		got := []string{scalarStr(t, st, "SELECT title_joined_key FROM album"),
			scalarStr(t, st, "SELECT group_concat(title_joined_key, ' ') FROM (SELECT title_joined_key FROM podcast ORDER BY id)")}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("joined title keys = %q, want %q", got, want)
		}
		assertVerifyClean(t, st)
	}
	feed := model.UpsertFeedInput{FeedURL: "http://feed.example/a", IdentityKey: "podcast:feed.example/a",
		Feed: model.Feed{Title: "Fed Show", Episodes: []model.FeedEpisode{
			{GUID: "g1", Title: "One", EnclosureURL: "http://feed.example/1.mp3", EnclosureType: "audio/mpeg"}}}, FetchedAtNS: 1}
	if _, err := st.UpsertFeed(ctx, feed); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	show := model.UpsertShowInput{IdentityKey: "manual:m", FeedURL: "manual:m", SourceType: model.SourceManual, Title: "Hand Made"}
	if _, _, err := st.UpsertShow(ctx, show); err != nil {
		t.Fatalf("UpsertShow: %v", err)
	}
	keys("firstdraft", "fedshow handmade")

	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album"))
	if _, err := st.RenameEntity(ctx, model.MergeAlbum, albumPID, map[string]string{"album": "What's Going On"},
		model.Attribution{}, model.LockUnchanged, false); err != nil {
		t.Fatalf("RenameEntity: %v", err)
	}
	feed.Feed.Title, feed.FetchedAtNS = "Dan Carlin's Hardcore History", 2
	if _, err := st.UpsertFeed(ctx, feed); err != nil {
		t.Fatalf("UpsertFeed again: %v", err)
	}
	show.Title = "Mom's Tapes"
	if _, _, err := st.UpsertShow(ctx, show); err != nil {
		t.Fatalf("UpsertShow again: %v", err)
	}
	keys("whatsgoingon", "dancarlinshardcorehistory momstapes")
	for q, group := range map[string]func(*read.SearchResult) []read.SearchHit{
		"whats going on": func(r *read.SearchResult) []read.SearchHit { return r.Albums },
		"moms tapes":     func(r *read.SearchResult) []read.SearchHit { return r.Podcasts },
	} {
		if hits := group(search(t, st, q, read.SearchOptions{})); len(hits) != 1 || !hits[0].Exact {
			t.Errorf("search %q = %+v, want the renamed one, exact", q, hits)
		}
	}

	if _, err := st.wdb().ExecContext(ctx, "UPDATE album SET title_joined_key = ''; UPDATE podcast SET title_joined_key = 'stale'"); err != nil {
		t.Fatalf("stale the keys: %v", err)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || rep.SortKeyDrift != 3 {
		t.Fatalf("verify = %+v (err %v), want the three stale keys counted", rep, err)
	}
	if n, err := st.RefreshSortKeys(ctx); err != nil || n != 3 {
		t.Fatalf("RefreshSortKeys = %d (err %v), want 3 keys rewritten", n, err)
	}
	keys("whatsgoingon", "dancarlinshardcorehistory momstapes")
}
