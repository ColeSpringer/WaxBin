package sqlite

import (
	"context"
	"testing"
)

// countRows is a tiny helper for the identity hard-case assertions.
func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.read.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestVariousArtistsCompilationGroups verifies a Various Artists compilation (many
// track artists under one album-artist) collapses to a single album and release
// group keyed by the album-artist, not fragmented per track artist.
func TestVariousArtistsCompilationGroups(t *testing.T) {
	st, lib := entityFixture(t)
	for i, artist := range []string{"Artist A", "Artist B", "Artist C"} {
		putTrack(t, st, lib.ID, trackSpec{
			path:        "/lib/VA/Now100/" + string(rune('1'+i)) + ".flac",
			essence:     "e" + string(rune('1'+i)),
			content:     "c" + string(rune('1'+i)),
			title:       "Track " + string(rune('1'+i)),
			artist:      artist,
			albumArt:    "Various Artists",
			album:       "Now 100",
			year:        2020,
			compilation: true,
		})
	}
	if got := countRows(t, st, "album"); got != 1 {
		t.Errorf("VA compilation produced %d albums, want 1", got)
	}
	if got := countRows(t, st, "release_group"); got != 1 {
		t.Errorf("VA compilation produced %d release groups, want 1", got)
	}
	// The release group is anchored on the Various Artists album-artist entity.
	var rgArtist string
	if err := st.read.QueryRowContext(context.Background(),
		`SELECT a.name FROM release_group rg JOIN artist a ON a.id = rg.primary_artist_id`).Scan(&rgArtist); err != nil {
		t.Fatal(err)
	}
	if rgArtist != "Various Artists" {
		t.Errorf("release-group primary artist = %q, want Various Artists", rgArtist)
	}
}

// TestClassicalMultiPerformerAlbumGroups verifies an album whose tracks credit
// different performers (a classical recording with per-movement soloists) still
// groups into one album, because album identity keys on the album-artist, not the
// varying track artist.
func TestClassicalMultiPerformerAlbumGroups(t *testing.T) {
	st, lib := entityFixture(t)
	performers := []string{"Soloist One", "Soloist Two", "Full Orchestra"}
	for i, p := range performers {
		putTrack(t, st, lib.ID, trackSpec{
			path:     "/lib/Classical/Symphony/" + string(rune('1'+i)) + ".flac",
			essence:  "ce" + string(rune('1'+i)),
			content:  "cc" + string(rune('1'+i)),
			title:    "Movement " + string(rune('1'+i)),
			artist:   p,
			albumArt: "Berlin Philharmonic",
			album:    "Beethoven: Symphony No. 9",
			composer: "Ludwig van Beethoven",
			year:     2019,
		})
	}
	if got := countRows(t, st, "album"); got != 1 {
		t.Errorf("classical multi-performer album produced %d albums, want 1", got)
	}
	// Every distinct performer plus the album-artist becomes its own artist entity.
	if got := countRows(t, st, "artist"); got != 4 {
		t.Errorf("artist entities = %d, want 4 (3 performers + album artist)", got)
	}
}

// TestBoxSetDiscsShareReleaseGroup verifies a multi-disc box set, with tracks laid
// out in per-disc folders, yields one release group above its per-disc album rows.
// Browse groups the set as one work while the disc folders stay distinct editions
// (the album key includes the folder; the release-group key does not).
func TestBoxSetDiscsShareReleaseGroup(t *testing.T) {
	st, lib := entityFixture(t)
	discs := []string{"Disc 1", "Disc 2", "Disc 3"}
	for i, disc := range discs {
		putTrack(t, st, lib.ID, trackSpec{
			path:      "/lib/Zeppelin/Complete/" + disc + "/1.flac",
			essence:   "be" + string(rune('1'+i)),
			content:   "bc" + string(rune('1'+i)),
			title:     "Song " + disc,
			artist:    "Led Zeppelin",
			albumArt:  "Led Zeppelin",
			album:     "The Complete Studio Recordings",
			year:      1993,
			discTotal: 3,
		})
	}
	if got := countRows(t, st, "release_group"); got != 1 {
		t.Errorf("box set produced %d release groups, want 1 (discs share the work)", got)
	}
	if got := countRows(t, st, "album"); got != 3 {
		t.Errorf("box set produced %d albums, want 3 (one per disc folder)", got)
	}
	// All three disc albums hang under the single release group.
	var linked int
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM album WHERE release_group_id IS NOT NULL").Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 3 {
		t.Errorf("%d albums linked to a release group, want 3", linked)
	}
}
