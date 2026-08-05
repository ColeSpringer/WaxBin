package sqlite

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// This file covers the scanner-written track artist credit: one item_contributor row
// per artist the credit names, the primary artist on track.artist_id, and the MBID
// pairing rule.

// creditedArtists returns an item's RoleArtist credits in credited order.
func creditedArtists(t *testing.T, st *Store, itemPID model.PID) []string {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(), `SELECT a.name
		FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id
		JOIN playable_item pi ON pi.id = ic.item_id
		WHERE pi.pid = ? AND ic.role = 'artist' ORDER BY ic.position`, string(itemPID))
	if err != nil {
		t.Fatalf("read credits: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan credit: %v", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("credits: %v", err)
	}
	return out
}

// trackArtistName returns the artist entity track.artist_id points at.
func trackArtistName(t *testing.T, st *Store, itemPID model.PID) string {
	t.Helper()
	var name string
	err := st.read.QueryRowContext(context.Background(), `SELECT COALESCE(a.name,'')
		FROM track tr JOIN playable_item pi ON pi.id = tr.item_id
		LEFT JOIN artist a ON a.id = tr.artist_id WHERE pi.pid = ?`, string(itemPID)).Scan(&name)
	if err != nil {
		t.Fatalf("read track artist: %v", err)
	}
	return name
}

func TestScanSplitsAFeaturedCreditIntoOneArtistEach(t *testing.T) {
	st, lib := entityFixture(t)
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Empire State of Mind",
		artist: "Jay-Z feat. Alicia Keys", artists: []string{"Jay-Z", "Alicia Keys"},
		albumArt: "Jay-Z", album: "The Blueprint 3", durationMS: 100,
	})
	if got := creditedArtists(t, st, res.ItemPID); !reflect.DeepEqual(got, []string{"Jay-Z", "Alicia Keys"}) {
		t.Errorf("credits = %v, want [Jay-Z Alicia Keys]", got)
	}
	// artist_id points at the primary, not a combined entity and not NULL: NULL would
	// drop the track out of the artist facet, the rollups, and artist drilldown.
	if got := trackArtistName(t, st, res.ItemPID); got != "Jay-Z" {
		t.Errorf("track.artist_id names %q, want Jay-Z", got)
	}
	// The denormalized column keeps the file's raw string, so artist_sort and the
	// organize path stay in step with what the file says.
	if got := scalarStr(t, st, "SELECT artist FROM track"); got != "Jay-Z feat. Alicia Keys" {
		t.Errorf("track.artist = %q, want the file's raw credit", got)
	}
}

func TestScanKeepsAnUnsplitCreditAsOneArtist(t *testing.T) {
	st, lib := entityFixture(t)
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "September",
		artist: "Earth, Wind & Fire", artists: []string{"Earth, Wind", "Fire"},
		albumArt: "Earth, Wind & Fire", album: "The Best Of", durationMS: 100,
	})
	// The splitter's answer is taken as given here: the scanner decides, the store
	// stores. What this pins is that a two-name credit produces two rows and the first
	// is primary, with no re-splitting or re-joining in between.
	if got := creditedArtists(t, st, res.ItemPID); !reflect.DeepEqual(got, []string{"Earth, Wind", "Fire"}) {
		t.Errorf("credits = %v, want the two names as supplied", got)
	}

	// A pre-split caller (a cue sheet, an older path) supplies only the raw string,
	// and a credit with no marker in it stays one artist.
	res2 := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/1.flac", essence: "e2", content: "c2", title: "Suite: Judy Blue Eyes",
		artist: "Crosby, Stills, Nash", albumArt: "Crosby, Stills, Nash", album: "CSN", durationMS: 100,
	})
	if got := creditedArtists(t, st, res2.ItemPID); !reflect.DeepEqual(got, []string{"Crosby, Stills, Nash"}) {
		t.Errorf("unsplit credit = %v, want one artist", got)
	}
}

