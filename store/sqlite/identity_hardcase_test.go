package sqlite

import (
	"context"
	"strconv"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// countRows is a tiny helper for the identity hard-case assertions.
func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.rdb().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestVariousArtistsCompilationGroups verifies a Various Artists compilation (many
// track artists under one album-artist) collapses to a single album and release
// group keyed by the album-artist, not fragmented per track artist.
func TestVariousArtistsCompilationGroups(t *testing.T) {
	t.Parallel()
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
	if err := st.rdb().QueryRowContext(context.Background(),
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
	t.Parallel()
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

// TestBoxSetDiscFoldersAreOneAlbum verifies a multi-disc box set, with tracks laid out
// in per-disc folders ("Disc 1", "CD2"), is one album under one release group: a disc
// folder names a disc, not an edition, so the album key takes the folder above it. A
// folder that is not a disc folder still keys an album of its own.
func TestBoxSetDiscFoldersAreOneAlbum(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	discs := []string{"Disc 1", "Disc 2", "CD3"}
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
		t.Errorf("box set produced %d release groups, want 1", got)
	}
	if got := countRows(t, st, "album"); got != 1 {
		t.Errorf("box set produced %d albums, want 1 across its disc folders", got)
	}

	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Zeppelin/Complete/Bonus/1.flac", essence: "be9", content: "bc9",
		title: "Bonus Song", artist: "Led Zeppelin", albumArt: "Led Zeppelin",
		album: "The Complete Studio Recordings", year: 1993, discTotal: 3,
	})
	if got := countRows(t, st, "album"); got != 2 {
		t.Errorf("albums = %d, want the Bonus folder keyed as an album of its own", got)
	}
}

// TestDiscFolderMembersStayOneAlbumThroughAnEdit: an edit re-resolves its members through
// the same album folder a scan keys them by, so retitling every disc of a disc-folder album
// moves it in place through the rename pre-pass, and an edit of one member, which
// re-resolves that member alone, keeps it one album.
func TestDiscFolderMembersStayOneAlbumThroughAnEdit(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	var pids []model.PID
	for i, disc := range []string{"CD1", "CD2"} {
		pids = append(pids, putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Floyd/Wall/" + disc + "/1.flac", essence: "w" + string(rune('1'+i)), content: "wc" + string(rune('1'+i)),
			title: "Song " + disc, artist: "Pink Floyd", albumArt: "Pink Floyd", album: "The Wall",
		}).ItemPID)
	}
	if got := countRows(t, st, "album"); got != 1 {
		t.Fatalf("albums = %d before the edit, want 1", got)
	}
	albumID := scalarInt(t, st, "SELECT id FROM album")
	seq, _ := st.LatestChangeSeq(context.Background())
	if _, err := st.EditManyFields(context.Background(), pids, map[string]string{"album": "The Wall (Remastered)"},
		model.Attribution{Source: model.SourceUser}, model.LockOn, false, false); err != nil {
		t.Fatalf("retitle: %v", err)
	}
	if got := countRows(t, st, "album"); got != 1 {
		t.Errorf("albums = %d after retitling every disc, want 1", got)
	}
	if id := scalarInt(t, st, "SELECT id FROM album"); id != albumID {
		t.Errorf("album id = %d, want %d renamed in place", id, albumID)
	}
	// In place, not forked onto a new row and carried back by the re-key reconcile.
	for _, op := range []model.ChangeOp{model.OpCreate, model.OpDelete} {
		if n := changeCount(t, st, seq, "album", op); n != 0 {
			t.Errorf("album %s deltas = %d, want none", op, n)
		}
	}
	if err := st.EditItemField(context.Background(), pids[1], "genre", "Rock",
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("edit genre: %v", err)
	}
	if got := countRows(t, st, "album"); got != 1 {
		t.Errorf("albums = %d after editing one disc's member, want 1", got)
	}
}

// TestOddTrackYearKeepsOneAlbum: a release whose tracks disagree on the year, eleven
// tagged 2015 and one 2016, is one album under one release group rather than two albums
// keyed apart by the year.
func TestOddTrackYearKeepsOneAlbum(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for i := 1; i <= 12; i++ {
		year := 2015
		if i == 7 {
			year = 2016
		}
		n := strconv.Itoa(i)
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Anderson .Paak/Malibu/" + n + ".flac", essence: "malibu" + n, content: "mc" + n,
			title: "Track " + n, artist: "Anderson .Paak", albumArt: "Anderson .Paak", album: "Malibu", year: year,
		})
	}
	if got := countRows(t, st, "album"); got != 1 {
		t.Errorf("albums = %d, want 1 for twelve tracks of one release", got)
	}
	if got := countRows(t, st, "release_group"); got != 1 {
		t.Errorf("release groups = %d, want 1", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM track WHERE album_id = (SELECT id FROM album)"); n != 12 {
		t.Errorf("tracks on the album = %d, want 12", n)
	}
}

// TestYearlessTrackJoinsItsDatedSiblings: a track with no year tag lands on the album its
// dated siblings make, whichever is read first.
func TestYearlessTrackJoinsItsDatedSiblings(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for i, year := range []int{0, 1977, 1977} {
		n := strconv.Itoa(i + 1)
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/Pink Floyd/Animals/" + n + ".flac", essence: "an" + n, content: "ac" + n,
			title: "Track " + n, artist: "Pink Floyd", albumArt: "Pink Floyd", album: "Animals", year: year,
		})
	}
	if got := countRows(t, st, "album"); got != 1 {
		t.Errorf("albums = %d, want the year-less track on its siblings' album", got)
	}
}
