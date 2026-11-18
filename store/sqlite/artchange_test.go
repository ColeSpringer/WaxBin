package sqlite

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// changeLog reads what the change log named since a mark, as "type pid" lines, and moves
// the mark to its end.
type changeLog struct {
	t   *testing.T
	st  *Store
	seq int64
}

func newChangeLog(t *testing.T, st *Store) *changeLog {
	t.Helper()
	l := &changeLog{t: t, st: st}
	l.since()
	return l
}

func (l *changeLog) since() []string {
	l.t.Helper()
	ctx := context.Background()
	changes, err := l.st.ChangesSince(ctx, l.seq)
	if err != nil {
		l.t.Fatalf("changes: %v", err)
	}
	var out []string
	for _, c := range changes {
		out = append(out, c.EntityType+" "+string(c.EntityPID))
		l.seq = c.Seq
	}
	return out
}

func pidWhere(t *testing.T, st *Store, table, col, val string) model.PID {
	t.Helper()
	return model.PID(scalarStr(t, st, "SELECT pid FROM "+table+" WHERE "+col+" = ?", val))
}

// albumTrack puts one track of an album as a scan does, with the cover it found.
func albumTrack(t *testing.T, st *Store, libID int64, n int, album string, cover *model.ArtImage) model.PID {
	t.Helper()
	in := trackSpecInput(libID, trackSpec{
		path: fmt.Sprintf("/lib/%s/%d.flac", album, n), essence: fmt.Sprintf("%s-e%d", album, n),
		content: fmt.Sprintf("%s-c%d", album, n), title: fmt.Sprintf("%s %d", album, n),
		artist: "Artist of " + album, albumArt: "Artist of " + album, album: album,
	})
	in.CoverArt, in.NoCover, in.PreserveLocks = cover, cover == nil, true
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s %d: %v", album, n, err)
	}
	return res.ItemPID
}

// TestAnAlbumIsNamedWhenTheCoverItBorrowsMoves: an album's cover is worked out from its
// tracks, so a track gaining, changing or losing its cover can move what the album's
// card shows with no write to the album. The change log names the album then, and only
// then: a later track's cover the album does not borrow says nothing about it.
func TestAnAlbumIsNamedWhenTheCoverItBorrowsMoves(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	first, second, other := testPNG(t, 40, 40), testPNG(t, 41, 41), testPNG(t, 42, 42)
	one := albumTrack(t, st, lib.ID, 1, "Al", nil)
	two := albumTrack(t, st, lib.ID, 2, "Al", nil)
	album := "album " + string(pidWhere(t, st, "album", "title", "Al"))
	log := newChangeLog(t, st)
	names := func(what string, wantAlbum bool) {
		t.Helper()
		got := log.since()
		if slices.Contains(got, album) != wantAlbum {
			t.Errorf("%s: the album named = %v, want %v, in %v", what, !wantAlbum, wantAlbum, got)
		}
		if n := len(got) - len(slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s == album })); n > 1 {
			t.Errorf("%s: the album is named %d times: %v", what, n, got)
		}
	}

	albumTrack(t, st, lib.ID, 1, "Al", first)
	names("its first track gains a cover", true)
	albumTrack(t, st, lib.ID, 2, "Al", other)
	names("a later track gains one the album does not borrow", false)
	albumTrack(t, st, lib.ID, 1, "Al", second)
	names("the borrowed cover is replaced", true)
	albumTrack(t, st, lib.ID, 1, "Al", second)
	names("the same cover is read again", false)
	albumTrack(t, st, lib.ID, 1, "Al", nil)
	names("the borrowed cover goes and the album falls to the next track's", true)

	user := model.Attribution{Source: model.SourceUser}
	if _, err := st.SetItemArt(ctx, one, model.ArtRoleFront, first.Data, "", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	names("a cover is set on the first track by hand", true)
	if _, err := st.SetEntityArt(ctx, model.ArtTrack, one, model.ArtRoleFront, nil, "", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	names("it is cleared through the entity surface", true)

	// A track leaving takes the cover the album borrowed with it.
	in := trackSpecInput(lib.ID, trackSpec{
		path: "/lib/Al/2.flac", essence: "Al-e2", content: "Al-c2-retagged", title: "Al 2",
		artist: "Artist of Al", albumArt: "Artist of Al", album: "Elsewhere",
	})
	in.CoverArt, in.PreserveLocks = other, true
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatal(err)
	}
	names("the track it borrowed from moves to another album", true)

	// An album holding a cover of its own draws that, whatever its tracks do.
	if _, err := st.SetEntityArt(ctx, model.ArtAlbum, pidWhere(t, st, "album", "title", "Al"), model.ArtRoleFront, first.Data, "", user, model.LockOf(false), false); err != nil {
		t.Fatal(err)
	}
	log.since()
	albumTrack(t, st, lib.ID, 1, "Al", second)
	names("a track changes cover under an album with its own", false)
	_ = two
}

