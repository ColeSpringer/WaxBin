package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// scalarStr reads one string cell, for white-box entity-pid lookups.
func scalarStr(t *testing.T, st *Store, q string, args ...any) string {
	t.Helper()
	var s string
	if err := st.read.QueryRowContext(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

// epSpec is one seeded episode. A zero pubNS stores NULL (nullInt64), which is the
// undated episode recent-episodes excludes; the two explicit flags are independent
// of each other and of the show's, which is what the paired advisory query fields
// exist to express.
type epSpec struct {
	title    string
	pubNS    int64
	year     int
	explicit bool
}

// putFeedSpec is the one feed-seeding helper the package uses; putFeed and
// putFeedAdvisory are the two common shorthands over it. Every feed it writes is
// titled "Cast" whatever its URL, so a test asserting on the show's display name does
// not have to know which helper seeded it.
func putFeedSpec(t *testing.T, st *Store, feedURL string, chExplicit bool, eps ...epSpec) *model.UpsertFeedResult {
	t.Helper()
	fe := make([]model.FeedEpisode, len(eps))
	for i, e := range eps {
		fe[i] = model.FeedEpisode{
			GUID: "guid-" + e.title, Title: e.title, EnclosureURL: feedURL + "/" + e.title + ".mp3",
			EnclosureType: "audio/mpeg", DurationMS: 1000,
			PubDateNS: e.pubNS, Year: e.year, Explicit: e.explicit,
		}
	}
	res, err := st.UpsertFeed(context.Background(), model.UpsertFeedInput{
		FeedURL:     feedURL,
		IdentityKey: identity.PodcastKey("", feedURL),
		Feed:        model.Feed{Title: "Cast", Author: "Host", Explicit: chExplicit, Episodes: fe},
		FetchedAtNS: 1,
	})
	if err != nil {
		t.Fatalf("upsert feed %s: %v", feedURL, err)
	}
	return res
}

// putFeed upserts a podcast feed with remote (undownloaded) episodes, dated in
// ascending order so a listing has a deterministic sequence.
func putFeed(t *testing.T, st *Store, feedURL string, titles ...string) *model.UpsertFeedResult {
	t.Helper()
	eps := make([]epSpec, len(titles))
	for i, tt := range titles {
		eps[i] = epSpec{title: tt, pubNS: int64(i+1) * 1_000_000_000}
	}
	return putFeedSpec(t, st, feedURL, false, eps...)
}

// putFeedAdvisory upserts a feed carrying advisory flags: chExplicit is the
// channel-level <itunes:explicit> and epExplicit names the episodes carrying the
// item-level one.
func putFeedAdvisory(t *testing.T, st *Store, feedURL string, chExplicit bool, epExplicit map[string]bool, titles ...string) *model.UpsertFeedResult {
	t.Helper()
	eps := make([]epSpec, len(titles))
	for i, tt := range titles {
		eps[i] = epSpec{title: tt, pubNS: int64(i+1) * 1_000_000_000, explicit: epExplicit[tt]}
	}
	return putFeedSpec(t, st, feedURL, chExplicit, eps...)
}

// countWhere counts items matching one condition over the items entity.
func countWhere(t *testing.T, st *Store, field string, op query.Op, value any) int {
	t.Helper()
	b := query.New(query.EntityItems)
	if op == query.OpIsPresent || op == query.OpIsMissing {
		b.WherePresence(field, op)
	} else {
		b.Where(field, op, value)
	}
	n, err := st.CountItems(context.Background(), b.Build(), "")
	if err != nil {
		t.Fatalf("count %s %s: %v", field, op, err)
	}
	return n
}

// countValuesWhere counts items matching one set-membership condition.
func countValuesWhere(t *testing.T, st *Store, field string, op query.Op, values ...any) int {
	t.Helper()
	n, err := st.CountItems(context.Background(), query.New(query.EntityItems).
		WhereValues(field, op, values...).Build(), "")
	if err != nil {
		t.Fatalf("count %s %s: %v", field, op, err)
	}
	return n
}

func TestEntityPIDFieldsMirrorFacets(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/r1.flac", essence: "e1", content: "c1",
		title: "One", artist: "Radiohead", albumArt: "Radiohead", album: "OK Computer", genre: "Rock; Electronic"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/r2.flac", essence: "e2", content: "c2",
		title: "Two", artist: "Radiohead", albumArt: "Radiohead", album: "OK Computer", genre: "Rock"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/o1.flac", essence: "e3", content: "c3",
		title: "Three", artist: "Orbital", albumArt: "Orbital", album: "Insides", genre: "Electronic"})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/earthsea.m4b", essence: "be1", content: "bc1",
		title: "A Wizard of Earthsea", author: "Ursula K. Le Guin", genres: []string{"Fantasy"}})
	putFeed(t, st, "http://cast.example/f", "Ep1", "Ep2")

	// A default-user playlist, so the playlist row of the mirror table below has a
	// bucket to check rather than passing vacuously.
	all, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(all) == 0 {
		t.Fatalf("items = %d (err %v)", len(all), err)
	}
	mine, err := st.CreatePlaylist(ctx, "Mine", "", model.PlaylistStatic, model.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, mine, []model.PID{all[0].PID, all[1].PID}); err != nil {
		t.Fatalf("add to playlist: %v", err)
	}

	// The mirror guarantee: for every entity-keyed facet bucket, filtering by the
	// matching pid field returns exactly the bucket's count. The facet specs and
	// the pid fields share their entity expressions, which is what keeps the two
	// sides in agreement.
	for _, mirror := range []struct {
		group read.GroupBy
		field string
	}{
		{read.GroupArtist, "artist_pid"},
		{read.GroupCreditArtist, "credit_artist_pid"},
		{read.GroupAlbumArtist, "album_artist_pid"},
		{read.GroupAlbum, "album_pid"},
		{read.GroupReleaseGroup, "release_group_pid"},
		{read.GroupGenre, "genre_pid"},
		{read.GroupPodcast, "podcast_pid"},
		// The playlist dimension holds too, even though its buckets are owner-scoped
		// and the field is not: scoping selects which playlists get buckets, never
		// which items land in one. The loop is one-directional, so the divergence is
		// pinned separately below.
		{read.GroupPlaylist, "playlist_pid"},
	} {
		res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), mirror.group, "", 0, "")
		if err != nil {
			t.Fatalf("facet %s: %v", mirror.group, err)
		}
		for _, b := range res.Buckets {
			if b.EntityPID == "" {
				continue
			}
			if n := countWhere(t, st, mirror.field, query.OpIs, string(b.EntityPID)); n != b.Count {
				t.Errorf("%s bucket %q count %d != %s filter count %d",
					mirror.group, b.Display, b.Count, mirror.field, n)
			}
		}
	}

	// A book matches by its author, the same entity the artist facet groups it
	// under.
	authorPID := scalarStr(t, st, "SELECT pid FROM artist WHERE name = ?", "Ursula K. Le Guin")
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("artist_pid", query.OpIs, authorPID).Build(), "")
	if err != nil {
		t.Fatalf("query by author pid: %v", err)
	}
	if len(items) != 1 || items[0].Title != "A Wizard of Earthsea" {
		t.Errorf("artist_pid over a book author = %v, want the book", titlesOf(items))
	}

	// album_pid selects one album's tracks and reads NULL for other kinds, so
	// isMissing works there.
	albumPID := scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "OK Computer")
	if n := countWhere(t, st, "album_pid", query.OpIs, albumPID); n != 2 {
		t.Errorf("album_pid filter = %d, want 2", n)
	}
	// Albums are track-only, so the book and both episodes read NULL there.
	if n := countWhere(t, st, "album_pid", query.OpIsMissing, nil); n != 3 {
		t.Errorf("album_pid isMissing = %d, want 3 (the book and two episodes)", n)
	}

	// Where the playlist mirror stops: another user's private playlist has a non-zero
	// playlist_pid count and no bucket at all, which is the one asymmetry the
	// bucket-walking loop above cannot see.
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	hidden, err := st.CreatePlaylist(ctx, "BobPrivate", bob.PID, model.PlaylistStatic, model.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("create bob's playlist: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, hidden, []model.PID{all[2].PID}); err != nil {
		t.Fatalf("add to bob's playlist: %v", err)
	}
	if n := countWhere(t, st, "playlist_pid", query.OpIs, string(hidden)); n != 1 {
		t.Errorf("playlist_pid is bob's private list = %d, want 1", n)
	}
	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupPlaylist, "", 0, "")
	if err != nil {
		t.Fatalf("facet playlist: %v", err)
	}
	for _, b := range res.Buckets {
		if b.EntityPID == hidden {
			t.Errorf("bob's private playlist has a bucket for the default user: %+v", b)
		}
	}
}

