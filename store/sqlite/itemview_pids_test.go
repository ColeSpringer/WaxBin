package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
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