// TestScanPrefersARepeatedArtistFrameOverSplitting pins the exclusive two-branch
// precedence: a file that repeated the ARTIST frame stated its list, so those values
// are taken verbatim even when one of them contains a splitter marker.
func TestScanPrefersARepeatedArtistFrameOverSplitting(t *testing.T) {
	st, lib := entityFixture(t)
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Collab",
		artist: "A vs. B; Sunn O)))", artists: []string{"A vs. B", "Sunn O)))"},
		albumArt: "A vs. B", album: "Split", durationMS: 100,
	})
	if got := creditedArtists(t, st, res.ItemPID); !reflect.DeepEqual(got, []string{"A vs. B", "Sunn O)))"}) {
		t.Errorf("credits = %v, want the repeated frame's values verbatim", got)
	}
}

// TestRescanReplacesOnlyTheArtistRoleOnATrack is the highest-value test here: it
// catches a copy of the book path's wholesale contributor delete. A track's other
// music credits have no scan-side input, so a rescan that cleared them would destroy
// a curated producer list on every content change.
func TestRescanReplacesOnlyTheArtistRoleOnATrack(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z feat. Alicia Keys", artists: []string{"Jay-Z", "Alicia Keys"},
		albumArt: "Jay-Z", album: "Album", durationMS: 100,
	}
	res := putTrack(t, st, lib.ID, spec)

	if _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleProducer,
		[]string{"Rick Rubin"}, model.SourceUser, false, false); err != nil {
		t.Fatalf("set producer: %v", err)
	}

	// A content change re-derives the credit from the file.
	spec.content = "c2"
	spec.artist, spec.artists = "Jay-Z feat. Beyonce", []string{"Jay-Z", "Beyonce"}
	putTrack(t, st, lib.ID, spec)

	if got := creditedArtists(t, st, res.ItemPID); !reflect.DeepEqual(got, []string{"Jay-Z", "Beyonce"}) {
		t.Errorf("artist credits after rescan = %v, want the new pair", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM item_contributor WHERE role = 'producer'"); n != 1 {
		t.Errorf("producer credits after rescan = %d, want the curated one to survive", n)
	}
	// The dropped artist keeps a zero rollup rather than none, so db verify stays
	// clean (a missing row reads as -1 against a recompute of 0).
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Errorf("verify after rescan: %+v (err %v)", r, err)
	}
}

// TestLockedArtistCreditSurvivesAForcedRescan checks the widened lock arm: either
// spelling preserves the display, the derived sort, AND the split list, which cannot
// be re-derived because the display joins with a comma and the splitter does not
// split on one.
func TestLockedArtistCreditSurvivesAForcedRescan(t *testing.T) {
	for _, field := range []string{"artist", "credit.artist"} {
		t.Run(field, func(t *testing.T) {
			st, lib := entityFixture(t)
			ctx := context.Background()
			spec := trackSpec{
				path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
				artist: "Wrong Name", albumArt: "Wrong Name", album: "Album", durationMS: 100,
			}
			res := putTrack(t, st, lib.ID, spec)

			if field == "artist" {
				if err := st.EditItemField(ctx, res.ItemPID, "artist", "Jay-Z feat. Alicia Keys",
					model.SourceUser, true, false); err != nil {
					t.Fatalf("edit artist: %v", err)
				}
			} else {
				if _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleArtist,
					[]string{"Jay-Z", "Alicia Keys"}, model.SourceUser, true, false); err != nil {
					t.Fatalf("set artist credit: %v", err)
				}
			}
			before := creditedArtists(t, st, res.ItemPID)
			beforeDisplay := scalarStr(t, st, "SELECT artist FROM track")

			// A forced rescan re-derives everything from the file, which still says
			// "Wrong Name".
			spec.content = "c2"
			spec.preserveLocks = true
			putTrack(t, st, lib.ID, spec)

			if got := creditedArtists(t, st, res.ItemPID); !reflect.DeepEqual(got, before) {
				t.Errorf("credits after rescan = %v, want the locked %v", got, before)
			}
			if got := scalarStr(t, st, "SELECT artist FROM track"); got != beforeDisplay {
				t.Errorf("track.artist after rescan = %q, want the locked %q", got, beforeDisplay)
			}
			if got := scalarStr(t, st, "SELECT artist_sort FROM track"); got != model.SortKey(beforeDisplay) {
				t.Errorf("artist_sort after rescan = %q, want it to ride with the locked display", got)
			}
		})
	}
}

