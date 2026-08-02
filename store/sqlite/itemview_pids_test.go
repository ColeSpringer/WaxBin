package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// TestItemViewEntityPIDs pins the entity-handle columns the item view projects: a
// track carries its artist, album-artist, and album entity pids; a book resolves
// its author for the two artist pids and has no album; an episode, which has no
// track or book row, carries none of the three.
func TestItemViewEntityPIDs(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// A track projects all three handles, each resolving the right entity: the
	// track artist, the (distinct) album artist, and the album.
	tr := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/al/01.flac", essence: "e1", content: "c1",
		title: "Airbag", artist: "Track Artist", albumArt: "Album Artist", album: "OK Computer",
		year: 1997, durationMS: 100,
	})
	trackView, err := st.ItemByPID(ctx, tr.ItemPID)
	if err != nil {
		t.Fatalf("track view: %v", err)
	}
	trackArtistPID := entityPIDByName(t, st, "artist", "name", "Track Artist")
	albumArtistPID := entityPIDByName(t, st, "artist", "name", "Album Artist")
	albumPID := entityPIDByName(t, st, "album", "title", "OK Computer")
	if trackView.ArtistPID != trackArtistPID {
		t.Errorf("track ArtistPID = %s, want %s", trackView.ArtistPID, trackArtistPID)
	}
	if trackView.AlbumArtistPID != albumArtistPID {
		t.Errorf("track AlbumArtistPID = %s, want %s", trackView.AlbumArtistPID, albumArtistPID)
	}
	if trackView.AlbumPID != albumPID {
		t.Errorf("track AlbumPID = %s, want %s", trackView.AlbumPID, albumPID)
	}

	// A book resolves its author for both artist handles (the facet membership
	// rule) and has no album pid: a book is not an album member.
	bk := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/book.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 3000,
	})
	bookView, err := st.ItemByPID(ctx, bk.ItemPID)
	if err != nil {
		t.Fatalf("book view: %v", err)
	}
	authorPID := entityPIDByName(t, st, "artist", "name", "J.R.R. Tolkien")
	if bookView.ArtistPID != authorPID {
		t.Errorf("book ArtistPID = %s, want the author %s", bookView.ArtistPID, authorPID)
	}
	if bookView.AlbumArtistPID != authorPID {
		t.Errorf("book AlbumArtistPID = %s, want the author %s", bookView.AlbumArtistPID, authorPID)
	}
	if bookView.AlbumPID != "" {
		t.Errorf("book AlbumPID = %q, want empty (a book is not an album member)", bookView.AlbumPID)
	}

	// An episode has neither a track nor a book row, so all three handles are empty.
	feedURL := "http://feed.example/f"
	feed := model.UpsertFeedInput{
		FeedURL:     feedURL,
		IdentityKey: identity.PodcastKey("", feedURL),
		Feed: model.Feed{Title: "My Show", Author: "Host", Episodes: []model.FeedEpisode{{
			GUID: "g1", Title: "Ep1", EnclosureURL: feedURL + "/e.mp3",
			EnclosureType: "audio/mpeg", DurationMS: 1000, PubDateNS: 1_000_000_000,
		}}},
		FetchedAtNS: 1,
	}
	if _, err := st.UpsertFeed(ctx, feed); err != nil {
		t.Fatalf("upsert feed: %v", err)
	}
	var epPID model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='episode'").Scan(&epPID); err != nil {
		t.Fatalf("episode pid: %v", err)
	}
	epView, err := st.ItemByPID(ctx, epPID)
	if err != nil {
		t.Fatalf("episode view: %v", err)
	}
	if epView.ArtistPID != "" || epView.AlbumArtistPID != "" || epView.AlbumPID != "" {
		t.Errorf("episode pids = %q/%q/%q, want all empty",
			epView.ArtistPID, epView.AlbumArtistPID, epView.AlbumPID)
	}
}