// TestPodcastPIDField pins the field WaxDeck asked for: the unplayed count of one
// show is a single CountItems bounded by that show, not a walk over its episodes in
// Go. isMissing is the complement over the whole catalog (every non-episode carries a
// NULL podcast_id), which is what makes the field usable as a kind discriminator too.
func TestPodcastPIDField(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al"})
	one := putFeed(t, st, "http://cast.example/one", "Ep1", "Ep2", "Ep3")
	putFeed(t, st, "http://cast.example/two", "Other")

	if n := countWhere(t, st, "podcast_pid", query.OpIs, string(one.PodcastPID)); n != 3 {
		t.Errorf("podcast_pid is show one = %d, want 3", n)
	}
	// Every non-episode reads NULL, so isMissing is the whole rest of the catalog.
	if n := countWhere(t, st, "podcast_pid", query.OpIsMissing, nil); n != 1 {
		t.Errorf("podcast_pid isMissing = %d, want 1 (the track)", n)
	}
	if n := countWhere(t, st, "podcast_pid", query.OpIsPresent, nil); n != 4 {
		t.Errorf("podcast_pid isPresent = %d, want 4 (every episode)", n)
	}

	// The target shape: how many of this show's episodes are unplayed. Playing one
	// must move the count by exactly one.
	unplayed := func() int {
		t.Helper()
		n, err := st.CountItems(ctx, query.New(query.EntityItems).
			Where("kind", query.OpIs, "episode").
			Where("podcast_pid", query.OpIs, string(one.PodcastPID)).
			Where("played", query.OpIs, 0).Build(), "")
		if err != nil {
			t.Fatalf("unplayed count: %v", err)
		}
		return n
	}
	if n := unplayed(); n != 3 {
		t.Fatalf("unplayed episodes of show one = %d, want 3", n)
	}
	eps, err := st.EpisodesByPodcast(ctx, one.PodcastPID, 0)
	if err != nil || len(eps) != 3 {
		t.Fatalf("episodes = %v (err %v), want 3", eps, err)
	}
	if err := st.MarkPlayed(ctx, "", eps[0].PID, true); err != nil {
		t.Fatalf("mark played: %v", err)
	}
	if n := unplayed(); n != 2 {
		t.Errorf("unplayed after one play = %d, want 2", n)
	}
}