// TestAFeaturedArtistRollsUpToZeroWithoutDrifting pins the reason resolveTrackArtists
// records every resolved artist as affected: a featured artist backs no track's
// artist_id, so without a rollup row of its own the drift query reads a missing row
// as -1 against a recompute of 0 and reports permanent drift.
func TestAFeaturedArtistRollsUpToZeroWithoutDrifting(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z feat. Alicia Keys", artists: []string{"Jay-Z", "Alicia Keys"},
		albumArt: "Jay-Z", album: "Album", durationMS: 100,
	})
	if n := scalarInt(t, st,
		`SELECT COALESCE(ar.track_count, -1) FROM artist a
		 LEFT JOIN artist_rollup ar ON ar.artist_id = a.id WHERE a.name = 'Alicia Keys'`); n != 0 {
		t.Errorf("featured artist rollup track_count = %d, want a real 0 row", n)
	}
	r, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if r.ArtistRollupDrift != 0 {
		t.Errorf("ArtistRollupDrift = %d, want 0", r.ArtistRollupDrift)
	}
}

// TestArtistFacetStillBucketsAJointCreditOnce guards the read surface: GroupArtist and
// itemFields["artist_pid"] share one expression over track.artist_id, so the split
// must not fan a track across buckets. A featured artist reads with ItemCount 0,
// exactly as a book narrator already does.
func TestArtistFacetStillBucketsAJointCreditOnce(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z feat. Alicia Keys", artists: []string{"Jay-Z", "Alicia Keys"},
		albumArt: "Jay-Z", album: "Album", durationMS: 100,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE artist_id IS NOT NULL"); n != 1 {
		t.Errorf("tracks with an artist_id = %d, want 1", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(DISTINCT artist_id) FROM track WHERE artist_id IS NOT NULL"); n != 1 {
		t.Errorf("distinct artist buckets = %d, want 1 (the primary)", n)
	}
	if n := scalarInt(t, st,
		`SELECT COALESCE(ar.track_count,-1) FROM artist a JOIN artist_rollup ar ON ar.artist_id = a.id
		 WHERE a.name = 'Jay-Z'`); n != 1 {
		t.Errorf("primary artist rollup = %d, want 1", n)
	}
}

// TestCreditArtistFacetFansOutWhereArtistDoesNot is the whole point of the dimension:
// the same track buckets once under its primary in GroupArtist and once per credited
// artist here, so a featured artist is browsable.
func TestCreditArtistFacetFansOutWhereArtistDoesNot(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z feat. Alicia Keys", artists: []string{"Jay-Z", "Alicia Keys"},
		albumArt: "Jay-Z", album: "Album", durationMS: 100,
	})

	buckets := func(g read.GroupBy) map[string]int {
		res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), g, "", 0, "")
		if err != nil {
			t.Fatalf("facet %s: %v", g, err)
		}
		out := map[string]int{}
		for _, b := range res.Buckets {
			out[b.Display] = b.Count
		}
		return out
	}

	if got := buckets(read.GroupArtist); len(got) != 1 || got["Jay-Z"] != 1 {
		t.Errorf("artist buckets = %v, want only Jay-Z", got)
	}
	credit := buckets(read.GroupCreditArtist)
	if credit["Jay-Z"] != 1 || credit["Alicia Keys"] != 1 {
		t.Errorf("credit-artist buckets = %v, want both artists at 1", credit)
	}
	// One item, two buckets: the counts sum above the item count, as genre's do.
	if len(credit) != 2 {
		t.Errorf("credit-artist buckets = %v, want exactly two", credit)
	}
}