// TestItemViewReleaseGroupPID pins the fourth entity handle and its three empty
// states, which are not one state: a book and an episode have no track row at all,
// while a track whose album carries no release group has an album and still no group
// above it. AlbumPID is re-asserted here because it is now produced by a joined
// column rather than a correlated seek, and the value must not have moved.
func TestItemViewReleaseGroupPID(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	grouped := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/al/01.flac", essence: "e1", content: "c1",
		title: "Everything In Its Right Place", artist: "Radiohead", albumArt: "Radiohead",
		album: "Kid A", year: 2000,
	})
	orphan := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/orphan/01.flac", essence: "e2", content: "c2",
		title: "Loose", artist: "Nobody", albumArt: "Nobody", album: "Orphan",
	})
	detachReleaseGroup(t, st, "Orphan")

	groupedView, err := st.ItemByPID(ctx, grouped.ItemPID)
	if err != nil {
		t.Fatalf("grouped track view: %v", err)
	}
	rgPID := entityPIDByName(t, st, "release_group", "title", "Kid A")
	albumPID := entityPIDByName(t, st, "album", "title", "Kid A")
	if groupedView.ReleaseGroupPID != rgPID {
		t.Errorf("track ReleaseGroupPID = %s, want %s", groupedView.ReleaseGroupPID, rgPID)
	}
	if groupedView.AlbumPID != albumPID {
		t.Errorf("track AlbumPID = %s, want %s (the joined column must read what the seek did)",
			groupedView.AlbumPID, albumPID)
	}

	orphanView, err := st.ItemByPID(ctx, orphan.ItemPID)
	if err != nil {
		t.Fatalf("orphan track view: %v", err)
	}
	if orphanView.AlbumPID == "" {
		t.Error("orphan track AlbumPID is empty; it does have an album, only no release group")
	}
	if orphanView.ReleaseGroupPID != "" {
		t.Errorf("orphan track ReleaseGroupPID = %q, want empty", orphanView.ReleaseGroupPID)
	}

	bk := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/book.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 3000,
	})
	bookView, err := st.ItemByPID(ctx, bk.ItemPID)
	if err != nil {
		t.Fatalf("book view: %v", err)
	}
	if bookView.ReleaseGroupPID != "" {
		t.Errorf("book ReleaseGroupPID = %q, want empty", bookView.ReleaseGroupPID)
	}

	putFeed(t, st, "http://cast.example/f", "Ep1")
	var epPID model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='episode'").Scan(&epPID); err != nil {
		t.Fatalf("episode pid: %v", err)
	}
	epView, err := st.ItemByPID(ctx, epPID)
	if err != nil {
		t.Fatalf("episode view: %v", err)
	}
	if epView.ReleaseGroupPID != "" {
		t.Errorf("episode ReleaseGroupPID = %q, want empty", epView.ReleaseGroupPID)
	}
}

// TestItemViewExplicit pins the projected advisory flag against the field that
// filters on it: a row's Explicit and an `explicit is 1` match must agree for
// every kind, which is the contract the field map carries. A track and a book read
// false because WaxBin stores no music advisory flag, and an episode of a
// channel-marked show reads false too: the show's flag is reached through
// podcast_pid, not projected onto the item's own row.
func TestItemViewExplicit(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	tr := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Track", artist: "X", album: "Al"})
	bk := putBook(t, st, lib.ID, bookSpec{path: "/lib/b.m4b", essence: "be1", content: "bc1",
		title: "Book", author: "A. Author"})
	// A channel-explicit show with one item-marked episode and one unmarked.
	putFeedAdvisory(t, st, "http://cast.example/f", true, map[string]bool{"Marked": true}, "Marked", "Unmarked")

	// The set of items the explicit field matches, by pid.
	matched, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("explicit", query.OpIs, 1).Build(), "")
	if err != nil {
		t.Fatalf("explicit query: %v", err)
	}
	byField := make(map[model.PID]bool, len(matched))
	for _, v := range matched {
		byField[v.PID] = true
	}

	pids := []model.PID{tr.ItemPID, bk.ItemPID}
	rows, err := st.read.QueryContext(ctx, "SELECT pid FROM playable_item WHERE kind='episode'")
	if err != nil {
		t.Fatalf("episode pids: %v", err)
	}
	for rows.Next() {
		var pid model.PID
		if err := rows.Scan(&pid); err != nil {
			t.Fatalf("scan episode pid: %v", err)
		}
		pids = append(pids, pid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("episode pid rows: %v", err)
	}

	var explicitTitles []string
	for _, pid := range pids {
		v, err := st.ItemByPID(ctx, pid)
		if err != nil {
			t.Fatalf("item %s: %v", pid, err)
		}
		if v.Explicit != byField[pid] {
			t.Errorf("%s (%s) Explicit = %v, but the explicit field match = %v",
				v.Title, v.Kind, v.Explicit, byField[pid])
		}
		if v.Explicit {
			explicitTitles = append(explicitTitles, v.Title)
		}
	}
	if !equalStrings(explicitTitles, []string{"Marked"}) {
		t.Errorf("projected Explicit = %v, want just [Marked]", explicitTitles)
	}
}