// TestLoweredIdentityFieldPlans pins the plans the value-side lowering exists to
// produce, which is the whole point of the change: the pid is resolved once per
// query by a plain SCALAR SUBQUERY rather than a CORRELATED one evaluated per row,
// and the three fields whose id column is indexed drive the comparison off that
// index instead of scanning. The correlated-subquery assertion is the important
// one, since it catches a regression back to per-row lowering on all five fields
// at once.
func TestLoweredIdentityFieldPlans(t *testing.T) {
	st, lib := entityFixture(t)
	// Real rows in every table the lowered fields touch, so the planner is choosing
	// over a populated schema rather than over empty tables. Nothing here runs ANALYZE,
	// which matches production: WaxBin never collects statistics, so these are the
	// plans a real catalog gets for an equality on an indexed column.
	for i, artist := range []string{"Radiohead", "Orbital", "Aphex Twin"} {
		n := string(rune('a' + i))
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "T" + n, artist: artist, album: "Al" + n, genre: "Rock"})
	}
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b.m4b", essence: "eb", content: "cb",
		title: "Tome", author: "Auth", asin: "BX"})
	putFeed(t, st, "http://cast.example/f", "Ep1", "Ep2")

	fm, ok := fieldMapFor(query.EntityItems)
	if !ok {
		t.Fatal("no field map for items")
	}
	for _, tc := range []struct {
		field string
		seek  string // an index seek the plan must contain ("" = the expr is not indexable)
	}{
		{"podcast_pid", "SEARCH ep USING COVERING INDEX episode_podcast"},
		{"album_pid", "SEARCH t USING COVERING INDEX track_album_id"},
		{"library", "SEARCH f USING COVERING INDEX file_library"},
		// artist_pid and album_artist_pid COALESCE across track and book, so no single
		// index covers the expression and the scan stays. What lowering buys them is
		// one pid lookup per query instead of one per row.
		{"artist_pid", ""},
		{"album_artist_pid", ""},
		// release_group_pid belongs here and not with album_pid above, however much
		// the joined alb alias makes it look indexed. Its plan is SCAN pi with a
		// per-row probe into alb; the album_rg line that appears in this COUNT shape
		// is that inner probe, and the itemSelect shape SQLite actually runs in
		// production reaches alb by rowid without album_rg at all. Asserting the
		// album_rg substring here would pass on a full catalog scan, which is the
		// silent-false-pass this test exists to prevent, so it is asserted the other
		// way: see TestReleaseGroupPIDScansOuterLoop below.
		{"release_group_pid", ""},
	} {
		c, err := query.Compile(query.New(query.EntityItems).
			Where(tc.field, query.OpIs, "some-pid").Build(), fm)
		if err != nil {
			t.Fatalf("%s compile: %v", tc.field, err)
		}
		plan := explainPlan(t, st, "SELECT COUNT(*)"+itemJoins+" WHERE "+c.Where, c.Args...)
		if strings.Contains(plan, "CORRELATED") {
			t.Errorf("%s plans a correlated subquery (lowering regressed to per-row):\n%s", tc.field, plan)
		}
		if !strings.Contains(plan, "SCALAR SUBQUERY") {
			t.Errorf("%s does not lower the pid to a subquery:\n%s", tc.field, plan)
		}
		if tc.seek != "" && !strings.Contains(plan, tc.seek) {
			t.Errorf("%s does not seek its index (want %q):\n%s", tc.field, tc.seek, plan)
		}
	}

	// `in` must seek the same index at every arity. Arity 1 is the regression guard:
	// without the single-value special case it silently becomes a scan with a LIST
	// SUBQUERY, and an arity-2 test alone would never catch that.
	const seek = "SEARCH ep USING COVERING INDEX episode_podcast"
	for _, n := range []int{1, 2, 500} { // 500 is the compiler's per-condition cap
		vals := make([]any, n)
		for i := range vals {
			vals[i] = "some-pid"
		}
		c, err := query.Compile(query.New(query.EntityItems).
			WhereValues("podcast_pid", query.OpIn, vals...).Build(), fm)
		if err != nil {
			t.Fatalf("podcast_pid in (%d values) compile: %v", n, err)
		}
		plan := explainPlan(t, st, "SELECT COUNT(*)"+itemJoins+" WHERE "+c.Where, c.Args...)
		if !strings.Contains(plan, seek) {
			t.Errorf("podcast_pid in (%d values) does not seek episode_podcast:\n%s", n, plan)
		}
	}
}

// TestPodcastPIDInSemantics pins what `in` answers on a lowered field, next to the
// `is` coverage above: the union of the listed shows, an arity-1 list matching what
// `is` matches, an empty list matching nothing, and an unknown pid contributing
// nothing rather than poisoning the whole condition.
func TestPodcastPIDInSemantics(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al"})
	one := putFeed(t, st, "http://cast.example/one", "Ep1", "Ep2", "Ep3")
	two := putFeed(t, st, "http://cast.example/two", "Other")

	a, b := string(one.PodcastPID), string(two.PodcastPID)
	if n := countValuesWhere(t, st, "podcast_pid", query.OpIn, a, b); n != 4 {
		t.Errorf("in [one two] = %d, want 4 (every episode of both)", n)
	}
	if n := countValuesWhere(t, st, "podcast_pid", query.OpIn, a); n != countWhere(t, st, "podcast_pid", query.OpIs, a) {
		t.Errorf("in [one] = %d, want the same as is one", n)
	}
	if n := countValuesWhere(t, st, "podcast_pid", query.OpIn); n != 0 {
		t.Errorf("in [] = %d, want 0", n)
	}
	if n := countValuesWhere(t, st, "podcast_pid", query.OpIn, a, "no-such-podcast-pid"); n != 3 {
		t.Errorf("in [one unknown] = %d, want 3 (the unknown pid contributes nothing)", n)
	}
}