// TestSetArtistCreditRewritesTheTrackDisplayAndItsArtistID covers the credit-edit arm
// of syncCreditDenormTx, where the display IS rebuilt from the names because the user
// typed them.
func TestSetArtistCreditRewritesTheTrackDisplayAndItsArtistID(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Old Name", albumArt: "Old Name", album: "Album", durationMS: 100,
	})
	if _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleArtist,
		[]string{"Jay-Z", "Alicia Keys"}, model.SourceUser, false, false); err != nil {
		t.Fatalf("set artist credit: %v", err)
	}
	if got := scalarStr(t, st, "SELECT artist FROM track"); got != "Jay-Z, Alicia Keys" {
		t.Errorf("track.artist = %q, want the rebuilt display", got)
	}
	if got := scalarStr(t, st, "SELECT artist_sort FROM track"); got != model.SortKey("Jay-Z, Alicia Keys") {
		t.Errorf("artist_sort = %q, want it derived from the new display", got)
	}
	if got := trackArtistName(t, st, res.ItemPID); got != "Jay-Z" {
		t.Errorf("track.artist_id names %q, want the first credited artist", got)
	}
}

// TestSetArtistCreditRefreshesTheOutgoingArtistRollup covers a catalog scanned before
// credits existed: it has no RoleArtist rows, so the artist track.artist_id points
// away from is not a prior contributor and its rollup would drift unrecomputed.
func TestSetArtistCreditRefreshesTheOutgoingArtistRollup(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Old Name", albumArt: "Old Name", album: "Album", durationMS: 100,
	})
	// Strip the credit rows to reproduce a pre-Phase-6 catalog.
	if _, err := st.write.ExecContext(ctx, "DELETE FROM item_contributor WHERE role = 'artist'"); err != nil {
		t.Fatal(err)
	}

	if _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleArtist,
		[]string{"New Name"}, model.SourceUser, false, false); err != nil {
		t.Fatalf("set artist credit: %v", err)
	}
	if n := scalarInt(t, st,
		`SELECT COALESCE(ar.track_count,-1) FROM artist a
		 LEFT JOIN artist_rollup ar ON ar.artist_id = a.id WHERE a.name = 'Old Name'`); n != 0 {
		t.Errorf("outgoing artist rollup track_count = %d, want 0", n)
	}
	r, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if r.ArtistRollupDrift != 0 {
		t.Errorf("ArtistRollupDrift = %d, want 0", r.ArtistRollupDrift)
	}
}

// TestSetArtistCreditRefreshesTheTrackSearchRow is the only cover for the FTS path a
// track credit edit takes. The scan and scalar-edit paths reach syncSearchFTS through
// resolveAndLinkEntities; this one rewrites track.artist without passing through it,
// and answering its ftsDirty with the book rebuild would write the wrong row.
func TestSetArtistCreditRefreshesTheTrackSearchRow(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Old Name", albumArt: "Old Name", album: "Album", durationMS: 100,
	})
	if _, err := st.SetItemCredits(ctx, res.ItemPID, model.RoleArtist,
		[]string{"Jay-Z"}, model.SourceUser, false, false); err != nil {
		t.Fatalf("set artist credit: %v", err)
	}
	got := scalarStr(t, st, "SELECT artist FROM search_fts WHERE rowid = (SELECT id FROM playable_item)")
	if got == "" || got == "Old Name Old Name" {
		t.Fatalf("search row artist = %q, want it refreshed to the new credit", got)
	}
	// A track's search artist is its artist plus its album artist, not a book's
	// author plus narrator.
	if want := "Jay-Z Old Name"; got != want {
		t.Errorf("search row artist = %q, want %q", got, want)
	}
}

// --- the MBID pairing rule -------------------------------------------------