// TestCardsThatInheritACoverAreNamedWithIt: a release group's cover is what its coverless
// albums draw, and an artist's what its groups and their albums do. A change to one names
// those cards, and leaves out the ones that hold a cover nearer to hand.
func TestCardsThatInheritACoverAreNamedWithIt(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	user := model.Attribution{Source: model.SourceUser}
	albumTrack(t, st, lib.ID, 1, "Bare", nil)
	albumTrack(t, st, lib.ID, 1, "Covered", testPNG(t, 40, 40))
	pid := func(table, col, val string) model.PID { return pidWhere(t, st, table, col, val) }
	group := func(album string) model.PID {
		return model.PID(scalarStr(t, st,
			"SELECT rg.pid FROM album al JOIN release_group rg ON rg.id = al.release_group_id WHERE al.title = ?", album))
	}
	set := func(typ model.ArtEntity, p model.PID, img *model.ArtImage) {
		t.Helper()
		var data []byte
		if img != nil {
			data = img.Data
		}
		if _, err := st.SetEntityArt(ctx, typ, p, model.ArtRoleFront, data, "", user, model.LockOf(false), false); err != nil {
			t.Fatalf("set %s: %v", typ, err)
		}
	}
	log := newChangeLog(t, st)
	expect := func(what string, want ...string) {
		t.Helper()
		got := log.since()
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s named %v, want %v", what, got, want)
		}
	}
	bareAlbum, bareGroup := "album "+string(pid("album", "title", "Bare")), "release_group "+string(group("Bare"))
	coveredGroup := "release_group " + string(group("Covered"))
	bareArtist, coveredArtist := pid("artist", "name", "Artist of Bare"), pid("artist", "name", "Artist of Covered")

	// The artist's picture reaches its group and that group's album, neither holding one.
	set(model.ArtArtist, bareArtist, testPNG(t, 41, 41))
	expect("an artist's cover", "artist "+string(bareArtist), bareGroup, bareAlbum)
	// The other artist's reaches its group, and stops at the album, which has a track's.
	set(model.ArtArtist, coveredArtist, testPNG(t, 42, 42))
	expect("an artist's cover over a covered album", "artist "+string(coveredArtist), coveredGroup)

	// The group's own picture reaches its album; the artist's no longer reaches either.
	set(model.ArtReleaseGroup, group("Bare"), testPNG(t, 43, 43))
	expect("a release group's cover", bareGroup, bareAlbum)
	set(model.ArtArtist, bareArtist, testPNG(t, 44, 44))
	expect("an artist's cover under a group with its own", "artist "+string(bareArtist))
	// The same picture set again moves nothing below it.
	set(model.ArtReleaseGroup, group("Bare"), testPNG(t, 43, 43))
	expect("the same cover again", bareGroup)
	// Cleared, the album falls back to the artist's.
	set(model.ArtReleaseGroup, group("Bare"), nil)
	expect("a release group's cover cleared", bareGroup, bareAlbum)
}