// TestReleaseGroupPIDScansOuterLoop records what the release_group_pid filter
// actually costs, over the itemSelect shape production runs rather than the COUNT
// shape the plan table above compiles.
//
// It asserts the scan rather than an index, which is the unusual direction, so the
// reason matters: an assertion that the field seeks album_rg passes on a plan whose
// outer loop is a full catalog scan, because the album_rg line is the inner per-row
// probe. Pinning the honest shape means a later change that genuinely narrows the
// outer loop, by putting release_group_id on track, say, fails here and gets the
// field promoted to the seeking group in the field map header along with this test.
func TestReleaseGroupPIDScansOuterLoop(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca",
		title: "A", artist: "X", albumArt: "X", album: "Al"})

	fm, ok := fieldMapFor(query.EntityItems)
	if !ok {
		t.Fatal("no field map for items")
	}
	c, err := query.Compile(query.New(query.EntityItems).
		Where("release_group_pid", query.OpIs, "some-pid").Build(), fm)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	lines := explainPlanLines(t, st, itemSelect+" WHERE "+c.Where, c.Args...)
	plan := strings.Join(lines, "\n")
	if !strings.Contains(plan, "SCAN pi") {
		t.Errorf("release_group_pid no longer scans playable_item; if it now narrows the "+
			"outer loop, move it to the seeking group in the itemFields header and in "+
			"TestLoweredIdentityFieldPlans:\n%s", plan)
	}
	// The lowering itself must still hold: one pid resolution per query, not per row.
	// The filter's subquery is the one that searches rgq, and its parent node is the
	// line above it. Asserting on that relationship rather than on a subquery ordinal
	// keeps the check working when the projection's own correlated subqueries are
	// added to or removed (phase 4 changed that count, which is what a
	// "CORRELATED SCALAR SUBQUERY 4" assertion would have silently tracked).
	var parent string
	for i, l := range lines {
		if strings.Contains(l, "rgq") && i > 0 {
			parent = lines[i-1]
		}
	}
	if parent == "" {
		t.Fatalf("no rgq lookup in the plan, so the pid is not being lowered at all:\n%s", plan)
	}
	if !strings.Contains(parent, "SCALAR SUBQUERY") || strings.Contains(parent, "CORRELATED") {
		t.Errorf("release_group_pid lowering regressed to a per-row subquery (rgq sits under %q):\n%s",
			parent, plan)
	}
}

// explainPlan returns the EXPLAIN QUERY PLAN detail lines for a statement, joined by
// newlines, for the plan assertions above.
func explainPlan(t *testing.T, st *Store, stmt string, args ...any) string {
	t.Helper()
	return strings.Join(explainPlanLines(t, st, stmt, args...), "\n")
}

// explainPlanLines returns the plan's detail lines in order, for an assertion that
// needs a node's position relative to its neighbours rather than a substring of the
// whole plan.
func explainPlanLines(t *testing.T, st *Store, stmt string, args ...any) []string {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+stmt, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return details
}

// TestLoweredIdentityFieldOperators pins the narrowed operator set on a value-lowered
// field: is/isNot/in/notIn/isPresent/isMissing are accepted and everything else is
// rejected, because the column is an internal id and an ordered or LIKE comparison
// against it is not a question an identity handle answers. Sorting by one is rejected
// for the same reason, mirroring the tag-field restriction.
func TestLoweredIdentityFieldOperators(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	for _, field := range []string{"artist_pid", "release_group_pid"} {
		for _, op := range []query.Op{query.OpGt, query.OpLt, query.OpGte, query.OpLte,
			query.OpContains, query.OpStartsWith, query.OpEndsWith} {
			_, err := st.QueryItems(ctx, query.New(query.EntityItems).
				Where(field, op, "some-pid").Build(), "")
			if !waxerr.Is(err, waxerr.CodeInvalid) {
				t.Errorf("%s %s: want CodeInvalid, got %v", field, op, err)
			}
		}
	}
	// in and notIn are both accepted on every lowered field, at arity 1 and at 0,
	// since the empty list takes a shortcut before the operator is ever consulted.
	for _, field := range []string{"artist_pid", "album_artist_pid", "album_pid",
		"release_group_pid", "podcast_pid", "library"} {
		for _, op := range []query.Op{query.OpIn, query.OpNotIn} {
			for _, vals := range [][]any{{"some-pid"}, {}} {
				if _, err := st.QueryItems(ctx, query.New(query.EntityItems).
					WhereValues(field, op, vals...).Build(), ""); err != nil {
					t.Errorf("%s %s %v: %v", field, op, vals, err)
				}
			}
		}
	}
	_, err := st.QueryItems(ctx, query.New(query.EntityItems).
		OrderBy("album_pid", false).Build(), "")
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("sort by album_pid: want CodeInvalid, got %v", err)
	}
}