func TestEachSplitArtistTakesItsOwnMBID(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z feat. Alicia Keys", artists: []string{"Jay-Z", "Alicia Keys"},
		albumArt: "Jay-Z", album: "Album", durationMS: 100,
		mbArtists: []string{"jayz-id", "keys-id"}, mbAlbumArtists: []string{"jayz-id"},
	})
	if got := artistMBID(t, st, "Jay-Z"); got != "jayz-id" {
		t.Errorf("Jay-Z mbid = %q, want jayz-id", got)
	}
	if got := artistMBID(t, st, "Alicia Keys"); got != "keys-id" {
		t.Errorf("Alicia Keys mbid = %q, want keys-id (positional pairing)", got)
	}
}

func TestACreditWhoseIDCountDisagreesTakesNone(t *testing.T) {
	st, lib := entityFixture(t)
	// Three ids against two names: the file and the split describe different things,
	// so every id is dropped rather than paired by position into a wrong answer.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z feat. Alicia Keys", artists: []string{"Jay-Z", "Alicia Keys"},
		albumArt: "Jay-Z", album: "Album", durationMS: 100,
		mbArtists: []string{"jayz-id", "keys-id", "extra-id"},
	})
	if got := artistMBID(t, st, "Jay-Z"); got != "" {
		t.Errorf("Jay-Z mbid = %q, want empty on an arity mismatch", got)
	}
	if got := artistMBID(t, st, "Alicia Keys"); got != "" {
		t.Errorf("Alicia Keys mbid = %q, want empty on an arity mismatch", got)
	}
}

// TestASingleTaggedIDKeepsASplitCreditWhole is the reason the arity rule exists. One
// id against a credit the splitter broke apart is the file saying MusicBrainz models
// this as one entity, so it collapses back to the credit it names and takes that id.
// No artists here, so the list is a split rather than one the file stated.
func TestASingleTaggedIDKeepsASplitCreditWhole(t *testing.T) {
	st, lib := entityFixture(t)
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist:   "Brooks; Dunn",
		albumArt: "Brooks; Dunn", album: "The Best Of", durationMS: 100,
		mbArtists: []string{"bd-id"},
	})
	if got := creditedArtists(t, st, res.ItemPID); !reflect.DeepEqual(got, []string{"Brooks; Dunn"}) {
		t.Errorf("credits = %v, want the whole credit as one artist", got)
	}
	if got := artistMBID(t, st, "Brooks; Dunn"); got != "bd-id" {
		t.Errorf("collapsed credit mbid = %q, want bd-id", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name IN ('Brooks','Dunn')"); n != 0 {
		t.Errorf("split artists = %d, want none created", n)
	}
}

// TestScanDoesNotSplitABandNameOnItsPunctuation guards the population phase 6
// targets, an untagged library with no artist ids to recover from. A wrong split there
// creates artist rows nothing later removes.
func TestScanDoesNotSplitABandNameOnItsPunctuation(t *testing.T) {
	st, lib := entityFixture(t)
	for i, name := range []string{"AC/DC", "Hall & Oates", "Sly & the Family Stone"} {
		spec := trackSpec{
			path: "/lib/" + string(rune('a'+i)) + "/1.flac", essence: "e" + string(rune('1'+i)),
			content: "c" + string(rune('1'+i)), title: "Song", durationMS: 100,
			artist: name, albumArt: name, album: "Album " + name,
		}
		res := putTrack(t, st, lib.ID, spec)
		if got := creditedArtists(t, st, res.ItemPID); !reflect.DeepEqual(got, []string{name}) {
			t.Errorf("credits for %q = %v, want the one band", name, got)
		}
		if got := trackArtistName(t, st, res.ItemPID); got != name {
			t.Errorf("track.artist_id for %q names %q", name, got)
		}
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist"); n != 3 {
		t.Errorf("artist rows = %d, want 3 (one per band, none split)", n)
	}
}

// TestAStatedListSurvivesASingleTaggedID: the collapse applies to a split credit, not
// to a list the file stated. Collapsing here would drop every artist after the first,
// since the raw display carries only the first value.
func TestAStatedListSurvivesASingleTaggedID(t *testing.T) {
	st, lib := entityFixture(t)
	res := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z", artists: []string{"Jay-Z", "Alicia Keys"},
		albumArt: "Jay-Z", album: "Album", durationMS: 100,
		mbArtists: []string{"jayz-id"},
	})
	if got := creditedArtists(t, st, res.ItemPID); !reflect.DeepEqual(got, []string{"Jay-Z", "Alicia Keys"}) {
		t.Errorf("credits = %v, want both stated artists", got)
	}
	// The arity still disagrees, so neither takes the id.
	if got := artistMBID(t, st, "Jay-Z"); got != "" {
		t.Errorf("Jay-Z mbid = %q, want empty (one id against two stated names)", got)
	}
}

// TestArtistMBIDBackfillReachesASplitArtist: an id arriving on a later scan lands on
// an artist row the earlier scan created without one.
func TestArtistMBIDBackfillReachesASplitArtist(t *testing.T) {
	st, lib := entityFixture(t)
	spec := trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z feat. Alicia Keys", artists: []string{"Jay-Z", "Alicia Keys"},
		albumArt: "Jay-Z", album: "Album", durationMS: 100,
	}
	putTrack(t, st, lib.ID, spec)
	if got := artistMBID(t, st, "Alicia Keys"); got != "" {
		t.Fatalf("untagged scan already set an mbid: %q", got)
	}
	spec.content = "c2"
	spec.mbArtists = []string{"jayz-id", "keys-id"}
	putTrack(t, st, lib.ID, spec)
	if got := artistMBID(t, st, "Alicia Keys"); got != "keys-id" {
		t.Errorf("backfilled featured artist mbid = %q, want keys-id", got)
	}
}

// TestARepeatedArtistFrameDoesNotRekeyTheAlbum: with no ALBUMARTIST the release group
// is keyed off the track artist, and a file that repeated its ARTIST frame anchored
// that on the FIRST value. track.artist now shows the whole list, so this pins that
// the key did not follow it: otherwise the first rescan after upgrade would fork every
// heuristically-grouped album in such a library, abandoning its pid and everything
// hanging off it.
func TestARepeatedArtistFrameDoesNotRekeyTheAlbum(t *testing.T) {
	st, lib := entityFixture(t)
	// Scanned before the display joined the list: one ARTIST value, no ALBUMARTIST.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z", album: "Album", durationMS: 100,
	})
	rgPID := scalarStr(t, st, "SELECT pid FROM release_group")
	albumPID := scalarStr(t, st, "SELECT pid FROM album")

	// The same file after the upgrade: the display is the joined list, and the stated
	// list carries the artists.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c2", title: "Song",
		artist: "Jay-Z, Alicia Keys", artists: []string{"Jay-Z", "Alicia Keys"},
		album: "Album", durationMS: 100,
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Errorf("release groups = %d, want 1 (the joined display must not re-key)", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("albums = %d, want 1", n)
	}
	if got := scalarStr(t, st, "SELECT pid FROM release_group"); got != rgPID {
		t.Errorf("release-group pid changed from %s to %s", rgPID, got)
	}
	if got := scalarStr(t, st, "SELECT pid FROM album"); got != albumPID {
		t.Errorf("album pid changed from %s to %s", albumPID, got)
	}
	// And the album artist is a real artist, not an entity named for the joined list.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name = 'Jay-Z, Alicia Keys'"); n != 0 {
		t.Errorf("combined album-artist entities = %d, want none", n)
	}
}

// TestAlbumArtistResolvesToItsPrimary: a joint album-artist credit no longer creates
// an entity named for the whole string, so the primary takes its own id.
func TestAlbumArtistResolvesToItsPrimary(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z", albumArt: "Jay-Z feat. Alicia Keys", album: "Album", durationMS: 100,
		mbAlbumArtists: []string{"jayz-id", "keys-id"},
	})
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM artist WHERE name = 'Jay-Z feat. Alicia Keys'"); n != 0 {
		t.Errorf("combined album-artist entities = %d, want none", n)
	}
	if got := artistMBID(t, st, "Jay-Z"); got != "jayz-id" {
		t.Errorf("primary album artist mbid = %q, want jayz-id", got)
	}
	if got := scalarStr(t, st,
		`SELECT a.name FROM release_group rg JOIN artist a ON a.id = rg.primary_artist_id`); got != "Jay-Z" {
		t.Errorf("release group primary artist = %q, want Jay-Z", got)
	}
}