func TestLibraryNotInIsADenyList(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib2"), DisplayRoot: "/lib2", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure lib2: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "X", album: "Al"})
	putTrack(t, st, lib2.ID, trackSpec{path: "/lib2/3.flac", essence: "e3", content: "c3", title: "C", artist: "X", album: "Al"})
	putFeed(t, st, "http://cast.example/f", "Ep1")

	notIn := func(vals ...any) int {
		t.Helper()
		n, err := st.CountItems(ctx, query.New(query.EntityItems).
			WhereValues("library", query.OpNotIn, vals...).Build(), "")
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	// Denying lib1 leaves lib2's track and the fileless episode.
	if n := notIn(string(lib.PID)); n != 2 {
		t.Errorf("library notIn [lib1] = %d, want 2", n)
	}
	if n := notIn(string(lib.PID), string(lib2.PID)); n != 1 {
		t.Errorf("library notIn [lib1, lib2] = %d, want 1 (the episode)", n)
	}
	if n := notIn(); n != 4 {
		t.Errorf("library notIn [] = %d, want 4 (an empty deny-list denies nothing)", n)
	}
}

// TestLoweredNotInKeepsItemsWithoutTheHandle passes a stale pid alongside a live one
// deliberately: with only live pids the IS NULL disjunct changes nothing, because
// IS NOT is already null-safe, and the test would pass either way.
func TestLoweredNotInKeepsItemsWithoutTheHandle(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al"})
	one := putFeed(t, st, "http://cast.example/one", "Ep1")
	putFeed(t, st, "http://cast.example/two", "Other")

	notIn := func(vals ...any) int {
		t.Helper()
		n, err := st.CountItems(context.Background(), query.New(query.EntityItems).
			WhereValues("podcast_pid", query.OpNotIn, vals...).Build(), "")
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	// The track has no podcast at all, so it survives a deny-list naming one show.
	if n := notIn(string(one.PodcastPID)); n != 2 {
		t.Errorf("podcast_pid notIn [one] = %d, want 2 (the track and the other episode)", n)
	}
	if n := notIn(string(one.PodcastPID), "no-such-podcast-pid"); n != 2 {
		t.Errorf("podcast_pid notIn [one, stale] = %d, want 2; a stale entry must not "+
			"drop the handle-less track, which is what the IS NULL disjunct is for", n)
	}
}

func TestLoweredNotInStalePIDExcludesNothing(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "Y", album: "Al2"})

	total, err := st.CountItems(context.Background(), query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	n, err := st.CountItems(context.Background(), query.New(query.EntityItems).
		WhereValues("artist_pid", query.OpNotIn, "no-such-artist-pid").Build(), "")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != total {
		t.Errorf("artist_pid notIn [stale] = %d, want all %d; one stale entry must exclude "+
			"nothing rather than emptying the result", n, total)
	}
}

// TestLoweredNotInScansTheCatalog records the honest cost, in
// TestReleaseGroupPIDScansOuterLoop's voice: a deny-list is an anti-join, so even
// library, which `is` drives off file_library, scans here.
func TestLoweredNotInScansTheCatalog(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca",
		title: "A", artist: "X", albumArt: "X", album: "Al"})

	fm, ok := fieldMapFor(query.EntityItems)
	if !ok {
		t.Fatal("no field map for items")
	}
	c, err := query.Compile(query.New(query.EntityItems).
		WhereValues("library", query.OpNotIn, "some-pid").Build(), fm)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	lines := explainPlanLines(t, st, itemSelect+" WHERE "+c.Where, c.Args...)
	plan := strings.Join(lines, "\n")
	if !strings.Contains(plan, "SCAN pi") {
		t.Errorf("library notIn no longer scans playable_item:\n%s", plan)
	}
	if strings.Contains(plan, "file_library") {
		t.Errorf("library notIn seeks file_library; a deny-list cannot drive off that index, "+
			"so this plan is not the one the field map header describes:\n%s", plan)
	}
	// The projection's own columns are correlated by design, so search for the filter's
	// own subquery (the one resolving the pid against libq) and check its parent node.
	var parent string
	for i, l := range lines {
		if strings.Contains(l, "libq") && i > 0 {
			parent = lines[i-1]
		}
	}
	if parent == "" {
		t.Fatalf("no libq line; the filter did not lower its pid:\n%s", plan)
	}
	if strings.Contains(parent, "CORRELATED") {
		t.Errorf("library notIn resolves its pid per row (%q); lowering regressed:\n%s", parent, plan)
	}
}

// TestNotInDiffersFromNotOfInOnAStalePID pins why Not{Cond{in}} is not a spelling of
// notIn. A stale pid lowers to NULL, `x IN (NULL, ...)` is UNKNOWN, and negating that
// is still not true, so the condition collapses to "no rows" the moment one listed pid
// stops resolving. notIn keeps those rows; see TestLoweredNotInStalePIDExcludesNothing.
func TestNotInDiffersFromNotOfInOnAStalePID(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al"})
	one := putFeed(t, st, "http://cast.example/one", "Ep1", "Ep2")
	putFeed(t, st, "http://cast.example/two", "Other")

	notIn := func(vals ...any) int {
		t.Helper()
		n, err := st.CountItems(context.Background(), query.New(query.EntityItems).
			WhereNode(query.Not{Node: query.Cond{Field: "podcast_pid", Op: query.OpIn, Values: vals}}).Build(), "")
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	a := string(one.PodcastPID)
	// Every pid resolves, so the negation answers what it looks like it answers.
	if n := notIn(a); n != 1 {
		t.Errorf("NOT in [one] = %d, want 1 (the other show's episode)", n)
	}
	// One stale pid takes the answer to zero, not to "the same minus nothing".
	if n := notIn(a, "no-such-podcast-pid"); n != 0 {
		t.Errorf("NOT in [one, stale] = %d, want 0 (the documented hole)", n)
	}
	if n := notIn("no-such-podcast-pid"); n != 0 {
		t.Errorf("NOT in [stale] = %d, want 0 (the documented hole)", n)
	}
}

// TestLoweredIdentityIsNotUnknownPID pins the one semantic the lowering changes. The
// value lowers to NULL when no entity holds the pid, and `x <> NULL` is NULL, so
// isNot against an unknown pid now matches nothing where the unlowered text compare
// matched everything. `is` matched nothing before and still does.
func TestLoweredIdentityIsNotUnknownPID(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "Y", album: "Al2"})

	if n := countWhere(t, st, "artist_pid", query.OpIsNot, "no-such-artist-pid"); n != 0 {
		t.Errorf("artist_pid isNot an unknown pid = %d, want 0 (the lowered value is NULL)", n)
	}
	if n := countWhere(t, st, "artist_pid", query.OpIs, "no-such-artist-pid"); n != 0 {
		t.Errorf("artist_pid is an unknown pid = %d, want 0", n)
	}
	// Against a real pid, isNot is still the complement among items that have one.
	xPID := scalarStr(t, st, "SELECT pid FROM artist WHERE name = ?", "X")
	if n := countWhere(t, st, "artist_pid", query.OpIsNot, xPID); n != 1 {
		t.Errorf("artist_pid isNot X = %d, want 1", n)
	}
}

func TestGenrePIDSetSemantics(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "A", artist: "X", album: "Al", genre: "Rock; Pop"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "B", artist: "X", album: "Al", genre: "Rock"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3",
		title: "C", artist: "X", album: "Al"})

	rockPID := scalarStr(t, st, "SELECT pid FROM genre WHERE name = ?", "Rock")
	if n := countWhere(t, st, "genre_pid", query.OpIs, rockPID); n != 2 {
		t.Errorf("genre_pid is Rock = %d, want 2", n)
	}
	// isNot is the deny-list: an item with no genre at all matches, while an item
	// carrying Rock (even among other genres) does not.
	if n := countWhere(t, st, "genre_pid", query.OpIsNot, rockPID); n != 1 {
		t.Errorf("genre_pid isNot Rock = %d, want 1 (the genreless track)", n)
	}
	if n := countWhere(t, st, "genre_pid", query.OpIsPresent, nil); n != 2 {
		t.Errorf("genre_pid isPresent = %d, want 2", n)
	}
	if n := countWhere(t, st, "genre_pid", query.OpIsMissing, nil); n != 1 {
		t.Errorf("genre_pid isMissing = %d, want 1", n)
	}
	// Ordered operators are rejected on a set field.
	_, err := st.QueryItems(context.Background(), query.New(query.EntityItems).
		Where("genre_pid", query.OpGt, rockPID).Build(), "")
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("genre_pid gt: want CodeInvalid, got %v", err)
	}
}

// TestPlaylistPIDSetSemantics pins the field's contract: static membership only, isNot
// as a deny-list, and presence catalog-wide rather than caller-scoped. The second
// user's private playlist is in the fixture so the documented boundary is asserted and
// not only described.
func TestPlaylistPIDSetSemantics(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al"}).ItemPID
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "X", album: "Al"}).ItemPID
	c := putTrack(t, st, lib.ID, trackSpec{path: "/lib/c.flac", essence: "ec", content: "cc", title: "C", artist: "X", album: "Al"}).ItemPID
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/d.flac", essence: "ed", content: "cd", title: "D", artist: "X", album: "Al"})

	mine, err := st.CreatePlaylist(ctx, "Mine", "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create static: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, mine, []model.PID{a, b}); err != nil {
		t.Fatalf("add: %v", err)
	}
	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	hidden, err := st.CreatePlaylist(ctx, "BobPrivate", bob.PID, model.PlaylistStatic, model.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("create bob's playlist: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, hidden, []model.PID{c}); err != nil {
		t.Fatalf("add to bob's playlist: %v", err)
	}
	rule := query.New(query.EntityItems).Where("artist", query.OpIs, "X").Build()
	smart, err := st.CreatePlaylist(ctx, "Smart", "", model.PlaylistSmart, "", &rule)
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}

	if n := countWhere(t, st, "playlist_pid", query.OpIs, string(mine)); n != 2 {
		t.Errorf("playlist_pid is Mine = %d, want 2", n)
	}
	// isNot is the deny-list: an item in no playlist matches, an item in Mine does not.
	if n := countWhere(t, st, "playlist_pid", query.OpIsNot, string(mine)); n != 2 {
		t.Errorf("playlist_pid isNot Mine = %d, want 2 (bob's item and the unfiled one)", n)
	}
	// Presence is catalog-wide, so an item held only in another user's private
	// playlist reads present here even though it gets no facet bucket.
	if n := countWhere(t, st, "playlist_pid", query.OpIsPresent, nil); n != 3 {
		t.Errorf("playlist_pid isPresent = %d, want 3 (bob's private member counts)", n)
	}
	if n := countWhere(t, st, "playlist_pid", query.OpIsMissing, nil); n != 1 {
		t.Errorf("playlist_pid isMissing = %d, want 1 (the unfiled track)", n)
	}
	if n := countValuesWhere(t, st, "playlist_pid", query.OpIn, string(mine), string(hidden)); n != 3 {
		t.Errorf("playlist_pid in [Mine, BobPrivate] = %d, want 3", n)
	}
	if n := countValuesWhere(t, st, "playlist_pid", query.OpNotIn, string(mine)); n != 2 {
		t.Errorf("playlist_pid notIn [Mine] = %d, want 2", n)
	}
	// A smart playlist stores no playlist_item rows, so its pid matches nothing. That
	// is what keeps a rule referencing this field from ever evaluating another rule.
	if n := countWhere(t, st, "playlist_pid", query.OpIs, string(smart)); n != 0 {
		t.Errorf("playlist_pid is a smart playlist = %d, want 0", n)
	}
	if n := countWhere(t, st, "playlist_pid", query.OpIsNot, string(smart)); n != 4 {
		t.Errorf("playlist_pid isNot a smart playlist = %d, want every item", n)
	}
	for _, op := range []query.Op{query.OpGt, query.OpLt, query.OpGte, query.OpLte} {
		_, err := st.QueryItems(ctx, query.New(query.EntityItems).
			Where("playlist_pid", op, string(mine)).Build(), "")
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("playlist_pid %s: want CodeInvalid, got %v", op, err)
		}
	}
	_, err = st.QueryItems(ctx, query.New(query.EntityItems).
		OrderBy("playlist_pid", false).Build(), "")
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("sorting by playlist_pid: want CodeInvalid, got %v", err)
	}
}

func TestLibraryFieldAndFacet(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib2"), DisplayRoot: "/lib2", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure lib2: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "X", album: "Al"})
	putTrack(t, st, lib2.ID, trackSpec{path: "/lib2/3.flac", essence: "e3", content: "c3", title: "C", artist: "X", album: "Al"})
	// One remote (fileless) episode: no primary file, so no library.
	putFeed(t, st, "http://cast.example/f", "Ep1")

	if n := countWhere(t, st, "library", query.OpIs, string(lib.PID)); n != 2 {
		t.Errorf("library is lib1 = %d, want 2", n)
	}
	if n := countWhere(t, st, "library", query.OpIs, string(lib2.PID)); n != 1 {
		t.Errorf("library is lib2 = %d, want 1", n)
	}
	// A fileless item has a NULL library, so isMissing matches the undownloaded.
	if n := countWhere(t, st, "library", query.OpIsMissing, nil); n != 1 {
		t.Errorf("library isMissing = %d, want 1 (the remote episode)", n)
	}

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupLibrary, "", 0, "")
	if err != nil {
		t.Fatalf("facet library: %v", err)
	}
	if b, ok := bucketByDisplay(res, "/lib"); !ok || b.Count != 2 || b.EntityPID != lib.PID {
		t.Errorf("/lib bucket = %+v, want count 2 keyed by the library pid", b)
	}
	if b, ok := bucketByDisplay(res, "/lib2"); !ok || b.Count != 1 || b.EntityPID != lib2.PID {
		t.Errorf("/lib2 bucket = %+v, want count 1", b)
	}
	if b, ok := bucketByDisplay(res, read.NoFile); !ok || b.Count != 1 || !b.IsUnknown {
		t.Errorf("no-file bucket = %+v, want count 1 + unknown (the episode)", b)
	}
	// Mirror guarantee for the library dimension.
	for _, b := range res.Buckets {
		if b.EntityPID == "" {
			continue
		}
		if n := countWhere(t, st, "library", query.OpIs, string(b.EntityPID)); n != b.Count {
			t.Errorf("library bucket %q count %d != filter count %d", b.Display, b.Count, n)
		}
	}
}