// TestAlbumArtistSplitDoesNotRekeyTheReleaseGroup is the guard that made the split
// safe. ReleaseGroupKey reads the RAW album-artist string, not the resolved entity,
// so an album scanned before the split keeps its match_key, its row, and its pid.
func TestAlbumArtistSplitDoesNotRekeyTheReleaseGroup(t *testing.T) {
	st, lib := entityFixture(t)
	spec := trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z", albumArt: "Jay-Z feat. Alicia Keys", album: "Album", durationMS: 100,
	}
	putTrack(t, st, lib.ID, spec)
	key := scalarStr(t, st, "SELECT match_key FROM release_group")
	pid := scalarStr(t, st, "SELECT pid FROM release_group")
	if !strings.Contains(key, identity.MatchKey("Jay-Z feat. Alicia Keys")) {
		t.Fatalf("release-group key %q does not carry the raw combined credit", key)
	}

	spec.content = "c2"
	putTrack(t, st, lib.ID, spec)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Errorf("release groups after rescan = %d, want 1 (no fork)", n)
	}
	if got := scalarStr(t, st, "SELECT pid FROM release_group"); got != pid {
		t.Errorf("release-group pid changed from %s to %s", pid, got)
	}
}

// TestReleaseGroupAdoptsANewPrimaryArtist covers the transition on an existing
// catalog: a group left pointing at a combined artist repoints on rescan, and both
// artists' rollups are recomputed so db verify stays clean.
func TestReleaseGroupAdoptsANewPrimaryArtist(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "Song",
		artist: "Jay-Z", albumArt: "Jay-Z feat. Alicia Keys", album: "Album", durationMS: 100,
	}
	putTrack(t, st, lib.ID, spec)

	// Reproduce the pre-split shape: a combined artist holding the group.
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO artist(pid, name, sort_key, match_key) VALUES ('OLD','Jay-Z feat. Alicia Keys','x','jay-z feat. alicia keys')"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.ExecContext(ctx,
		"UPDATE release_group SET primary_artist_id = (SELECT id FROM artist WHERE pid='OLD')"); err != nil {
		t.Fatal(err)
	}
	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("refresh rollups: %v", err)
	}

	spec.content = "c2"
	putTrack(t, st, lib.ID, spec)

	if got := scalarStr(t, st,
		`SELECT a.name FROM release_group rg JOIN artist a ON a.id = rg.primary_artist_id`); got != "Jay-Z" {
		t.Errorf("release group primary artist = %q, want the repointed Jay-Z", got)
	}
	r, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if r.ArtistRollupDrift != 0 {
		t.Errorf("ArtistRollupDrift = %d, want 0", r.ArtistRollupDrift)
	}
}

func TestCreditMBIDsReducesToTheSoleCase(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		ids   []string
		want  []string
	}{
		{"one and one", []string{"Jay-Z"}, []string{"jayz-id"}, []string{"jayz-id"}},
		{"one and none", []string{"Jay-Z"}, nil, []string{""}},
		{"one and two", []string{"Jay-Z"}, []string{"a", "b"}, []string{""}},
		{"two and two", []string{"A", "B"}, []string{"x", "y"}, []string{"x", "y"}},
		{"two and one", []string{"A", "B"}, []string{"x"}, []string{"", ""}},
		{"blanks do not count", []string{"Jay-Z"}, []string{" ", "jayz-id"}, []string{"jayz-id"}},
		{"none", nil, []string{"x"}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := model.CreditMBIDs(c.names, c.ids)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("CreditMBIDs(%v, %v) = %v, want %v", c.names, c.ids, got, c.want)
			}
		})
	}
}