func TestHasArtAcrossKinds(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// Track with its own cover; track without.
	covered := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 32, 32))
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/al/2.flac", essence: "e2", content: "c2",
		title: "Bare", artist: "X", album: "Al"})

	// Book with a cover (item art lives under the 'track' slot for books too).
	if _, err := st.PutScannedBook(ctx, model.PutScannedBookInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/b.m4b"), DisplayPath: "/lib/b.m4b", RelPath: []byte("b.m4b"),
			Kind: model.FileAudio, Size: 1, MTimeNS: 1, ContentHash: "bc", EssenceHash: "be",
			ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: "Covered Book",
			SortKey: model.SortKey("Covered Book"), IdentityKey: "essence:be",
		},
		Book:     model.Book{Author: "A. Author", AuthorSort: model.SortKey("A. Author"), Authors: []string{"A. Author"}},
		CoverArt: testPNG(t, 24, 24),
	}); err != nil {
		t.Fatalf("put covered book: %v", err)
	}

	// Two remote episodes; downloading one attaches its feed cover under the
	// 'episode' slot, which only the kind-switched predicate finds.
	feed := putFeed(t, st, "http://cast.example/f", "Ep1", "Ep2")
	eps, err := st.EpisodesByPodcast(ctx, feed.PodcastPID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %v (err %v), want 2", eps, err)
	}
	// The listing is newest-first; pick Ep1 by title rather than by position.
	ep1 := eps[0]
	if ep1.Title != "Ep1" {
		ep1 = eps[1]
	}
	libID, err := st.EnsurePodcastLibrary(ctx, "/podcasts")
	if err != nil {
		t.Fatalf("podcast library: %v", err)
	}
	if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: ep1.PID, LibraryID: libID,
		File: model.File{
			Path: []byte("/podcasts/ep1.mp3"), DisplayPath: "/podcasts/ep1.mp3", RelPath: []byte("ep1.mp3"),
			Kind: model.FileAudio, ContentHash: "h1", ScanState: model.ScanIndexed,
		},
		Image: testPNG(t, 16, 16),
	}); err != nil {
		t.Fatalf("attach episode file: %v", err)
	}

	withArt, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("has_art", query.OpIs, 1).OrderBy("title", false).Build(), "")
	if err != nil {
		t.Fatalf("has_art query: %v", err)
	}
	got := titlesOf(withArt)
	want := map[string]bool{"Te1": true, "Covered Book": true, "Ep1": true}
	if len(got) != 3 {
		t.Fatalf("has_art is 1 = %v, want the covered track, book, and episode", got)
	}
	for _, title := range got {
		if !want[title] {
			t.Errorf("unexpected has_art item %q", title)
		}
	}
	if n := countWhere(t, st, "has_art", query.OpIs, 0); n != 2 {
		t.Errorf("has_art is 0 = %d, want 2 (the bare track and the remote episode)", n)
	}
	_ = covered

	// Chain-inherited art does not count: album-level art on the bare track's
	// album leaves its own cover absent, which is exactly what has_art exists
	// to find.
	albumPID := scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "Al")
	if err := st.SetEntityArt(ctx, model.ArtAlbum, model.PID(albumPID), model.ArtRoleFront, tinyPNG(t), "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set album art: %v", err)
	}
	if n := countWhere(t, st, "has_art", query.OpIs, 0); n != 2 {
		t.Errorf("has_art is 0 after album art = %d, want still 2 (inherited art is not own art)", n)
	}
}

// TestAdvisoryFields pins why the advisory ask needs two fields rather than one.
// A feed may mark <itunes:explicit> once at the channel level and leave every
// episode unmarked, and WaxBin's parser reads the two flags independently with no
// inheritance, so the channel-marked-only show is the case a single-field design
// gets wrong: `explicit is 0` still returns every one of its episodes. The
// restricted browse is the conjunction of both fields.
func TestAdvisoryFields(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Clean Track", artist: "X", album: "Al"})
	// A wholly-explicit show: the flag sits on the channel and on no episode.
	putFeedAdvisory(t, st, "http://cast.example/adult", true, nil, "A1", "A2")
	// A clean show with one explicit episode: the flag sits on the item only.
	putFeedAdvisory(t, st, "http://cast.example/mixed", false, map[string]bool{"B1": true}, "B1", "B2")

	explicitItems, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("explicit", query.OpIs, 1).Build(), "")
	if err != nil {
		t.Fatalf("explicit query: %v", err)
	}
	if got := titlesOf(explicitItems); !equalStrings(got, []string{"B1"}) {
		t.Errorf("explicit is 1 = %v, want just the item-marked episode [B1]", got)
	}
	showExplicit, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("podcast_explicit", query.OpIs, 1).OrderBy("title", false).Build(), "")
	if err != nil {
		t.Fatalf("podcast_explicit query: %v", err)
	}
	if got := titlesOf(showExplicit); !equalStrings(got, []string{"A1", "A2"}) {
		t.Errorf("podcast_explicit is 1 = %v, want the channel-marked show's episodes [A1 A2]", got)
	}
	// The channel-marked episodes carry no item flag of their own, which is exactly
	// what an explicit-only filter would miss.
	if n := countWhere(t, st, "explicit", query.OpIs, 0); n != 4 {
		t.Errorf("explicit is 0 = %d, want 4 (the track, A1, A2, and B2)", n)
	}

	// The restricted browse: neither the episode nor its show is marked. It keeps the
	// track (a track reads 0 on both, since WaxBin stores no music advisory flag).
	clean, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("explicit", query.OpIs, 0).
		Where("podcast_explicit", query.OpIs, 0).
		OrderBy("title", false).Build(), "")
	if err != nil {
		t.Fatalf("restricted query: %v", err)
	}
	if got := titlesOf(clean); !equalStrings(got, []string{"B2", "Clean Track"}) {
		t.Errorf("restricted browse = %v, want [B2 Clean Track]", got)
	}
}

func TestHasLyricsField(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	if _, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/l.flac"), DisplayPath: "/lib/l.flac", RelPath: []byte("l.flac"),
			Kind: model.FileAudio, ContentHash: "cl", EssenceHash: "el", ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "Sung",
			SortKey: model.SortKey("Sung"), IdentityKey: "essence:el",
		},
		Track:  model.Track{Artist: "X", Album: "Al"},
		Lyrics: &model.Lyrics{Source: model.SourceTag, Unsynced: "la la la"},
	}); err != nil {
		t.Fatalf("put track with lyrics: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/i.flac", essence: "ei", content: "ci",
		title: "Instrumental", artist: "X", album: "Al"})

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("has_lyrics", query.OpIs, 1).Build(), "")
	if err != nil {
		t.Fatalf("has_lyrics query: %v", err)
	}
	if got := titlesOf(items); !equalStrings(got, []string{"Sung"}) {
		t.Errorf("has_lyrics is 1 = %v, want [Sung]", got)
	}
	if n := countWhere(t, st, "has_lyrics", query.OpIs, 0); n != 1 {
		t.Errorf("has_lyrics is 0 = %d, want 1", n)
	}
}

// TestPresenceFieldPlansSeekIndexes is the EXPLAIN sanity check: the has_art
// probe must seek art_map's primary key (the CASE'd entity_type still seeks,
// with the key computed per row; the (entity_type, entity_id, role) PK covers
// the whole probe) and has_lyrics must seek the lyrics rowid PK, with neither
// table scanned. art_source rides the same key, seeking the PK and then fetching
// the row for the source column. Loose string matching, since plan wording varies
// by SQLite version.
func TestPresenceFieldPlansSeekIndexes(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()

	fm, ok := fieldMapFor(query.EntityItems)
	if !ok {
		t.Fatal("no field map for items")
	}
	c, err := query.Compile(query.New(query.EntityItems).
		Where("has_art", query.OpIs, 1).Where("has_lyrics", query.OpIs, 0).
		Where("art_source", query.OpIs, "tag").Build(), fm)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rows, err := st.read.QueryContext(ctx, "EXPLAIN QUERY PLAN "+itemSelect+" WHERE "+c.Where, c.Args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	plan := strings.Join(details, "\n")
	// The PK autoindex name is a SQLite implementation detail; assert a SEARCH on
	// art_map keyed by entity_type rather than the index's exact name.
	if !strings.Contains(plan, "SEARCH amq") || !strings.Contains(plan, "entity_type=?") {
		t.Errorf("has_art probe does not seek the art_map primary key:\n%s", plan)
	}
	if !strings.Contains(plan, "SEARCH asq") {
		t.Errorf("art_source does not seek the art_map primary key:\n%s", plan)
	}
	for _, d := range details {
		if strings.Contains(d, "SCAN") && (strings.Contains(d, "art_map") || strings.Contains(d, "lyrics")) {
			t.Errorf("presence probe full-scans a table: %s", d)
		}
	}
}

func TestSmartRuleRoundTripsNewFields(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	covered := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 32, 32))
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/al/2.flac", essence: "e2", content: "c2",
		title: "Bare", artist: "X", album: "Al"})

	rule := query.New(query.EntityItems).
		Where("library", query.OpIs, string(lib.PID)).
		Where("has_art", query.OpIs, 1).
		Build()
	data, err := query.MarshalRule(rule)
	if err != nil {
		t.Fatalf("marshal rule: %v", err)
	}
	parsed, err := query.ParseRule(data)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	pl, err := st.CreatePlaylist(ctx, "Covered here", "", model.PlaylistSmart, "", &parsed)
	if err != nil {
		t.Fatalf("create smart playlist: %v", err)
	}
	items, err := st.PlaylistItems(ctx, pl, "")
	if err != nil {
		t.Fatalf("playlist items: %v", err)
	}
	if len(items) != 1 || items[0].PID != covered {
		t.Errorf("smart membership = %v, want just the covered track", titlesOf(items))
	}
}

// TestSmartRuleRoundTripsPlaylistPID is the "tracks not in my Archive list" shape
// stored as a rule: it survives the marshal/parse round trip and evaluates as a
// deny-list against the referenced static playlist.
func TestSmartRuleRoundTripsPlaylistPID(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	filed := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Filed", artist: "X", album: "Al"}).ItemPID
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "Loose", artist: "X", album: "Al"})

	archive, err := st.CreatePlaylist(ctx, "Archive", "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, archive, []model.PID{filed}); err != nil {
		t.Fatalf("add: %v", err)
	}

	rule := query.New(query.EntityItems).
		Where("playlist_pid", query.OpIsNot, string(archive)).
		Build()
	data, err := query.MarshalRule(rule)
	if err != nil {
		t.Fatalf("marshal rule: %v", err)
	}
	parsed, err := query.ParseRule(data)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	pl, err := st.CreatePlaylist(ctx, "Unfiled", "", model.PlaylistSmart, "", &parsed)
	if err != nil {
		t.Fatalf("create smart playlist: %v", err)
	}
	items, err := st.PlaylistItems(ctx, pl, "")
	if err != nil {
		t.Fatalf("playlist items: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Loose" {
		t.Errorf("membership = %v, want just the unfiled track", titlesOf(items))
	}
}
